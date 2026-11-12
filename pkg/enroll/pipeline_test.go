package enroll

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const sampleProto = `syntax = "proto3";
package kv.v1;
service KV {
  rpc Put(PutRequest) returns (PutResponse);
}
message PutRequest {
  string key = 1;
  bytes value = 2;
}
message PutResponse {}
`

type funcLLMClient struct {
	fn func(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

func (f *funcLLMClient) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return f.fn(ctx, systemPrompt, userPrompt)
}

func createTestRepoFS(t *testing.T, specContent string, testContent string) string {
	t.Helper()
	dir := t.TempDir()

	protoDir := filepath.Join(dir, "proto")
	if err := os.MkdirAll(protoDir, 0755); err != nil {
		t.Fatalf("failed to create proto dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(protoDir, "kv.proto"), []byte(sampleProto), 0644); err != nil {
		t.Fatalf("failed to write proto: %v", err)
	}

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
		return copyDir(repoDir, targetDir)
	}

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
	if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
		t.Errorf("expected stage ALIGNED, got %v", resp.GetStage())
	}
	if resp.GetLastError() != "" {
		t.Errorf("expected empty last_error, got %q", resp.GetLastError())
	}

	// Verify pstore record
	record, err := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
	if err != nil {
		t.Fatalf("failed to get enrollment from store: %v", err)
	}
	if record.GetAlignmentPercentage() != 100 {
		t.Errorf("expected stored alignment 100, got %d", record.GetAlignmentPercentage())
	}
	if record.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
		t.Errorf("expected stored stage ALIGNED, got %v", record.GetStage())
	}
	if record.GetLastError() != "" {
		t.Errorf("expected empty stored last_error, got %q", record.GetLastError())
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
	if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
		t.Errorf("expected stage AWAITING_TEST_REVIEW, got %v", resp.GetStage())
	}
	if resp.GetLastError() != "" {
		t.Errorf("expected empty last_error, got %q", resp.GetLastError())
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
	if record.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
		t.Errorf("expected stored stage AWAITING_TEST_REVIEW, got %v", record.GetStage())
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
	if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
		t.Errorf("expected stage AWAITING_TEST_REVIEW, got %v", resp.GetStage())
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

	t.Run("missing specs directory records AWAITING_SEED and transitions when seed added", func(t *testing.T) {
		repoDir := createTestRepoFS(t, "", "") // Has proto/, no specs/

		mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
		mockStore := pstore.NewMockStore()

		seedIssue, err := mockGH.CreateIssue(ctx, &ghclient.CreateIssueRequest{
			Title:  "[Speculate] Add initial seed proto and spec files",
			Body:   "Please add seed files",
			Labels: []string{ghclient.LabelSeed},
		})
		if err != nil {
			t.Fatalf("failed to pre-create seed issue: %v", err)
		}

		llm := &funcLLMClient{
			fn: func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
				if strings.Contains(userPrompt, "TestPut") {
					return `{"coverage":[{"id":"Core-1","covered":true,"test":"TestPut","reasoning":"covers Put"}]}`, nil
				}
				return `{
					"id": "Core-1",
					"stage": "Core",
					"requirement": "Put stores key",
					"target_rpc": "Put",
					"title": "Put stores key",
					"given": "Empty store",
					"when": "Put is called",
					"then": "Value is stored",
					"test_func_name": "TestPut_Basic"
				}`, nil
			},
		}

		pipeline := NewPipeline(
			"test-token",
			func(token, owner, repo string) ghclient.Client {
				return mockGH
			},
			evaluator.NewEvaluator(llm),
			synthesizer.NewSynthesizer(llm),
			mockStore,
		)
		pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
			return copyDir(repoDir, targetDir)
		}

		// 1. Missing specs/ -> AWAITING_SEED (no error)
		resp, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("expected nil error for missing specs/ dir, got: %v", err)
		}
		if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED {
			t.Errorf("expected stage AWAITING_SEED, got %v", resp.GetStage())
		}
		if resp.GetAlignmentPercentage() != 0 {
			t.Errorf("expected alignment 0, got %d", resp.GetAlignmentPercentage())
		}
		if resp.GetIssueUrl() != seedIssue.HTMLURL {
			t.Errorf("expected seed issue URL %q, got %q", seedIssue.HTMLURL, resp.GetIssueUrl())
		}
		if resp.GetLastError() != "" {
			t.Errorf("expected empty last_error, got %q", resp.GetLastError())
		}

		iss, err := mockGH.GetIssue(ctx, seedIssue.Number)
		if err != nil {
			t.Fatalf("failed to get seed issue: %v", err)
		}
		if iss.State != "open" {
			t.Errorf("expected seed issue to remain open, got %q", iss.State)
		}

		rec1, err := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("failed to get stored enrollment: %v", err)
		}
		if rec1.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED {
			t.Errorf("expected stored stage AWAITING_SEED, got %v", rec1.GetStage())
		}
		if rec1.GetIssueUrl() != seedIssue.HTMLURL {
			t.Errorf("expected stored issue_url %q, got %q", seedIssue.HTMLURL, rec1.GetIssueUrl())
		}

		// 2. Add seed spec (without tests) -> transitions to AWAITING_TEST_REVIEW and closes seed issue
		specsDir := filepath.Join(repoDir, "specs")
		if err := os.MkdirAll(specsDir, 0755); err != nil {
			t.Fatalf("failed to create specs dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(specsDir, "spec.md"), []byte(sampleSpec), 0644); err != nil {
			t.Fatalf("failed to write spec.md: %v", err)
		}

		resp2, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("unexpected error after adding spec.md: %v", err)
		}
		if resp2.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
			t.Errorf("expected stage AWAITING_TEST_REVIEW, got %v", resp2.GetStage())
		}
		issAfter, _ := mockGH.GetIssue(ctx, seedIssue.Number)
		if issAfter.State != "closed" {
			t.Errorf("expected seed issue to be closed once seed files exist, got %q", issAfter.State)
		}

		// 3. Close loop issue & add passing test -> transitions to ALIGNED
		loopIssues, _ := mockGH.ListIssues(ctx, "open", []string{ghclient.LabelAgenticLoop})
		for _, li := range loopIssues {
			_ = mockGH.CloseIssue(ctx, li.Number)
		}
		testsDir := filepath.Join(repoDir, "tests")
		if err := os.MkdirAll(testsDir, 0755); err != nil {
			t.Fatalf("failed to create tests dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(testsDir, "kv_test.go"), []byte(sampleTest), 0644); err != nil {
			t.Fatalf("failed to write kv_test.go: %v", err)
		}

		resp3, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("unexpected error after adding tests: %v", err)
		}
		if resp3.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
			t.Errorf("expected stage ALIGNED, got %v", resp3.GetStage())
		}
		rec3, err := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("failed to get stored enrollment: %v", err)
		}
		if rec3.GetEnrolledAtUnix() != rec1.GetEnrolledAtUnix() {
			t.Errorf("expected EnrolledAtUnix %d to be preserved, got %d", rec1.GetEnrolledAtUnix(), rec3.GetEnrolledAtUnix())
		}
	})

	t.Run("specs directory with only gitkeep and empty md records AWAITING_SEED with empty IssueUrl when no seed issue", func(t *testing.T) {
		repoDir := createTestRepoFS(t, "", "")
		specsDir := filepath.Join(repoDir, "specs")
		if err := os.MkdirAll(specsDir, 0755); err != nil {
			t.Fatalf("failed to create specs dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(specsDir, ".gitkeep"), []byte(""), 0644); err != nil {
			t.Fatalf("failed to write .gitkeep: %v", err)
		}
		if err := os.WriteFile(filepath.Join(specsDir, "empty.md"), []byte(""), 0644); err != nil {
			t.Fatalf("failed to write empty.md: %v", err)
		}

		mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
		mockStore := pstore.NewMockStore()

		pipeline := NewPipeline(
			"test-token",
			func(token, owner, repo string) ghclient.Client {
				return mockGH
			},
			evaluator.NewEvaluator(nil),
			synthesizer.NewSynthesizer(nil),
			mockStore,
		)
		pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
			return copyDir(repoDir, targetDir)
		}

		resp, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("expected nil error for empty specs/ dir, got: %v", err)
		}
		if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED {
			t.Errorf("expected stage AWAITING_SEED, got %v", resp.GetStage())
		}
		if resp.GetIssueUrl() != "" {
			t.Errorf("expected empty IssueUrl when no seed issue exists, got %q", resp.GetIssueUrl())
		}
	})
}

func TestEnroll_InvalidRepo(t *testing.T) {
	ctx := context.Background()
	mockStore := pstore.NewMockStore()
	pipeline := NewPipeline("test-token", nil, nil, nil, mockStore)

	_, err := pipeline.Enroll(ctx, "invalid-repo-without-slash")
	if err == nil {
		t.Fatal("expected error for invalid repo, got nil")
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got %v (err: %v)", st.Code(), err)
	}

	// Must not persist a record on invalid repository format
	records, listErr := mockStore.ListEnrollments(ctx)
	if listErr != nil {
		t.Fatalf("unexpected ListEnrollments error: %v", listErr)
	}
	if len(records) != 0 {
		t.Errorf("expected 0 stored records for invalid repo, got %d", len(records))
	}
}

func TestEnroll_MissingProtoFiles(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{
			name: "missing proto directory",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				specsDir := filepath.Join(dir, "specs")
				if err := os.MkdirAll(specsDir, 0755); err != nil {
					t.Fatalf("failed to create specs dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(specsDir, "spec.md"), []byte(sampleSpec), 0644); err != nil {
					t.Fatalf("failed to write spec: %v", err)
				}
				testsDir := filepath.Join(dir, "tests")
				if err := os.MkdirAll(testsDir, 0755); err != nil {
					t.Fatalf("failed to create tests dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(testsDir, "kv_test.go"), []byte(sampleTest), 0644); err != nil {
					t.Fatalf("failed to write test: %v", err)
				}
				return dir
			},
		},
		{
			name: "proto directory with only gitkeep and zero-byte proto",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				protoDir := filepath.Join(dir, "proto")
				if err := os.MkdirAll(protoDir, 0755); err != nil {
					t.Fatalf("failed to create proto dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(protoDir, ".gitkeep"), []byte("placeholder"), 0644); err != nil {
					t.Fatalf("failed to write .gitkeep: %v", err)
				}
				if err := os.WriteFile(filepath.Join(protoDir, ".hidden.proto"), []byte(sampleProto), 0644); err != nil {
					t.Fatalf("failed to write hidden proto: %v", err)
				}
				if err := os.WriteFile(filepath.Join(protoDir, "empty.proto"), []byte(""), 0644); err != nil {
					t.Fatalf("failed to write empty proto: %v", err)
				}
				specsDir := filepath.Join(dir, "specs")
				if err := os.MkdirAll(specsDir, 0755); err != nil {
					t.Fatalf("failed to create specs dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(specsDir, "spec.md"), []byte(sampleSpec), 0644); err != nil {
					t.Fatalf("failed to write spec: %v", err)
				}
				testsDir := filepath.Join(dir, "tests")
				if err := os.MkdirAll(testsDir, 0755); err != nil {
					t.Fatalf("failed to create tests dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(testsDir, "kv_test.go"), []byte(sampleTest), 0644); err != nil {
					t.Fatalf("failed to write test: %v", err)
				}
				return dir
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := tc.setup(t)
			mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
			mockStore := pstore.NewMockStore()

			seedIssue, err := mockGH.CreateIssue(ctx, &ghclient.CreateIssueRequest{
				Title:  "[Speculate] Add initial seed proto and spec files",
				Body:   "Please add seed files",
				Labels: []string{ghclient.LabelSeed},
			})
			if err != nil {
				t.Fatalf("failed to pre-create seed issue: %v", err)
			}

			evalLLM := &evaluator.MockLLMClient{
				Response: `{"coverage":[{"id":"Core-1","covered":true,"test":"TestPut","reasoning":"covers Put"}]}`,
			}

			pipeline := NewPipeline(
				"test-token",
				func(token, owner, repo string) ghclient.Client {
					return mockGH
				},
				evaluator.NewEvaluator(evalLLM),
				synthesizer.NewSynthesizer(evalLLM),
				mockStore,
			)
			pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
				return copyDir(repoDir, targetDir)
			}

			resp, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
			if err != nil {
				t.Fatalf("expected nil error for missing valid proto files, got: %v", err)
			}
			if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED {
				t.Errorf("expected stage AWAITING_SEED, got %v", resp.GetStage())
			}
			if resp.GetIssueUrl() != seedIssue.HTMLURL {
				t.Errorf("expected IssueUrl %q, got %q", seedIssue.HTMLURL, resp.GetIssueUrl())
			}
			if resp.GetLastError() != "" {
				t.Errorf("expected empty last_error, got %q", resp.GetLastError())
			}

			// Verify pre-existing open speculate-seed issue remains open
			iss, err := mockGH.GetIssue(ctx, seedIssue.Number)
			if err != nil {
				t.Fatalf("failed to get seed issue: %v", err)
			}
			if iss.State != "open" {
				t.Errorf("expected seed issue to remain open, got state %q", iss.State)
			}

			stored, err := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
			if err != nil {
				t.Fatalf("failed to get stored enrollment: %v", err)
			}
			if stored.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED {
				t.Errorf("expected stored stage AWAITING_SEED, got %v", stored.GetStage())
			}

			// Add valid proto file -> transitions cleanly to ALIGNED and closes seed issue
			protoDir := filepath.Join(repoDir, "proto")
			if err := os.MkdirAll(protoDir, 0755); err != nil {
				t.Fatalf("failed to create proto dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(protoDir, "kv.proto"), []byte(sampleProto), 0644); err != nil {
				t.Fatalf("failed to write kv.proto: %v", err)
			}

			resp2, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
			if err != nil {
				t.Fatalf("unexpected error once proto added: %v", err)
			}
			if resp2.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
				t.Errorf("expected stage ALIGNED after adding proto, got %v", resp2.GetStage())
			}
			issClosed, _ := mockGH.GetIssue(ctx, seedIssue.Number)
			if issClosed.State != "closed" {
				t.Errorf("expected seed issue to be closed, got %q", issClosed.State)
			}
		})
	}
}

func TestEnroll_AutoClosesSeedIssue(t *testing.T) {
	ctx := context.Background()
	repoDir := createTestRepoFS(t, sampleSpec, "")

	mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
	mockStore := pstore.NewMockStore()

	// Pre-create an open issue with ghclient.LabelSeed on MockClient
	seedIssue, err := mockGH.CreateIssue(ctx, &ghclient.CreateIssueRequest{
		Title:  "[Speculate] Add initial seed proto and spec files",
		Body:   "Please add seed files",
		Labels: []string{ghclient.LabelSeed},
	})
	if err != nil {
		t.Fatalf("failed to pre-create seed issue: %v", err)
	}

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
	if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
		t.Errorf("expected stage AWAITING_TEST_REVIEW, got %v", resp.GetStage())
	}

	// Verify closure comment on seed issue
	comments := mockGH.GetIssueComments(seedIssue.Number)
	if len(comments) == 0 {
		t.Fatalf("expected closure comment on seed issue #%d, got 0 comments", seedIssue.Number)
	}
	expectedComment := "Seed protobuf and specification files detected on `main`. Closing this tracking issue and initiating the Speculate autonomous alignment loop."
	if !strings.Contains(comments[0], expectedComment) {
		t.Errorf("expected comment %q, got %q", expectedComment, comments[0])
	}

	// Verify seed issue state is closed
	closedSeed, err := mockGH.GetIssue(ctx, seedIssue.Number)
	if err != nil {
		t.Fatalf("failed to get seed issue: %v", err)
	}
	if closedSeed.State != "closed" {
		t.Errorf("expected seed issue state 'closed', got %q", closedSeed.State)
	}

	// Verify speculate-agentic-loop scenario card issue is created and returned in EnrollResponse.IssueUrl
	loopIssues, err := mockGH.ListIssues(ctx, "open", []string{ghclient.LabelAgenticLoop})
	if err != nil {
		t.Fatalf("failed to list agentic loop issues: %v", err)
	}
	if len(loopIssues) != 1 {
		t.Fatalf("expected 1 open issue with label %s, got %d", ghclient.LabelAgenticLoop, len(loopIssues))
	}
	if resp.GetIssueUrl() == "" || resp.GetIssueUrl() != loopIssues[0].HTMLURL {
		t.Errorf("expected EnrollResponse.IssueUrl %q to match created loop issue %q", resp.GetIssueUrl(), loopIssues[0].HTMLURL)
	}
}

func TestEnroll_LifecycleStages_AligningAndStalled(t *testing.T) {
	ctx := context.Background()
	repoDir := createTestRepoFS(t, sampleSpec, "")

	t.Run("open speculate-align issue resolves to LIFECYCLE_STAGE_ALIGNING without creating loop issue", func(t *testing.T) {
		mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
		mockStore := pstore.NewMockStore()

		alignIssue, err := mockGH.CreateIssue(ctx, &ghclient.CreateIssueRequest{
			Title:  "[Speculate Align] Implement Core-1",
			Body:   "Aligning internal scaffolding",
			Labels: []string{ghclient.LabelAlign},
		})
		if err != nil {
			t.Fatalf("failed to create align issue: %v", err)
		}

		llm := &evaluator.MockLLMClient{Response: `{}`}
		pipeline := NewPipeline(
			"test-token",
			func(token, owner, repo string) ghclient.Client { return mockGH },
			evaluator.NewEvaluator(llm),
			synthesizer.NewSynthesizer(llm),
			mockStore,
		)
		pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
			return copyDir(repoDir, targetDir)
		}

		resp, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
			t.Errorf("expected stage ALIGNING, got %v", resp.GetStage())
		}
		if resp.GetIssueUrl() != alignIssue.HTMLURL {
			t.Errorf("expected IssueUrl %q, got %q", alignIssue.HTMLURL, resp.GetIssueUrl())
		}

		record, err := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("failed to get stored enrollment: %v", err)
		}
		if record.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
			t.Errorf("expected stored stage ALIGNING, got %v", record.GetStage())
		}
		if record.GetIssueUrl() != alignIssue.HTMLURL {
			t.Errorf("expected stored IssueUrl %q, got %q", alignIssue.HTMLURL, record.GetIssueUrl())
		}

		loopIssues, _ := mockGH.ListIssues(ctx, "open", []string{ghclient.LabelAgenticLoop})
		if len(loopIssues) != 0 {
			t.Errorf("expected 0 loop issues created while ALIGNING, got %d", len(loopIssues))
		}
	})

	t.Run("open speculate-stalled issue takes priority and resolves to LIFECYCLE_STAGE_STALLED", func(t *testing.T) {
		mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
		mockStore := pstore.NewMockStore()

		_, _ = mockGH.CreateIssue(ctx, &ghclient.CreateIssueRequest{
			Title:  "[Speculate Align] Implement Core-1",
			Body:   "Aligning",
			Labels: []string{ghclient.LabelAlign},
		})
		stalledIssue, err := mockGH.CreateIssue(ctx, &ghclient.CreateIssueRequest{
			Title:  "[Speculate Stalled] Core-1 alignment failed",
			Body:   "Human attention required",
			Labels: []string{ghclient.LabelStalled},
		})
		if err != nil {
			t.Fatalf("failed to create stalled issue: %v", err)
		}

		llm := &evaluator.MockLLMClient{Response: `{}`}
		pipeline := NewPipeline(
			"test-token",
			func(token, owner, repo string) ghclient.Client { return mockGH },
			evaluator.NewEvaluator(llm),
			synthesizer.NewSynthesizer(llm),
			mockStore,
		)
		pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
			return copyDir(repoDir, targetDir)
		}

		resp, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_STALLED {
			t.Errorf("expected stage STALLED, got %v", resp.GetStage())
		}
		if resp.GetIssueUrl() != stalledIssue.HTMLURL {
			t.Errorf("expected IssueUrl %q, got %q", stalledIssue.HTMLURL, resp.GetIssueUrl())
		}

		record, err := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("failed to get stored enrollment: %v", err)
		}
		if record.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_STALLED {
			t.Errorf("expected stored stage STALLED, got %v", record.GetStage())
		}
		if record.GetIssueUrl() != stalledIssue.HTMLURL {
			t.Errorf("expected stored IssueUrl %q, got %q", stalledIssue.HTMLURL, record.GetIssueUrl())
		}
	})
}

func TestEnroll_FaultPreservationAndRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("first check saves initial EVALUATING record and preserves EVALUATING on clone error", func(t *testing.T) {
		mockGH := ghclient.NewMockClient("brotherlogic", "new-repo")
		mockStore := pstore.NewMockStore()

		var sawInitialEvaluating bool
		pipeline := NewPipeline(
			"test-token",
			func(token, owner, repo string) ghclient.Client { return mockGH },
			evaluator.NewEvaluator(nil),
			synthesizer.NewSynthesizer(nil),
			mockStore,
		)
		pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
			rec, err := mockStore.GetEnrollment(ctx, "brotherlogic/new-repo")
			if err == nil && rec != nil && rec.GetStage() == pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING {
				sawInitialEvaluating = true
			}
			return errors.New("network timeout cloning repo")
		}

		_, err := pipeline.Enroll(ctx, "brotherlogic/new-repo")
		if err == nil {
			t.Fatal("expected clone error, got nil")
		}
		if !sawInitialEvaluating {
			t.Errorf("expected initial EVALUATING record to be saved before cloning")
		}

		rec, getErr := mockStore.GetEnrollment(ctx, "brotherlogic/new-repo")
		if getErr != nil {
			t.Fatalf("expected fault record in store, got error: %v", getErr)
		}
		if rec.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING {
			t.Errorf("expected preserved stage EVALUATING, got %v", rec.GetStage())
		}
		if !strings.Contains(rec.GetLastError(), "network timeout cloning repo") {
			t.Errorf("expected LastError to contain clone error, got %q", rec.GetLastError())
		}
		if rec.GetLastEvaluatedAtUnix() <= 0 {
			t.Errorf("expected LastEvaluatedAtUnix > 0, got %d", rec.GetLastEvaluatedAtUnix())
		}
	})

	t.Run("clone error, malformed spec, and LLM error preserve prior stage and clean check clears LastError", func(t *testing.T) {
		repoDir := createTestRepoFS(t, sampleSpec, sampleTest)
		mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
		mockStore := pstore.NewMockStore()

		priorTime := time.Now().Unix() - 3600
		priorEval := priorTime + 60
		priorIssue := "https://github.com/brotherlogic/speculate-kv/issues/7"
		if err := mockStore.SaveEnrollment(ctx, &pb.EnrollmentRecord{
			Repository:          "brotherlogic/speculate-kv",
			AlignmentPercentage: 50,
			IssueUrl:            priorIssue,
			EnrolledAtUnix:      priorTime,
			LastEvaluatedAtUnix: priorEval,
			StatusMessage:       "aligning (50%)",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
			LastError:           "",
		}); err != nil {
			t.Fatalf("failed to seed prior record: %v", err)
		}

		var failClone atomic.Bool
		var failLLM atomic.Bool
		llm := &funcLLMClient{
			fn: func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
				if failLLM.Load() {
					return "", errors.New("ollama 503 service unavailable")
				}
				return `{"coverage":[{"id":"Core-1","covered":true,"test":"TestPut","reasoning":"covers Put"}]}`, nil
			},
		}

		pipeline := NewPipeline(
			"test-token",
			func(token, owner, repo string) ghclient.Client { return mockGH },
			evaluator.NewEvaluator(llm),
			synthesizer.NewSynthesizer(llm),
			mockStore,
		)
		pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
			if failClone.Load() {
				return errors.New("git clone exit status 128")
			}
			return copyDir(repoDir, targetDir)
		}

		// 1. Clone failure preserves ALIGNING stage
		failClone.Store(true)
		if _, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv"); err == nil {
			t.Fatal("expected error on clone failure")
		}
		recAfterCloneErr, err := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("failed to read record: %v", err)
		}
		if recAfterCloneErr.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
			t.Errorf("expected preserved stage ALIGNING, got %v", recAfterCloneErr.GetStage())
		}
		if recAfterCloneErr.GetAlignmentPercentage() != 50 {
			t.Errorf("expected preserved alignment 50, got %d", recAfterCloneErr.GetAlignmentPercentage())
		}
		if recAfterCloneErr.GetIssueUrl() != priorIssue {
			t.Errorf("expected preserved IssueUrl %q, got %q", priorIssue, recAfterCloneErr.GetIssueUrl())
		}
		if recAfterCloneErr.GetEnrolledAtUnix() != priorTime {
			t.Errorf("expected preserved EnrolledAtUnix %d, got %d", priorTime, recAfterCloneErr.GetEnrolledAtUnix())
		}
		if recAfterCloneErr.GetLastEvaluatedAtUnix() <= priorEval {
			t.Errorf("expected LastEvaluatedAtUnix > %d, got %d", priorEval, recAfterCloneErr.GetLastEvaluatedAtUnix())
		}
		if !strings.Contains(recAfterCloneErr.GetLastError(), "git clone exit status 128") {
			t.Errorf("expected clone error in LastError, got %q", recAfterCloneErr.GetLastError())
		}
		failClone.Store(false)

		// 2. Malformed non-empty spec file (scanner token too long -> ParseSpecFile error) preserves ALIGNING stage
		specPath := filepath.Join(repoDir, "specs", "spec.md")
		oversizedLine := strings.Repeat("x", 70*1024)
		if err := os.WriteFile(specPath, []byte(oversizedLine), 0644); err != nil {
			t.Fatalf("failed to write malformed spec: %v", err)
		}
		if _, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv"); err == nil {
			t.Fatal("expected error on malformed spec file")
		}
		recAfterSpecErr, _ := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if recAfterSpecErr.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
			t.Errorf("expected preserved stage ALIGNING on malformed spec, got %v", recAfterSpecErr.GetStage())
		}
		if recAfterSpecErr.GetLastError() == "" {
			t.Errorf("expected non-empty LastError on malformed spec")
		}

		// Also test non-empty spec file lacking any [Stage: ...] sections
		if err := os.WriteFile(specPath, []byte("# Title Only\nNo stages defined here.\n"), 0644); err != nil {
			t.Fatalf("failed to write stage-less spec: %v", err)
		}
		if _, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv"); err == nil {
			t.Fatal("expected error on non-empty spec without valid stages")
		}
		recAfterStagelessErr, _ := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if recAfterStagelessErr.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
			t.Errorf("expected preserved stage ALIGNING, got %v", recAfterStagelessErr.GetStage())
		}
		if recAfterStagelessErr.GetLastError() == "" {
			t.Errorf("expected non-empty LastError on stage-less non-empty spec")
		}

		// Restore valid spec
		if err := os.WriteFile(specPath, []byte(sampleSpec), 0644); err != nil {
			t.Fatalf("failed to restore valid spec: %v", err)
		}

		// 3. LLM evaluation failure preserves ALIGNING stage
		failLLM.Store(true)
		if _, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv"); err == nil {
			t.Fatal("expected error on LLM failure")
		}
		recAfterLLMErr, _ := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if recAfterLLMErr.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
			t.Errorf("expected preserved stage ALIGNING on LLM error, got %v", recAfterLLMErr.GetStage())
		}
		if !strings.Contains(recAfterLLMErr.GetLastError(), "ollama 503 service unavailable") {
			t.Errorf("expected LLM error in LastError, got %q", recAfterLLMErr.GetLastError())
		}
		failLLM.Store(false)

		// 4. Subsequent clean check clears LastError to "" and resolves stage to ALIGNED
		respClean, err := pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
		if err != nil {
			t.Fatalf("expected clean check to succeed, got: %v", err)
		}
		if respClean.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
			t.Errorf("expected stage ALIGNED on recovery, got %v", respClean.GetStage())
		}
		if respClean.GetLastError() != "" {
			t.Errorf("expected response LastError cleared to empty string, got %q", respClean.GetLastError())
		}

		recClean, _ := mockStore.GetEnrollment(ctx, "brotherlogic/speculate-kv")
		if recClean.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
			t.Errorf("expected stored stage ALIGNED on recovery, got %v", recClean.GetStage())
		}
		if recClean.GetLastError() != "" {
			t.Errorf("expected stored LastError cleared to empty string, got %q", recClean.GetLastError())
		}
		if recClean.GetEnrolledAtUnix() != priorTime {
			t.Errorf("expected EnrolledAtUnix %d preserved across recovery, got %d", priorTime, recClean.GetEnrolledAtUnix())
		}
	})
}

func TestEnroll_SingleflightConcurrency(t *testing.T) {
	ctx := context.Background()
	repoDir := createTestRepoFS(t, sampleSpec, sampleTest)

	var cloneCalls atomic.Int64
	var llmCalls atomic.Int64
	cloneEntered := make(chan struct{})
	releaseClone := make(chan struct{})
	var enterOnce sync.Once

	llm := &funcLLMClient{
		fn: func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
			llmCalls.Add(1)
			return `{"coverage":[{"id":"Core-1","covered":true,"test":"TestPut","reasoning":"covers Put"}]}`, nil
		},
	}

	var ghMu sync.Mutex
	ghClients := make(map[string]*ghclient.MockClient)
	mockStore := pstore.NewMockStore()

	pipeline := NewPipeline(
		"test-token",
		func(token, owner, repo string) ghclient.Client {
			ghMu.Lock()
			defer ghMu.Unlock()
			key := owner + "/" + repo
			if c, ok := ghClients[key]; ok {
				return c
			}
			c := ghclient.NewMockClient(owner, repo)
			ghClients[key] = c
			return c
		},
		evaluator.NewEvaluator(llm),
		synthesizer.NewSynthesizer(llm),
		mockStore,
	)
	pipeline.cloneFn = func(ctx context.Context, repoURL, targetDir string) error {
		cloneCalls.Add(1)
		if strings.Contains(repoURL, "speculate-kv") {
			enterOnce.Do(func() {
				close(cloneEntered)
			})
			<-releaseClone
		}
		return copyDir(repoDir, targetDir)
	}

	const concurrency = 8
	var wg sync.WaitGroup
	resps := make([]*pb.EnrollResponse, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			repoArg := "brotherlogic/speculate-kv"
			if idx%2 == 1 {
				repoArg = "https://github.com/brotherlogic/speculate-kv.git"
			}
			resps[idx], errs[idx] = pipeline.Enroll(ctx, repoArg)
		}(i)
	}

	<-cloneEntered
	// Give sibling goroutines a brief moment to enter Pipeline.Enroll singleflight
	time.Sleep(30 * time.Millisecond)
	close(releaseClone)
	wg.Wait()

	for i := 0; i < concurrency; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d returned unexpected error: %v", i, errs[i])
		}
		if resps[i].GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
			t.Errorf("goroutine %d expected ALIGNED, got %v", i, resps[i].GetStage())
		}
	}

	if got := cloneCalls.Load(); got != 1 {
		t.Errorf("expected singleflight to deduplicate cloneCalls to 1, got %d", got)
	}
	if got := llmCalls.Load(); got != 1 {
		t.Errorf("expected singleflight to deduplicate llmCalls to 1, got %d", got)
	}

	// Evaluating a distinct repository executes independently
	if _, err := pipeline.Enroll(ctx, "brotherlogic/other-repo"); err != nil {
		t.Fatalf("unexpected error enrolling second repo: %v", err)
	}
	if got := cloneCalls.Load(); got != 2 {
		t.Errorf("expected 2 total clone calls after second distinct repo, got %d", got)
	}
}

func TestPipeline_ListEnrollments(t *testing.T) {
	ctx := context.Background()

	t.Run("nil store returns nil slice and nil error", func(t *testing.T) {
		pipeline := NewPipeline("test-token", nil, nil, nil, nil)
		records, err := pipeline.ListEnrollments(ctx)
		if err != nil {
			t.Fatalf("expected nil error when store is nil, got: %v", err)
		}
		if records != nil {
			t.Errorf("expected nil records slice when store is nil, got: %v", records)
		}
	})

	t.Run("delegates to pstore Store ListEnrollments", func(t *testing.T) {
		mockStore := pstore.NewMockStore()
		_ = mockStore.SaveEnrollment(ctx, &pb.EnrollmentRecord{
			Repository:          "brotherlogic/zeta",
			AlignmentPercentage: 100,
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
		})
		_ = mockStore.SaveEnrollment(ctx, &pb.EnrollmentRecord{
			Repository:          "brotherlogic/alpha",
			AlignmentPercentage: 0,
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
		})

		pipeline := NewPipeline("test-token", nil, nil, nil, mockStore)
		records, err := pipeline.ListEnrollments(ctx)
		if err != nil {
			t.Fatalf("unexpected error from ListEnrollments: %v", err)
		}
		if len(records) != 2 {
			t.Fatalf("expected 2 records, got %d", len(records))
		}
		if records[0].GetRepository() != "brotherlogic/alpha" || records[1].GetRepository() != "brotherlogic/zeta" {
			t.Errorf("expected sorted records [alpha, zeta], got [%s, %s]", records[0].GetRepository(), records[1].GetRepository())
		}
	})
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
	return strings.Contains(content, expected)
}
