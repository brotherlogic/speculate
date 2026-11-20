package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	ghwebhookpb "github.com/brotherlogic/ghwebhook/proto/ghwebhook/v1"
	"github.com/brotherlogic/speculate/pkg/dcm"
	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/orchestrator"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const (
	simProtoFile = `syntax = "proto3";
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
	simSpecFile = `# Key-Value Service
## [Stage: Core] Basic Key-Value Storage
- Put: Stores a key and associated value.
`
	simTestFile = `package tests

import "testing"

// Stage: Core
// Scenario: Core-1 Basic Put Key
func TestPut_Basic(t *testing.T) {
	t.Fatal("Put unimplemented")
}
`
)

type simulationHarness struct {
	owner         string
	repo          string
	canonicalRepo string
	mockGH        *ghclient.MockClient
	mockDCM       *dcm.MockClient
	mockStore     *pstore.MockStore
	reconciler    *orchestrator.Reconciler
	specClient    pb.SpeculateServiceClient
	webhookClient ghwebhookpb.WebhookHandlerClient
	conn          *grpc.ClientConn
	grpcServer    *grpc.Server
	lis           *bufconn.Listener
}

func (h *simulationHarness) close() {
	if h == nil {
		return
	}
	if h.conn != nil {
		_ = h.conn.Close()
	}
	if h.grpcServer != nil {
		h.grpcServer.Stop()
	}
	if h.lis != nil {
		_ = h.lis.Close()
	}
}

type simulationHook struct {
	afterRedUp         func(ctx context.Context, h *simulationHarness)
	afterRedPR         func(ctx context.Context, h *simulationHarness)
	afterApproval      func(ctx context.Context, h *simulationHarness)
	afterGreenMerge    func(ctx context.Context, h *simulationHarness)
	afterTamperReset   func(ctx context.Context, h *simulationHarness)
	afterStrike3Stall  func(ctx context.Context, h *simulationHarness)
	afterUnstallResume func(ctx context.Context, h *simulationHarness)
}

type simulationSpeculateServer struct {
	pb.UnimplementedSpeculateServiceServer
	reconciler *orchestrator.Reconciler
}

func (s *simulationSpeculateServer) Enroll(ctx context.Context, req *pb.EnrollRequest) (*pb.EnrollResponse, error) {
	if req == nil || strings.TrimSpace(req.GetRepository()) == "" {
		return nil, status.Error(codes.InvalidArgument, "repository is required")
	}
	return s.reconciler.Reconcile(ctx, strings.TrimSpace(req.GetRepository()))
}

func (s *simulationSpeculateServer) ListEnrollments(ctx context.Context, _ *pb.ListEnrollmentsRequest) (*pb.ListEnrollmentsResponse, error) {
	records, err := s.reconciler.ListEnrollments(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing enrollments: %v", err)
	}
	return &pb.ListEnrollmentsResponse{Records: records}, nil
}

type simulationWebhookServer struct {
	ghwebhookpb.UnimplementedWebhookHandlerServer
	reconciler *orchestrator.Reconciler
}

func (s *simulationWebhookServer) ReceiveWebhook(ctx context.Context, event *ghwebhookpb.WebhookEvent) (*ghwebhookpb.WebhookResponse, error) {
	repo := strings.TrimSpace(extractSimulationWebhookRepo(event))
	if repo == "" {
		return &ghwebhookpb.WebhookResponse{Success: true, Message: "ignored empty repository"}, nil
	}
	resp, err := s.reconciler.Reconcile(ctx, repo)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reconciling %s: %v", repo, err)
	}
	return &ghwebhookpb.WebhookResponse{
		Success: true,
		Message: fmt.Sprintf("reconciled %s (%s)", repo, resp.GetStage()),
	}, nil
}

func extractSimulationWebhookRepo(event *ghwebhookpb.WebhookEvent) string {
	if event == nil {
		return ""
	}
	switch p := event.GetPayload().(type) {
	case *ghwebhookpb.WebhookEvent_Push:
		return p.Push.GetRepository().GetFullName()
	case *ghwebhookpb.WebhookEvent_PullRequest:
		return p.PullRequest.GetRepository().GetFullName()
	case *ghwebhookpb.WebhookEvent_PullRequestReview:
		return p.PullRequestReview.GetRepository().GetFullName()
	case *ghwebhookpb.WebhookEvent_CheckSuite:
		return p.CheckSuite.GetRepository().GetFullName()
	case *ghwebhookpb.WebhookEvent_Issue:
		return p.Issue.GetRepository().GetFullName()
	default:
		return ""
	}
}

type simulationLLMClient struct{}

func (c *simulationLLMClient) Complete(_ context.Context, _, userPrompt string) (string, error) {
	if strings.Contains(userPrompt, "TestPut_Basic") {
		return `{"coverage":[{"id":"Core-1","covered":true,"test":"TestPut_Basic","reasoning":"exercises Put RPC"}]}`, nil
	}
	return `{
		"id": "Core-1",
		"stage": "Core",
		"requirement": "Put: Stores a key and associated value.",
		"target_rpc": "Put",
		"title": "Basic Put Key",
		"given": "An empty KV service",
		"when": "Put is called",
		"then": "Value is stored",
		"test_func_name": "TestPut_Basic"
	}`, nil
}

func newSimulationHarness(owner, repo string) (*simulationHarness, error) {
	canonicalRepo := fmt.Sprintf("%s/%s", owner, repo)
	mockGH := ghclient.NewMockClient(owner, repo)
	mockDCM := dcm.NewMockClient()
	mockStore := pstore.NewMockStore()
	llm := &simulationLLMClient{}

	rec := orchestrator.NewReconciler(
		"simulation-token",
		func(_, _, _ string) ghclient.Client { return mockGH },
		mockDCM,
		evaluator.NewEvaluator(llm),
		synthesizer.NewSynthesizer(llm),
		mockStore,
	).
		WithHarness(dcm.HarnessAntigravity).
		WithPollInterval(5 * time.Millisecond).
		WithCloneFn(func(ctx context.Context, _, targetDir string) error {
			if err := writeSimFile(filepath.Join(targetDir, "proto", "kv.proto"), simProtoFile); err != nil {
				return err
			}
			if err := writeSimFile(filepath.Join(targetDir, "specs", "kv.md"), simSpecFile); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(targetDir, "tests"), 0755); err != nil {
				return err
			}
			if fc, err := mockGH.GetFileContent(ctx, "tests/kv_test.go", "main"); err == nil && fc != nil {
				if err := writeSimFile(filepath.Join(targetDir, "tests", "kv_test.go"), fc.Content); err != nil {
					return err
				}
			}
			return nil
		})

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	pb.RegisterSpeculateServiceServer(grpcServer, &simulationSpeculateServer{reconciler: rec})
	ghwebhookpb.RegisterWebhookHandlerServer(grpcServer, &simulationWebhookServer{reconciler: rec})

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		grpcServer.Stop()
		_ = lis.Close()
		return nil, fmt.Errorf("dialing simulation bufconn server: %w", err)
	}

	return &simulationHarness{
		owner:         owner,
		repo:          repo,
		canonicalRepo: canonicalRepo,
		mockGH:        mockGH,
		mockDCM:       mockDCM,
		mockStore:     mockStore,
		reconciler:    rec,
		specClient:    pb.NewSpeculateServiceClient(conn),
		webhookClient: ghwebhookpb.NewWebhookHandlerClient(conn),
		conn:          conn,
		grpcServer:    grpcServer,
		lis:           lis,
	}, nil
}

func writeSimFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func runSimulationProber(ctx context.Context, targetRepo string) error {
	return runSimulationProberWithHook(ctx, targetRepo, nil)
}

func runSimulationProberWithHook(ctx context.Context, targetRepo string, hook *simulationHook) error {
	owner, repo, err := ghclient.ParseRepo(targetRepo)
	if err != nil {
		return fmt.Errorf("parsing target repo %q: %w", targetRepo, err)
	}
	if hook == nil {
		hook = &simulationHook{}
	}

	fmt.Println("🔍 Scenario 1: Happy-Path Phase 4 Red-to-Green Promotion Cycle...")
	if err := runHappyPathSimulation(ctx, owner, repo, hook); err != nil {
		return fmt.Errorf("scenario 1 (happy path): %w", err)
	}

	fmt.Println("🔍 Scenario 2: Post-Approval Protected-Path Tampering Guard...")
	if err := runTamperGuardSimulation(ctx, owner, repo, hook); err != nil {
		return fmt.Errorf("scenario 2 (tamper guard): %w", err)
	}

	fmt.Println("🔍 Scenario 3: 3-Strike Circuit Breaker & GitHub-Native Un-stall...")
	if err := runCircuitBreakerSimulation(ctx, owner, repo, hook); err != nil {
		return fmt.Errorf("scenario 3 (circuit breaker & un-stall): %w", err)
	}

	return nil
}

func (h *simulationHarness) sendWebhook(ctx context.Context, event *ghwebhookpb.WebhookEvent) error {
	resp, err := h.webhookClient.ReceiveWebhook(ctx, event)
	if err != nil {
		return fmt.Errorf("ReceiveWebhook failed: %w", err)
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("ReceiveWebhook returned Success=false: %s", resp.GetMessage())
	}
	return nil
}

func (h *simulationHarness) driveToAligning(ctx context.Context, hook *simulationHook) (
	branch string,
	prNumber int,
	sApproved string,
	redContainerID string,
	greenContainerID string,
	err error,
) {
	repoObj := &ghwebhookpb.Repository{FullName: h.canonicalRepo}

	// 1. Deliver PushEvent on main (0% aligned) -> Red phase DCM.Up + [Speculate Loop] issue
	if err := h.sendWebhook(ctx, &ghwebhookpb.WebhookEvent{
		Header: &ghwebhookpb.EventHeader{EventType: "push"},
		Payload: &ghwebhookpb.WebhookEvent_Push{
			Push: &ghwebhookpb.PushEvent{
				Ref:        "refs/heads/main",
				After:      "0123456789abcdef0123456789abcdef01234567",
				Repository: repoObj,
			},
		},
	}); err != nil {
		return "", 0, "", "", "", fmt.Errorf("push event on main: %w", err)
	}

	if hook.afterRedUp != nil {
		hook.afterRedUp(ctx, h)
	}

	rec1, err := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
	if err != nil {
		return "", 0, "", "", "", fmt.Errorf("get enrollment after Red Up: %w", err)
	}
	if rec1.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING {
		return "", 0, "", "", "", fmt.Errorf("expected stage EVALUATING after PushEvent, got %v", rec1.GetStage())
	}
	if rec1.GetAlignmentPercentage() != 0 {
		return "", 0, "", "", "", fmt.Errorf("expected 0%% initial alignment, got %d%%", rec1.GetAlignmentPercentage())
	}
	redContainerID = rec1.GetActiveContainerId()
	branch = rec1.GetActiveBranch()
	if redContainerID == "" || branch == "" {
		return "", 0, "", "", "", fmt.Errorf("expected active Red container and branch, got container=%q branch=%q", redContainerID, branch)
	}

	containers, err := h.mockDCM.List(ctx)
	if err != nil || len(containers) != 1 {
		return "", 0, "", "", "", fmt.Errorf("expected 1 active Red DCM container, got %d (err=%v)", len(containers), err)
	}
	if containers[0].GetRequest().GetHarness() != dcm.HarnessAntigravity {
		return "", 0, "", "", "", fmt.Errorf("expected HARNESS_ANTIGRAVITY, got %v", containers[0].GetRequest().GetHarness())
	}
	loopIssue, err := h.mockGH.FindOpenIssueByLabel(ctx, ghclient.LabelAgenticLoop)
	if err != nil || loopIssue == nil || !strings.Contains(loopIssue.Title, "[Speculate Loop]") {
		return "", 0, "", "", "", fmt.Errorf("expected open [Speculate Loop] issue, got %+v (err=%v)", loopIssue, err)
	}

	// 2. Simulate Red PR opened + CheckSuiteEvent (compile=success, test=failure) -> DCM.Down + AWAITING_TEST_REVIEW
	if err := h.mockGH.UpdateFile(ctx, "tests/kv_test.go", simTestFile, "", branch, "test: add failing Core-1 test"); err != nil {
		return "", 0, "", "", "", fmt.Errorf("commit Red test on %s: %w", branch, err)
	}
	pr, err := h.mockGH.CreatePullRequest(ctx, &ghclient.CreatePRRequest{
		Title: "test(kv): Core-1 Basic Put Key",
		Head:  branch,
		Base:  "main",
		Body:  fmt.Sprintf("Closes #%d", loopIssue.Number),
	})
	if err != nil {
		return "", 0, "", "", "", fmt.Errorf("create Red PR: %w", err)
	}
	prNumber = pr.Number
	sApproved = pr.HeadSHA

	h.mockGH.SetCheckRunsForRef(sApproved, []*ghclient.CheckRun{
		{ID: 1, Name: "compile", Status: "completed", Conclusion: "success"},
		{ID: 2, Name: "test", Status: "completed", Conclusion: "failure", OutputSummary: "Put unimplemented"},
	})

	if err := h.sendWebhook(ctx, &ghwebhookpb.WebhookEvent{
		Header: &ghwebhookpb.EventHeader{EventType: "check_suite"},
		Payload: &ghwebhookpb.WebhookEvent_CheckSuite{
			CheckSuite: &ghwebhookpb.CheckSuiteEvent{
				Action:     "completed",
				Status:     "completed",
				Conclusion: "failure",
				HeadBranch: branch,
				HeadSha:    sApproved,
				Repository: repoObj,
			},
		},
	}); err != nil {
		return "", 0, "", "", "", fmt.Errorf("red check_suite webhook: %w", err)
	}

	if hook.afterRedPR != nil {
		hook.afterRedPR(ctx, h)
	}

	rec2, err := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
	if err != nil {
		return "", 0, "", "", "", fmt.Errorf("get enrollment after Red check_suite: %w", err)
	}
	if rec2.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
		return "", 0, "", "", "", fmt.Errorf("expected stage AWAITING_TEST_REVIEW, got %v", rec2.GetStage())
	}
	if rec2.GetActiveContainerId() != "" {
		return "", 0, "", "", "", fmt.Errorf("expected active_container_id cleared after DCM.Down, got %q", rec2.GetActiveContainerId())
	}
	if !containsString(h.mockDCM.DownCalls, redContainerID) {
		return "", 0, "", "", "", fmt.Errorf("expected DCM.Down called for Red container %q, got %v", redContainerID, h.mockDCM.DownCalls)
	}

	// 3. Deliver PullRequestReviewEvent (@owner APPROVED at S_approved) -> closes Red issue, opens Align issue, fresh DCM.Up
	h.mockGH.AddPullRequestReview(prNumber, &ghclient.PullRequestReview{
		ID:          101,
		Author:      h.owner,
		State:       "APPROVED",
		CommitID:    sApproved,
		SubmittedAt: time.Now().UTC(),
	})

	if err := h.sendWebhook(ctx, &ghwebhookpb.WebhookEvent{
		Header: &ghwebhookpb.EventHeader{EventType: "pull_request_review"},
		Payload: &ghwebhookpb.WebhookEvent_PullRequestReview{
			PullRequestReview: &ghwebhookpb.PullRequestReviewEvent{
				Action:            "submitted",
				State:             "approved",
				CommitId:          sApproved,
				PullRequestNumber: int32(prNumber),
				Reviewer:          &ghwebhookpb.User{Login: h.owner},
				Repository:        repoObj,
			},
		},
	}); err != nil {
		return "", 0, "", "", "", fmt.Errorf("pull_request_review webhook: %w", err)
	}

	if hook.afterApproval != nil {
		hook.afterApproval(ctx, h)
	}

	rec3, err := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
	if err != nil {
		return "", 0, "", "", "", fmt.Errorf("get enrollment after approval: %w", err)
	}
	if rec3.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
		return "", 0, "", "", "", fmt.Errorf("expected stage ALIGNING after approval, got %v", rec3.GetStage())
	}
	if rec3.GetApprovedTestSha() != sApproved {
		return "", 0, "", "", "", fmt.Errorf("expected approved_test_sha %q, got %q", sApproved, rec3.GetApprovedTestSha())
	}
	greenContainerID = rec3.GetActiveContainerId()
	if greenContainerID == "" || greenContainerID == redContainerID {
		return "", 0, "", "", "", fmt.Errorf("expected fresh Green container ID distinct from Red %q, got %q", redContainerID, greenContainerID)
	}

	closedLoopIssue, err := h.mockGH.GetIssue(ctx, loopIssue.Number)
	if err != nil || closedLoopIssue.State != "closed" {
		return "", 0, "", "", "", fmt.Errorf("expected Red issue #%d closed, got %+v (err=%v)", loopIssue.Number, closedLoopIssue, err)
	}
	alignIssue, err := h.mockGH.FindOpenIssueByLabel(ctx, ghclient.LabelAlign)
	if err != nil || alignIssue == nil || !strings.Contains(alignIssue.Title, "[Speculate Align]") {
		return "", 0, "", "", "", fmt.Errorf("expected open [Speculate Align] issue, got %+v (err=%v)", alignIssue, err)
	}

	return branch, prNumber, sApproved, redContainerID, greenContainerID, nil
}

func runHappyPathSimulation(ctx context.Context, owner, repo string, hook *simulationHook) error {
	h, err := newSimulationHarness(owner, repo)
	if err != nil {
		return err
	}
	defer h.close()

	branch, prNumber, _, redContainerID, greenContainerID, err := h.driveToAligning(ctx, hook)
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ Red phase (%s) -> Review -> Green session (%s) on %s PR #%d\n", redContainerID, greenContainerID, branch, prNumber)

	// 4. Simulate Green commit + CheckSuiteEvent (compile, test, review-gate = success)
	if err := h.mockGH.UpdateFile(ctx, "internal/server/server.go", "package server\n", "", branch, "feat: implement Put"); err != nil {
		return fmt.Errorf("commit Green implementation: %w", err)
	}
	b, err := h.mockGH.GetBranch(ctx, branch)
	if err != nil {
		return fmt.Errorf("get branch after Green commit: %w", err)
	}
	greenSHA := b.SHA

	h.mockGH.SetCheckRunsForRef(greenSHA, []*ghclient.CheckRun{
		{ID: 10, Name: "compile", Status: "completed", Conclusion: "success"},
		{ID: 11, Name: "test", Status: "completed", Conclusion: "success"},
		{ID: 12, Name: "review-gate", Status: "completed", Conclusion: "success"},
	})

	if err := h.sendWebhook(ctx, &ghwebhookpb.WebhookEvent{
		Header: &ghwebhookpb.EventHeader{EventType: "check_suite"},
		Payload: &ghwebhookpb.WebhookEvent_CheckSuite{
			CheckSuite: &ghwebhookpb.CheckSuiteEvent{
				Action:     "completed",
				Status:     "completed",
				Conclusion: "success",
				HeadBranch: branch,
				HeadSha:    greenSHA,
				Repository: &ghwebhookpb.Repository{FullName: h.canonicalRepo},
			},
		},
	}); err != nil {
		return fmt.Errorf("green check_suite webhook: %w", err)
	}

	if hook.afterGreenMerge != nil {
		hook.afterGreenMerge(ctx, h)
	}

	recFinal, err := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
	if err != nil {
		return fmt.Errorf("get enrollment after Green merge: %w", err)
	}
	if recFinal.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
		return fmt.Errorf("expected stage ALIGNED, got %v", recFinal.GetStage())
	}
	if recFinal.GetAlignmentPercentage() != 100 {
		return fmt.Errorf("expected 100%% alignment after merge, got %d%%", recFinal.GetAlignmentPercentage())
	}
	if !containsString(h.mockDCM.DownCalls, greenContainerID) {
		return fmt.Errorf("expected DCM.Down called for Green container %q, got %v", greenContainerID, h.mockDCM.DownCalls)
	}

	mergedPR, err := h.mockGH.GetPullRequest(ctx, prNumber)
	if err != nil || !mergedPR.Merged {
		return fmt.Errorf("expected PR #%d squash-merged, got %+v (err=%v)", prNumber, mergedPR, err)
	}
	if _, err := h.mockGH.GetBranch(ctx, branch); err == nil {
		return fmt.Errorf("expected branch %q deleted after merge", branch)
	}

	// Verify SpeculateService.ListEnrollments over bufconn reports the same 100% ALIGNED record.
	listResp, err := h.specClient.ListEnrollments(ctx, &pb.ListEnrollmentsRequest{})
	if err != nil || len(listResp.GetRecords()) != 1 || listResp.GetRecords()[0].GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
		return fmt.Errorf("ListEnrollments mismatch after Green merge: resp=%+v err=%v", listResp, err)
	}

	recordAlignmentScore(h.canonicalRepo, int(recFinal.GetAlignmentPercentage()))
	fmt.Println("  ✓ Green checks passed -> DCM.Down, SquashMerge, branch deleted, 100% ALIGNED")
	return nil
}

func runTamperGuardSimulation(ctx context.Context, owner, repo string, hook *simulationHook) error {
	h, err := newSimulationHarness(owner, repo)
	if err != nil {
		return err
	}
	defer h.close()

	branch, _, sApproved, _, c1, err := h.driveToAligning(ctx, &simulationHook{})
	if err != nil {
		return err
	}

	// Align agent modifies protected tests/kv_test.go after S_approved
	if err := h.mockGH.UpdateFile(ctx, "tests/kv_test.go", "package tests // tampered\n", "", branch, "chore: alter test"); err != nil {
		return fmt.Errorf("simulate protected-path tamper commit: %w", err)
	}
	tamperBranch, err := h.mockGH.GetBranch(ctx, branch)
	if err != nil {
		return fmt.Errorf("get branch after tamper commit: %w", err)
	}

	if err := h.sendWebhook(ctx, &ghwebhookpb.WebhookEvent{
		Header: &ghwebhookpb.EventHeader{EventType: "push"},
		Payload: &ghwebhookpb.WebhookEvent_Push{
			Push: &ghwebhookpb.PushEvent{
				Ref:        "refs/heads/" + branch,
				Before:     sApproved,
				After:      tamperBranch.SHA,
				Repository: &ghwebhookpb.Repository{FullName: h.canonicalRepo},
			},
		},
	}); err != nil {
		return fmt.Errorf("tamper push webhook: %w", err)
	}

	if hook.afterTamperReset != nil {
		hook.afterTamperReset(ctx, h)
	}

	resetBranch, err := h.mockGH.GetBranch(ctx, branch)
	if err != nil {
		return fmt.Errorf("get branch after tamper reset: %w", err)
	}
	if resetBranch.SHA != sApproved {
		return fmt.Errorf("expected branch %q reset to %s, got %s", branch, sApproved, resetBranch.SHA)
	}

	rec, err := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
	if err != nil {
		return fmt.Errorf("get enrollment after tamper reset: %w", err)
	}
	if rec.GetAttemptCount() != 2 {
		return fmt.Errorf("expected attempt_count=2 after tamper strike, got %d", rec.GetAttemptCount())
	}
	if rec.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
		return fmt.Errorf("expected stage ALIGNING after tamper reset, got %v", rec.GetStage())
	}
	if !containsString(h.mockDCM.DownCalls, c1) || rec.GetActiveContainerId() == "" || rec.GetActiveContainerId() == c1 {
		return fmt.Errorf("expected container %q torn down and fresh container launched, got active=%q down=%v", c1, rec.GetActiveContainerId(), h.mockDCM.DownCalls)
	}

	fmt.Printf("  ✓ Tampered branch %s force-reset to %s; attempt_count incremented to %d\n", branch, sApproved[:8], rec.GetAttemptCount())
	return nil
}

func runCircuitBreakerSimulation(ctx context.Context, owner, repo string, hook *simulationHook) error {
	h, err := newSimulationHarness(owner, repo)
	if err != nil {
		return err
	}
	defer h.close()

	branch, _, _, _, c1, err := h.driveToAligning(ctx, &simulationHook{})
	if err != nil {
		return err
	}
	repoObj := &ghwebhookpb.Repository{FullName: h.canonicalRepo}

	prevContainer := c1
	for strike := 1; strike <= 3; strike++ {
		content := fmt.Sprintf("package server // strike %d\n", strike)
		if err := h.mockGH.UpdateFile(ctx, "internal/server/server.go", content, "", branch, fmt.Sprintf("wip: strike %d", strike)); err != nil {
			return fmt.Errorf("commit for strike %d: %w", strike, err)
		}
		b, err := h.mockGH.GetBranch(ctx, branch)
		if err != nil {
			return fmt.Errorf("get branch for strike %d: %w", strike, err)
		}

		h.mockGH.SetCheckRunsForRef(b.SHA, []*ghclient.CheckRun{
			{ID: int64(strike*10 + 1), Name: "compile", Status: "completed", Conclusion: "success"},
			{ID: int64(strike*10 + 2), Name: "test", Status: "completed", Conclusion: "failure", OutputSummary: fmt.Sprintf("strike %d test failed", strike)},
			{ID: int64(strike*10 + 3), Name: "review-gate", Status: "completed", Conclusion: "success"},
		})

		if err := h.sendWebhook(ctx, &ghwebhookpb.WebhookEvent{
			Header: &ghwebhookpb.EventHeader{EventType: "check_suite"},
			Payload: &ghwebhookpb.WebhookEvent_CheckSuite{
				CheckSuite: &ghwebhookpb.CheckSuiteEvent{
					Action:     "completed",
					Status:     "completed",
					Conclusion: "failure",
					HeadBranch: branch,
					HeadSha:    b.SHA,
					Repository: repoObj,
				},
			},
		}); err != nil {
			return fmt.Errorf("strike %d check_suite webhook: %w", strike, err)
		}

		rec, err := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
		if err != nil {
			return fmt.Errorf("get enrollment after strike %d: %w", strike, err)
		}
		if !containsString(h.mockDCM.DownCalls, prevContainer) {
			return fmt.Errorf("strike %d expected DCM.Down called on %q, got %v", strike, prevContainer, h.mockDCM.DownCalls)
		}

		if strike < 3 {
			if rec.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
				return fmt.Errorf("strike %d expected stage ALIGNING, got %v", strike, rec.GetStage())
			}
			expectedAttempt := int32(strike + 1)
			if rec.GetAttemptCount() != expectedAttempt {
				return fmt.Errorf("strike %d expected attempt_count=%d, got %d", strike, expectedAttempt, rec.GetAttemptCount())
			}
			nextContainer := rec.GetActiveContainerId()
			if nextContainer == "" || nextContainer == prevContainer {
				return fmt.Errorf("strike %d expected fresh container ID distinct from %q, got %q", strike, prevContainer, nextContainer)
			}
			prevContainer = nextContainer
		}
	}

	if hook.afterStrike3Stall != nil {
		hook.afterStrike3Stall(ctx, h)
	}

	recStall, err := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
	if err != nil {
		return fmt.Errorf("get enrollment after strike 3: %w", err)
	}
	if recStall.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_STALLED {
		return fmt.Errorf("expected stage STALLED after strike 3, got %v", recStall.GetStage())
	}
	if recStall.GetActiveContainerId() != "" {
		return fmt.Errorf("expected empty active_container_id when STALLED, got %q", recStall.GetActiveContainerId())
	}

	stalledIssue, err := h.mockGH.GetIssue(ctx, int(recStall.GetActiveIssueNumber()))
	if err != nil {
		return fmt.Errorf("get stalled issue #%d: %w", recStall.GetActiveIssueNumber(), err)
	}
	if !containsString(stalledIssue.Labels, ghclient.LabelStalled) {
		return fmt.Errorf("expected label %q on issue #%d, got %v", ghclient.LabelStalled, stalledIssue.Number, stalledIssue.Labels)
	}

	// Remove speculate-stalled label and deliver IssueEvent (unlabeled)
	if err := h.mockGH.RemoveIssueLabel(ctx, stalledIssue.Number, ghclient.LabelStalled); err != nil {
		return fmt.Errorf("remove stalled label: %w", err)
	}

	if err := h.sendWebhook(ctx, &ghwebhookpb.WebhookEvent{
		Header: &ghwebhookpb.EventHeader{EventType: "issues"},
		Payload: &ghwebhookpb.WebhookEvent_Issue{
			Issue: &ghwebhookpb.IssueEvent{
				Action:     "unlabeled",
				Number:     int32(stalledIssue.Number),
				Labels:     []string{ghclient.LabelAlign},
				Repository: repoObj,
			},
		},
	}); err != nil {
		return fmt.Errorf("issue unlabeled webhook: %w", err)
	}

	if hook.afterUnstallResume != nil {
		hook.afterUnstallResume(ctx, h)
	}

	recResumed, err := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
	if err != nil {
		return fmt.Errorf("get enrollment after un-stall: %w", err)
	}
	if recResumed.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING {
		return fmt.Errorf("expected stage ALIGNING after un-stall, got %v", recResumed.GetStage())
	}
	if recResumed.GetAttemptCount() != 0 {
		return fmt.Errorf("expected attempt_count reset to 0 after un-stall, got %d", recResumed.GetAttemptCount())
	}
	if recResumed.GetActiveContainerId() == "" {
		return fmt.Errorf("expected fresh DCM container launched on un-stall resume")
	}

	fmt.Printf("  ✓ 3 strikes stalled (#%d labeled %s) -> unlabeled IssueEvent reset attempt_count=0 and resumed ALIGNING\n", stalledIssue.Number, ghclient.LabelStalled)
	return nil
}

func containsString(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}
