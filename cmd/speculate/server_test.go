package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brotherlogic/speculate/pkg/enroll"
	"github.com/brotherlogic/speculate/pkg/setup"
	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

var (
	_ EnrollmentPipeline = (*enroll.Pipeline)(nil)
	_ EnrollmentLister   = (*enroll.Pipeline)(nil)
	_ EnrollmentPipeline = (*mockPipeline)(nil)
	_ EnrollmentLister   = (*mockPipeline)(nil)
)

type mockPipeline struct {
	enrollFn  func(ctx context.Context, repo string) (*pb.EnrollResponse, error)
	listFn    func(ctx context.Context) ([]*pb.EnrollmentRecord, error)
	calls     atomic.Int64
	listCalls atomic.Int64
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

func (m *mockPipeline) ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
	m.listCalls.Add(1)
	if m.listFn != nil {
		return m.listFn(ctx)
	}
	return nil, nil
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

func TestParseInitConfig_CollaboratorFlag(t *testing.T) {
	t.Run("defaults collaborator to brotherlogic-speculate", func(t *testing.T) {
		cfg, err := parseInitConfig([]string{})
		if err != nil {
			t.Fatalf("parseInitConfig returned unexpected error: %v", err)
		}
		if cfg.Collaborator != setup.DefaultCollaborator {
			t.Errorf("cfg.Collaborator = %q, want DefaultCollaborator %q", cfg.Collaborator, setup.DefaultCollaborator)
		}
		if cfg.Collaborator != "brotherlogic-speculate" {
			t.Errorf("cfg.Collaborator = %q, want %q", cfg.Collaborator, "brotherlogic-speculate")
		}
	})

	t.Run("respects custom collaborator flag", func(t *testing.T) {
		cfg, err := parseInitConfig([]string{"--collaborator=my-bot"})
		if err != nil {
			t.Fatalf("parseInitConfig returned unexpected error: %v", err)
		}
		if cfg.Collaborator != "my-bot" {
			t.Errorf("cfg.Collaborator = %q, want %q", cfg.Collaborator, "my-bot")
		}
	})

	t.Run("preserves empty collaborator flag to skip ConfigureCollaborator", func(t *testing.T) {
		cfg, err := parseInitConfig([]string{"--collaborator="})
		if err != nil {
			t.Fatalf("parseInitConfig returned unexpected error: %v", err)
		}
		if cfg.Collaborator != "" {
			t.Errorf("cfg.Collaborator = %q, want empty string", cfg.Collaborator)
		}
	})

	t.Run("parses other init flags alongside collaborator", func(t *testing.T) {
		cfg, err := parseInitConfig([]string{
			"--repo=owner/repo",
			"--token=ghp_test123",
			"--dir=/tmp/project",
			"--force",
			"--skip-push",
		})
		if err != nil {
			t.Fatalf("parseInitConfig returned unexpected error: %v", err)
		}
		if cfg.Repo != "owner/repo" {
			t.Errorf("cfg.Repo = %q, want %q", cfg.Repo, "owner/repo")
		}
		if cfg.Token != "ghp_test123" {
			t.Errorf("cfg.Token = %q, want %q", cfg.Token, "ghp_test123")
		}
		if cfg.RootDir != "/tmp/project" {
			t.Errorf("cfg.RootDir = %q, want %q", cfg.RootDir, "/tmp/project")
		}
		if !cfg.Force {
			t.Errorf("cfg.Force = false, want true")
		}
		if !cfg.SkipPush {
			t.Errorf("cfg.SkipPush = false, want true")
		}
		if cfg.Collaborator != "brotherlogic-speculate" {
			t.Errorf("cfg.Collaborator = %q, want %q", cfg.Collaborator, "brotherlogic-speculate")
		}
	})
}

func TestSpeculateServer_ListEnrollments(t *testing.T) {
	ctx := context.Background()

	t.Run("empty list", func(t *testing.T) {
		mockPipe := &mockPipeline{
			listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
				return []*pb.EnrollmentRecord{}, nil
			},
		}
		server := NewSpeculateServer(mockPipe)

		resp, err := server.ListEnrollments(ctx, &pb.ListEnrollmentsRequest{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(resp.GetRecords()) != 0 {
			t.Errorf("expected 0 records, got %d", len(resp.GetRecords()))
		}
		if mockPipe.listCalls.Load() != 1 {
			t.Errorf("expected 1 list call, got %d", mockPipe.listCalls.Load())
		}
	})

	t.Run("populated records", func(t *testing.T) {
		expected := []*pb.EnrollmentRecord{
			{
				Repository:          "brotherlogic/alpha",
				AlignmentPercentage: 100,
				Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
				EnrolledAtUnix:      1700000000,
				LastEvaluatedAtUnix: 1700000300,
				StatusMessage:       "aligned",
			},
			{
				Repository:          "brotherlogic/beta",
				AlignmentPercentage: 40,
				IssueUrl:            "https://github.com/brotherlogic/beta/issues/7",
				Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_STALLED,
				EnrolledAtUnix:      1700000100,
				LastEvaluatedAtUnix: 1700000400,
				StatusMessage:       "stalled after max retries",
				LastError:           "rate limit exceeded",
			},
		}
		mockPipe := &mockPipeline{
			listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
				return expected, nil
			},
		}
		server := NewSpeculateServer(mockPipe)

		resp, err := server.ListEnrollments(ctx, &pb.ListEnrollmentsRequest{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(resp.GetRecords()) != 2 {
			t.Fatalf("expected 2 records, got %d", len(resp.GetRecords()))
		}
		if resp.GetRecords()[0].GetRepository() != "brotherlogic/alpha" ||
			resp.GetRecords()[0].GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
			t.Errorf("unexpected first record: %+v", resp.GetRecords()[0])
		}
		if resp.GetRecords()[1].GetRepository() != "brotherlogic/beta" ||
			resp.GetRecords()[1].GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_STALLED ||
			resp.GetRecords()[1].GetLastError() != "rate limit exceeded" {
			t.Errorf("unexpected second record: %+v", resp.GetRecords()[1])
		}
	})

	t.Run("error propagation", func(t *testing.T) {
		mockPipe := &mockPipeline{
			listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
				return nil, status.Error(codes.Unavailable, "pstore backend unavailable")
			},
		}
		server := NewSpeculateServer(mockPipe)

		_, err := server.ListEnrollments(ctx, &pb.ListEnrollmentsRequest{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if st, ok := status.FromError(err); !ok || st.Code() != codes.Unavailable {
			t.Errorf("expected Unavailable status error, got %v", err)
		}

		mockPipePlainErr := &mockPipeline{
			listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
				return nil, errors.New("disk read error")
			},
		}
		serverPlain := NewSpeculateServer(mockPipePlainErr)
		_, err = serverPlain.ListEnrollments(ctx, &pb.ListEnrollmentsRequest{})
		if err == nil {
			t.Fatal("expected error for plain error, got nil")
		}
		if st, ok := status.FromError(err); !ok || st.Code() != codes.Internal || !strings.Contains(st.Message(), "disk read error") {
			t.Errorf("expected Internal status wrapping 'disk read error', got %v", err)
		}
	})
}

func TestSpeculateServer_ListEnrollments_Bufconn(t *testing.T) {
	ctx := context.Background()
	lis := bufconn.Listen(1024 * 1024)
	defer lis.Close()

	var currentFn atomic.Value
	mockPipe := &mockPipeline{
		listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
			fn := currentFn.Load().(func(context.Context) ([]*pb.EnrollmentRecord, error))
			return fn(ctx)
		},
	}

	s := grpc.NewServer()
	pb.RegisterSpeculateServiceServer(s, NewSpeculateServer(mockPipe))
	go func() {
		_ = s.Serve(lis)
	}()
	defer s.Stop()

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}
	defer conn.Close()

	client := pb.NewSpeculateServiceClient(conn)

	// 1. Empty list over bufconn
	currentFn.Store(func(context.Context) ([]*pb.EnrollmentRecord, error) {
		return nil, nil
	})
	emptyResp, err := client.ListEnrollments(ctx, &pb.ListEnrollmentsRequest{})
	if err != nil {
		t.Fatalf("bufconn empty ListEnrollments failed: %v", err)
	}
	if len(emptyResp.GetRecords()) != 0 {
		t.Errorf("expected 0 records over bufconn, got %d", len(emptyResp.GetRecords()))
	}

	// 2. Populated records over bufconn
	currentFn.Store(func(context.Context) ([]*pb.EnrollmentRecord, error) {
		return []*pb.EnrollmentRecord{
			{
				Repository:          "brotherlogic/speculate-kv",
				AlignmentPercentage: 60,
				IssueUrl:            "https://github.com/brotherlogic/speculate-kv/issues/12",
				Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
				StatusMessage:       "scenario card proposed",
				LastError:           "transient github timeout",
			},
		}, nil
	})
	popResp, err := client.ListEnrollments(ctx, &pb.ListEnrollmentsRequest{})
	if err != nil {
		t.Fatalf("bufconn populated ListEnrollments failed: %v", err)
	}
	if len(popResp.GetRecords()) != 1 {
		t.Fatalf("expected 1 record over bufconn, got %d", len(popResp.GetRecords()))
	}
	rec := popResp.GetRecords()[0]
	if rec.GetRepository() != "brotherlogic/speculate-kv" ||
		rec.GetStage() != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW ||
		rec.GetLastError() != "transient github timeout" {
		t.Errorf("unexpected record over bufconn: %+v", rec)
	}

	// 3. Error propagation over bufconn
	currentFn.Store(func(context.Context) ([]*pb.EnrollmentRecord, error) {
		return nil, status.Error(codes.Unavailable, "remote store down")
	})
	_, err = client.ListEnrollments(ctx, &pb.ListEnrollmentsRequest{})
	if err == nil {
		t.Fatal("expected error over bufconn, got nil")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Unavailable {
		t.Errorf("expected Unavailable over bufconn, got %v", err)
	}
}

func TestRunEnroll_StageAndLastErrorFormatting(t *testing.T) {
	tests := []struct {
		name      string
		stage     pb.LifecycleStage
		wantStage string
		lastError string
	}{
		{
			name:      "awaiting seed without error",
			stage:     pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
			wantStage: "Awaiting Seed",
		},
		{
			name:      "evaluating without error",
			stage:     pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
			wantStage: "Evaluating",
		},
		{
			name:      "awaiting test review without error",
			stage:     pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
			wantStage: "Awaiting Test Review",
		},
		{
			name:      "aligning without error",
			stage:     pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
			wantStage: "Aligning",
		},
		{
			name:      "stalled with last error",
			stage:     pb.LifecycleStage_LIFECYCLE_STAGE_STALLED,
			wantStage: "Stalled",
			lastError: "git clone failed: authentication required",
		},
		{
			name:      "aligned without error",
			stage:     pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
			wantStage: "Aligned",
		},
		{
			name:      "unspecified stage",
			stage:     pb.LifecycleStage_LIFECYCLE_STAGE_UNSPECIFIED,
			wantStage: "Unspecified",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lis := bufconn.Listen(1024 * 1024)
			defer lis.Close()

			mockPipe := &mockPipeline{
				enrollFn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
					return &pb.EnrollResponse{
						Repository:          repo,
						AlignmentPercentage: 50,
						IssueUrl:            "https://github.com/brotherlogic/test/issues/1",
						StatusMessage:       "checked",
						Stage:               tc.stage,
						LastError:           tc.lastError,
					}, nil
				},
			}

			s := grpc.NewServer()
			pb.RegisterSpeculateServiceServer(s, NewSpeculateServer(mockPipe))
			go func() {
				_ = s.Serve(lis)
			}()
			defer s.Stop()

			var stdout, stderr bytes.Buffer
			dialOpt := grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return lis.Dial()
			})

			err := runEnroll(context.Background(), []string{"--daemon-addr", "passthrough://bufnet", "brotherlogic/test"}, &stdout, &stderr, dialOpt)
			if err != nil {
				t.Fatalf("runEnroll failed: %v\nstderr: %s", err, stderr.String())
			}

			out := stdout.String()
			wantStageLine := fmt.Sprintf("  Stage:        %s\n", tc.wantStage)
			if !strings.Contains(out, wantStageLine) {
				t.Errorf("stdout missing %q, got:\n%s", wantStageLine, out)
			}

			if tc.lastError == "" {
				if strings.Contains(out, "Last Error:") {
					t.Errorf("expected no Last Error line when LastError is empty, got:\n%s", out)
				}
			} else {
				wantErrLine := fmt.Sprintf("  Last Error:   %s\n", tc.lastError)
				if !strings.Contains(out, wantErrLine) {
					t.Errorf("stdout missing %q, got:\n%s", wantErrLine, out)
				}
			}
		})
	}
}

func TestParseDaemonConfig_SyncInterval(t *testing.T) {
	t.Run("defaults to 5m", func(t *testing.T) {
		t.Setenv("SYNC_INTERVAL", "")
		cfg := parseDaemonConfig([]string{})
		if cfg.SyncInterval != 5*time.Minute {
			t.Errorf("cfg.SyncInterval = %v, want %v", cfg.SyncInterval, 5*time.Minute)
		}
	})

	t.Run("reads SYNC_INTERVAL env var", func(t *testing.T) {
		t.Setenv("SYNC_INTERVAL", "2m30s")
		cfg := parseDaemonConfig([]string{})
		want := 2*time.Minute + 30*time.Second
		if cfg.SyncInterval != want {
			t.Errorf("cfg.SyncInterval = %v, want %v", cfg.SyncInterval, want)
		}
	})

	t.Run("falls back to 5m on invalid or non-positive SYNC_INTERVAL", func(t *testing.T) {
		for _, bad := range []string{"invalid", "0s", "-1m"} {
			t.Setenv("SYNC_INTERVAL", bad)
			cfg := parseDaemonConfig([]string{})
			if cfg.SyncInterval != 5*time.Minute {
				t.Errorf("for SYNC_INTERVAL=%q, cfg.SyncInterval = %v, want %v", bad, cfg.SyncInterval, 5*time.Minute)
			}
		}
	})

	t.Run("flag overrides default and env var", func(t *testing.T) {
		t.Setenv("SYNC_INTERVAL", "3m")
		cfg := parseDaemonConfig([]string{"--sync-interval=45s"})
		if cfg.SyncInterval != 45*time.Second {
			t.Errorf("cfg.SyncInterval = %v, want %v", cfg.SyncInterval, 45*time.Second)
		}
	})
}

func TestInitHTTPMux_Routes(t *testing.T) {
	cfg := &DaemonConfig{
		HTTPPort:       8080,
		GRPCPort:       50051,
		OllamaEndpoint: "http://localhost:11434/v1",
		TargetRepo:     "brotherlogic/speculate-kv",
		SyncInterval:   5 * time.Minute,
	}

	mockPipe := &mockPipeline{
		listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
			return []*pb.EnrollmentRecord{
				{
					Repository:          "brotherlogic/speculate-kv",
					Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
					AlignmentPercentage: 100,
					LastEvaluatedAtUnix: time.Now().Add(-1 * time.Minute).Unix(),
					StatusMessage:       "aligned",
				},
			}, nil
		},
	}

	mux := initHTTPMux(cfg, mockPipe)

	t.Run("GET / serves HTML dashboard", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET / status = %d, want %d", rec.Code, http.StatusOK)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET / Content-Type = %q, want text/html prefix", ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "brotherlogic/speculate-kv") || !strings.Contains(body, "Aligned") {
			t.Errorf("GET / body missing expected tracked repository content:\n%s", body)
		}
	})

	t.Run("GET /healthz returns 200 OK", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET /healthz status = %d, want %d", rec.Code, http.StatusOK)
		}
		if rec.Body.String() != "OK\n" {
			t.Errorf("GET /healthz body = %q, want %q", rec.Body.String(), "OK\n")
		}
	})

	t.Run("GET /status returns JSON status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET /status status = %d, want %d", rec.Code, http.StatusOK)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET /status Content-Type = %q, want application/json", ct)
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("GET /status returned invalid JSON: %v", err)
		}
		if payload["status"] != "running" {
			t.Errorf("payload[status] = %v, want running", payload["status"])
		}
		if payload["target_repo"] != "brotherlogic/speculate-kv" {
			t.Errorf("payload[target_repo] = %v, want brotherlogic/speculate-kv", payload["target_repo"])
		}
	})

	t.Run("GET /metrics serves Prometheus metrics", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET /metrics status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !strings.Contains(rec.Body.String(), "# HELP") {
			t.Errorf("GET /metrics body missing Prometheus '# HELP' lines")
		}
	})

	t.Run("GET /nonexistent returns 404 Not Found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /nonexistent status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
}


