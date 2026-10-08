package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type mockPipeline struct {
	enrollFn func(ctx context.Context, repo string) (*pb.EnrollResponse, error)
	calls    atomic.Int64
}

func (m *mockPipeline) Enroll(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
	m.calls.Add(1)
	if m.enrollFn != nil {
		return m.enrollFn(ctx, repo)
	}
	return &pb.EnrollResponse{
		Repository:          repo,
		AlignmentPercentage: 100,
		StatusMessage:       "enrolled",
	}, nil
}

func TestSpeculateServer_Enroll_Success(t *testing.T) {
	ctx := context.Background()
	mockPipe := &mockPipeline{
		enrollFn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			return &pb.EnrollResponse{
				Repository:          repo,
				AlignmentPercentage: 75,
				IssueUrl:            "https://github.com/owner/repo/issues/10",
				StatusMessage:       "repository enrolled with 75% alignment",
			}, nil
		},
	}

	server := NewSpeculateServer(mockPipe)
	resp, err := server.Enroll(ctx, &pb.EnrollRequest{Repository: "owner/repo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.GetRepository() != "owner/repo" {
		t.Errorf("expected repository owner/repo, got %s", resp.GetRepository())
	}
	if resp.GetAlignmentPercentage() != 75 {
		t.Errorf("expected alignment 75, got %d", resp.GetAlignmentPercentage())
	}
	if resp.GetIssueUrl() != "https://github.com/owner/repo/issues/10" {
		t.Errorf("expected issue url, got %s", resp.GetIssueUrl())
	}
	if resp.GetStatusMessage() != "repository enrolled with 75% alignment" {
		t.Errorf("expected status message, got %s", resp.GetStatusMessage())
	}
	if mockPipe.calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", mockPipe.calls.Load())
	}
}

func TestSpeculateServer_Enroll_Validation(t *testing.T) {
	ctx := context.Background()
	mockPipe := &mockPipeline{}
	server := NewSpeculateServer(mockPipe)

	// Nil request
	_, err := server.Enroll(ctx, nil)
	if err == nil {
		t.Fatal("expected error for nil request, got nil")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument code, got %v", err)
	}

	// Empty repository
	_, err = server.Enroll(ctx, &pb.EnrollRequest{Repository: "   "})
	if err == nil {
		t.Fatal("expected error for empty repository, got nil")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument code, got %v", err)
	}
}

func TestSpeculateServer_Enroll_SingleflightConcurrency(t *testing.T) {
	ctx := context.Background()
	var slowCalls atomic.Int64

	mockPipe := &mockPipeline{
		enrollFn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			slowCalls.Add(1)
			time.Sleep(100 * time.Millisecond)
			return &pb.EnrollResponse{
				Repository:          repo,
				AlignmentPercentage: 80,
				StatusMessage:       "ok",
			}, nil
		},
	}

	server := NewSpeculateServer(mockPipe)

	const concurrency = 10
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	resps := make([]*pb.EnrollResponse, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r, err := server.Enroll(ctx, &pb.EnrollRequest{Repository: "brotherlogic/concurrent-test"})
			errs[idx] = err
			resps[idx] = r
		}(i)
	}

	wg.Wait()

	for i := 0; i < concurrency; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d failed: %v", i, errs[i])
		}
		if resps[i].GetRepository() != "brotherlogic/concurrent-test" {
			t.Errorf("goroutine %d returned invalid repository: %v", i, resps[i])
		}
		if resps[i].GetAlignmentPercentage() != 80 {
			t.Errorf("goroutine %d returned invalid percentage: %v", i, resps[i])
		}
	}

	// Singleflight must deduplicate concurrent calls for the same repo to 1 execution
	if calls := slowCalls.Load(); calls != 1 {
		t.Errorf("expected singleflight to deduplicate concurrent calls to 1, got %d", calls)
	}

	// A subsequent call for a different repo must execute separately
	resp2, err := server.Enroll(ctx, &pb.EnrollRequest{Repository: "brotherlogic/other-repo"})
	if err != nil {
		t.Fatalf("unexpected error for other-repo: %v", err)
	}
	if resp2.GetRepository() != "brotherlogic/other-repo" {
		t.Errorf("expected other-repo, got %s", resp2.GetRepository())
	}
	if calls := slowCalls.Load(); calls != 2 {
		t.Errorf("expected 2 total calls, got %d", calls)
	}
}

func TestRunEnroll_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runEnroll(context.Background(), []string{"--help"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error for --help: %v", err)
	}
	if !strings.Contains(stdout.String(), "Usage: speculate enroll") {
		t.Errorf("expected usage in stdout, got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "daemon-addr") {
		t.Errorf("expected daemon-addr flag in help text, got: %s", stdout.String())
	}
}

func TestRunEnroll_MissingRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runEnroll(context.Background(), []string{}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when repo argument is missing")
	}
	if !strings.Contains(stderr.String(), "Error:") || !strings.Contains(stderr.String(), "owner/repo") {
		t.Errorf("expected missing repo error message in stderr, got: %s", stderr.String())
	}
}

func TestRunEnroll_Success_Bufconn(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	defer lis.Close()

	mockPipe := &mockPipeline{
		enrollFn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			return &pb.EnrollResponse{
				Repository:          repo,
				AlignmentPercentage: 85,
				IssueUrl:            "https://github.com/brotherlogic/speculate-kv/issues/42",
				StatusMessage:       "repository enrolled with 85% alignment",
			}, nil
		},
	}

	server := NewSpeculateServer(mockPipe)
	s := grpc.NewServer()
	pb.RegisterSpeculateServiceServer(s, server)

	go func() {
		_ = s.Serve(lis)
	}()
	defer s.Stop()

	var stdout, stderr bytes.Buffer
	dialOpt := grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	})

	err := runEnroll(context.Background(), []string{"--daemon-addr", "passthrough://bufnet", "brotherlogic/speculate-kv"}, &stdout, &stderr, dialOpt)
	if err != nil {
		t.Fatalf("runEnroll failed: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "brotherlogic/speculate-kv") {
		t.Errorf("output missing repository: %s", out)
	}
	if !strings.Contains(out, "85%") {
		t.Errorf("output missing alignment percentage: %s", out)
	}
	if !strings.Contains(out, "https://github.com/brotherlogic/speculate-kv/issues/42") {
		t.Errorf("output missing issue URL: %s", out)
	}
	if !strings.Contains(out, "repository enrolled with 85% alignment") {
		t.Errorf("output missing status message: %s", out)
	}
}

func TestRunEnroll_GRPCStatusErrors(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	defer lis.Close()

	mockPipe := &mockPipeline{
		enrollFn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			if strings.Contains(repo, "invalid") {
				return nil, status.Error(codes.InvalidArgument, "invalid repository format")
			}
			return nil, errors.New("internal unexpected failure")
		},
	}

	server := NewSpeculateServer(mockPipe)
	s := grpc.NewServer()
	pb.RegisterSpeculateServiceServer(s, server)

	go func() {
		_ = s.Serve(lis)
	}()
	defer s.Stop()

	dialOpt := grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	})

	// Test InvalidArgument handling
	{
		var stdout, stderr bytes.Buffer
		err := runEnroll(context.Background(), []string{"--daemon-addr", "passthrough://bufnet", "invalid-repo"}, &stdout, &stderr, dialOpt)
		if err == nil {
			t.Fatal("expected error for invalid repo")
		}
		if !strings.Contains(stderr.String(), "Invalid argument") && !strings.Contains(stderr.String(), "invalid repository format") {
			t.Errorf("stderr does not contain formatted invalid argument error: %s", stderr.String())
		}
	}

	// Test Unavailable daemon handling
	{
		var stdout, stderr bytes.Buffer
		// Dial a non-existent or closed listener/address with short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		closedLis := bufconn.Listen(1024)
		closedLis.Close()
		closedDialOpt := grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return nil, fmt.Errorf("connection refused")
		})

		err := runEnroll(ctx, []string{"--daemon-addr", "passthrough://closed", "brotherlogic/test"}, &stdout, &stderr, closedDialOpt)
		if err == nil {
			t.Fatal("expected error for unavailable daemon")
		}
		if !strings.Contains(stderr.String(), "unavailable") && !strings.Contains(stderr.String(), "connection refused") && !strings.Contains(stderr.String(), "Unavailable") {
			t.Errorf("stderr does not contain formatted unavailable error: %s", stderr.String())
		}
	}
}
