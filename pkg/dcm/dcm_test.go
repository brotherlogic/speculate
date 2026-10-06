package dcm

import (
	"context"
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
}
