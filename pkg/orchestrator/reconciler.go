package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/brotherlogic/speculate/pkg/dcm"
	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/parser"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	// DefaultAttemptTimeout is the default inactivity timeout per DCM attempt before triggering a strike.
	DefaultAttemptTimeout = 30 * time.Minute

	maxStrikesPerPhase = 3
	badgeBranchName    = "chore/readme-badge"
)

// Reconciler drives the idempotent, single-flight Phase 4 Red-to-Green lifecycle for enrolled repositories.
type Reconciler struct {
	sf                  singleflight.Group
	token               string
	githubClientFactory func(token, owner, repo string) ghclient.Client
	dcmClient           dcm.Client
	evaluator           *evaluator.Evaluator
	synthesizer         *synthesizer.Synthesizer
	store               pstore.Store
	harness             dcm.Harness
	model               string
	attemptTimeout      time.Duration
	pollInterval        time.Duration
	nowFn               func() time.Time
	cloneFn             func(ctx context.Context, repoURL, targetDir string) error
	legacyMainBadge     bool
}

// NewReconciler constructs a Phase 4 Reconciler with sensible defaults.
func NewReconciler(
	token string,
	githubClientFactory func(token, owner, repo string) ghclient.Client,
	dcmClient dcm.Client,
	eval *evaluator.Evaluator,
	synth *synthesizer.Synthesizer,
	store pstore.Store,
) *Reconciler {
	if githubClientFactory == nil {
		githubClientFactory = func(token, owner, repo string) ghclient.Client {
			return ghclient.NewRealClient(token, owner, repo)
		}
	}
	if eval == nil {
		eval = evaluator.NewEvaluator(nil)
	}
	if synth == nil {
		synth = synthesizer.NewSynthesizer(nil)
	}
	if isNilInterface(dcmClient) {
		dcmClient = nil
	}

	return &Reconciler{
		token:               token,
		githubClientFactory: githubClientFactory,
		dcmClient:           dcmClient,
		evaluator:           eval,
		synthesizer:         synth,
		store:               store,
		harness:             dcm.HarnessAntigravity,
		attemptTimeout:      DefaultAttemptTimeout,
		pollInterval:        50 * time.Millisecond,
		nowFn:               time.Now,
		cloneFn:             defaultClone,
	}
}

// WithDCMClient configures the devcontainer-manager client (optional; nil enables tracking/card-only fallback).
func (r *Reconciler) WithDCMClient(client dcm.Client) *Reconciler {
	if isNilInterface(client) {
		r.dcmClient = nil
		return r
	}
	r.dcmClient = client
	return r
}

// WithHarness configures the DCM agent harness (defaults to HarnessAntigravity).
func (r *Reconciler) WithHarness(harness dcm.Harness) *Reconciler {
	if harness != dcm.HarnessUnspecified {
		r.harness = harness
	}
	return r
}

// WithModel configures the LLM model passed to DCM Up requests.
func (r *Reconciler) WithModel(model string) *Reconciler {
	r.model = model
	return r
}

// WithAttemptTimeout overrides the per-attempt inactivity timeout (defaults to 30m).
func (r *Reconciler) WithAttemptTimeout(d time.Duration) *Reconciler {
	if d > 0 {
		r.attemptTimeout = d
	}
	return r
}

// WithPollInterval overrides the DCM WaitForGone poll interval.
func (r *Reconciler) WithPollInterval(d time.Duration) *Reconciler {
	if d > 0 {
		r.pollInterval = d
	}
	return r
}

// WithNowFn overrides the clock function for deterministic testing.
func (r *Reconciler) WithNowFn(fn func() time.Time) *Reconciler {
	if fn != nil {
		r.nowFn = fn
	}
	return r
}

// WithCloneFn configures a custom clone function for hermetic testing.
func (r *Reconciler) WithCloneFn(fn func(ctx context.Context, repoURL, targetDir string) error) *Reconciler {
	if fn != nil {
		r.cloneFn = fn
	} else {
		r.cloneFn = defaultClone
	}
	return r
}

// WithLegacyMainBadge controls whether card-only fallback mode (dcmClient == nil)
// writes README.md directly on main for backward compatibility with legacy callers.
func (r *Reconciler) WithLegacyMainBadge(enabled bool) *Reconciler {
	r.legacyMainBadge = enabled
	return r
}

// Reconcile executes a single-flight reconciliation pass for the given repository.
func (r *Reconciler) Reconcile(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
	owner, repoName, err := ghclient.ParseRepo(repo)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid repository %q: %v", repo, err)
	}
	canonicalRepo := fmt.Sprintf("%s/%s", owner, repoName)

	res, err, _ := r.sf.Do(canonicalRepo, func() (any, error) {
		return r.reconcileOnce(ctx, owner, repoName, canonicalRepo)
	})
	if err != nil {
		return nil, err
	}

	resp, ok := res.(*pb.EnrollResponse)
	if !ok {
		return nil, status.Error(codes.Internal, "unexpected response type from reconciler")
	}
	return proto.Clone(resp).(*pb.EnrollResponse), nil
}

// Enroll satisfies enroll.EnrollmentRunner by delegating to Reconcile.
func (r *Reconciler) Enroll(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
	return r.Reconcile(ctx, repo)
}

// ListEnrollments returns all tracked repository enrollment records from storage.
func (r *Reconciler) ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
	if isNilInterface(r.store) {
		return nil, nil
	}
	return r.store.ListEnrollments(ctx)
}

type reconcileState struct {
	owner                string
	repoName             string
	canonicalRepo        string
	gh                   ghclient.Client
	enrolledAt           int64
	priorStage           pb.LifecycleStage
	priorAlignment       int32
	priorIssueURL        string
	priorStatusMsg       string
	activeBranch         string
	activePRNumber       int32
	activeIssueNumber    int32
	approvedTestSHA      string
	lastEvaluatedSHA     string
	attemptCount         int32
	activeContainerID    string
	attemptStartedAtUnix int64
	wasUnstalled         bool
}

func (s *reconcileState) clearScenario() {
	s.activeBranch = ""
	s.activePRNumber = 0
	s.activeIssueNumber = 0
	s.approvedTestSHA = ""
	s.lastEvaluatedSHA = ""
	s.attemptCount = 0
	s.activeContainerID = ""
	s.attemptStartedAtUnix = 0
}

type mainSnapshot struct {
	hasProto     bool
	specsMissing bool
	spec         *parser.Specification
	evalResult   *evaluator.EvaluationResult
	tempDir      string
}

func (r *Reconciler) reconcileOnce(ctx context.Context, owner, repoName, canonicalRepo string) (*pb.EnrollResponse, error) {
	now := r.nowFn().Unix()
	st := &reconcileState{
		owner:          owner,
		repoName:       repoName,
		canonicalRepo:  canonicalRepo,
		enrolledAt:     now,
		priorStage:     pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
		priorStatusMsg: "evaluating repository",
	}

	if err := r.loadOrInitRecord(ctx, st, now); err != nil {
		return nil, err
	}

	snap, cleanup, err := r.cloneMain(ctx, st)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return r.recordFault(ctx, st, err)
	}

	st.gh = r.githubClientFactory(r.token, owner, repoName)

	// 1. Seed Check (AWAITING_SEED)
	if !snap.hasProto || snap.specsMissing {
		return r.handleAwaitingSeed(ctx, st)
	}

	// Auto-close any open speculate-seed issue once seed files exist on main.
	if err := r.closeOpenSeedIssue(ctx, st.gh); err != nil {
		return r.recordFault(ctx, st, err)
	}

	// 2. Parse tests/ & Evaluate Alignment on main
	testsDir := filepath.Join(snap.tempDir, "tests")
	suite, err := parser.ParseTestDir(testsDir)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("parsing tests directory: %w", err))
	}

	evalResult, err := r.evaluator.Evaluate(ctx, snap.spec, suite)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("evaluating alignment: %w", err))
	}
	snap.evalResult = evalResult

	if err := st.gh.EnsureLabels(ctx, ghclient.RequiredLabels()); err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("ensuring labels on %s: %w", canonicalRepo, err))
	}

	// 3. Stalled Check & GitHub-Native Un-stall
	stalledIssue, err := st.gh.FindOpenIssueByLabel(ctx, ghclient.LabelStalled)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("checking for open issue with label %s: %w", ghclient.LabelStalled, err))
	}

	if stalledIssue != nil {
		if err := r.downContainer(ctx, st.activeContainerID); err != nil {
			return r.recordFault(ctx, st, err)
		}
		st.activeContainerID = ""
		if st.activeIssueNumber == 0 {
			st.activeIssueNumber = int32(stalledIssue.Number)
		}
		return r.saveAndRespond(
			ctx, st,
			pb.LifecycleStage_LIFECYCLE_STAGE_STALLED,
			int32(evalResult.Percentage),
			stalledIssue.HTMLURL,
			fmt.Sprintf("alignment stalled (%d%%)", evalResult.Percentage),
			fmt.Sprintf("repository enrolled: alignment stalled (%d%%)", evalResult.Percentage),
		)
	}

	if st.priorStage == pb.LifecycleStage_LIFECYCLE_STAGE_STALLED {
		st.attemptCount = 0
		st.attemptStartedAtUnix = 0
		st.lastEvaluatedSHA = ""
		st.wasUnstalled = true
		if st.activeIssueNumber > 0 {
			if iss, getErr := st.gh.GetIssue(ctx, int(st.activeIssueNumber)); getErr == nil && iss != nil && !strings.EqualFold(iss.State, "open") {
				st.activeIssueNumber = 0
			}
		}
	}

	// 4. Tracking/Card-Only Fallback when DCM is not configured
	if !r.hasDCM() {
		return r.reconcileCardOnly(ctx, st, snap)
	}

	// 5. Phase 4 Autonomous Orchestration (DCM enabled)
	return r.reconcilePhase4(ctx, st, snap)
}

func (r *Reconciler) loadOrInitRecord(ctx context.Context, st *reconcileState, now int64) error {
	if isNilInterface(r.store) {
		return nil
	}

	existing, err := r.store.GetEnrollment(ctx, st.canonicalRepo)
	if err == nil && existing != nil {
		if existing.GetEnrolledAtUnix() > 0 {
			st.enrolledAt = existing.GetEnrolledAtUnix()
		}
		if existing.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_UNSPECIFIED {
			st.priorStage = existing.GetStage()
		}
		st.priorAlignment = existing.GetAlignmentPercentage()
		st.priorIssueURL = existing.GetIssueUrl()
		if existing.GetStatusMessage() != "" {
			st.priorStatusMsg = existing.GetStatusMessage()
		}
		st.activeBranch = existing.GetActiveBranch()
		st.activePRNumber = existing.GetActivePrNumber()
		st.activeIssueNumber = existing.GetActiveIssueNumber()
		st.approvedTestSHA = existing.GetApprovedTestSha()
		st.lastEvaluatedSHA = existing.GetLastEvaluatedSha()
		st.attemptCount = existing.GetAttemptCount()
		st.activeContainerID = existing.GetActiveContainerId()
		st.attemptStartedAtUnix = existing.GetAttemptStartedAtUnix()
		return nil
	}

	if errors.Is(err, pstore.ErrNotFound) || (err == nil && existing == nil) {
		initialRecord := &pb.EnrollmentRecord{
			Repository:          st.canonicalRepo,
			AlignmentPercentage: 0,
			IssueUrl:            "",
			EnrolledAtUnix:      st.enrolledAt,
			LastEvaluatedAtUnix: now,
			StatusMessage:       "evaluating repository",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
			LastError:           "",
		}
		if saveErr := r.store.SaveEnrollment(ctx, initialRecord); saveErr != nil {
			return fmt.Errorf("persisting initial enrollment record: %w", saveErr)
		}
	}
	return nil
}

func (r *Reconciler) cloneMain(ctx context.Context, st *reconcileState) (*mainSnapshot, func(), error) {
	tempDir, err := os.MkdirTemp("", "speculate-reconcile-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating temp directory for clone: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }

	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", st.owner, st.repoName)
	if r.token != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", r.token, st.owner, st.repoName)
	}

	cloneFunc := r.cloneFn
	if cloneFunc == nil {
		cloneFunc = defaultClone
	}
	if err := cloneFunc(ctx, cloneURL, tempDir); err != nil {
		return nil, cleanup, fmt.Errorf("cloning repository %s: %w", st.canonicalRepo, err)
	}

	hasProto := hasSeedProto(filepath.Join(tempDir, "proto"))
	combinedSpec, specsMissing, err := parseSeedSpecs(filepath.Join(tempDir, "specs"), st.canonicalRepo)
	if err != nil {
		return nil, cleanup, err
	}

	return &mainSnapshot{
		hasProto:     hasProto,
		specsMissing: specsMissing,
		spec:         combinedSpec,
		tempDir:      tempDir,
	}, cleanup, nil
}

func (r *Reconciler) handleAwaitingSeed(ctx context.Context, st *reconcileState) (*pb.EnrollResponse, error) {
	if r.hasDCM() && st.activeContainerID != "" {
		if err := r.downContainer(ctx, st.activeContainerID); err != nil {
			return r.recordFault(ctx, st, err)
		}
		st.activeContainerID = ""
	}

	seedIssue, err := st.gh.FindOpenIssueByLabel(ctx, ghclient.LabelSeed)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("checking for open seed issue: %w", err))
	}
	seedIssueURL := ""
	if seedIssue != nil {
		seedIssueURL = seedIssue.HTMLURL
	}

	return r.saveAndRespond(
		ctx, st,
		pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
		0,
		seedIssueURL,
		"awaiting initial seed proto and spec files",
		"repository enrolled: awaiting initial seed proto and spec files",
	)
}

func (r *Reconciler) closeOpenSeedIssue(ctx context.Context, gh ghclient.Client) error {
	seedIssue, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelSeed)
	if err != nil {
		return fmt.Errorf("checking for open seed issue: %w", err)
	}
	if seedIssue == nil {
		return nil
	}

	commentBody := "Seed protobuf and specification files detected on `main`. Closing this tracking issue and initiating the Speculate autonomous alignment loop."
	if err := gh.AddIssueComment(ctx, seedIssue.Number, commentBody); err != nil {
		return fmt.Errorf("commenting on seed issue #%d: %w", seedIssue.Number, err)
	}
	if err := gh.CloseIssue(ctx, seedIssue.Number); err != nil {
		return fmt.Errorf("closing seed issue #%d: %w", seedIssue.Number, err)
	}
	return nil
}

func (r *Reconciler) reconcileCardOnly(ctx context.Context, st *reconcileState, snap *mainSnapshot) (*pb.EnrollResponse, error) {
	evalResult := snap.evalResult
	if r.legacyMainBadge {
		if err := st.gh.UpdateReadmeBadge(ctx, "main", evalResult.BadgeMarkdown); err != nil {
			return r.recordFault(ctx, st, fmt.Errorf("updating README badge on %s: %w", st.canonicalRepo, err))
		}
	}

	alignIssue, err := st.gh.FindOpenIssueByLabel(ctx, ghclient.LabelAlign)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("checking for open issue with label %s: %w", ghclient.LabelAlign, err))
	}
	if alignIssue != nil {
		return r.saveAndRespond(
			ctx, st,
			pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
			int32(evalResult.Percentage),
			alignIssue.HTMLURL,
			fmt.Sprintf("aligning (%d%%)", evalResult.Percentage),
			fmt.Sprintf("repository enrolled: aligning (%d%%)", evalResult.Percentage),
		)
	}

	if evalResult.Percentage == 100 {
		if !r.legacyMainBadge {
			if err := r.ensureReadmeBadgePR(ctx, st.gh, evalResult.BadgeMarkdown, evalResult.Percentage); err != nil {
				return r.recordFault(ctx, st, err)
			}
		}
		return r.saveAndRespond(
			ctx, st,
			pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
			int32(evalResult.Percentage),
			"",
			"fully aligned (100%)",
			"repository enrolled: fully aligned (100%)",
		)
	}

	loopIssue, err := r.ensureScenarioLoopIssue(ctx, st, snap.spec, evalResult, snap.tempDir)
	if err != nil {
		return r.recordFault(ctx, st, err)
	}
	st.activeIssueNumber = int32(loopIssue.Number)

	return r.saveAndRespond(
		ctx, st,
		pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
		int32(evalResult.Percentage),
		loopIssue.HTMLURL,
		fmt.Sprintf("alignment %d%%", evalResult.Percentage),
		fmt.Sprintf("repository enrolled with %d%% alignment", evalResult.Percentage),
	)
}

func (r *Reconciler) reconcilePhase4(ctx context.Context, st *reconcileState, snap *mainSnapshot) (*pb.EnrollResponse, error) {
	pr, err := r.resolveActivePR(ctx, st)
	if err != nil {
		return r.recordFault(ctx, st, err)
	}

	if pr != nil && pr.Merged {
		if err := r.finalizeMergedPR(ctx, st, pr); err != nil {
			return r.recordFault(ctx, st, err)
		}
		pr = nil
	} else if pr != nil && strings.EqualFold(pr.State, "closed") {
		if err := r.downContainer(ctx, st.activeContainerID); err != nil {
			return r.recordFault(ctx, st, err)
		}
		st.clearScenario()
		pr = nil
	}

	// No active scenario branch yet: either ALIGNED or start a new Red phase.
	if st.activeBranch == "" {
		if snap.evalResult.Percentage == 100 {
			return r.completeAligned(ctx, st, snap.evalResult)
		}
		return r.startRedScenario(ctx, st, snap)
	}

	alignIssue, err := st.gh.FindOpenIssueByLabel(ctx, ghclient.LabelAlign)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("checking open align issue: %w", err))
	}

	isGreen := st.approvedTestSHA != "" || st.priorStage == pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING || alignIssue != nil
	if !isGreen && pr != nil {
		approval, err := r.findOwnerApproval(ctx, st, pr.Number)
		if err != nil {
			return r.recordFault(ctx, st, err)
		}
		if approval != nil {
			return r.transitionRedToGreen(ctx, st, pr, approval, snap.evalResult)
		}
	}

	if isGreen {
		return r.reconcileGreen(ctx, st, pr, alignIssue, snap.evalResult)
	}

	if st.priorStage == pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW && !st.wasUnstalled {
		issueURL, err := r.resolveIssueURL(ctx, st, ghclient.LabelAgenticLoop)
		if err != nil {
			return r.recordFault(ctx, st, err)
		}
		return r.saveAndRespond(
			ctx, st,
			pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
			int32(snap.evalResult.Percentage),
			issueURL,
			fmt.Sprintf("awaiting test review (%d%%)", snap.evalResult.Percentage),
			fmt.Sprintf("repository enrolled: awaiting test review (%d%%)", snap.evalResult.Percentage),
		)
	}

	return r.reconcileRed(ctx, st, pr, snap)
}

func (r *Reconciler) completeAligned(ctx context.Context, st *reconcileState, evalResult *evaluator.EvaluationResult) (*pb.EnrollResponse, error) {
	if err := r.ensureReadmeBadgePR(ctx, st.gh, evalResult.BadgeMarkdown, evalResult.Percentage); err != nil {
		return r.recordFault(ctx, st, err)
	}
	st.clearScenario()
	return r.saveAndRespond(
		ctx, st,
		pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
		int32(evalResult.Percentage),
		"",
		"fully aligned (100%)",
		"repository enrolled: fully aligned (100%)",
	)
}

func (r *Reconciler) startRedScenario(ctx context.Context, st *reconcileState, snap *mainSnapshot) (*pb.EnrollResponse, error) {
	stage, req := pickNextRequirement(snap.spec, snap.evalResult)
	protoContract := findProtoContracts(snap.tempDir)

	card, err := r.synthesizer.GenerateScenarioCard(ctx, stage, req, protoContract)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("generating scenario card: %w", err))
	}

	loopIssue, err := r.ensureLoopIssueWithCard(ctx, st.gh, card)
	if err != nil {
		return r.recordFault(ctx, st, err)
	}

	branchSeed := card.ID
	if strings.TrimSpace(branchSeed) == "" {
		branchSeed = card.Title
	}
	branchName := ScenarioBranchName(branchSeed)

	if _, err := st.gh.GetBranch(ctx, branchName); err != nil {
		if _, createErr := st.gh.CreateBranch(ctx, branchName, "main"); createErr != nil {
			return r.recordFault(ctx, st, fmt.Errorf("creating branch %q: %w", branchName, createErr))
		}
	}

	if err := r.updateBranchReadmeBadgeIfNeeded(ctx, st.gh, branchName, snap.evalResult.BadgeMarkdown); err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("updating README badge on %s: %w", branchName, err))
	}

	attemptCount := int32(1)
	if st.wasUnstalled {
		attemptCount = 0
	}

	if err := r.launchContainer(ctx, st, branchName, int32(loopIssue.Number), 0, BuildRedPrompt(st.canonicalRepo, branchName, card, "")); err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("launching Red DCM session on %s: %w", branchName, err))
	}

	st.activeBranch = branchName
	st.activePRNumber = 0
	st.activeIssueNumber = int32(loopIssue.Number)
	st.approvedTestSHA = ""
	st.lastEvaluatedSHA = ""
	st.attemptCount = attemptCount

	return r.saveAndRespond(
		ctx, st,
		pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
		int32(snap.evalResult.Percentage),
		loopIssue.HTMLURL,
		fmt.Sprintf("generating Red test (%d%%)", snap.evalResult.Percentage),
		fmt.Sprintf("repository enrolled: generating Red test (%d%%)", snap.evalResult.Percentage),
	)
}

func (r *Reconciler) reconcileRed(ctx context.Context, st *reconcileState, pr *ghclient.PullRequest, snap *mainSnapshot) (*pb.EnrollResponse, error) {
	loopIssue, err := r.ensureScenarioLoopIssue(ctx, st, snap.spec, snap.evalResult, snap.tempDir)
	if err != nil {
		return r.recordFault(ctx, st, err)
	}
	st.activeIssueNumber = int32(loopIssue.Number)

	// 1. Check PR check runs on pr.HeadSHA if not already evaluated
	if pr != nil && pr.HeadSHA != "" && pr.HeadSHA != st.lastEvaluatedSHA {
		runs, err := st.gh.ListCheckRunsForRef(ctx, pr.HeadSHA)
		if err != nil {
			return r.recordFault(ctx, st, fmt.Errorf("listing check runs for %s: %w", pr.HeadSHA, err))
		}
		byName := latestCheckRunsByName(runs)
		compileRun := byName["compile"]
		testRun := byName["test"]

		if isSuccess(compileRun) && isCompleted(testRun) && strings.EqualFold(testRun.Conclusion, "failure") {
			if err := r.downContainer(ctx, st.activeContainerID); err != nil {
				return r.recordFault(ctx, st, err)
			}
			st.activeContainerID = ""
			st.lastEvaluatedSHA = pr.HeadSHA
			return r.saveAndRespond(
				ctx, st,
				pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
				int32(snap.evalResult.Percentage),
				loopIssue.HTMLURL,
				fmt.Sprintf("Red test verified; awaiting review (%d%%)", snap.evalResult.Percentage),
				fmt.Sprintf("Red test verified on PR #%d; awaiting review", pr.Number),
			)
		}

		var diag string
		if isFailureConclusion(compileRun) {
			diag = formatCheckFailure("Red phase compile check failed", compileRun)
		} else if isSuccess(compileRun) && isSuccess(testRun) {
			diag = "Red phase test passed GREEN (tautological test); tests must FAIL against unimplemented code."
		} else if isCompleted(compileRun) && isCompleted(testRun) {
			diag = formatCheckFailure("Red phase check runs did not produce compile=success, test=failure", compileRun, testRun)
		}

		if diag != "" {
			st.lastEvaluatedSHA = pr.HeadSHA
			if !st.wasUnstalled {
				return r.handlePhaseStrike(ctx, st, loopIssue, snap.evalResult.Percentage, diag, false)
			}
		}
	}

	// 2. If no active container is running (e.g., resumed after un-stall or transient Up fault), launch a fresh Red DCM session.
	if st.activeContainerID == "" {
		if pr != nil && pr.HeadSHA != "" {
			st.lastEvaluatedSHA = pr.HeadSHA
		}
		if err := r.launchContainer(ctx, st, st.activeBranch, int32(loopIssue.Number), st.activePRNumber, BuildRedPromptFromMarkdown(st.canonicalRepo, st.activeBranch, loopIssue.Body, "")); err != nil {
			return r.recordFault(ctx, st, fmt.Errorf("launching Red DCM session on %s: %w", st.activeBranch, err))
		}
		return r.saveAndRespond(
			ctx, st,
			pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
			int32(snap.evalResult.Percentage),
			loopIssue.HTMLURL,
			fmt.Sprintf("generating Red test (%d%%)", snap.evalResult.Percentage),
			fmt.Sprintf("generating Red test (%d%%)", snap.evalResult.Percentage),
		)
	}

	// 3. Check container health for DCM_FAILED / DCM_HARD_FAILED
	if diag, err := r.checkContainerFailed(ctx, st.activeContainerID); err != nil {
		return r.recordFault(ctx, st, err)
	} else if diag != "" {
		return r.handlePhaseStrike(ctx, st, loopIssue, snap.evalResult.Percentage, diag, false)
	}

	// 4. Check 30m inactivity timeout without a new evaluated SHA
	noNewSHA := pr == nil || pr.HeadSHA == "" || pr.HeadSHA == st.lastEvaluatedSHA
	if st.activeContainerID != "" && noNewSHA && r.attemptTimedOut(st.attemptStartedAtUnix) {
		diag := fmt.Sprintf("Red attempt %d timed out after %s without a new commit on %s.", st.attemptCount, r.attemptTimeout, st.activeBranch)
		return r.handlePhaseStrike(ctx, st, loopIssue, snap.evalResult.Percentage, diag, false)
	}

	return r.saveAndRespond(
		ctx, st,
		pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
		int32(snap.evalResult.Percentage),
		loopIssue.HTMLURL,
		fmt.Sprintf("generating Red test (%d%%)", snap.evalResult.Percentage),
		fmt.Sprintf("generating Red test (%d%%)", snap.evalResult.Percentage),
	)
}

func (r *Reconciler) transitionRedToGreen(
	ctx context.Context,
	st *reconcileState,
	pr *ghclient.PullRequest,
	approval *ghclient.PullRequestReview,
	evalResult *evaluator.EvaluationResult,
) (*pb.EnrollResponse, error) {
	approvedSHA := strings.TrimSpace(approval.CommitID)
	if approvedSHA == "" {
		approvedSHA = pr.HeadSHA
	}

	if err := r.closeOpenIssuesByLabel(ctx, st.gh, ghclient.LabelAgenticLoop, st.activeIssueNumber); err != nil {
		return r.recordFault(ctx, st, err)
	}

	alignIssue, err := r.ensureAlignIssue(ctx, st.gh, pr, approvedSHA)
	if err != nil {
		return r.recordFault(ctx, st, err)
	}

	if err := r.downAndWaitForGone(ctx, st.activeContainerID); err != nil {
		return r.recordFault(ctx, st, err)
	}
	st.activeContainerID = ""

	st.approvedTestSHA = approvedSHA
	st.activeIssueNumber = int32(alignIssue.Number)
	st.activePRNumber = int32(pr.Number)
	st.attemptCount = 1
	st.lastEvaluatedSHA = ""

	if err := r.launchContainer(ctx, st, st.activeBranch, int32(alignIssue.Number), int32(pr.Number), BuildGreenPrompt(st.canonicalRepo, st.activeBranch, approvedSHA, "")); err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("launching Green DCM session on %s: %w", st.activeBranch, err))
	}

	return r.saveAndRespond(
		ctx, st,
		pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
		int32(evalResult.Percentage),
		alignIssue.HTMLURL,
		fmt.Sprintf("aligning (%d%%)", evalResult.Percentage),
		fmt.Sprintf("Red test approved; started Green alignment on PR #%d", pr.Number),
	)
}

func (r *Reconciler) reconcileGreen(
	ctx context.Context,
	st *reconcileState,
	pr *ghclient.PullRequest,
	alignIssue *ghclient.Issue,
	evalResult *evaluator.EvaluationResult,
) (*pb.EnrollResponse, error) {
	if alignIssue == nil {
		var err error
		alignIssue, err = r.ensureAlignIssue(ctx, st.gh, pr, st.approvedTestSHA)
		if err != nil {
			return r.recordFault(ctx, st, err)
		}
	}
	st.activeIssueNumber = int32(alignIssue.Number)

	// 1. Inspect new PR commits (pr.HeadSHA != approved_test_sha && pr.HeadSHA != last_evaluated_sha)
	if pr != nil && pr.HeadSHA != "" && pr.HeadSHA != st.approvedTestSHA && pr.HeadSHA != st.lastEvaluatedSHA {
		if st.approvedTestSHA != "" {
			changedFiles, err := st.gh.ListChangedFiles(ctx, st.approvedTestSHA, pr.HeadSHA)
			if err != nil {
				return r.recordFault(ctx, st, fmt.Errorf("listing changed files %s..%s: %w", st.approvedTestSHA, pr.HeadSHA, err))
			}
			var protected []string
			for _, f := range changedFiles {
				if ghclient.IsProtectedPath(f) {
					protected = append(protected, f)
				}
			}
			if len(protected) > 0 {
				if err := st.gh.ResetBranch(ctx, st.activeBranch, st.approvedTestSHA); err != nil {
					return r.recordFault(ctx, st, fmt.Errorf("resetting branch %q to %s: %w", st.activeBranch, st.approvedTestSHA, err))
				}
				st.lastEvaluatedSHA = st.approvedTestSHA
				diag := fmt.Sprintf("Protected path(s) modified after approval (%s..%s): %s. Branch was force-reset to %s.",
					st.approvedTestSHA, pr.HeadSHA, strings.Join(protected, ", "), st.approvedTestSHA)
				return r.handlePhaseStrike(ctx, st, alignIssue, evalResult.Percentage, diag, true)
			}
		}

		runs, err := st.gh.ListCheckRunsForRef(ctx, pr.HeadSHA)
		if err != nil {
			return r.recordFault(ctx, st, fmt.Errorf("listing check runs for %s: %w", pr.HeadSHA, err))
		}
		byName := latestCheckRunsByName(runs)
		compileRun := byName["compile"]
		testRun := byName["test"]
		reviewGateRun := byName["review-gate"]

		if isSuccess(compileRun) && isSuccess(testRun) && isSuccess(reviewGateRun) {
			return r.promoteGreenPR(ctx, st, pr, alignIssue)
		}

		if isFailureConclusion(compileRun) || isFailureConclusion(testRun) || isFailureConclusion(reviewGateRun) {
			st.lastEvaluatedSHA = pr.HeadSHA
			if !st.wasUnstalled {
				diag := formatCheckFailure("Green phase CI check failed", compileRun, testRun, reviewGateRun)
				return r.handlePhaseStrike(ctx, st, alignIssue, evalResult.Percentage, diag, true)
			}
		}
	}

	// 2. If no active container is running (e.g., resumed after un-stall or transient Up fault), launch a fresh Green DCM session.
	if st.activeContainerID == "" {
		if pr != nil && pr.HeadSHA != "" {
			st.lastEvaluatedSHA = pr.HeadSHA
		}
		if err := r.launchContainer(ctx, st, st.activeBranch, int32(alignIssue.Number), st.activePRNumber, BuildGreenPrompt(st.canonicalRepo, st.activeBranch, st.approvedTestSHA, "")); err != nil {
			return r.recordFault(ctx, st, fmt.Errorf("launching Green DCM session on %s: %w", st.activeBranch, err))
		}
		return r.saveAndRespond(
			ctx, st,
			pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
			int32(evalResult.Percentage),
			alignIssue.HTMLURL,
			fmt.Sprintf("aligning (%d%%)", evalResult.Percentage),
			fmt.Sprintf("aligning (%d%%)", evalResult.Percentage),
		)
	}

	// 3. Check container health for DCM_FAILED / DCM_HARD_FAILED
	if diag, err := r.checkContainerFailed(ctx, st.activeContainerID); err != nil {
		return r.recordFault(ctx, st, err)
	} else if diag != "" {
		return r.handlePhaseStrike(ctx, st, alignIssue, evalResult.Percentage, diag, true)
	}

	// 4. Check 30m inactivity timeout without a new SHA
	noNewSHA := pr == nil || pr.HeadSHA == "" || pr.HeadSHA == st.approvedTestSHA || pr.HeadSHA == st.lastEvaluatedSHA
	if st.activeContainerID != "" && noNewSHA && r.attemptTimedOut(st.attemptStartedAtUnix) {
		diag := fmt.Sprintf("Green attempt %d timed out after %s without a new commit on %s.", st.attemptCount, r.attemptTimeout, st.activeBranch)
		return r.handlePhaseStrike(ctx, st, alignIssue, evalResult.Percentage, diag, true)
	}

	return r.saveAndRespond(
		ctx, st,
		pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
		int32(evalResult.Percentage),
		alignIssue.HTMLURL,
		fmt.Sprintf("aligning (%d%%)", evalResult.Percentage),
		fmt.Sprintf("repository enrolled: aligning (%d%%)", evalResult.Percentage),
	)
}

func (r *Reconciler) promoteGreenPR(
	ctx context.Context,
	st *reconcileState,
	pr *ghclient.PullRequest,
	alignIssue *ghclient.Issue,
) (*pb.EnrollResponse, error) {
	if err := r.downContainer(ctx, st.activeContainerID); err != nil {
		return r.recordFault(ctx, st, err)
	}
	st.activeContainerID = ""

	if err := r.closeOpenIssuesByLabel(ctx, st.gh, ghclient.LabelAlign, int32(alignIssue.Number)); err != nil {
		return r.recordFault(ctx, st, err)
	}

	commitTitle := fmt.Sprintf("feat: align %s", pr.Title)
	commitMsg := fmt.Sprintf("Automated zero-touch alignment merge for PR #%d", pr.Number)
	if _, mergeErr := st.gh.SquashMergePullRequest(ctx, pr.Number, commitTitle, commitMsg); mergeErr != nil {
		latestPR, getErr := st.gh.GetPullRequest(ctx, pr.Number)
		if getErr != nil || latestPR == nil || !latestPR.Merged {
			return r.recordFault(ctx, st, fmt.Errorf("squash merging PR #%d: %w", pr.Number, mergeErr))
		}
	}

	_ = st.gh.DeleteBranch(ctx, st.activeBranch)
	st.clearScenario()

	// Re-evaluate updated main to either promote to ALIGNED or begin the next scenario.
	snap, cleanup, err := r.cloneMain(ctx, st)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return r.recordFault(ctx, st, err)
	}

	testsDir := filepath.Join(snap.tempDir, "tests")
	suite, err := parser.ParseTestDir(testsDir)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("parsing tests directory after merge: %w", err))
	}
	evalResult, err := r.evaluator.Evaluate(ctx, snap.spec, suite)
	if err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("evaluating alignment after merge: %w", err))
	}
	snap.evalResult = evalResult

	if evalResult.Percentage == 100 {
		return r.completeAligned(ctx, st, evalResult)
	}
	return r.startRedScenario(ctx, st, snap)
}

func (r *Reconciler) handlePhaseStrike(
	ctx context.Context,
	st *reconcileState,
	activeIssue *ghclient.Issue,
	alignmentPct int,
	diagnostic string,
	isGreen bool,
) (*pb.EnrollResponse, error) {
	phaseName := "Red"
	stage := pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING
	if isGreen {
		phaseName = "Green"
		stage = pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING
	}

	if st.attemptCount < maxStrikesPerPhase {
		if err := r.downAndWaitForGone(ctx, st.activeContainerID); err != nil {
			return r.recordFault(ctx, st, err)
		}
		st.activeContainerID = ""

		prompt := ""
		if isGreen {
			prompt = BuildGreenPrompt(st.canonicalRepo, st.activeBranch, st.approvedTestSHA, diagnostic)
		} else {
			prompt = BuildRedPromptFromMarkdown(st.canonicalRepo, st.activeBranch, activeIssue.Body, diagnostic)
		}

		if err := r.launchContainer(ctx, st, st.activeBranch, int32(activeIssue.Number), st.activePRNumber, prompt); err != nil {
			return r.recordFault(ctx, st, fmt.Errorf("relaunching %s DCM session on %s: %w", phaseName, st.activeBranch, err))
		}

		st.attemptCount++

		msg := fmt.Sprintf("%s attempt %d/%d (%d%%)", phaseName, st.attemptCount, maxStrikesPerPhase, alignmentPct)
		return r.saveAndRespond(ctx, st, stage, int32(alignmentPct), activeIssue.HTMLURL, msg, msg)
	}

	// Strike 3 exhausted: tear down container, comment, label speculate-stalled, transition to STALLED.
	if err := r.downContainer(ctx, st.activeContainerID); err != nil {
		return r.recordFault(ctx, st, err)
	}
	st.activeContainerID = ""

	comment := fmt.Sprintf("### Speculate %s Phase Stalled (3/3 Strikes Exhausted)\n\n%s\n\nRemove the `%s` label or close this issue after resolving the problem to reset attempts to `0` and resume.",
		phaseName, diagnostic, ghclient.LabelStalled)
	if err := st.gh.AddIssueComment(ctx, activeIssue.Number, comment); err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("commenting on stalled issue #%d: %w", activeIssue.Number, err))
	}
	if err := st.gh.AddIssueLabels(ctx, activeIssue.Number, []string{ghclient.LabelStalled}); err != nil {
		return r.recordFault(ctx, st, fmt.Errorf("adding %s label to issue #%d: %w", ghclient.LabelStalled, activeIssue.Number, err))
	}

	return r.saveAndRespond(
		ctx, st,
		pb.LifecycleStage_LIFECYCLE_STAGE_STALLED,
		int32(alignmentPct),
		activeIssue.HTMLURL,
		fmt.Sprintf("alignment stalled after %d %s strikes (%d%%)", maxStrikesPerPhase, phaseName, alignmentPct),
		fmt.Sprintf("alignment stalled after %d %s strikes (%d%%)", maxStrikesPerPhase, phaseName, alignmentPct),
	)
}

func (r *Reconciler) resolveActivePR(ctx context.Context, st *reconcileState) (*ghclient.PullRequest, error) {
	if st.activePRNumber > 0 {
		pr, err := st.gh.GetPullRequest(ctx, int(st.activePRNumber))
		if err == nil && pr != nil {
			return pr, nil
		}
	}
	if st.activeBranch != "" {
		pr, err := st.gh.FindOpenPullRequestByHead(ctx, st.activeBranch)
		if err != nil {
			return nil, fmt.Errorf("finding open PR for %q: %w", st.activeBranch, err)
		}
		if pr != nil {
			st.activePRNumber = int32(pr.Number)
			return pr, nil
		}
	}
	return nil, nil
}

func (r *Reconciler) finalizeMergedPR(ctx context.Context, st *reconcileState, pr *ghclient.PullRequest) error {
	if err := r.downContainer(ctx, st.activeContainerID); err != nil {
		return err
	}
	if err := r.closeOpenIssuesByLabel(ctx, st.gh, ghclient.LabelAlign, st.activeIssueNumber); err != nil {
		return err
	}
	if err := r.closeOpenIssuesByLabel(ctx, st.gh, ghclient.LabelAgenticLoop, 0); err != nil {
		return err
	}
	if st.activeBranch != "" {
		_ = st.gh.DeleteBranch(ctx, st.activeBranch)
	}
	st.clearScenario()
	return nil
}

func (r *Reconciler) findOwnerApproval(ctx context.Context, st *reconcileState, prNumber int) (*ghclient.PullRequestReview, error) {
	reviews, err := st.gh.ListPullRequestReviews(ctx, prNumber)
	if err != nil {
		return nil, fmt.Errorf("listing reviews for PR #%d: %w", prNumber, err)
	}

	var latest *ghclient.PullRequestReview
	for _, rev := range reviews {
		if rev == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(rev.Author), st.owner) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(rev.State), "APPROVED") {
			continue
		}
		if latest == nil || !rev.SubmittedAt.Before(latest.SubmittedAt) {
			latest = rev
		}
	}
	return latest, nil
}

func (r *Reconciler) ensureScenarioLoopIssue(
	ctx context.Context,
	st *reconcileState,
	spec *parser.Specification,
	evalResult *evaluator.EvaluationResult,
	tempDir string,
) (*ghclient.Issue, error) {
	existing, err := st.gh.FindOpenIssueByLabel(ctx, ghclient.LabelAgenticLoop)
	if err != nil {
		return nil, fmt.Errorf("checking open %s issue: %w", ghclient.LabelAgenticLoop, err)
	}
	if existing != nil {
		return existing, nil
	}

	stage, req := pickNextRequirement(spec, evalResult)
	protoContract := findProtoContracts(tempDir)
	card, err := r.synthesizer.GenerateScenarioCard(ctx, stage, req, protoContract)
	if err != nil {
		return nil, fmt.Errorf("generating scenario card: %w", err)
	}
	return r.ensureLoopIssueWithCard(ctx, st.gh, card)
}

func (r *Reconciler) ensureLoopIssueWithCard(ctx context.Context, gh ghclient.Client, card *synthesizer.ScenarioCard) (*ghclient.Issue, error) {
	existing, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelAgenticLoop)
	if err != nil {
		return nil, fmt.Errorf("checking open %s issue: %w", ghclient.LabelAgenticLoop, err)
	}
	if existing != nil {
		return existing, nil
	}

	scenarioName := card.Title
	if scenarioName == "" {
		scenarioName = card.ID
	}
	return gh.CreateIssue(ctx, &ghclient.CreateIssueRequest{
		Title:  fmt.Sprintf("[Speculate Loop] Add integration test for %s", scenarioName),
		Body:   card.FormatMarkdown(),
		Labels: []string{ghclient.LabelAgenticLoop},
	})
}

func (r *Reconciler) ensureAlignIssue(ctx context.Context, gh ghclient.Client, pr *ghclient.PullRequest, approvedSHA string) (*ghclient.Issue, error) {
	existing, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelAlign)
	if err != nil {
		return nil, fmt.Errorf("checking open %s issue: %w", ghclient.LabelAlign, err)
	}
	if existing != nil {
		return existing, nil
	}

	titleSuffix := "approved test scenario"
	prDesc := "open PR"
	if pr != nil {
		if strings.TrimSpace(pr.Title) != "" {
			titleSuffix = pr.Title
		}
		prDesc = fmt.Sprintf("PR #%d (%s)", pr.Number, pr.HTMLURL)
	}

	body := fmt.Sprintf("Human review approved for %s at commit `%s`.\n\nImplement the required functionality under `internal/**` without modifying protected paths.", prDesc, approvedSHA)
	return gh.CreateIssue(ctx, &ghclient.CreateIssueRequest{
		Title:  fmt.Sprintf("[Speculate Align] Implement %s", titleSuffix),
		Body:   body,
		Labels: []string{ghclient.LabelAlign},
	})
}

func (r *Reconciler) closeOpenIssuesByLabel(ctx context.Context, gh ghclient.Client, label string, knownIssueNum int32) error {
	if knownIssueNum > 0 {
		if iss, err := gh.GetIssue(ctx, int(knownIssueNum)); err == nil && iss != nil && strings.EqualFold(iss.State, "open") {
			if closeErr := gh.CloseIssue(ctx, iss.Number); closeErr != nil {
				return fmt.Errorf("closing issue #%d: %w", iss.Number, closeErr)
			}
		}
	}

	issues, err := gh.ListIssues(ctx, "open", []string{label})
	if err != nil {
		return fmt.Errorf("listing open %s issues: %w", label, err)
	}
	for _, iss := range issues {
		if iss == nil {
			continue
		}
		if err := gh.CloseIssue(ctx, iss.Number); err != nil {
			return fmt.Errorf("closing issue #%d: %w", iss.Number, err)
		}
	}
	return nil
}

func (r *Reconciler) resolveIssueURL(ctx context.Context, st *reconcileState, fallbackLabel string) (string, error) {
	if st.activeIssueNumber > 0 {
		if iss, err := st.gh.GetIssue(ctx, int(st.activeIssueNumber)); err == nil && iss != nil {
			return iss.HTMLURL, nil
		}
	}
	iss, err := st.gh.FindOpenIssueByLabel(ctx, fallbackLabel)
	if err != nil {
		return "", fmt.Errorf("looking up issue with label %s: %w", fallbackLabel, err)
	}
	if iss != nil {
		st.activeIssueNumber = int32(iss.Number)
		return iss.HTMLURL, nil
	}
	return st.priorIssueURL, nil
}

func (r *Reconciler) ensureReadmeBadgePR(ctx context.Context, gh ghclient.Client, badgeMarkdown string, pct int) error {
	stale, err := r.isReadmeBadgeStale(ctx, gh, "main", badgeMarkdown)
	if err != nil || !stale {
		return err
	}

	if _, err := gh.GetBranch(ctx, badgeBranchName); err != nil {
		if _, createErr := gh.CreateBranch(ctx, badgeBranchName, "main"); createErr != nil {
			return fmt.Errorf("creating badge branch %q: %w", badgeBranchName, createErr)
		}
	}

	if err := r.updateBranchReadmeBadgeIfNeeded(ctx, gh, badgeBranchName, badgeMarkdown); err != nil {
		return fmt.Errorf("updating README badge on %s: %w", badgeBranchName, err)
	}

	existingPR, err := gh.FindOpenPullRequestByHead(ctx, badgeBranchName)
	if err != nil {
		return fmt.Errorf("checking open badge PR: %w", err)
	}
	if existingPR != nil {
		return nil
	}

	_, err = gh.CreatePullRequest(ctx, &ghclient.CreatePRRequest{
		Title: fmt.Sprintf("chore: update spec alignment badge to %d%%", pct),
		Head:  badgeBranchName,
		Base:  "main",
		Body:  fmt.Sprintf("Automated README Spec Alignment badge update to %d%%.", pct),
	})
	if err != nil {
		return fmt.Errorf("creating badge PR: %w", err)
	}
	return nil
}

func (r *Reconciler) isReadmeBadgeStale(ctx context.Context, gh ghclient.Client, branch, badgeMarkdown string) (bool, error) {
	file, err := gh.GetFileContent(ctx, "README.md", branch)
	var currentContent string
	if err == nil && file != nil {
		currentContent = file.Content
	}
	updated := ghclient.InjectOrUpdateBadge(currentContent, badgeMarkdown)
	return updated != currentContent, nil
}

func (r *Reconciler) updateBranchReadmeBadgeIfNeeded(ctx context.Context, gh ghclient.Client, branch, badgeMarkdown string) error {
	stale, err := r.isReadmeBadgeStale(ctx, gh, branch, badgeMarkdown)
	if err != nil || !stale {
		return err
	}
	return gh.UpdateReadmeBadge(ctx, branch, badgeMarkdown)
}

func (r *Reconciler) launchContainer(ctx context.Context, st *reconcileState, branch string, issueNum, prNum int32, prompt string) error {
	cfg, err := r.dcmClient.Up(ctx, &dcm.UpParams{
		Repo:        st.canonicalRepo,
		Branch:      branch,
		Harness:     r.harness,
		IssueNumber: issueNum,
		PRNumber:    prNum,
		Prompt:      prompt,
		Model:       r.model,
	})
	if err != nil {
		return err
	}
	st.activeContainerID = cfg.GetId()
	st.attemptStartedAtUnix = r.nowFn().Unix()
	return nil
}

func (r *Reconciler) downContainer(ctx context.Context, containerID string) error {
	if !r.hasDCM() || strings.TrimSpace(containerID) == "" {
		return nil
	}
	if err := r.dcmClient.Down(ctx, containerID); err != nil {
		return fmt.Errorf("tearing down DCM container %s: %w", containerID, err)
	}
	return nil
}

func (r *Reconciler) downAndWaitForGone(ctx context.Context, containerID string) error {
	if !r.hasDCM() || strings.TrimSpace(containerID) == "" {
		return nil
	}
	if err := r.downContainer(ctx, containerID); err != nil {
		return err
	}
	if err := r.dcmClient.WaitForGone(ctx, containerID, r.pollInterval); err != nil {
		return fmt.Errorf("waiting for DCM container %s removal: %w", containerID, err)
	}
	return nil
}

func (r *Reconciler) checkContainerFailed(ctx context.Context, containerID string) (string, error) {
	if !r.hasDCM() || strings.TrimSpace(containerID) == "" {
		return "", nil
	}
	configs, err := r.dcmClient.List(ctx)
	if err != nil {
		return "", fmt.Errorf("listing DCM containers: %w", err)
	}
	for _, cfg := range configs {
		if cfg.GetId() != containerID {
			continue
		}
		if cfg.GetState() == dcm.StateFailed || cfg.GetState() == dcm.StateHardFailed {
			return fmt.Sprintf("DCM container %s entered %v: %s", containerID, cfg.GetState(), cfg.GetErrorMessage()), nil
		}
	}
	return "", nil
}

func (r *Reconciler) attemptTimedOut(startedAtUnix int64) bool {
	if startedAtUnix <= 0 || r.attemptTimeout <= 0 {
		return false
	}
	return r.nowFn().Sub(time.Unix(startedAtUnix, 0)) >= r.attemptTimeout
}

func (r *Reconciler) hasDCM() bool {
	return !isNilInterface(r.dcmClient)
}

func (r *Reconciler) saveAndRespond(
	ctx context.Context,
	st *reconcileState,
	stage pb.LifecycleStage,
	alignment int32,
	issueURL string,
	recordStatusMsg string,
	respStatusMsg string,
) (*pb.EnrollResponse, error) {
	record := &pb.EnrollmentRecord{
		Repository:           st.canonicalRepo,
		AlignmentPercentage:  alignment,
		IssueUrl:             issueURL,
		EnrolledAtUnix:       st.enrolledAt,
		LastEvaluatedAtUnix:  r.nowFn().Unix(),
		StatusMessage:        recordStatusMsg,
		Stage:                stage,
		LastError:            "",
		ActiveBranch:         st.activeBranch,
		ActivePrNumber:       st.activePRNumber,
		ActiveIssueNumber:    st.activeIssueNumber,
		ApprovedTestSha:      st.approvedTestSHA,
		LastEvaluatedSha:     st.lastEvaluatedSHA,
		AttemptCount:         st.attemptCount,
		ActiveContainerId:    st.activeContainerID,
		AttemptStartedAtUnix: st.attemptStartedAtUnix,
	}
	if !isNilInterface(r.store) {
		if err := r.store.SaveEnrollment(ctx, record); err != nil {
			return nil, fmt.Errorf("persisting enrollment record: %w", err)
		}
	}

	return &pb.EnrollResponse{
		Repository:          st.canonicalRepo,
		AlignmentPercentage: alignment,
		IssueUrl:            issueURL,
		StatusMessage:       respStatusMsg,
		Stage:               stage,
		LastError:           "",
	}, nil
}

func (r *Reconciler) recordFault(ctx context.Context, st *reconcileState, checkErr error) (*pb.EnrollResponse, error) {
	if !isNilInterface(r.store) {
		faultRecord := &pb.EnrollmentRecord{
			Repository:           st.canonicalRepo,
			AlignmentPercentage:  st.priorAlignment,
			IssueUrl:             st.priorIssueURL,
			EnrolledAtUnix:       st.enrolledAt,
			LastEvaluatedAtUnix:  r.nowFn().Unix(),
			StatusMessage:        st.priorStatusMsg,
			Stage:                st.priorStage,
			LastError:            checkErr.Error(),
			ActiveBranch:         st.activeBranch,
			ActivePrNumber:       st.activePRNumber,
			ActiveIssueNumber:    st.activeIssueNumber,
			ApprovedTestSha:      st.approvedTestSHA,
			LastEvaluatedSha:     st.lastEvaluatedSHA,
			AttemptCount:         st.attemptCount,
			ActiveContainerId:    st.activeContainerID,
			AttemptStartedAtUnix: st.attemptStartedAtUnix,
		}
		_ = r.store.SaveEnrollment(ctx, faultRecord)
	}
	return nil, checkErr
}

func defaultClone(ctx context.Context, repoURL, targetDir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", repoURL, targetDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func hasSeedProto(protoDir string) bool {
	hasProto := false
	_ = filepath.WalkDir(protoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != protoDir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || !strings.HasSuffix(d.Name(), ".proto") {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr == nil && info.Size() > 0 {
			hasProto = true
		}
		return nil
	})
	return hasProto
}

func parseSeedSpecs(specsDir, canonicalRepo string) (*parser.Specification, bool, error) {
	specEntries, err := os.ReadDir(specsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("reading specs/ directory: %w", err)
	}

	var combinedSpec *parser.Specification
	nonEmptySpecFiles := 0

	for _, entry := range specEntries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, false, fmt.Errorf("reading spec file info %s: %w", entry.Name(), err)
		}
		if info.Size() == 0 {
			continue
		}
		nonEmptySpecFiles++
		s, err := parser.ParseSpecFile(filepath.Join(specsDir, entry.Name()))
		if err != nil {
			return nil, false, status.Errorf(codes.InvalidArgument, "failed to parse spec file %s: %v", entry.Name(), err)
		}
		if combinedSpec == nil {
			combinedSpec = s
		} else {
			combinedSpec.Stages = append(combinedSpec.Stages, s.Stages...)
		}
	}

	if nonEmptySpecFiles == 0 {
		return nil, true, nil
	}
	if combinedSpec == nil || len(combinedSpec.Stages) == 0 || combinedSpec.TotalRequirements() == 0 {
		return nil, false, status.Errorf(codes.InvalidArgument, "no valid specification files found in specs/ for %s", canonicalRepo)
	}
	return combinedSpec, false, nil
}

func pickNextRequirement(spec *parser.Specification, evalResult *evaluator.EvaluationResult) (*parser.Stage, *parser.Requirement) {
	stage := evalResult.ActiveFrontierStage
	req := evalResult.NextRequirement
	if stage != nil && req != nil {
		return stage, req
	}

	for _, st := range spec.Stages {
		for _, r := range st.Requirements {
			if stStatus, ok := evalResult.RequirementStatuses[r.ID]; !ok || !stStatus.Covered {
				return st, r
			}
		}
	}
	return spec.Stages[0], spec.Stages[0].Requirements[0]
}

func findProtoContracts(dir string) string {
	var protos []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".proto") {
			data, err := os.ReadFile(path)
			if err == nil {
				protos = append(protos, string(data))
			}
		}
		return nil
	})
	return strings.Join(protos, "\n\n")
}

func latestCheckRunsByName(runs []*ghclient.CheckRun) map[string]*ghclient.CheckRun {
	res := make(map[string]*ghclient.CheckRun)
	for _, cr := range runs {
		if cr == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(cr.Name))
		if name == "" {
			continue
		}
		prev, exists := res[name]
		if !exists || cr.ID >= prev.ID {
			res[name] = cr
		}
	}
	return res
}

func isCompleted(cr *ghclient.CheckRun) bool {
	return cr != nil && strings.EqualFold(cr.Status, "completed")
}

func isSuccess(cr *ghclient.CheckRun) bool {
	return isCompleted(cr) && strings.EqualFold(cr.Conclusion, "success")
}

func isFailureConclusion(cr *ghclient.CheckRun) bool {
	return isCompleted(cr) && !strings.EqualFold(cr.Conclusion, "success")
}

func formatCheckFailure(header string, runs ...*ghclient.CheckRun) string {
	var parts []string
	for _, cr := range runs {
		if cr == nil {
			continue
		}
		line := fmt.Sprintf("- `%s`: status=%s conclusion=%s", cr.Name, cr.Status, cr.Conclusion)
		if summary := strings.TrimSpace(cr.OutputSummary); summary != "" {
			line += " (" + summary + ")"
		}
		parts = append(parts, line)
	}
	if len(parts) == 0 {
		return header
	}
	return header + ":\n" + strings.Join(parts, "\n")
}

func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return rv.IsNil()
	default:
		return false
	}
}
