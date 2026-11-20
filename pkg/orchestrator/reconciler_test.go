package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brotherlogic/speculate/pkg/dcm"
	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
)

const testProto = `syntax = "proto3";
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

const testSpec = `# Key-Value Service
## [Stage: Core] Basic Key-Value Storage
- Put: Stores a key and associated value.
`

const testGoFile = `package tests

import "testing"

// Stage: Core
// Scenario: Core-1 Put stores key
func TestPut(t *testing.T) {}
`

type funcLLM struct {
	fn func(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

func (f *funcLLM) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return f.fn(ctx, systemPrompt, userPrompt)
}

func newDefaultMockLLM() evaluator.LLMClient {
	return &funcLLM{
		fn: func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
			if strings.Contains(userPrompt, "TestPut") {
				return `{"coverage":[{"id":"Core-1","covered":true,"test":"TestPut","reasoning":"covers Put"}]}`, nil
			}
			return `{
				"id": "Core-1",
				"stage": "Core",
				"requirement": "Put: Stores a key and associated value.",
				"target_rpc": "Put",
				"title": "Put stores key",
				"given": "An empty store",
				"when": "Put is called",
				"then": "Value is stored",
				"test_func_name": "TestPut_Basic"
			}`, nil
		},
	}
}

func createRepoSnapshotDir(t *testing.T, withProto, withSpec, withTest bool) string {
	t.Helper()
	dir := t.TempDir()

	if withProto {
		protoDir := filepath.Join(dir, "proto")
		if err := os.MkdirAll(protoDir, 0755); err != nil {
			t.Fatalf("mkdir proto: %v", err)
		}
		if err := os.WriteFile(filepath.Join(protoDir, "kv.proto"), []byte(testProto), 0644); err != nil {
			t.Fatalf("write proto: %v", err)
		}
	}

	if withSpec {
		specsDir := filepath.Join(dir, "specs")
		if err := os.MkdirAll(specsDir, 0755); err != nil {
			t.Fatalf("mkdir specs: %v", err)
		}
		if err := os.WriteFile(filepath.Join(specsDir, "kv.md"), []byte(testSpec), 0644); err != nil {
			t.Fatalf("write spec: %v", err)
		}
	}

	if withTest {
		testsDir := filepath.Join(dir, "tests")
		if err := os.MkdirAll(testsDir, 0755); err != nil {
			t.Fatalf("mkdir tests: %v", err)
		}
		if err := os.WriteFile(filepath.Join(testsDir, "kv_test.go"), []byte(testGoFile), 0644); err != nil {
			t.Fatalf("write test: %v", err)
		}
	}

	return dir
}

func copyTree(src, dst string) error {
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

type failingDCMClient struct {
	*dcm.MockClient
}

func (f *failingDCMClient) FailContainer(id, errMsg string) {
	list, _ := f.MockClient.List(context.Background())
	for _, cfg := range list {
		if cfg.GetId() == id {
			cfg.State = dcm.StateFailed
			cfg.ErrorMessage = errMsg
		}
	}
}

func setupTestHarness(t *testing.T, repoDir string) (
	*ghclient.MockClient,
	*failingDCMClient,
	*pstore.MockStore,
	*Reconciler,
	*time.Time,
) {
	t.Helper()
	mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
	mockDCM := &failingDCMClient{MockClient: dcm.NewMockClient()}
	mockStore := pstore.NewMockStore()
	llm := newDefaultMockLLM()

	currentTime := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	rec := NewReconciler(
		"test-token",
		func(token, owner, repo string) ghclient.Client { return mockGH },
		mockDCM,
		evaluator.NewEvaluator(llm),
		synthesizer.NewSynthesizer(llm),
		mockStore,
	).WithPollInterval(5 * time.Millisecond).WithNowFn(func() time.Time {
		return currentTime
	}).WithCloneFn(func(ctx context.Context, repoURL, targetDir string) error {
		return copyTree(repoDir, targetDir)
	})

	return mockGH, mockDCM, mockStore, rec, &currentTime
}

func TestReconciler_Phase4Transitions(t *testing.T) {
	ctx := context.Background()
	const repo = "brotherlogic/speculate-kv"

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "seed missing -> AWAITING_SEED -> seed added -> EVALUATING + DCM.Up with PR-based badge",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, false, false) // missing specs/
				mockGH, mockDCM, mockStore, rec, _ := setupTestHarness(t, repoDir)

				seedIssue, err := mockGH.CreateIssue(ctx, &ghclient.CreateIssueRequest{
					Title:  "[Speculate] Add initial seed proto and spec files",
					Body:   "Add seed files",
					Labels: []string{ghclient.LabelSeed},
				})
				if err != nil {
					t.Fatalf("create seed issue: %v", err)
				}

				// 1. Seed missing -> AWAITING_SEED
				resp1, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("reconcile awaiting seed failed: %v", err)
				}
				if resp1.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED {
					t.Fatalf("expected AWAITING_SEED, got %v", resp1.GetStage())
				}
				containers, _ := mockDCM.List(ctx)
				if len(containers) != 0 {
					t.Fatalf("expected 0 DCM containers while AWAITING_SEED, got %d", len(containers))
				}

				// 2. Add seed spec -> closes seed issue, creates feat/<scenario>, updates badge on feat branch (not main), calls DCM.Up
				specsDir := filepath.Join(repoDir, "specs")
				if err := os.MkdirAll(specsDir, 0755); err != nil {
					t.Fatalf("mkdir specs: %v", err)
				}
				if err := os.WriteFile(filepath.Join(specsDir, "kv.md"), []byte(testSpec), 0644); err != nil {
					t.Fatalf("write spec: %v", err)
				}

				resp2, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("reconcile after seed added failed: %v", err)
				}
				if resp2.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING {
					t.Fatalf("expected EVALUATING, got %v", resp2.GetStage())
				}

				closedSeed, _ := mockGH.GetIssue(ctx, seedIssue.Number)
				if closedSeed.State != "closed" {
					t.Errorf("expected seed issue closed, got %q", closedSeed.State)
				}

				// Badge must NOT be written directly to main
				mainReadme, err := mockGH.GetFileContent(ctx, "README.md", "main")
				if err != nil {
					t.Fatalf("read main README: %v", err)
				}
				if strings.Contains(mainReadme.Content, "Spec%20Alignment") {
					t.Errorf("expected main README NOT to be updated directly, got:\n%s", mainReadme.Content)
				}

				// Verify record state and DCM container
				stored, err := mockStore.GetEnrollment(ctx, repo)
				if err != nil {
					t.Fatalf("get stored record: %v", err)
				}
				if stored.GetActiveBranch() == "" || !strings.HasPrefix(stored.GetActiveBranch(), "feat/") {
					t.Errorf("expected feat/ active_branch, got %q", stored.GetActiveBranch())
				}
				if stored.GetAttemptCount() != 1 {
					t.Errorf("expected attempt_count 1, got %d", stored.GetAttemptCount())
				}
				if stored.GetActiveContainerId() == "" {
					t.Errorf("expected non-empty active_container_id")
				}
				if stored.GetActiveIssueNumber() == 0 {
					t.Errorf("expected non-zero active_issue_number")
				}

				branchReadme, err := mockGH.GetFileContent(ctx, "README.md", stored.GetActiveBranch())
				if err != nil {
					t.Fatalf("read branch README: %v", err)
				}
				if !strings.Contains(branchReadme.Content, "Spec%20Alignment-0%25") {
					t.Errorf("expected branch README to contain 0%% badge, got:\n%s", branchReadme.Content)
				}

				containers, _ = mockDCM.List(ctx)
				if len(containers) != 1 {
					t.Fatalf("expected 1 active DCM container, got %d", len(containers))
				}
				if containers[0].GetRequest().GetHarness() != dcm.HarnessAntigravity {
					t.Errorf("expected HARNESS_ANTIGRAVITY, got %v", containers[0].GetRequest().GetHarness())
				}
				if containers[0].GetRequest().GetBranch() != stored.GetActiveBranch() {
					t.Errorf("expected DCM branch %q, got %q", stored.GetActiveBranch(), containers[0].GetRequest().GetBranch())
				}
			},
		},
		{
			name: "Red compile=success, test=failure -> DCM.Down + AWAITING_TEST_REVIEW",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, true, false)
				mockGH, mockDCM, mockStore, rec, _ := setupTestHarness(t, repoDir)

				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("initial reconcile failed: %v", err)
				}
				rec1, _ := mockStore.GetEnrollment(ctx, repo)
				redContainerID := rec1.GetActiveContainerId()
				branch := rec1.GetActiveBranch()

				// Red agent commits compiling-but-RED test and opens PR
				if err := mockGH.UpdateFile(ctx, "tests/kv_test.go", testGoFile, "", branch, "test: add failing test for Core-1"); err != nil {
					t.Fatalf("update test file: %v", err)
				}
				pr, err := mockGH.CreatePullRequest(ctx, &ghclient.CreatePRRequest{
					Title: "test: Core-1 Put stores key",
					Head:  branch,
					Base:  "main",
					Body:  "Red test",
				})
				if err != nil {
					t.Fatalf("create PR: %v", err)
				}
				mockGH.SetCheckRunsForRef(pr.HeadSHA, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "failure", OutputSummary: "Put unimplemented"},
				})

				resp, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("reconcile red completion failed: %v", err)
				}
				if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
					t.Fatalf("expected AWAITING_TEST_REVIEW, got %v", resp.GetStage())
				}

				rec2, _ := mockStore.GetEnrollment(ctx, repo)
				if rec2.GetActiveContainerId() != "" {
					t.Errorf("expected active_container_id cleared after Down, got %q", rec2.GetActiveContainerId())
				}
				if rec2.GetActivePrNumber() != int32(pr.Number) {
					t.Errorf("expected active_pr_number %d, got %d", pr.Number, rec2.GetActivePrNumber())
				}
				if rec2.GetLastEvaluatedSha() != pr.HeadSHA {
					t.Errorf("expected last_evaluated_sha %q, got %q", pr.HeadSHA, rec2.GetLastEvaluatedSha())
				}
				if len(mockDCM.DownCalls) != 1 || mockDCM.DownCalls[0] != redContainerID {
					t.Errorf("expected Down called for %q, got %v", redContainerID, mockDCM.DownCalls)
				}
			},
		},
		{
			name: "Red tautological test=success and compile=failure -> Down+Up retries -> 3rd strike STALLED",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, true, false)
				mockGH, mockDCM, mockStore, rec, _ := setupTestHarness(t, repoDir)

				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("initial reconcile failed: %v", err)
				}
				rec1, _ := mockStore.GetEnrollment(ctx, repo)
				branch := rec1.GetActiveBranch()
				c1 := rec1.GetActiveContainerId()

				pr, err := mockGH.CreatePullRequest(ctx, &ghclient.CreatePRRequest{
					Title: "test: Core-1",
					Head:  branch,
					Base:  "main",
				})
				if err != nil {
					t.Fatalf("create PR: %v", err)
				}

				// Strike 1: tautological test (compile=success, test=success)
				sha1 := "1111111111111111111111111111111111111111"
				mockGH.SetBranchSHA(branch, sha1)
				mockGH.SetCheckRunsForRef(sha1, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "success"},
				})

				resp1, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("strike 1 reconcile failed: %v", err)
				}
				if resp1.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING {
					t.Fatalf("expected EVALUATING after strike 1, got %v", resp1.GetStage())
				}
				rec2, _ := mockStore.GetEnrollment(ctx, repo)
				c2 := rec2.GetActiveContainerId()
				if rec2.GetAttemptCount() != 2 {
					t.Errorf("expected attempt_count 2 after strike 1, got %d", rec2.GetAttemptCount())
				}
				if c2 == "" || c2 == c1 {
					t.Errorf("expected fresh container ID after strike 1, got %q (prev %q)", c2, c1)
				}

				// Strike 2: compile=failure
				sha2 := "2222222222222222222222222222222222222222"
				mockGH.SetBranchSHA(branch, sha2)
				mockGH.SetCheckRunsForRef(sha2, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "failure", OutputSummary: "syntax error in kv_test.go"},
					{Name: "test", Status: "completed", Conclusion: "failure"},
				})

				resp2, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("strike 2 reconcile failed: %v", err)
				}
				if resp2.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING {
					t.Fatalf("expected EVALUATING after strike 2, got %v", resp2.GetStage())
				}
				rec3, _ := mockStore.GetEnrollment(ctx, repo)
				c3 := rec3.GetActiveContainerId()
				if rec3.GetAttemptCount() != 3 {
					t.Errorf("expected attempt_count 3 after strike 2, got %d", rec3.GetAttemptCount())
				}
				if c3 == "" || c3 == c2 {
					t.Errorf("expected fresh container ID after strike 2, got %q (prev %q)", c3, c2)
				}

				// Strike 3: DCM_FAILED -> STALLED
				mockDCM.FailContainer(c3, "harness crashed")
				resp3, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("strike 3 reconcile failed: %v", err)
				}
				if resp3.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_STALLED {
					t.Fatalf("expected STALLED after strike 3, got %v", resp3.GetStage())
				}
				rec4, _ := mockStore.GetEnrollment(ctx, repo)
				if rec4.GetActiveContainerId() != "" {
					t.Errorf("expected empty active_container_id when STALLED, got %q", rec4.GetActiveContainerId())
				}

				iss, err := mockGH.GetIssue(ctx, int(rec4.GetActiveIssueNumber()))
				if err != nil {
					t.Fatalf("get active issue: %v", err)
				}
				hasStalledLabel := false
				for _, l := range iss.Labels {
					if l == ghclient.LabelStalled {
						hasStalledLabel = true
					}
				}
				if !hasStalledLabel {
					t.Errorf("expected issue #%d to have label %s, got %v", iss.Number, ghclient.LabelStalled, iss.Labels)
				}
				comments := mockGH.GetIssueComments(iss.Number)
				if len(comments) == 0 {
					t.Errorf("expected diagnostic comment posted on stalled issue #%d", iss.Number)
				}
				if len(mockDCM.DownCalls) != 3 {
					t.Errorf("expected 3 DCM Down calls across strikes, got %d (%v)", len(mockDCM.DownCalls), mockDCM.DownCalls)
				}
				_ = pr
			},
		},
		{
			name: "@Owner APPROVED review -> closes Red issue, opens Align issue, launches fresh DCM session (ALIGNING)",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, true, false)
				mockGH, mockDCM, mockStore, rec, _ := setupTestHarness(t, repoDir)

				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("initial reconcile failed: %v", err)
				}
				rec1, _ := mockStore.GetEnrollment(ctx, repo)
				branch := rec1.GetActiveBranch()
				redIssueNum := int(rec1.GetActiveIssueNumber())
				redContainerID := rec1.GetActiveContainerId()

				approvedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				mockGH.SetBranchSHA(branch, approvedSHA)
				pr, err := mockGH.CreatePullRequest(ctx, &ghclient.CreatePRRequest{
					Title: "test: Core-1",
					Head:  branch,
					Base:  "main",
				})
				if err != nil {
					t.Fatalf("create PR: %v", err)
				}
				mockGH.SetCheckRunsForRef(approvedSHA, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "failure"},
				})

				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("awaiting review reconcile failed: %v", err)
				}

				// @Owner approves PR at approvedSHA
				mockGH.AddPullRequestReview(pr.Number, &ghclient.PullRequestReview{
					ID:          101,
					Author:      "brotherlogic",
					State:       "APPROVED",
					CommitID:    approvedSHA,
					SubmittedAt: time.Now(),
				})

				resp, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("post-approval reconcile failed: %v", err)
				}
				if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
					t.Fatalf("expected ALIGNING, got %v", resp.GetStage())
				}

				redIssue, _ := mockGH.GetIssue(ctx, redIssueNum)
				if redIssue.State != "closed" {
					t.Errorf("expected Red loop issue #%d closed, got %q", redIssueNum, redIssue.State)
				}

				alignIssue, err := mockGH.FindOpenIssueByLabel(ctx, ghclient.LabelAlign)
				if err != nil || alignIssue == nil {
					t.Fatalf("expected open %s issue, got %v (err: %v)", ghclient.LabelAlign, alignIssue, err)
				}

				rec2, _ := mockStore.GetEnrollment(ctx, repo)
				if rec2.GetApprovedTestSha() != approvedSHA {
					t.Errorf("expected approved_test_sha %q, got %q", approvedSHA, rec2.GetApprovedTestSha())
				}
				if rec2.GetAttemptCount() != 1 {
					t.Errorf("expected attempt_count 1 for fresh Green phase, got %d", rec2.GetAttemptCount())
				}
				if rec2.GetActiveIssueNumber() != int32(alignIssue.Number) {
					t.Errorf("expected active_issue_number %d, got %d", alignIssue.Number, rec2.GetActiveIssueNumber())
				}
				if rec2.GetActiveContainerId() == "" || rec2.GetActiveContainerId() == redContainerID {
					t.Errorf("expected fresh Green container ID distinct from Red %q, got %q", redContainerID, rec2.GetActiveContainerId())
				}

				containers, _ := mockDCM.List(ctx)
				if len(containers) != 1 || !strings.Contains(containers[0].GetRequest().GetPrompt(), "internal/**") {
					t.Errorf("expected fresh Green DCM container with internal/** prompt, got %+v", containers)
				}
			},
		},
		{
			name: "post-approval edit to tests/foo_test.go -> ResetBranch to approved_test_sha + strike increment",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, true, false)
				mockGH, mockDCM, mockStore, rec, _ := setupTestHarness(t, repoDir)

				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("initial reconcile failed: %v", err)
				}
				rec1, _ := mockStore.GetEnrollment(ctx, repo)
				branch := rec1.GetActiveBranch()

				approvedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				mockGH.SetBranchSHA(branch, approvedSHA)
				pr, _ := mockGH.CreatePullRequest(ctx, &ghclient.CreatePRRequest{Title: "test: Core-1", Head: branch, Base: "main"})
				mockGH.SetCheckRunsForRef(approvedSHA, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "failure"},
				})
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("red verified reconcile failed: %v", err)
				}
				mockGH.AddPullRequestReview(pr.Number, &ghclient.PullRequestReview{
					ID:       101,
					Author:   "brotherlogic",
					State:    "APPROVED",
					CommitID: approvedSHA,
				})
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("approval reconcile failed: %v", err)
				}
				recAlign, _ := mockStore.GetEnrollment(ctx, repo)
				c1 := recAlign.GetActiveContainerId()

				// Align agent tampers with tests/foo_test.go
				tamperedSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				mockGH.SetBranchSHA(branch, tamperedSHA)
				mockGH.SetChangedFiles(approvedSHA, tamperedSHA, []string{"internal/server.go", "tests/foo_test.go"})

				resp, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("tamper reconcile failed: %v", err)
				}
				if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
					t.Fatalf("expected ALIGNING after strike 1 tamper reset, got %v", resp.GetStage())
				}

				b, err := mockGH.GetBranch(ctx, branch)
				if err != nil {
					t.Fatalf("get branch: %v", err)
				}
				if b.SHA != approvedSHA {
					t.Errorf("expected branch %q reset to %q, got %q", branch, approvedSHA, b.SHA)
				}

				recAfter, _ := mockStore.GetEnrollment(ctx, repo)
				if recAfter.GetAttemptCount() != 2 {
					t.Errorf("expected attempt_count incremented to 2, got %d", recAfter.GetAttemptCount())
				}
				if recAfter.GetActiveContainerId() == "" || recAfter.GetActiveContainerId() == c1 {
					t.Errorf("expected fresh container ID after tamper reset, got %q (prev %q)", recAfter.GetActiveContainerId(), c1)
				}
				if len(mockDCM.DownCalls) < 2 {
					t.Errorf("expected Down called on container %q, got %v", c1, mockDCM.DownCalls)
				}
			},
		},
		{
			name: "Green 30m timeout and 3-strike STALLED -> label speculate-stalled removed -> strikes reset to 0 and loop resumes",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, true, false)
				mockGH, mockDCM, mockStore, rec, nowPtr := setupTestHarness(t, repoDir)

				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("initial reconcile failed: %v", err)
				}
				rec1, _ := mockStore.GetEnrollment(ctx, repo)
				branch := rec1.GetActiveBranch()

				approvedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				mockGH.SetBranchSHA(branch, approvedSHA)
				pr, _ := mockGH.CreatePullRequest(ctx, &ghclient.CreatePRRequest{Title: "test: Core-1", Head: branch, Base: "main"})
				mockGH.SetCheckRunsForRef(approvedSHA, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "failure"},
				})
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("red verified reconcile failed: %v", err)
				}
				mockGH.AddPullRequestReview(pr.Number, &ghclient.PullRequestReview{
					ID:       101,
					Author:   "brotherlogic",
					State:    "APPROVED",
					CommitID: approvedSHA,
				})
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("approval reconcile failed: %v", err)
				}

				// Strike 1: 30m no-push timeout
				*nowPtr = nowPtr.Add(31 * time.Minute)
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("timeout strike 1 failed: %v", err)
				}
				recS1, _ := mockStore.GetEnrollment(ctx, repo)
				if recS1.GetAttemptCount() != 2 {
					t.Fatalf("expected attempt_count 2 after timeout strike 1, got %d", recS1.GetAttemptCount())
				}

				// Strike 2: failed check run
				badSHA1 := "cccccccccccccccccccccccccccccccccccccccc"
				mockGH.SetBranchSHA(branch, badSHA1)
				mockGH.SetChangedFiles(approvedSHA, badSHA1, []string{"internal/server/server.go"})
				mockGH.SetCheckRunsForRef(badSHA1, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "failure", OutputSummary: "still failing"},
					{Name: "review-gate", Status: "completed", Conclusion: "success"},
				})
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("check failure strike 2 failed: %v", err)
				}
				recS2, _ := mockStore.GetEnrollment(ctx, repo)
				if recS2.GetAttemptCount() != 3 {
					t.Fatalf("expected attempt_count 3 after strike 2, got %d", recS2.GetAttemptCount())
				}

				// Strike 3: failed check run -> STALLED
				badSHA2 := "dddddddddddddddddddddddddddddddddddddddd"
				mockGH.SetBranchSHA(branch, badSHA2)
				mockGH.SetChangedFiles(approvedSHA, badSHA2, []string{"internal/server/server.go"})
				mockGH.SetCheckRunsForRef(badSHA2, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "failure", OutputSummary: "failing again"},
					{Name: "review-gate", Status: "completed", Conclusion: "success"},
				})
				respStall, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("strike 3 failed: %v", err)
				}
				if respStall.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_STALLED {
					t.Fatalf("expected STALLED after 3rd strike, got %v", respStall.GetStage())
				}
				recStall, _ := mockStore.GetEnrollment(ctx, repo)
				if recStall.GetActiveContainerId() != "" {
					t.Errorf("expected empty active_container_id when STALLED, got %q", recStall.GetActiveContainerId())
				}

				// Human removes speculate-stalled label -> un-stalls, resets attempt_count to 0, resumes ALIGNING with fresh DCM.Up
				if err := mockGH.RemoveIssueLabel(ctx, int(recStall.GetActiveIssueNumber()), ghclient.LabelStalled); err != nil {
					t.Fatalf("remove stalled label: %v", err)
				}

				downCountBefore := len(mockDCM.DownCalls)
				respUnstall, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("unstall reconcile failed: %v", err)
				}
				if respUnstall.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
					t.Fatalf("expected resumed stage ALIGNING after un-stall, got %v", respUnstall.GetStage())
				}
				recResumed, _ := mockStore.GetEnrollment(ctx, repo)
				if recResumed.GetAttemptCount() != 0 {
					t.Errorf("expected attempt_count reset to 0 after un-stall, got %d", recResumed.GetAttemptCount())
				}
				if recResumed.GetLastError() != "" {
					t.Errorf("expected empty last_error after un-stall, got %q", recResumed.GetLastError())
				}
				if recResumed.GetActiveContainerId() == "" {
					t.Errorf("expected fresh DCM container launched on un-stall resume")
				}
				if len(mockDCM.DownCalls) != downCountBefore {
					t.Errorf("expected no premature strike Down on pre-stall SHA during un-stall resume")
				}
			},
		},
		{
			name: "Green compile=success, test=success, review-gate=success -> Down, SquashMerge, DeleteBranch, ALIGNED + chore/readme-badge PR",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, true, false)
				mockGH, mockDCM, mockStore, rec, _ := setupTestHarness(t, repoDir)

				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("initial reconcile failed: %v", err)
				}
				rec1, _ := mockStore.GetEnrollment(ctx, repo)
				branch := rec1.GetActiveBranch()

				approvedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				mockGH.SetBranchSHA(branch, approvedSHA)
				pr, _ := mockGH.CreatePullRequest(ctx, &ghclient.CreatePRRequest{Title: "test: Core-1", Head: branch, Base: "main"})
				mockGH.SetCheckRunsForRef(approvedSHA, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "failure"},
				})
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("red verified reconcile failed: %v", err)
				}
				mockGH.AddPullRequestReview(pr.Number, &ghclient.PullRequestReview{
					ID:       101,
					Author:   "brotherlogic",
					State:    "APPROVED",
					CommitID: approvedSHA,
				})
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("approval reconcile failed: %v", err)
				}
				recAlign, _ := mockStore.GetEnrollment(ctx, repo)
				alignContainerID := recAlign.GetActiveContainerId()
				alignIssueNum := int(recAlign.GetActiveIssueNumber())

				// Green agent implements internal/ and pushes green commit; simulate merged main test once SquashMerge runs
				greenSHA := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
				mockGH.SetBranchSHA(branch, greenSHA)
				mockGH.SetChangedFiles(approvedSHA, greenSHA, []string{"internal/server/server.go"})
				mockGH.SetCheckRunsForRef(greenSHA, []*ghclient.CheckRun{
					{Name: "compile", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "success"},
					{Name: "review-gate", Status: "completed", Conclusion: "success"},
				})
				// Switch cloneFn snapshot when PR is merged on main
				rec.WithCloneFn(func(ctx context.Context, repoURL, targetDir string) error {
					if err := copyTree(repoDir, targetDir); err != nil {
						return err
					}
					gotPR, err := mockGH.GetPullRequest(ctx, pr.Number)
					if err == nil && gotPR.Merged {
						testsDir := filepath.Join(targetDir, "tests")
						if err := os.MkdirAll(testsDir, 0755); err != nil {
							return err
						}
						if err := os.WriteFile(filepath.Join(testsDir, "kv_test.go"), []byte(testGoFile), 0644); err != nil {
							return err
						}
					}
					return nil
				})

				resp, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("green promotion reconcile failed: %v", err)
				}
				if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
					t.Fatalf("expected ALIGNED, got %v", resp.GetStage())
				}
				if resp.GetAlignmentPercentage() != 100 {
					t.Errorf("expected 100%% alignment, got %d%%", resp.GetAlignmentPercentage())
				}

				gotPR, _ := mockGH.GetPullRequest(ctx, pr.Number)
				if !gotPR.Merged {
					t.Errorf("expected PR #%d to be squash-merged", pr.Number)
				}
				alignIssue, _ := mockGH.GetIssue(ctx, alignIssueNum)
				if alignIssue.State != "closed" {
					t.Errorf("expected align issue #%d closed, got %q", alignIssueNum, alignIssue.State)
				}
				if _, err := mockGH.GetBranch(ctx, branch); err == nil {
					t.Errorf("expected branch %q to be deleted after merge", branch)
				}
				foundDown := false
				for _, dc := range mockDCM.DownCalls {
					if dc == alignContainerID {
						foundDown = true
					}
				}
				if !foundDown {
					t.Errorf("expected DCM.Down called for %q, got %v", alignContainerID, mockDCM.DownCalls)
				}

				recFinal, _ := mockStore.GetEnrollment(ctx, repo)
				if recFinal.GetActiveBranch() != "" || recFinal.GetActivePrNumber() != 0 || recFinal.GetActiveContainerId() != "" {
					t.Errorf("expected active scenario fields cleared, got branch=%q pr=%d container=%q",
						recFinal.GetActiveBranch(), recFinal.GetActivePrNumber(), recFinal.GetActiveContainerId())
				}

				// Main README should NOT be directly modified; chore/readme-badge PR should be open instead
				mainReadme, _ := mockGH.GetFileContent(ctx, "README.md", "main")
				if strings.Contains(mainReadme.Content, "100%25") {
					t.Errorf("expected main README not to be directly committed with 100%% badge, got:\n%s", mainReadme.Content)
				}
				badgePR, err := mockGH.FindOpenPullRequestByHead(ctx, "chore/readme-badge")
				if err != nil || badgePR == nil {
					t.Fatalf("expected open chore/readme-badge PR, got %v (err: %v)", badgePR, err)
				}
				badgeReadme, err := mockGH.GetFileContent(ctx, "README.md", "chore/readme-badge")
				if err != nil {
					t.Fatalf("read chore/readme-badge README: %v", err)
				}
				if !strings.Contains(badgeReadme.Content, "Spec%20Alignment-100%25") {
					t.Errorf("expected chore/readme-badge README to contain 100%% badge, got:\n%s", badgeReadme.Content)
				}

				// Calling Reconcile again while ALIGNED must be idempotent (no duplicate PRs)
				if _, err := rec.Reconcile(ctx, repo); err != nil {
					t.Fatalf("idempotent ALIGNED reconcile failed: %v", err)
				}
				badgePR2, err := mockGH.FindOpenPullRequestByHead(ctx, "chore/readme-badge")
				if err != nil || badgePR2 == nil || badgePR2.Number != badgePR.Number {
					t.Errorf("expected chore/readme-badge PR #%d reused idempotently, got %+v", badgePR.Number, badgePR2)
				}
			},
		},
		{
			name: "nil dcmClient falls back gracefully to tracking/card-only evaluation",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, true, false)
				mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
				mockStore := pstore.NewMockStore()
				llm := newDefaultMockLLM()

				rec := NewReconciler(
					"test-token",
					func(token, owner, repo string) ghclient.Client { return mockGH },
					nil,
					evaluator.NewEvaluator(llm),
					synthesizer.NewSynthesizer(llm),
					mockStore,
				).WithCloneFn(func(ctx context.Context, repoURL, targetDir string) error {
					return copyTree(repoDir, targetDir)
				})

				resp, err := rec.Reconcile(ctx, repo)
				if err != nil {
					t.Fatalf("reconcile with nil dcmClient failed: %v", err)
				}
				if resp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
					t.Errorf("expected AWAITING_TEST_REVIEW in card-only fallback, got %v", resp.GetStage())
				}
				if resp.GetIssueUrl() == "" {
					t.Errorf("expected scenario card issue URL in card-only fallback")
				}
			},
		},
		{
			name: "singleflight deduplicates concurrent Reconcile calls per canonical repo",
			run: func(t *testing.T) {
				repoDir := createRepoSnapshotDir(t, true, true, true)
				mockGH, _, _, rec, _ := setupTestHarness(t, repoDir)

				var cloneCount atomic.Int32
				entered := make(chan struct{})
				release := make(chan struct{})
				var once sync.Once

				rec.WithCloneFn(func(ctx context.Context, repoURL, targetDir string) error {
					cloneCount.Add(1)
					once.Do(func() { close(entered) })
					<-release
					return copyTree(repoDir, targetDir)
				})

				const n = 4
				var wg sync.WaitGroup
				errs := make([]error, n)
				resps := make([]*pb.EnrollResponse, n)
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						target := repo
						if idx%2 == 1 {
							target = "https://github.com/" + repo + ".git"
						}
						resps[idx], errs[idx] = rec.Reconcile(ctx, target)
					}(i)
				}

				select {
				case <-entered:
					time.Sleep(25 * time.Millisecond)
					close(release)
				case <-time.After(500 * time.Millisecond):
					close(release)
				}
				wg.Wait()

				for i := 0; i < n; i++ {
					if errs[i] != nil {
						t.Fatalf("goroutine %d failed: %v", i, errs[i])
					}
					if resps[i].GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
						t.Errorf("goroutine %d expected ALIGNED, got %v", i, resps[i].GetStage())
					}
				}
				if got := cloneCount.Load(); got != 1 {
					t.Errorf("expected 1 singleflight clone, got %d", got)
				}
				_ = mockGH
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}

func TestScenarioBranchName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Core-1", "feat/core-1"},
		{"  Stage 2: Batch Put/Get  ", "feat/stage-2-batch-put-get"},
		{"", "feat/scenario"},
		{"---", "feat/scenario"},
	}
	for _, tc := range cases {
		if got := ScenarioBranchName(tc.in); got != tc.want {
			t.Errorf("ScenarioBranchName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
