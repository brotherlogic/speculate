package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	ghwebhookpb "github.com/brotherlogic/ghwebhook/proto/ghwebhook/v1"
	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func startTestWebhookClient(t *testing.T, srv *WebhookServer) ghwebhookpb.WebhookHandlerClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	ghwebhookpb.RegisterWebhookHandlerServer(s, srv)

	go func() {
		_ = s.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		s.Stop()
		_ = lis.Close()
		t.Fatalf("failed to dial bufconn: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		s.Stop()
		_ = lis.Close()
	})

	return ghwebhookpb.NewWebhookHandlerClient(conn)
}

func TestWebhookServer_ReceiveWebhook_DispatchesAllEventTypes(t *testing.T) {
	tests := []struct {
		name  string
		event *ghwebhookpb.WebhookEvent
		want  string
	}{
		{
			name: "push event",
			event: &ghwebhookpb.WebhookEvent{
				Header: &ghwebhookpb.EventHeader{EventType: "push"},
				Payload: &ghwebhookpb.WebhookEvent_Push{
					Push: &ghwebhookpb.PushEvent{
						Ref:        "refs/heads/main",
						After:      "abc123",
						Repository: &ghwebhookpb.Repository{FullName: "brotherlogic/speculate-kv"},
					},
				},
			},
			want: "brotherlogic/speculate-kv",
		},
		{
			name: "pull_request event",
			event: &ghwebhookpb.WebhookEvent{
				Header: &ghwebhookpb.EventHeader{EventType: "pull_request"},
				Payload: &ghwebhookpb.WebhookEvent_PullRequest{
					PullRequest: &ghwebhookpb.PullRequestEvent{
						Action:     "opened",
						Number:     12,
						HeadRef:    "feat/put",
						HeadSha:    "def456",
						BaseRef:    "main",
						Repository: &ghwebhookpb.Repository{FullName: "brotherlogic/speculate-kv"},
					},
				},
			},
			want: "brotherlogic/speculate-kv",
		},
		{
			name: "pull_request_review event",
			event: &ghwebhookpb.WebhookEvent{
				Header: &ghwebhookpb.EventHeader{EventType: "pull_request_review"},
				Payload: &ghwebhookpb.WebhookEvent_PullRequestReview{
					PullRequestReview: &ghwebhookpb.PullRequestReviewEvent{
						Action:            "submitted",
						State:             "approved",
						CommitId:          "def456",
						PullRequestNumber: 12,
						Reviewer:          &ghwebhookpb.User{Login: "brotherlogic"},
						Repository:        &ghwebhookpb.Repository{FullName: "brotherlogic/speculate-kv"},
					},
				},
			},
			want: "brotherlogic/speculate-kv",
		},
		{
			name: "check_suite event",
			event: &ghwebhookpb.WebhookEvent{
				Header: &ghwebhookpb.EventHeader{EventType: "check_suite"},
				Payload: &ghwebhookpb.WebhookEvent_CheckSuite{
					CheckSuite: &ghwebhookpb.CheckSuiteEvent{
						Action:     "completed",
						Status:     "completed",
						Conclusion: "success",
						HeadBranch: "feat/put",
						HeadSha:    "def456",
						Repository: &ghwebhookpb.Repository{FullName: "brotherlogic/speculate-kv"},
					},
				},
			},
			want: "brotherlogic/speculate-kv",
		},
		{
			name: "issue event",
			event: &ghwebhookpb.WebhookEvent{
				Header: &ghwebhookpb.EventHeader{EventType: "issues"},
				Payload: &ghwebhookpb.WebhookEvent_Issue{
					Issue: &ghwebhookpb.IssueEvent{
						Action:     "unlabeled",
						Number:     5,
						Labels:     []string{"speculate-agentic-loop"},
						Repository: &ghwebhookpb.Repository{FullName: "brotherlogic/speculate-kv"},
					},
				},
			},
			want: "brotherlogic/speculate-kv",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dispatchedCh := make(chan string, 1)
			ctxErrCh := make(chan error, 1)
			rpcReturned := make(chan struct{})

			mockPipe := &mockPipeline{
				enrollFn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
					// Ensure async dispatch outlives the cancelled RPC context.
					select {
					case <-rpcReturned:
					case <-time.After(2 * time.Second):
					}
					ctxErrCh <- ctx.Err()
					dispatchedCh <- repo
					return &pb.EnrollResponse{Repository: repo}, nil
				},
			}

			client := startTestWebhookClient(t, NewWebhookServer(mockPipe))

			rpcCtx, cancel := context.WithCancel(context.Background())
			resp, err := client.ReceiveWebhook(rpcCtx, tc.event)
			cancel()
			close(rpcReturned)

			if err != nil {
				t.Fatalf("ReceiveWebhook RPC failed: %v", err)
			}
			if !resp.GetSuccess() {
				t.Errorf("ReceiveWebhook Success = false, want true (message=%q)", resp.GetMessage())
			}

			select {
			case gotRepo := <-dispatchedCh:
				if gotRepo != tc.want {
					t.Errorf("dispatched repo = %q, want %q", gotRepo, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for async Enroll dispatch")
			}

			select {
			case bgErr := <-ctxErrCh:
				if bgErr != nil {
					t.Errorf("expected detached background context without cancellation, got err: %v", bgErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for context error check")
			}
		})
	}
}

func TestWebhookServer_ReceiveWebhook_IgnoresEmptyInvalidAndUntrackedRepos(t *testing.T) {
	var mu sync.Mutex
	var dispatched []string

	mockPipe := &mockPipeline{
		listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
			return []*pb.EnrollmentRecord{
				{Repository: "brotherlogic/speculate-kv"},
			}, nil
		},
		enrollFn: func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
			mu.Lock()
			dispatched = append(dispatched, repo)
			mu.Unlock()
			return &pb.EnrollResponse{Repository: repo}, nil
		},
	}

	client := startTestWebhookClient(t, NewWebhookServer(mockPipe))

	ignoredEvents := []struct {
		name  string
		event *ghwebhookpb.WebhookEvent
	}{
		{
			name:  "empty payload",
			event: &ghwebhookpb.WebhookEvent{},
		},
		{
			name: "empty repository full_name",
			event: &ghwebhookpb.WebhookEvent{
				Payload: &ghwebhookpb.WebhookEvent_Push{
					Push: &ghwebhookpb.PushEvent{
						Repository: &ghwebhookpb.Repository{FullName: "   "},
					},
				},
			},
		},
		{
			name: "malformed repository full_name",
			event: &ghwebhookpb.WebhookEvent{
				Payload: &ghwebhookpb.WebhookEvent_Issue{
					Issue: &ghwebhookpb.IssueEvent{
						Repository: &ghwebhookpb.Repository{FullName: "not-an-owner-repo"},
					},
				},
			},
		},
		{
			name: "untracked repository",
			event: &ghwebhookpb.WebhookEvent{
				Payload: &ghwebhookpb.WebhookEvent_PullRequest{
					PullRequest: &ghwebhookpb.PullRequestEvent{
						Repository: &ghwebhookpb.Repository{FullName: "other/untracked-repo"},
					},
				},
			},
		},
	}

	for _, tc := range ignoredEvents {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.ReceiveWebhook(context.Background(), tc.event)
			if err != nil {
				t.Fatalf("ReceiveWebhook returned error: %v", err)
			}
			if !resp.GetSuccess() {
				t.Errorf("expected Success=true for ignored event so ghwebhook avoids delivery strikes, got false (%q)", resp.GetMessage())
			}
		})
	}

	// Give any unintended goroutines a moment, then confirm zero dispatches for ignored events.
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	gotIgnoredCount := len(dispatched)
	mu.Unlock()
	if gotIgnoredCount != 0 {
		t.Fatalf("expected 0 Enroll calls for ignored events, got: %v", dispatched)
	}

	// Confirm tracked repo with different casing still dispatches cleanly.
	trackedDone := make(chan string, 1)
	mockPipe.enrollFn = func(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
		trackedDone <- repo
		return &pb.EnrollResponse{Repository: repo}, nil
	}

	resp, err := client.ReceiveWebhook(context.Background(), &ghwebhookpb.WebhookEvent{
		Payload: &ghwebhookpb.WebhookEvent_Push{
			Push: &ghwebhookpb.PushEvent{
				Repository: &ghwebhookpb.Repository{FullName: "BrotherLogic/Speculate-KV"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReceiveWebhook for tracked repo failed: %v", err)
	}
	if !resp.GetSuccess() {
		t.Errorf("expected Success=true for tracked repo, got false (%q)", resp.GetMessage())
	}

	select {
	case got := <-trackedDone:
		if got != "BrotherLogic/Speculate-KV" {
			t.Errorf("dispatched repo = %q, want BrotherLogic/Speculate-KV", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tracked repo dispatch")
	}
}
