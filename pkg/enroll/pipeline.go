package enroll

import (
	"context"
	"fmt"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Pipeline orchestrates repository enrollment: cloning, spec validation,
// alignment evaluation, label ensuring, README badge updating, and scenario card dispatch.
type Pipeline struct {
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

func defaultClone(ctx context.Context, repoURL, targetDir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", repoURL, targetDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Enroll orchestrates the enrollment of a repository into Speculate.
func (p *Pipeline) Enroll(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
	// 1. Parse and validate <owner>/<repo>
	owner, repoName, err := ghclient.ParseRepo(repo)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid repository %q: %v", repo, err)
	}
	canonicalRepo := fmt.Sprintf("%s/%s", owner, repoName)

	// 2. Shallow clone repository to temporary directory
	tempDir, err := os.MkdirTemp("", "speculate-enroll-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp directory for clone: %w", err)
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
		return nil, fmt.Errorf("cloning repository %s: %w", canonicalRepo, err)
	}

	// 3. Verify specs/ directory exists and parse spec files with parser.ParseSpecFile
	specsDir := filepath.Join(tempDir, "specs")
	specEntries, err := os.ReadDir(specsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.InvalidArgument, "specs/ directory not found in repository %s", canonicalRepo)
		}
		return nil, fmt.Errorf("reading specs/ directory: %w", err)
	}

	var combinedSpec *parser.Specification
	for _, entry := range specEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		specFilePath := filepath.Join(specsDir, entry.Name())
		s, err := parser.ParseSpecFile(specFilePath)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to parse spec file %s: %v", entry.Name(), err)
		}
		if combinedSpec == nil {
			combinedSpec = s
		} else {
			combinedSpec.Stages = append(combinedSpec.Stages, s.Stages...)
		}
	}

	if combinedSpec == nil || len(combinedSpec.Stages) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "no valid specification files found in specs/ for %s", canonicalRepo)
	}

	// 4. Parse test scenarios from tests/ using parser.ParseTestDir
	testsDir := filepath.Join(tempDir, "tests")
	suite, err := parser.ParseTestDir(testsDir)
	if err != nil {
		return nil, fmt.Errorf("parsing tests directory: %w", err)
	}

	// 5. Evaluate alignment score using evaluator.Evaluate(ctx, spec, suite)
	evalResult, err := p.evaluator.Evaluate(ctx, combinedSpec, suite)
	if err != nil {
		return nil, fmt.Errorf("evaluating alignment: %w", err)
	}

	gh := p.githubClientFactory(p.token, owner, repoName)

	// 6. Ensure required labels exist on the repository using ghClient.EnsureLabels
	if err := gh.EnsureLabels(ctx, ghclient.RequiredLabels()); err != nil {
		return nil, fmt.Errorf("ensuring labels on %s: %w", canonicalRepo, err)
	}

	// 7. Update README badge on main branch using ghClient.UpdateReadmeBadge
	if err := gh.UpdateReadmeBadge(ctx, "main", evalResult.BadgeMarkdown); err != nil {
		return nil, fmt.Errorf("updating README badge on %s: %w", canonicalRepo, err)
	}

	now := time.Now().Unix()
	enrolledAt := now
	if p.store != nil {
		if existing, err := p.store.GetEnrollment(ctx, canonicalRepo); err == nil && existing != nil && existing.GetEnrolledAtUnix() > 0 {
			enrolledAt = existing.GetEnrolledAtUnix()
		}
	}

	// 8. If evalResult.Percentage == 100:
	if evalResult.Percentage == 100 {
		record := &pb.EnrollmentRecord{
			Repository:          canonicalRepo,
			AlignmentPercentage: 100,
			IssueUrl:            "",
			EnrolledAtUnix:      enrolledAt,
			LastEvaluatedAtUnix: now,
			StatusMessage:       "fully aligned (100%)",
		}
		if p.store != nil {
			if err := p.store.SaveEnrollment(ctx, record); err != nil {
				return nil, fmt.Errorf("persisting enrollment record: %w", err)
			}
		}

		return &pb.EnrollResponse{
			Repository:          canonicalRepo,
			AlignmentPercentage: 100,
			IssueUrl:            "",
			StatusMessage:       "repository enrolled: fully aligned (100%)",
		}, nil
	}

	// 9. If evalResult.Percentage < 100:
	var issueURL string

	existingIssue, err := gh.FindOpenIssueByLabel(ctx, ghclient.LabelAgenticLoop)
	if err != nil {
		return nil, fmt.Errorf("checking for existing open issue with label %s: %w", ghclient.LabelAgenticLoop, err)
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
					if status, ok := evalResult.RequirementStatuses[r.ID]; !ok || !status.Covered {
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
			return nil, fmt.Errorf("generating scenario card: %w", err)
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
			return nil, fmt.Errorf("creating scenario card issue: %w", err)
		}

		issueURL = createdIssue.HTMLURL
	}

	record := &pb.EnrollmentRecord{
		Repository:          canonicalRepo,
		AlignmentPercentage: int32(evalResult.Percentage),
		IssueUrl:            issueURL,
		EnrolledAtUnix:      enrolledAt,
		LastEvaluatedAtUnix: now,
		StatusMessage:       fmt.Sprintf("alignment %d%%", evalResult.Percentage),
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
		StatusMessage:       fmt.Sprintf("repository enrolled with %d%% alignment", evalResult.Percentage),
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
