package pstore_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/brotherlogic/speculate/pkg/pstore"
	pb "github.com/brotherlogic/speculate/proto"
	pstorepb "github.com/brotherlogic/pstore/proto"
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
)

// fakePStoreServer implements pstorepb.PStoreServiceServer in-memory for testing Client.
type fakePStoreServer struct {
	pstorepb.UnimplementedPStoreServiceServer
	mu       sync.RWMutex
	data     map[string]*anypb.Any
	lastReq  *pstorepb.WriteRequest
	readKeys []string
}

func newFakePStoreServer() *fakePStoreServer {
	return &fakePStoreServer{
		data: make(map[string]*anypb.Any),
	}
}

func (s *fakePStoreServer) Read(ctx context.Context, req *pstorepb.ReadRequest) (*pstorepb.ReadResponse, error) {
	s.mu.Lock()
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
	s.lastReq = req
	s.data[req.GetKey()] = req.GetValue()
	return &pstorepb.WriteResponse{Timestamp: time.Now().Unix()}, nil
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
