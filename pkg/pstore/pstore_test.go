package pstore_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	pstorepb "github.com/brotherlogic/pstore/proto"
	"github.com/brotherlogic/speculate/pkg/pstore"
	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Ensure interface compliance at compile time.
var (
	_ pstore.Store = (*pstore.MockStore)(nil)
	_ pstore.Store = (*pstore.Client)(nil)
	_ pstore.Store = (*pstore.FallbackStore)(nil)
)

// fakePStoreServer implements pstorepb.PStoreServiceServer in-memory for testing Client.
type fakePStoreServer struct {
	pstorepb.UnimplementedPStoreServiceServer
	mu             sync.RWMutex
	data           map[string]*anypb.Any
	lastReq        *pstorepb.WriteRequest
	readKeys       []string
	deletedKeys    []string
	extraGhostKeys []string
	rpcErr         error
}

func newFakePStoreServer() *fakePStoreServer {
	return &fakePStoreServer{
		data: make(map[string]*anypb.Any),
	}
}

func (s *fakePStoreServer) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rpcErr = err
}

func (s *fakePStoreServer) Read(ctx context.Context, req *pstorepb.ReadRequest) (*pstorepb.ReadResponse, error) {
	s.mu.Lock()
	if s.rpcErr != nil {
		err := s.rpcErr
		s.mu.Unlock()
		return nil, err
	}
	s.readKeys = append(s.readKeys, req.GetKey())
	val, ok := s.data[req.GetKey()]
	s.mu.Unlock()

	if !ok {
		return nil, status.Errorf(codes.NotFound, "key %s not found", req.GetKey())
	}
	return &pstorepb.ReadResponse{Value: val}, nil
}

func (s *fakePStoreServer) Write(ctx context.Context, req *pstorepb.WriteRequest) (*pstorepb.WriteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rpcErr != nil {
		return nil, s.rpcErr
	}
	s.lastReq = req
	s.data[req.GetKey()] = req.GetValue()
	return &pstorepb.WriteResponse{Timestamp: time.Now().Unix()}, nil
}

func (s *fakePStoreServer) GetKeys(ctx context.Context, req *pstorepb.GetKeysRequest) (*pstorepb.GetKeysResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.rpcErr != nil {
		return nil, s.rpcErr
	}

	var keys []string
	for k := range s.data {
		if strings.HasPrefix(k, req.GetPrefix()) {
			keys = append(keys, k)
		}
	}
	for _, gk := range s.extraGhostKeys {
		if strings.HasPrefix(gk, req.GetPrefix()) {
			keys = append(keys, gk)
		}
	}
	return &pstorepb.GetKeysResponse{Keys: keys}, nil
}

func (s *fakePStoreServer) Delete(ctx context.Context, req *pstorepb.DeleteRequest) (*pstorepb.DeleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rpcErr != nil {
		return nil, s.rpcErr
	}
	s.deletedKeys = append(s.deletedKeys, req.GetKey())
	if _, ok := s.data[req.GetKey()]; !ok {
		return nil, status.Errorf(codes.NotFound, "key %s not found", req.GetKey())
	}
	delete(s.data, req.GetKey())
	return &pstorepb.DeleteResponse{}, nil
}

func setupTestGRPCServer(t *testing.T, srv pstorepb.PStoreServiceServer) (*grpc.ClientConn, func()) {
	t.Helper()
	buffer := 1024 * 1024
	lis := bufconn.Listen(buffer)

	baseServer := grpc.NewServer()
	pstorepb.RegisterPStoreServiceServer(baseServer, srv)

	go func() {
		if err := baseServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Logf("Server exited with error: %v", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("Failed to dial bufnet: %v", err)
	}

	cleanup := func() {
		conn.Close()
		baseServer.GracefulStop()
		lis.Close()
	}

	return conn, cleanup
}

func TestMockStore_SaveAndGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	record := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/speculate",
		AlignmentPercentage: 85,
		IssueUrl:            "https://github.com/brotherlogic/speculate/issues/100",
		EnrolledAtUnix:      1700000000,
		LastEvaluatedAtUnix: 1700001000,
		StatusMessage:       "Enrolled successfully",
	}

	if err := store.SaveEnrollment(ctx, record); err != nil {
		t.Fatalf("SaveEnrollment failed: %v", err)
	}

	got, err := store.GetEnrollment(ctx, "brotherlogic/speculate")
	if err != nil {
		t.Fatalf("GetEnrollment failed: %v", err)
	}

	if !proto.Equal(got, record) {
		t.Errorf("GetEnrollment returned %+v, want %+v", got, record)
	}
}

func TestMockStore_Isolation(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	record := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/speculate",
		AlignmentPercentage: 50,
	}

	if err := store.SaveEnrollment(ctx, record); err != nil {
		t.Fatalf("SaveEnrollment failed: %v", err)
	}

	// Mutate original record after saving
	record.AlignmentPercentage = 99

	got, err := store.GetEnrollment(ctx, "brotherlogic/speculate")
	if err != nil {
		t.Fatalf("GetEnrollment failed: %v", err)
	}
	if got.GetAlignmentPercentage() != 50 {
		t.Errorf("store was not isolated from caller mutation: got %d, want 50", got.GetAlignmentPercentage())
	}

	// Mutate returned record
	got.AlignmentPercentage = 10

	got2, err := store.GetEnrollment(ctx, "brotherlogic/speculate")
	if err != nil {
		t.Fatalf("GetEnrollment 2 failed: %v", err)
	}
	if got2.GetAlignmentPercentage() != 50 {
		t.Errorf("store was not isolated from return value mutation: got %d, want 50", got2.GetAlignmentPercentage())
	}
}

func TestMockStore_Normalization(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	record := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/speculate",
		AlignmentPercentage: 77,
	}

	if err := store.SaveEnrollment(ctx, record); err != nil {
		t.Fatalf("SaveEnrollment failed: %v", err)
	}

	// Lookup using key with prefix
	got, err := store.GetEnrollment(ctx, "speculate/enrollment/brotherlogic/speculate")
	if err != nil {
		t.Fatalf("GetEnrollment with prefixed key failed: %v", err)
	}
	if got.GetAlignmentPercentage() != 77 {
		t.Errorf("got %d, want 77", got.GetAlignmentPercentage())
	}

	// Lookup using key with surrounding slashes
	got2, err := store.GetEnrollment(ctx, "/brotherlogic/speculate/")
	if err != nil {
		t.Fatalf("GetEnrollment with slashes failed: %v", err)
	}
	if got2.GetAlignmentPercentage() != 77 {
		t.Errorf("got %d, want 77", got2.GetAlignmentPercentage())
	}
}

func TestMockStore_Validation(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	if err := store.SaveEnrollment(ctx, nil); err == nil {
		t.Error("expected error saving nil record")
	}

	if err := store.SaveEnrollment(ctx, &pb.EnrollmentRecord{}); err == nil {
		t.Error("expected error saving record with empty repo")
	}

	if _, err := store.GetEnrollment(ctx, ""); err == nil {
		t.Error("expected error getting empty repo")
	}
}

func TestMockStore_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			repo := fmt.Sprintf("owner/repo-%d", id)
			rec := &pb.EnrollmentRecord{
				Repository:          repo,
				AlignmentPercentage: int32(id),
			}
			if err := store.SaveEnrollment(ctx, rec); err != nil {
				t.Errorf("concurrent save failed: %v", err)
			}
			got, err := store.GetEnrollment(ctx, repo)
			if err != nil || got.GetAlignmentPercentage() != int32(id) {
				t.Errorf("concurrent get mismatch for %s: got %v, err %v", repo, got, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestMockStore_NotFound(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	_, err := store.GetEnrollment(ctx, "nonexistent/repo")
	if err == nil {
		t.Fatalf("expected error for nonexistent repo, got nil")
	}
	if err != pstore.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestClient_SaveAndGetRoundTrip(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	client := pstore.NewFromClientConn(conn)
	ctx := context.Background()

	record := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/speculate-kv",
		AlignmentPercentage: 92,
		IssueUrl:            "https://github.com/brotherlogic/speculate-kv/issues/42",
		EnrolledAtUnix:      1710000000,
		LastEvaluatedAtUnix: 1710002000,
		StatusMessage:       "Alignment verified",
	}

	if err := client.SaveEnrollment(ctx, record); err != nil {
		t.Fatalf("SaveEnrollment failed: %v", err)
	}

	// Verify key construction in pstore WriteRequest
	wantKey := "speculate/enrollment/brotherlogic/speculate-kv"
	if fakeServer.lastReq.GetKey() != wantKey {
		t.Errorf("WriteRequest key = %q, want %q", fakeServer.lastReq.GetKey(), wantKey)
	}

	got, err := client.GetEnrollment(ctx, "brotherlogic/speculate-kv")
	if err != nil {
		t.Fatalf("GetEnrollment failed: %v", err)
	}

	if !proto.Equal(got, record) {
		t.Errorf("GetEnrollment returned %+v, want %+v", got, record)
	}
}

func TestClient_DirectUnmarshalFallback(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	client := pstore.NewFromClientConn(conn)
	ctx := context.Background()

	record := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/notes",
		AlignmentPercentage: 100,
		StatusMessage:       "Fully aligned",
	}

	bytes, err := proto.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Store raw Any without TypeUrl (simulating legacy / raw byte stores)
	key := pstore.EnrollmentKey("brotherlogic/notes")
	fakeServer.data[key] = &anypb.Any{
		Value: bytes,
	}

	got, err := client.GetEnrollment(ctx, "brotherlogic/notes")
	if err != nil {
		t.Fatalf("GetEnrollment with fallback failed: %v", err)
	}
	if !proto.Equal(got, record) {
		t.Errorf("got %+v, want %+v", got, record)
	}
}

func TestClient_NotFound(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	client := pstore.NewFromClientConn(conn)
	ctx := context.Background()

	_, err := client.GetEnrollment(ctx, "unknown/repo")
	if err == nil {
		t.Fatalf("expected error for unknown repo, got nil")
	}
	if err != pstore.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestClient_Validation(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	client := pstore.NewFromClientConn(conn)
	ctx := context.Background()

	if err := client.SaveEnrollment(ctx, nil); err == nil {
		t.Error("expected error saving nil record")
	}

	if err := client.SaveEnrollment(ctx, &pb.EnrollmentRecord{}); err == nil {
		t.Error("expected error saving record with empty repo")
	}

	if _, err := client.GetEnrollment(ctx, ""); err == nil {
		t.Error("expected error getting empty repo")
	}
}

func TestClient_Close(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	client := pstore.NewFromClientConn(conn)
	if err := client.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}

	// Close on client without conn should be a no-op
	clientNoConn := pstore.NewClient(pstorepb.NewPStoreServiceClient(conn))
	if err := clientNoConn.Close(); err != nil {
		t.Errorf("Close on client without conn failed: %v", err)
	}
}

func TestMockStore_ListEnrollments(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	// Empty store returns empty list
	initial, err := store.ListEnrollments(ctx)
	if err != nil {
		t.Fatalf("ListEnrollments on empty store failed: %v", err)
	}
	if len(initial) != 0 {
		t.Fatalf("expected 0 records, got %d", len(initial))
	}

	recZeta := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/zeta",
		AlignmentPercentage: 40,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
	}
	recAlpha := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/alpha",
		AlignmentPercentage: 100,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
	}
	recMid := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/mid",
		AlignmentPercentage: 15,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_STALLED,
		LastError:           "transient timeout",
	}

	for _, r := range []*pb.EnrollmentRecord{recZeta, recAlpha, recMid} {
		if err := store.SaveEnrollment(ctx, r); err != nil {
			t.Fatalf("SaveEnrollment(%s) failed: %v", r.GetRepository(), err)
		}
	}

	list, err := store.ListEnrollments(ctx)
	if err != nil {
		t.Fatalf("ListEnrollments failed: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 records, got %d", len(list))
	}

	wantOrder := []*pb.EnrollmentRecord{recAlpha, recMid, recZeta}
	for i, want := range wantOrder {
		if !proto.Equal(list[i], want) {
			t.Errorf("list[%d] = %+v, want %+v", i, list[i], want)
		}
	}

	// Verify deep-copy isolation of returned slice elements
	list[0].AlignmentPercentage = 1
	list[0].LastError = "mutated"
	gotAlpha, err := store.GetEnrollment(ctx, "brotherlogic/alpha")
	if err != nil {
		t.Fatalf("GetEnrollment failed: %v", err)
	}
	if gotAlpha.GetAlignmentPercentage() != 100 || gotAlpha.GetLastError() != "" {
		t.Errorf("MockStore.ListEnrollments did not return deep copies: got %+v", gotAlpha)
	}
}

func TestMockStore_DeleteEnrollment(t *testing.T) {
	ctx := context.Background()
	store := pstore.NewMockStore()

	if err := store.DeleteEnrollment(ctx, ""); err == nil {
		t.Error("expected validation error when deleting empty repo")
	}
	if err := store.DeleteEnrollment(ctx, "   "); err == nil {
		t.Error("expected validation error when deleting whitespace repo")
	}

	// Idempotent delete on missing key
	if err := store.DeleteEnrollment(ctx, "brotherlogic/missing"); err != nil {
		t.Errorf("expected nil error deleting missing key, got %v", err)
	}

	rec := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/speculate",
		AlignmentPercentage: 80,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
	}
	if err := store.SaveEnrollment(ctx, rec); err != nil {
		t.Fatalf("SaveEnrollment failed: %v", err)
	}

	// Delete using normalized/prefixed form
	if err := store.DeleteEnrollment(ctx, "/speculate/enrollment/brotherlogic/speculate/"); err != nil {
		t.Fatalf("DeleteEnrollment failed: %v", err)
	}

	if _, err := store.GetEnrollment(ctx, "brotherlogic/speculate"); !errors.Is(err, pstore.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}

	list, err := store.ListEnrollments(ctx)
	if err != nil {
		t.Fatalf("ListEnrollments failed: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected 0 records after delete, got %d", len(list))
	}
}

func TestClient_ListEnrollments(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	client := pstore.NewFromClientConn(conn)
	ctx := context.Background()

	// Empty list
	empty, err := client.ListEnrollments(ctx)
	if err != nil {
		t.Fatalf("ListEnrollments on empty server failed: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected 0 records, got %d", len(empty))
	}

	// Seed an unrelated non-enrollment key in pstore
	fakeServer.data["other/namespace/item"] = &anypb.Any{Value: []byte("ignored")}
	// Simulate concurrent deletion race where GetKeys returns a key that disappears before Read
	fakeServer.extraGhostKeys = []string{"speculate/enrollment/org/deleted-race"}

	recC := &pb.EnrollmentRecord{
		Repository:          "org/repo-c",
		AlignmentPercentage: 0,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
		StatusMessage:       "Waiting for proto and specs",
	}
	recA := &pb.EnrollmentRecord{
		Repository:          "org/repo-a",
		AlignmentPercentage: 100,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
		EnrolledAtUnix:      1710000000,
		LastEvaluatedAtUnix: 1710000500,
	}
	recB := &pb.EnrollmentRecord{
		Repository:          "org/repo-b",
		AlignmentPercentage: 60,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_STALLED,
		LastError:           "github API rate limit exceeded",
	}

	for _, r := range []*pb.EnrollmentRecord{recC, recA, recB} {
		if err := client.SaveEnrollment(ctx, r); err != nil {
			t.Fatalf("SaveEnrollment(%s) failed: %v", r.GetRepository(), err)
		}
	}

	got, err := client.ListEnrollments(ctx)
	if err != nil {
		t.Fatalf("ListEnrollments failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}

	want := []*pb.EnrollmentRecord{recA, recB, recC}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Transport failure propagates error
	fakeServer.setErr(status.Error(codes.Unavailable, "pstore down"))
	if _, err := client.ListEnrollments(ctx); err == nil {
		t.Error("expected error from Client.ListEnrollments when pstore is unavailable")
	}
}

func TestClient_DeleteEnrollment(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	client := pstore.NewFromClientConn(conn)
	ctx := context.Background()

	if err := client.DeleteEnrollment(ctx, ""); err == nil {
		t.Error("expected validation error for empty repo")
	}
	if err := client.DeleteEnrollment(ctx, "   "); err == nil {
		t.Error("expected validation error for whitespace repo")
	}

	rec := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/speculate-kv",
		AlignmentPercentage: 75,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
		LastError:           "temporary network error",
	}
	if err := client.SaveEnrollment(ctx, rec); err != nil {
		t.Fatalf("SaveEnrollment failed: %v", err)
	}

	if err := client.DeleteEnrollment(ctx, "brotherlogic/speculate-kv"); err != nil {
		t.Fatalf("DeleteEnrollment failed: %v", err)
	}

	if _, err := client.GetEnrollment(ctx, "brotherlogic/speculate-kv"); !errors.Is(err, pstore.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}

	// Idempotent deletion when key no longer exists in pstore (codes.NotFound)
	if err := client.DeleteEnrollment(ctx, "brotherlogic/speculate-kv"); err != nil {
		t.Errorf("expected idempotent nil error on missing key delete, got %v", err)
	}

	// Non-NotFound RPC error should propagate
	fakeServer.setErr(status.Error(codes.Unavailable, "pstore unavailable"))
	if err := client.DeleteEnrollment(ctx, "brotherlogic/speculate-kv"); err == nil {
		t.Error("expected error when pstore returns Unavailable on Delete")
	}
}

func TestFallbackStore_NormalOperationAndHydration(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	primary := pstore.NewFromClientConn(conn)
	cache := pstore.NewMockStore()
	fb := pstore.NewFallbackStore(primary, cache)
	ctx := context.Background()

	// 1. SaveEnrollment writes through to both primary and fallback cache
	rec1 := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/repo-one",
		AlignmentPercentage: 90,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
		LastError:           "prev error",
	}
	if err := fb.SaveEnrollment(ctx, rec1); err != nil {
		t.Fatalf("FallbackStore.SaveEnrollment failed: %v", err)
	}

	gotPrimary, err := primary.GetEnrollment(ctx, "brotherlogic/repo-one")
	if err != nil || !proto.Equal(gotPrimary, rec1) {
		t.Fatalf("primary did not receive write-through record: got %+v, err %v", gotPrimary, err)
	}
	gotCache, err := cache.GetEnrollment(ctx, "brotherlogic/repo-one")
	if err != nil || !proto.Equal(gotCache, rec1) {
		t.Fatalf("fallback cache did not receive record: got %+v, err %v", gotCache, err)
	}

	// 2. GetEnrollment hydrates fallback cache when record exists only on primary
	rec2 := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/repo-two",
		AlignmentPercentage: 100,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
	}
	if err := primary.SaveEnrollment(ctx, rec2); err != nil {
		t.Fatalf("primary.SaveEnrollment failed: %v", err)
	}

	got2, err := fb.GetEnrollment(ctx, "brotherlogic/repo-two")
	if err != nil || !proto.Equal(got2, rec2) {
		t.Fatalf("FallbackStore.GetEnrollment failed: got %+v, err %v", got2, err)
	}
	hydrated2, err := cache.GetEnrollment(ctx, "brotherlogic/repo-two")
	if err != nil || !proto.Equal(hydrated2, rec2) {
		t.Fatalf("fallback cache was not hydrated on GetEnrollment: got %+v, err %v", hydrated2, err)
	}

	// 3. ListEnrollments hydrates fallback cache when additional record exists on primary
	rec3 := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/repo-three",
		AlignmentPercentage: 0,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
	}
	if err := primary.SaveEnrollment(ctx, rec3); err != nil {
		t.Fatalf("primary.SaveEnrollment failed: %v", err)
	}

	all, err := fb.ListEnrollments(ctx)
	if err != nil {
		t.Fatalf("FallbackStore.ListEnrollments failed: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 records, got %d", len(all))
	}
	hydrated3, err := cache.GetEnrollment(ctx, "brotherlogic/repo-three")
	if err != nil || !proto.Equal(hydrated3, rec3) {
		t.Fatalf("fallback cache was not hydrated on ListEnrollments: got %+v, err %v", hydrated3, err)
	}

	// 4. ErrNotFound on healthy primary returns ErrNotFound
	if _, err := fb.GetEnrollment(ctx, "brotherlogic/does-not-exist"); !errors.Is(err, pstore.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing repo, got %v", err)
	}

	// 5. DeleteEnrollment removes from both primary and fallback
	if err := fb.DeleteEnrollment(ctx, "brotherlogic/repo-one"); err != nil {
		t.Fatalf("FallbackStore.DeleteEnrollment failed: %v", err)
	}
	if _, err := primary.GetEnrollment(ctx, "brotherlogic/repo-one"); !errors.Is(err, pstore.ErrNotFound) {
		t.Errorf("expected ErrNotFound on primary after delete, got %v", err)
	}
	if _, err := cache.GetEnrollment(ctx, "brotherlogic/repo-one"); !errors.Is(err, pstore.ErrNotFound) {
		t.Errorf("expected ErrNotFound on fallback cache after delete, got %v", err)
	}
}

func TestFallbackStore_OutageResilience(t *testing.T) {
	fakeServer := newFakePStoreServer()
	conn, cleanup := setupTestGRPCServer(t, fakeServer)
	defer cleanup()

	primary := pstore.NewFromClientConn(conn)
	// Pass nil fallback to verify NewFallbackStore initializes an internal MockStore
	fb := pstore.NewFallbackStore(primary, nil)
	ctx := context.Background()

	preOutage := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/pre-outage",
		AlignmentPercentage: 100,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
	}
	if err := fb.SaveEnrollment(ctx, preOutage); err != nil {
		t.Fatalf("SaveEnrollment pre-outage failed: %v", err)
	}

	// Simulate mid-run remote pstore outage (codes.Unavailable)
	fakeServer.setErr(status.Error(codes.Unavailable, "pstore service unreachable"))

	// GetEnrollment serves cached record without error
	gotPre, err := fb.GetEnrollment(ctx, "brotherlogic/pre-outage")
	if err != nil {
		t.Fatalf("GetEnrollment during outage failed: %v", err)
	}
	if !proto.Equal(gotPre, preOutage) {
		t.Errorf("GetEnrollment during outage = %+v, want %+v", gotPre, preOutage)
	}

	// SaveEnrollment during outage succeeds in-memory without returning an error
	duringOutage := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/during-outage",
		AlignmentPercentage: 35,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
		LastError:           "check warning",
	}
	if err := fb.SaveEnrollment(ctx, duringOutage); err != nil {
		t.Fatalf("SaveEnrollment during outage failed: %v", err)
	}

	gotDuring, err := fb.GetEnrollment(ctx, "brotherlogic/during-outage")
	if err != nil || !proto.Equal(gotDuring, duringOutage) {
		t.Fatalf("GetEnrollment for record saved during outage failed: got %+v, err %v", gotDuring, err)
	}

	// ListEnrollments during outage serves sorted records from fallback
	list, err := fb.ListEnrollments(ctx)
	if err != nil {
		t.Fatalf("ListEnrollments during outage failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 records during outage, got %d", len(list))
	}
	if list[0].GetRepository() != "brotherlogic/during-outage" || list[1].GetRepository() != "brotherlogic/pre-outage" {
		t.Errorf("unexpected sort order during outage: %q, %q", list[0].GetRepository(), list[1].GetRepository())
	}

	// DeleteEnrollment during outage removes from fallback without returning an error
	if err := fb.DeleteEnrollment(ctx, "brotherlogic/pre-outage"); err != nil {
		t.Fatalf("DeleteEnrollment during outage failed: %v", err)
	}
	if _, err := fb.GetEnrollment(ctx, "brotherlogic/pre-outage"); !errors.Is(err, pstore.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete during outage, got %v", err)
	}

	// Validation checks still fail fast
	if err := fb.SaveEnrollment(ctx, nil); err == nil {
		t.Error("expected error saving nil record")
	}
	if _, err := fb.GetEnrollment(ctx, ""); err == nil {
		t.Error("expected error getting empty repo")
	}
	if err := fb.DeleteEnrollment(ctx, ""); err == nil {
		t.Error("expected error deleting empty repo")
	}
}

func TestFallbackStore_NilPrimary(t *testing.T) {
	fb := pstore.NewFallbackStore(nil, nil)
	ctx := context.Background()

	rec := &pb.EnrollmentRecord{
		Repository:          "brotherlogic/local-only",
		AlignmentPercentage: 50,
		Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
	}
	if err := fb.SaveEnrollment(ctx, rec); err != nil {
		t.Fatalf("SaveEnrollment failed: %v", err)
	}

	got, err := fb.GetEnrollment(ctx, "brotherlogic/local-only")
	if err != nil || !proto.Equal(got, rec) {
		t.Fatalf("GetEnrollment failed: got %+v, err %v", got, err)
	}

	list, err := fb.ListEnrollments(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListEnrollments failed: len=%d, err=%v", len(list), err)
	}

	if err := fb.DeleteEnrollment(ctx, "brotherlogic/local-only"); err != nil {
		t.Fatalf("DeleteEnrollment failed: %v", err)
	}
	if _, err := fb.GetEnrollment(ctx, "brotherlogic/local-only"); !errors.Is(err, pstore.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}
