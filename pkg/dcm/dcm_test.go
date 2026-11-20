package dcm

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	dcmproto "github.com/brotherlogic/devcontainer-manager/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestRealClientWithGRPCServer(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	mockServer := NewMockServer()
	dcmproto.RegisterManagerServiceServer(server, mockServer)

	go func() {
		if err := server.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("server Serve error: %v", err)
		}
	}()
	t.Cleanup(func() {
		server.Stop()
	})

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
	})

	client := NewFromClientConn(conn)

	ctx := context.Background()

	// 1. HealthCheck
	if err := client.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}

	// 2. Up
	upParams := &UpParams{
		Repo:        "brotherlogic/speculate-kv",
		Branch:      "test/basic-put-get",
		Harness:     HarnessAntigravity,
		IssueNumber: 10,
		Prompt:      "Implement basic put get test",
		Model:       "deepseek-coder-v2:latest",
	}

	cfg, err := client.Up(ctx, upParams)
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if cfg.Id == "" {
		t.Fatalf("expected non-empty container ID, got empty")
	}
	if cfg.State != StateReady {
		t.Errorf("expected StateReady (%v), got %v", StateReady, cfg.State)
	}

	// 3. PushPrompt
	promptMsg := "Now run go test ./..."
	if err := client.PushPrompt(ctx, cfg.Id, promptMsg); err != nil {
		t.Fatalf("PushPrompt failed: %v", err)
	}

	prompts := mockServer.GetPrompts(cfg.Id)
	if len(prompts) != 1 || prompts[0] != promptMsg {
		t.Errorf("unexpected prompts on server: %v", prompts)
	}

	// 4. WaitForReady
	readyCfg, err := client.WaitForReady(ctx, cfg.Id, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForReady failed: %v", err)
	}
	if readyCfg.Id != cfg.Id {
		t.Errorf("expected ID %s, got %s", cfg.Id, readyCfg.Id)
	}

	// 5. Test failure state in WaitForReady
	mockServer.SetContainerState(cfg.Id, StateFailed, "docker daemon timeout")
	_, err = client.WaitForReady(ctx, cfg.Id, 10*time.Millisecond)
	if err == nil {
		t.Fatalf("expected error when container state is StateFailed, got nil")
	}
}

func TestMockClient(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient()

	if err := client.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}

	cfg, err := client.Up(ctx, &UpParams{
		Repo:    "brotherlogic/speculate-kv",
		Branch:  "feat/core",
		Harness: HarnessAntigravity,
	})
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	if err := client.PushPrompt(ctx, cfg.Id, "prompt text"); err != nil {
		t.Fatalf("PushPrompt failed: %v", err)
	}

	list, err := client.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 container in list, got %d (err: %v)", len(list), err)
	}

	if err := client.Down(ctx, ""); err != nil {
		t.Fatalf("Down with empty ID failed: %v", err)
	}
	if len(client.DownCalls) != 0 {
		t.Fatalf("expected 0 DownCalls after empty ID, got %v", client.DownCalls)
	}

	if err := client.Down(ctx, cfg.Id); err != nil {
		t.Fatalf("Down failed: %v", err)
	}
	if len(client.DownCalls) != 1 || client.DownCalls[0] != cfg.Id {
		t.Fatalf("expected DownCalls [%s], got %v", cfg.Id, client.DownCalls)
	}

	list, err = client.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("expected 0 containers in list after Down, got %d (err: %v)", len(list), err)
	}

	if err := client.WaitForGone(ctx, cfg.Id, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitForGone failed after Down: %v", err)
	}

	cfg2, err := client.Up(ctx, &UpParams{
		Repo:    "brotherlogic/speculate-kv",
		Branch:  "feat/timeout",
		Harness: HarnessPi,
	})
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	err = client.WaitForGone(timeoutCtx, cfg2.Id, 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded from WaitForGone, got %v", err)
	}
}

func TestRealClientDownAndWaitForGone(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	mockServer := NewMockServer()
	dcmproto.RegisterManagerServiceServer(server, mockServer)

	go func() {
		if err := server.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("server Serve error: %v", err)
		}
	}()
	t.Cleanup(func() {
		server.Stop()
	})

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
	})

	client := NewFromClientConn(conn)
	ctx := context.Background()

	// Empty container ID is idempotent.
	if err := client.Down(ctx, ""); err != nil {
		t.Fatalf("Down with empty ID failed: %v", err)
	}
	if err := client.WaitForGone(ctx, "", 10*time.Millisecond); err != nil {
		t.Fatalf("WaitForGone with empty ID failed: %v", err)
	}

	// Tear down an existing container and verify NotFound idempotency.
	cfg1, err := client.Up(ctx, &UpParams{
		Repo:    "brotherlogic/speculate-kv",
		Branch:  "feat/down-1",
		Harness: HarnessAntigravity,
	})
	if err != nil {
		t.Fatalf("Up cfg1 failed: %v", err)
	}

	if err := client.Down(ctx, cfg1.Id); err != nil {
		t.Fatalf("Down cfg1 failed: %v", err)
	}
	if err := client.WaitForGone(ctx, cfg1.Id, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitForGone cfg1 failed: %v", err)
	}
	if err := client.Down(ctx, cfg1.Id); err != nil {
		t.Fatalf("second Down cfg1 (NotFound) should be idempotent, got: %v", err)
	}

	// WaitForGone succeeds when container disappears after N polls.
	cfg2, err := client.Up(ctx, &UpParams{
		Repo:    "brotherlogic/speculate-kv",
		Branch:  "feat/down-2",
		Harness: HarnessAntigravity,
	})
	if err != nil {
		t.Fatalf("Up cfg2 failed: %v", err)
	}

	go func() {
		time.Sleep(35 * time.Millisecond)
		if _, err := mockServer.Down(context.Background(), &dcmproto.DownRequest{Id: cfg2.Id}); err != nil {
			t.Errorf("background mockServer.Down failed: %v", err)
		}
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer waitCancel()
	if err := client.WaitForGone(waitCtx, cfg2.Id, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitForGone cfg2 failed: %v", err)
	}

	// WaitForGone fails with context.DeadlineExceeded when container never disappears.
	cfg3, err := client.Up(ctx, &UpParams{
		Repo:    "brotherlogic/speculate-kv",
		Branch:  "feat/down-3",
		Harness: HarnessPi,
	})
	if err != nil {
		t.Fatalf("Up cfg3 failed: %v", err)
	}

	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer timeoutCancel()
	err = client.WaitForGone(timeoutCtx, cfg3.Id, 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded from WaitForGone, got %v", err)
	}
}

func TestParseHarness(t *testing.T) {
	tests := []struct {
		raw     string
		want    Harness
		wantErr bool
	}{
		{raw: "", want: HarnessAntigravity},
		{raw: "antigravity", want: HarnessAntigravity},
		{raw: "HARNESS_ANTIGRAVITY", want: HarnessAntigravity},
		{raw: "  Antigravity  ", want: HarnessAntigravity},
		{raw: "pi", want: HarnessPi},
		{raw: "HARNESS_PI", want: HarnessPi},
		{raw: "  Pi  ", want: HarnessPi},
		{raw: "unknown", want: HarnessUnspecified, wantErr: true},
		{raw: "claude", want: HarnessUnspecified, wantErr: true},
	}

	for _, tc := range tests {
		got, err := ParseHarness(tc.raw)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseHarness(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseHarness(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
