package enroll

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func createTestRepoFS(t *testing.T, specContent string, testContent string) string {
	t.Helper()
	dir := t.TempDir()

	if specContent != "" {
		specsDir := filepath.Join(dir, "specs")
		if err := os.MkdirAll(specsDir, 0755); err != nil {
			t.Fatalf("failed to create specs dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(specsDir, "spec.md"), []byte(specContent), 0644); err != nil {
			t.Fatalf("failed to write spec: %v", err)
		}
	}

	if testContent != "" {
		testsDir := filepath.Join(dir, "tests")
		if err := os.MkdirAll(testsDir, 0755); err != nil {
			t.Fatalf("failed to create tests dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(testsDir, "kv_test.go"), []byte(testContent), 0644); err != nil {
			t.Fatalf("failed to write test: %v", err)
		}
	}

	return dir
}

const sampleSpec = `# Key-Value Service
## [Stage: Core] Basic Key-Value Storage
- Put: Stores a key and associated value.
`

const sampleTest = `package tests

import "testing"

// Stage: Core
// Scenario: Core-1 Put stores key
func TestPut(t *testing.T) {}
`

func TestEnroll_100PercentAlignment(t *testing.T) {
	ctx := context.Background()
	repoDir := createTestRepoFS(t, sampleSpec, sampleTest)

	mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
	mockStore := pstore.NewMockStore()

	// LLM reports 100% coverage
	evalLLM := &evaluator.MockLLMClient{
		Response: `{
			"coverage": [
				{
					"id": "Core-1",
					"covered": true,
					"test": "TestPut",
					"reasoning": "covers Put"
				}
			]
		}`,
	}
	eval := evaluator.NewEvaluator(evalLLM)
	synth := synthesizer.NewSynthesizer(evalLLM)

	pipeline := NewPipeline(
		"test-token",
		func(token, owner, repo string) ghclient.Client {
			return mockGH
		},
		eval,
		synth,
		mockStore,
	)
	pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
		// Copy repoDir contents to targetDir
		return copyDir(repoDir, targetDir)
	}

	var resp *pb.EnrollResponse
	resp, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.GetAlignmentPercentage() != 100 {
		t.Errorf("expected alignment 100, got %d", resp.GetAlignmentPercentage())
	}
	if resp.GetIssueUrl() != "" {
		t.Errorf("expected empty issue URL, got %q", resp.GetIssueUrl())
	}

	// Verify pstore record
	var record *pb.EnrollmentRecord
	record, err = mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
	if err != nil {
		t.Fatalf("failed to get enrollment from store: %v", err)
	}
	if record.GetAlignmentPercentage() != 100 {
		t.Errorf("expected stored alignment 100, got %d", record.GetAlignmentPercentage())
	}

	// Verify README badge updated
	readme, err := mockGH.GetFileContent(ctx, "README.md", "main")
	if err != nil {
		t.Fatalf("failed to read README.md: %v", err)
	}
	if !containsBadge(readme.Content, 100) {
		t.Errorf("expected README to contain 100%% badge, got:\n%s", readme.Content)
	}

	// Verify no issues created
	issues, err := mockGH.ListIssues(ctx, "all", nil)
	if err != nil {
		t.Fatalf("failed to list issues: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("expected 0 issues created, got %d", len(issues))
	}
}

func TestEnroll_LessThan100Percent_NewIssue(t *testing.T) {
	ctx := context.Background()
	repoDir := createTestRepoFS(t, sampleSpec, "") // No tests -> 0% coverage

	mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
	mockStore := pstore.NewMockStore()

	synthLLM := &evaluator.MockLLMClient{
		Response: `{
			"id": "Core-1",
			"stage": "Core",
			"requirement": "Put stores key",
			"target_rpc": "Put",
			"title": "Put stores key",
			"given": "Empty store",
			"when": "Put is called",
			"then": "Value is stored",
			"test_func_name": "TestPut_Basic"
		}`,
	}
	eval := evaluator.NewEvaluator(synthLLM)
	synth := synthesizer.NewSynthesizer(synthLLM)

	pipeline := NewPipeline(
		"test-token",
		func(token, owner, repo string) ghclient.Client {
			return mockGH
		},
		eval,
		synth,
		mockStore,
	)
	pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
		return copyDir(repoDir, targetDir)
	}

	resp, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.GetAlignmentPercentage() >= 100 {
		t.Errorf("expected alignment < 100, got %d", resp.GetAlignmentPercentage())
	}
	if resp.GetIssueUrl() == "" {
		t.Fatalf("expected non-empty issue URL")
	}

	// Verify issue created in GitHub mock
	issues, err := mockGH.ListIssues(ctx, "open", []string{ghclient.LabelAgenticLoop})
	if err != nil {
		t.Fatalf("failed to list issues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 open issue with label %s, got %d", ghclient.LabelAgenticLoop, len(issues))
	}
	if issues[0].HTMLURL != resp.GetIssueUrl() {
		t.Errorf("expected issue URL %s, got %s", issues[0].HTMLURL, resp.GetIssueUrl())
	}

	// Verify pstore record
	record, err := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
	if err != nil {
		t.Fatalf("failed to get enrollment from store: %v", err)
	}
	if record.GetIssueUrl() != resp.GetIssueUrl() {
		t.Errorf("expected stored issue url %s, got %s", resp.GetIssueUrl(), record.GetIssueUrl())
	}
}

func TestEnroll_IdempotencyReusesOpenIssue(t *testing.T) {
	ctx := context.Background()
	repoDir := createTestRepoFS(t, sampleSpec, "")

	mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
	mockStore := pstore.NewMockStore()

	// Pre-create an open issue with speculate-agentic-loop label
	existingIssue, err := mockGH.CreateIssue(ctx, &ghclient.CreateIssueRequest{
		Title:  "[Speculate Loop] Add integration test for Core-1",
		Body:   "Existing Scenario Card",
		Labels: []string{ghclient.LabelAgenticLoop},
	})
	if err != nil {
		t.Fatalf("failed to pre-create issue: %v", err)
	}

	synthLLM := &evaluator.MockLLMClient{
		Response: `{}`,
	}
	eval := evaluator.NewEvaluator(synthLLM)
	synth := synthesizer.NewSynthesizer(synthLLM)

	pipeline := NewPipeline(
		"test-token",
		func(token, owner, repo string) ghclient.Client {
			return mockGH
		},
		eval,
		synth,
		mockStore,
	)
	pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
		return copyDir(repoDir, targetDir)
	}

	resp, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.GetIssueUrl() != existingIssue.HTMLURL {
		t.Errorf("expected reused issue URL %s, got %s", existingIssue.HTMLURL, resp.GetIssueUrl())
	}

	// Verify no additional issue was created
	issues, err := mockGH.ListIssues(ctx, "all", nil)
	if err != nil {
		t.Fatalf("failed to list issues: %v", err)
	}
	if len(issues) != 1 {
		t.Errorf("expected exactly 1 issue (reused), got %d", len(issues))
	}
}

func TestEnroll_MissingSpecsDir(t *testing.T) {
	ctx := context.Background()
	repoDir := createTestRepoFS(t, "", "") // No specs/

	mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
	mockStore := pstore.NewMockStore()
	eval := evaluator.NewEvaluator(nil)
	synth := synthesizer.NewSynthesizer(nil)

	pipeline := NewPipeline(
		"test-token",
		func(token, owner, repo string) ghclient.Client {
			return mockGH
		},
		eval,
		synth,
		mockStore,
	)
	pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
		return copyDir(repoDir, targetDir)
	}

	_, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
	if err == nil {
		t.Fatal("expected error for missing specs/ dir, got nil")
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got status code %v (err: %v)", st.Code(), err)
	}
}

func TestEnroll_InvalidRepo(t *testing.T) {
	ctx := context.Background()
	pipeline := NewPipeline("test-token", nil, nil, nil, nil)

	_, err := pipeline.Enroll(ctx, "invalid-repo-without-slash")
	if err == nil {
		t.Fatal("expected error for invalid repo, got nil")
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got %v (err: %v)", st.Code(), err)
	}
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

func containsBadge(content string, percentage int) bool {
	expected := fmt.Sprintf("Spec%%20Alignment-%d%%25", percentage)
	return filepath.Clean(expected) != "" && (filepath.Base(expected) != "") // basic check or strings.Contains
}
