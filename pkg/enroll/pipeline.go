package enroll

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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

// Pipeline orchestrates repository enrollment: cloning, spec validation,
// alignment evaluation, label ensuring, README badge updating, and scenario card dispatch.
type Pipeline struct {
	sf                  singleflight.Group
	githubClientFactory func(token, owner, repo string) ghclient.Client
	evaluator           *evaluator.Evaluator
	synthesizer         *synthesizer.Synthesizer
	store               pstore.Store
	token               string
	cloneFn             func(ctx context.Context, repoURL, targetDir string) error
}

// NewPipeline constructs an enrollment Pipeline orchestrator.
func NewPipeline(
	token string,
	githubClientFactory func(token, owner, repo string) ghclient.Client,
	eval *evaluator.Evaluator,
	synth *synthesizer.Synthesizer,
	store pstore.Store,
) *Pipeline {
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

	return &Pipeline{
		githubClientFactory: githubClientFactory,
		evaluator:           eval,
		synthesizer:         synth,
		store:               store,
		token:               token,
		cloneFn:             defaultClone,
	}
}

// WithCloneFn configures a custom clone function, useful for hermetic testing.
func (p *Pipeline) WithCloneFn(fn func(ctx context.Context, repoURL, targetDir string) error) *Pipeline {
	p.cloneFn = fn
	return p
}

// ListEnrollments returns all tracked repository enrollment records from storage.
func (p *Pipeline) ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
	if p.store == nil {
		return nil, nil
	}
	return p.store.ListEnrollments(ctx)
}

func defaultClone(ctx context.Context, repoURL, targetDir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", repoURL, targetDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Enroll orchestrates the enrollment of a repository into Speculate, deduplicating
// concurrent evaluations for the same canonical repository via singleflight.
func (p *Pipeline) Enroll(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
	// 1. Parse and validate <owner>/<repo>
	owner, repoName, err := ghclient.ParseRepo(repo)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid repository %q: %v", repo, err)
	}
	canonicalRepo := fmt.Sprintf("%s/%s", owner, repoName)

	res, err, _ := p.sf.Do(canonicalRepo, func() (any, error) {
		return p.enrollOnce(ctx, owner, repoName, canonicalRepo)
	})
	if err != nil {
		return nil, err
	}

	resp, ok := res.(*pb.EnrollResponse)
	if !ok {
		return nil, status.Error(codes.Internal, "unexpected response type from pipeline")
	}
	return proto.Clone(resp).(*pb.EnrollResponse), nil
}

func (p *Pipeline) enrollOnce(ctx context.Context, owner, repoName, canonicalRepo string) (*pb.EnrollResponse, error) {
	now := time.Now().Unix()
	enrolledAt := now
	priorStage := pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING
	var priorAlignment int32
	var priorIssueURL string
	priorStatusMsg := "evaluating repository"

	if p.store != nil {
		existing, getErr := p.store.GetEnrollment(ctx, canonicalRepo)
		if getErr == nil && existing != nil {
			if existing.GetEnrolledAtUnix() > 0 {
				enrolledAt = existing.GetEnrolledAtUnix()
			}
			if existing.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_UNSPECIFIED {
				priorStage = existing.GetStage()
			}
			priorAlignment = existing.GetAlignmentPercentage()
			priorIssueURL = existing.GetIssueUrl()
			if existing.GetStatusMessage() != "" {
				priorStatusMsg = existing.GetStatusMessage()
			}
		} else if errors.Is(getErr, pstore.ErrNotFound) || (getErr == nil && existing == nil) {
			initialRecord := &pb.EnrollmentRecord{
				Repository:          canonicalRepo,
				AlignmentPercentage: 0,
				IssueUrl:            "",
				EnrolledAtUnix:      enrolledAt,
				LastEvaluatedAtUnix: now,
				StatusMessage:       "evaluating repository",
				Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
				LastError:           "",
			}
			if saveErr := p.store.SaveEnrollment(ctx, initialRecord); saveErr != nil {
				return nil, fmt.Errorf("persisting initial enrollment record: %w", saveErr)
			}
		}
	}

	recordFault := func(checkErr error) (*pb.EnrollResponse, error) {
		if p.store != nil {
			faultRecord := &pb.EnrollmentRecord{
				Repository:          canonicalRepo,
				AlignmentPercentage: priorAlignment,
				IssueUrl:            priorIssueURL,
				EnrolledAtUnix:      enrolledAt,
				LastEvaluatedAtUnix: time.Now().Unix(),
				StatusMessage:       priorStatusMsg,
				Stage:               priorStage,
				LastError:           checkErr.Error(),
			}
			_ = p.store.SaveEnrollment(ctx, faultRecord)
		}
		return nil, checkErr
	}

	// 2. Shallow clone repository to temporary directory
	tempDir, err := os.MkdirTemp("", "speculate-enroll-*")
	if err != nil {
		return recordFault(fmt.Errorf("creating temp directory for clone: %w", err))
	}
	defer os.RemoveAll(tempDir)

	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repoName)
	if p.token != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", p.token, owner, repoName)
	}

	cloneFunc := p.cloneFn
	if cloneFunc == nil {
		cloneFunc = defaultClone
	}

	if err := cloneFunc(ctx, cloneURL, tempDir); err != nil {
		return recordFault(fmt.Errorf("cloning repository %s: %w", canonicalRepo, err))
	}

	// 3a. Verify seed .proto presence in proto/
	protoDir := filepath.Join(tempDir, "proto")
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
		info, err := d.Info()
		if err == nil && info.Size() > 0 {
			hasProto = true
		}
		return nil
	})

	// 3b. Verify specs/ directory exists and parse spec files with parser.ParseSpecFile
	specsDir := filepath.Join(tempDir, "specs")
	var combinedSpec *parser.Specification
	specsMissingOrEmpty := false

	specEntries, err := os.ReadDir(specsDir)
	if err != nil {
		if os.IsNotExist(err) {
			specsMissingOrEmpty = true
		} else {
			return recordFault(fmt.Errorf("reading specs/ directory: %w", err))
		}
	} else {
		nonEmptySpecFiles := 0
		for _, entry := range specEntries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return recordFault(fmt.Errorf("reading spec file info %s: %w", entry.Name(), err))
			}
			if info.Size() == 0 {
				continue
			}
			nonEmptySpecFiles++
			specFilePath := filepath.Join(specsDir, entry.Name())
			s, err := parser.ParseSpecFile(specFilePath)
			if err != nil {
				return recordFault(status.Errorf(codes.InvalidArgument, "failed to parse spec file %s: %v", entry.Name(), err))
			}
			if combinedSpec == nil {
				combinedSpec = s
			} else {
				combinedSpec.Stages = append(combinedSpec.Stages, s.Stages...)
			}
		}

		if nonEmptySpecFiles == 0 {
			specsMissingOrEmpty = true
		} else if combinedSpec == nil || len(combinedSpec.Stages) == 0 || combinedSpec.TotalRequirements() == 0 {
			return recordFault(status.Errorf(codes.InvalidArgument, "no valid specification files found in specs/ for %s", canonicalRepo))
		}
	}

	gh := p.githubClientFactory(p.token, owner, repoName)

	// Record AWAITING_SEED when seed proto or spec files are not yet present
	if !hasProto || specsMissingOrEmpty {
		seedIssue, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelSeed)
		if err != nil {
			return recordFault(fmt.Errorf("checking for open seed issue: %w", err))
		}
		seedIssueURL := ""
		if seedIssue != nil {
			seedIssueURL = seedIssue.HTMLURL
		}

		record := &pb.EnrollmentRecord{
			Repository:          canonicalRepo,
			AlignmentPercentage: 0,
			IssueUrl:            seedIssueURL,
			EnrolledAtUnix:      enrolledAt,
			LastEvaluatedAtUnix: time.Now().Unix(),
			StatusMessage:       "awaiting initial seed proto and spec files",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
			LastError:           "",
		}
		if p.store != nil {
			if err := p.store.SaveEnrollment(ctx, record); err != nil {
				return nil, fmt.Errorf("persisting enrollment record: %w", err)
			}
		}

		return &pb.EnrollResponse{
			Repository:          canonicalRepo,
			AlignmentPercentage: 0,
			IssueUrl:            seedIssueURL,
			StatusMessage:       "repository enrolled: awaiting initial seed proto and spec files",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
			LastError:           "",
		}, nil
	}

	// 3c. Auto-close open speculate-seed issue when seed files are verified
	seedIssue, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelSeed)
	if err != nil {
		return recordFault(fmt.Errorf("checking for open seed issue: %w", err))
	}
	if seedIssue != nil {
		commentBody := "Seed protobuf and specification files detected on `main`. Closing this tracking issue and initiating the Speculate autonomous alignment loop."
		if err := gh.AddIssueComment(ctx, seedIssue.Number, commentBody); err != nil {
			return recordFault(fmt.Errorf("commenting on seed issue #%d: %w", seedIssue.Number, err))
		}
		if err := gh.CloseIssue(ctx, seedIssue.Number); err != nil {
			return recordFault(fmt.Errorf("closing seed issue #%d: %w", seedIssue.Number, err))
		}
	}

	// 4. Parse test scenarios from tests/ using parser.ParseTestDir
	testsDir := filepath.Join(tempDir, "tests")
	suite, err := parser.ParseTestDir(testsDir)
	if err != nil {
		return recordFault(fmt.Errorf("parsing tests directory: %w", err))
	}

	// 5. Evaluate alignment score using evaluator.Evaluate(ctx, spec, suite)
	evalResult, err := p.evaluator.Evaluate(ctx, combinedSpec, suite)
	if err != nil {
		return recordFault(fmt.Errorf("evaluating alignment: %w", err))
	}

	// 6. Ensure required labels exist on the repository using ghClient.EnsureLabels
	if err := gh.EnsureLabels(ctx, ghclient.RequiredLabels()); err != nil {
		return recordFault(fmt.Errorf("ensuring labels on %s: %w", canonicalRepo, err))
	}

	// 7. Update README badge on main branch using ghClient.UpdateReadmeBadge
	if err := gh.UpdateReadmeBadge(ctx, "main", evalResult.BadgeMarkdown); err != nil {
		return recordFault(fmt.Errorf("updating README badge on %s: %w", canonicalRepo, err))
	}

	// 8. Resolve active lifecycle stage in priority order:
	//    1) STALLED -> 2) ALIGNING -> 3) ALIGNED (100%) -> 4) AWAITING_TEST_REVIEW (<100%)
	var resolvedStage pb.LifecycleStage
	var issueURL string
	var recordStatusMsg string
	var respStatusMsg string

	stalledIssue, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelStalled)
	if err != nil {
		return recordFault(fmt.Errorf("checking for open issue with label %s: %w", ghclient.LabelStalled, err))
	}

	if stalledIssue != nil {
		resolvedStage = pb.LifecycleStage_LIFECYCLE_STAGE_STALLED
		issueURL = stalledIssue.HTMLURL
		recordStatusMsg = fmt.Sprintf("alignment stalled (%d%%)", evalResult.Percentage)
		respStatusMsg = fmt.Sprintf("repository enrolled: alignment stalled (%d%%)", evalResult.Percentage)
	} else {
		alignIssue, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelAlign)
		if err != nil {
			return recordFault(fmt.Errorf("checking for open issue with label %s: %w", ghclient.LabelAlign, err))
		}

		if alignIssue != nil {
			resolvedStage = pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING
			issueURL = alignIssue.HTMLURL
			recordStatusMsg = fmt.Sprintf("aligning (%d%%)", evalResult.Percentage)
			respStatusMsg = fmt.Sprintf("repository enrolled: aligning (%d%%)", evalResult.Percentage)
		} else if evalResult.Percentage == 100 {
			resolvedStage = pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED
			issueURL = ""
			recordStatusMsg = "fully aligned (100%)"
			respStatusMsg = "repository enrolled: fully aligned (100%)"
		} else {
			existingIssue, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelAgenticLoop)
			if err != nil {
				return recordFault(fmt.Errorf("checking for existing open issue with label %s: %w", ghclient.LabelAgenticLoop, err))
			}

			if existingIssue != nil {
				issueURL = existingIssue.HTMLURL
			} else {
				// Discover frontier stage and next unexercised requirement
				stage := evalResult.ActiveFrontierStage
				req := evalResult.NextRequirement

				if stage == nil || req == nil {
					for _, st := range combinedSpec.Stages {
						for _, r := range st.Requirements {
							if stStatus, ok := evalResult.RequirementStatuses[r.ID]; !ok || !stStatus.Covered {
								stage = st
								req = r
								break
							}
						}
						if stage != nil && req != nil {
							break
						}
					}
				}

				if stage == nil || req == nil {
					stage = combinedSpec.Stages[0]
					req = stage.Requirements[0]
				}

				protoContract := findProtoContracts(tempDir)

				card, err := p.synthesizer.GenerateScenarioCard(ctx, stage, req, protoContract)
				if err != nil {
					return recordFault(fmt.Errorf("generating scenario card: %w", err))
				}

				scenarioName := card.Title
				if scenarioName == "" {
					scenarioName = card.ID
				}
				issueTitle := fmt.Sprintf("[Speculate Loop] Add integration test for %s", scenarioName)

				createdIssue, err := gh.CreateIssue(ctx, &ghclient.CreateIssueRequest{
					Title:  issueTitle,
					Body:   card.FormatMarkdown(),
					Labels: []string{ghclient.LabelAgenticLoop},
				})
				if err != nil {
					return recordFault(fmt.Errorf("creating scenario card issue: %w", err))
				}

				issueURL = createdIssue.HTMLURL
			}

			resolvedStage = pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW
			recordStatusMsg = fmt.Sprintf("alignment %d%%", evalResult.Percentage)
			respStatusMsg = fmt.Sprintf("repository enrolled with %d%% alignment", evalResult.Percentage)
		}
	}

	record := &pb.EnrollmentRecord{
		Repository:          canonicalRepo,
		AlignmentPercentage: int32(evalResult.Percentage),
		IssueUrl:            issueURL,
		EnrolledAtUnix:      enrolledAt,
		LastEvaluatedAtUnix: time.Now().Unix(),
		StatusMessage:       recordStatusMsg,
		Stage:               resolvedStage,
		LastError:           "",
	}
	if p.store != nil {
		if err := p.store.SaveEnrollment(ctx, record); err != nil {
			return nil, fmt.Errorf("persisting enrollment record: %w", err)
		}
	}

	return &pb.EnrollResponse{
		Repository:          canonicalRepo,
		AlignmentPercentage: int32(evalResult.Percentage),
		IssueUrl:            issueURL,
		StatusMessage:       respStatusMsg,
		Stage:               resolvedStage,
		LastError:           "",
	}, nil
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
