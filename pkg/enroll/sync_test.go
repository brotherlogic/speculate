package enroll

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
)

type mockEnrollmentRunner struct {
	mu     sync.Mutex
	calls  []string
	store  pstore.Store
	fn     func(ctx context.Context, repo string) (*pb.EnrollResponse, error)
}

func (m *mockEnrollmentRunner) Enroll(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, repo)
	fn := m.fn
	store := m.store
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, repo)
	}

	if store != nil {
		_ = store.SaveEnrollment(ctx, &pb.EnrollmentRecord{
			Repository:          repo,
			AlignmentPercentage: 100,
			EnrolledAtUnix:      1700000000,
			LastEvaluatedAtUnix: 1700000100,
			StatusMessage:       "fully aligned (100%)",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
		})
	}

	return &pb.EnrollResponse{
		Repository:          repo,
		AlignmentPercentage: 100,
		StatusMessage:       "repository enrolled: fully aligned (100%)",
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
	}, nil
}

func (m *mockEnrollmentRunner) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]string, len(m.calls))
	copy(copied, m.calls)
	return copied
}

func TestSyncOnce_NilAccountClient(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()
	_ = store.SaveEnrollment(ctx, &pb.EnrollmentRecord{
		Repository: "brotherlogic/existing",
		Stage:      pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
	})
	runner := &mockEnrollmentRunner{store: store}

	syncer := NewSyncer(nil, store, runner)
	if err := syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("expected nil error when accountClient is nil, got: %v", err)
	}

	if len(runner.Calls()) != 0 {
		t.Errorf("expected 0 Enroll calls when accountClient is nil, got: %v", runner.Calls())
	}

	// Existing record must remain untouched.
	if _, err := store.GetEnrollment(ctx, "brotherlogic/existing"); err != nil {
		t.Errorf("expected existing record to remain in store, got error: %v", err)
	}
}

func TestSyncOnce_AcceptsInvitationsAndEvaluatesActiveRepos(t *testing.T) {
	ctx := context.Background()
	accountClient := ghclient.NewMockAccountClient()
	accountClient.AddCollaboratorRepositories("brotherlogic/repo-a")
	accountClient.AddPendingInvitations("brotherlogic/repo-b", "brotherlogic/repo-c")

	store := pstore.NewMockStore()
	runner := &mockEnrollmentRunner{store: store}

	syncer := NewSyncer(accountClient, store, runner)
	if err := syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce returned unexpected error: %v", err)
	}

	if pending := accountClient.PendingInvitations(); len(pending) != 0 {
		t.Errorf("expected all pending invitations to be accepted, remaining: %v", pending)
	}

	wantCalls := []string{"brotherlogic/repo-a", "brotherlogic/repo-b", "brotherlogic/repo-c"}
	if got := runner.Calls(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("Enroll calls = %v, want %v", got, wantCalls)
	}

	records, err := store.ListEnrollments(ctx)
	if err != nil {
		t.Fatalf("ListEnrollments failed: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("expected 3 persisted enrollment records, got %d", len(records))
	}
}

func TestSyncOnce_PartialInvitationErrorContinuesDiscovery(t *testing.T) {
	ctx := context.Background()
	accountClient := ghclient.NewMockAccountClient()
	accountClient.AddCollaboratorRepositories("brotherlogic/existing-repo")
	accountClient.AddPendingInvitations("brotherlogic/bad-invite", "brotherlogic/good-invite")
	accountClient.SetAcceptInvitationError("brotherlogic/bad-invite", errors.New("403 invitation expired"))

	store := pstore.NewMockStore()
	runner := &mockEnrollmentRunner{store: store}

	syncer := NewSyncer(accountClient, store, runner)
	if err := syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("expected SyncOnce to continue past partial invitation error, got: %v", err)
	}

	wantCalls := []string{"brotherlogic/existing-repo", "brotherlogic/good-invite"}
	if got := runner.Calls(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("Enroll calls = %v, want %v", got, wantCalls)
	}
}

func TestSyncOnce_PrunesRemovedCollaboratorRepos(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	// Seed previously tracked repositories, including one with mixed casing.
	for _, repo := range []string{
		"brotherlogic/active-repo",
		"brotherlogic/removed-repo",
		"BrotherLogic/Case-Match",
	} {
		if err := store.SaveEnrollment(ctx, &pb.EnrollmentRecord{
			Repository: repo,
			Stage:      pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
		}); err != nil {
			t.Fatalf("SaveEnrollment(%s) failed: %v", repo, err)
		}
	}

	accountClient := ghclient.NewMockAccountClient()
	accountClient.SetCollaboratorRepositories([]string{
		"brotherlogic/active-repo",
		"brotherlogic/case-match",
	})

	runner := &mockEnrollmentRunner{}
	syncer := NewSyncer(accountClient, store, runner)

	if err := syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce returned unexpected error: %v", err)
	}

	// Removed collaborator repo must be pruned from pstore.
	if _, err := store.GetEnrollment(ctx, "brotherlogic/removed-repo"); !errors.Is(err, pstore.ErrNotFound) {
		t.Errorf("expected removed-repo to be pruned (ErrNotFound), got err: %v", err)
	}

	// Active repositories (including normalized case match) must NOT be pruned.
	if _, err := store.GetEnrollment(ctx, "brotherlogic/active-repo"); err != nil {
		t.Errorf("expected active-repo to remain in store, got: %v", err)
	}
	if _, err := store.GetEnrollment(ctx, "BrotherLogic/Case-Match"); err != nil {
		t.Errorf("expected Case-Match to remain in store via normalized comparison, got: %v", err)
	}
}

func TestSyncOnce_OutageSafetyPreservesStoredRecords(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	for _, repo := range []string{"brotherlogic/tracked-a", "brotherlogic/tracked-b"} {
		if err := store.SaveEnrollment(ctx, &pb.EnrollmentRecord{
			Repository: repo,
			Stage:      pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
		}); err != nil {
			t.Fatalf("SaveEnrollment(%s) failed: %v", repo, err)
		}
	}

	accountClient := ghclient.NewMockAccountClient()
	outageErr := errors.New("github API 502 bad gateway")
	accountClient.SetListCollaboratorRepositoriesError(outageErr)

	runner := &mockEnrollmentRunner{store: store}
	syncer := NewSyncer(accountClient, store, runner)

	err := syncer.SyncOnce(ctx)
	if !errors.Is(err, outageErr) {
		t.Fatalf("expected SyncOnce to return %v, got: %v", outageErr, err)
	}

	// Verify zero deletions occurred during the outage.
	records, listErr := store.ListEnrollments(ctx)
	if listErr != nil {
		t.Fatalf("ListEnrollments failed: %v", listErr)
	}
	if len(records) != 2 {
		t.Fatalf("expected all 2 stored records to be preserved on outage, got %d", len(records))
	}

	if got := runner.Calls(); len(got) != 0 {
		t.Errorf("expected 0 Enroll calls when listing collaborator repos fails, got: %v", got)
	}
}

func TestSyncOnce_PerRepoFailureIsolation(t *testing.T) {
	ctx := context.Background()
	accountClient := ghclient.NewMockAccountClient()
	accountClient.SetCollaboratorRepositories([]string{
		"brotherlogic/repo-a",
		"brotherlogic/repo-b",
		"brotherlogic/repo-c",
	})

	store := pstore.NewMockStore()
	runner := &mockEnrollmentRunner{
		fn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			if repo == "brotherlogic/repo-a" {
				return nil, errors.New("clone timeout for repo-a")
			}
			_ = store.SaveEnrollment(ctx, &pb.EnrollmentRecord{
				Repository: repo,
				Stage:      pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
			})
			return &pb.EnrollResponse{
				Repository: repo,
				Stage:      pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
			}, nil
		},
	}

	syncer := NewSyncer(accountClient, store, runner)
	if err := syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("expected SyncOnce to isolate per-repo failure and return nil, got: %v", err)
	}

	wantCalls := []string{"brotherlogic/repo-a", "brotherlogic/repo-b", "brotherlogic/repo-c"}
	if got := runner.Calls(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("Enroll calls = %v, want %v", got, wantCalls)
	}

	if _, err := store.GetEnrollment(ctx, "brotherlogic/repo-b"); err != nil {
		t.Errorf("expected repo-b to be evaluated and saved, got: %v", err)
	}
	if _, err := store.GetEnrollment(ctx, "brotherlogic/repo-c"); err != nil {
		t.Errorf("expected repo-c to be evaluated and saved, got: %v", err)
	}
}

func TestSyncOnce_ContextCancellationStopsEvaluation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accountClient := ghclient.NewMockAccountClient()
	accountClient.SetCollaboratorRepositories([]string{
		"brotherlogic/repo-a",
		"brotherlogic/repo-b",
		"brotherlogic/repo-c",
	})

	runner := &mockEnrollmentRunner{
		fn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			cancel()
			return &pb.EnrollResponse{Repository: repo}, nil
		},
	}

	syncer := NewSyncer(accountClient, pstore.NewMockStore(), runner)
	err := syncer.SyncOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	wantCalls := []string{"brotherlogic/repo-a"}
	if got := runner.Calls(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("expected iteration to stop after repo-a cancellation; got calls: %v", got)
	}
}

func TestSyncerRun_StartupSyncPeriodicTicksAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accountClient := ghclient.NewMockAccountClient()
	accountClient.SetCollaboratorRepositories([]string{"brotherlogic/repo-a"})

	var evalCount atomic.Int32
	threeEvaluationsDone := make(chan struct{})
	var closeOnce sync.Once

	runner := &mockEnrollmentRunner{
		fn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			if evalCount.Add(1) >= 3 {
				closeOnce.Do(func() {
					close(threeEvaluationsDone)
				})
			}
			return &pb.EnrollResponse{
				Repository: repo,
				Stage:      pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
			}, nil
		},
	}

	syncer := NewSyncer(accountClient, pstore.NewMockStore(), runner)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		syncer.Run(ctx, 15*time.Millisecond)
	}()

	select {
	case <-threeEvaluationsDone:
		// Startup sync + at least 2 periodic ticks succeeded.
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for startup sync + periodic ticks; count=%d", evalCount.Load())
	}

	cancel()

	select {
	case <-runDone:
		// Exited cleanly on context cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Syncer.Run to exit after context cancellation")
	}
}

func TestSyncerRun_ZeroIntervalDefaultsSafely(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	accountClient := ghclient.NewMockAccountClient()
	accountClient.SetCollaboratorRepositories([]string{"brotherlogic/repo-a"})

	startupCalled := make(chan struct{})
	runner := &mockEnrollmentRunner{
		fn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			close(startupCalled)
			return &pb.EnrollResponse{Repository: repo}, nil
		},
	}

	syncer := NewSyncer(accountClient, pstore.NewMockStore(), runner)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		// interval <= 0 must default to 5m rather than panicking in time.NewTicker.
		syncer.Run(ctx, 0)
	}()

	select {
	case <-startupCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for immediate startup SyncOnce")
	}

	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Syncer.Run with default interval to stop")
	}
}

func TestSyncOnce_ConcurrentWithPipelineEnrollSingleflightDeduplication(t *testing.T) {
	ctx := context.Background()
	repoDir := createTestRepoFS(t, sampleSpec, sampleTest)

	var cloneCalls atomic.Int32
	cloneEntered := make(chan struct{})
	releaseClone := make(chan struct{})
	var enterOnce sync.Once

	llm := &funcLLMClient{
		fn: func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
			return `{"coverage":[{"id":"Core-1","covered":true,"test":"TestPut","reasoning":"covers Put"}]}`, nil
		},
	}

	mockGH := ghclient.NewMockClient("brotherlogic", "speculate-kv")
	mockStore := pstore.NewMockStore()

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
		cloneCalls.Add(1)
		enterOnce.Do(func() {
			close(cloneEntered)
		})
		<-releaseClone
		return copyDir(repoDir, targetDir)
	}

	accountClient := ghclient.NewMockAccountClient()
	accountClient.SetCollaboratorRepositories([]string{"brotherlogic/speculate-kv"})

	syncer := NewSyncer(accountClient, mockStore, pipeline)

	var wg sync.WaitGroup
	var syncErr error
	var enrollResp *pb.EnrollResponse
	var enrollErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		syncErr = syncer.SyncOnce(ctx)
	}()

	// Wait until SyncOnce has entered Pipeline.Enroll's clone step.
	<-cloneEntered

	wg.Add(1)
	go func() {
		defer wg.Done()
		enrollResp, enrollErr = pipeline.Enroll(ctx, "brotherlogic/speculate-kv")
	}()

	// Allow the concurrent on-demand Enroll call to join Pipeline's singleflight group.
	time.Sleep(30 * time.Millisecond)
	close(releaseClone)
	wg.Wait()

	if syncErr != nil {
		t.Fatalf("SyncOnce returned unexpected error: %v", syncErr)
	}
	if enrollErr != nil {
		t.Fatalf("Pipeline.Enroll returned unexpected error: %v", enrollErr)
	}
	if enrollResp.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
		t.Errorf("expected stage ALIGNED, got %v", enrollResp.GetStage())
	}
	if got := cloneCalls.Load(); got != 1 {
		t.Errorf("expected concurrent SyncOnce + Pipeline.Enroll to deduplicate to 1 clone, got %d", got)
	}
}
