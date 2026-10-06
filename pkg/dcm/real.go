package dcm

import (
	"context"
	"fmt"
	"time"

	dcmproto "github.com/brotherlogic/devcontainer-manager/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RealClient manages communication with a real devcontainer-manager gRPC service.
type RealClient struct {
	conn       *grpc.ClientConn
	grpcClient dcmproto.ManagerServiceClient
}

// NewRealClient establishes a gRPC client connection to devcontainer-manager.
func NewRealClient(ctx context.Context, target string, opts ...grpc.DialOption) (*RealClient, error) {
	if len(opts) == 0 {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to DCM at %s: %w", target, err)
	}

	return &RealClient{
		conn:       conn,
		grpcClient: dcmproto.NewManagerServiceClient(conn),
	}, nil
}

// NewFromClientConn constructs a RealClient using an already established grpc.ClientConn.
func NewFromClientConn(conn *grpc.ClientConn) *RealClient {
	return &RealClient{
		conn:       conn,
		grpcClient: dcmproto.NewManagerServiceClient(conn),
	}
}

// Up triggers devcontainer provisioning on devcontainer-manager.
func (c *RealClient) Up(ctx context.Context, params *UpParams) (*DevcontainerConfig, error) {
	if params == nil {
		return nil, fmt.Errorf("up params cannot be nil")
	}

	req := &dcmproto.UpRequest{
		Repo:    params.Repo,
		Branch:  params.Branch,
		Prompt:  params.Prompt,
		Model:   params.Model,
		Harness: params.Harness,
	}

	if params.IssueNumber > 0 {
		req.Identifier = &dcmproto.Identifier{
			Id: &dcmproto.Identifier_IssueNumber{
				IssueNumber: params.IssueNumber,
			},
		}
	} else if params.PRNumber > 0 {
		req.Identifier = &dcmproto.Identifier{
			Id: &dcmproto.Identifier_PrNumber{
				PrNumber: params.PRNumber,
			},
		}
	}

	resp, err := c.grpcClient.Up(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("DCM Up RPC failed: %w", err)
	}

	return resp.GetConfig(), nil
}

// PushPrompt pushes a prompt to an active devcontainer.
func (c *RealClient) PushPrompt(ctx context.Context, containerID, prompt string) error {
	req := &dcmproto.PushPromptRequest{
		Id:     containerID,
		Prompt: prompt,
	}

	_, err := c.grpcClient.PushPrompt(ctx, req)
	if err != nil {
		return fmt.Errorf("DCM PushPrompt RPC failed for %s: %w", containerID, err)
	}

	return nil
}

// List retrieves all tracked devcontainers.
func (c *RealClient) List(ctx context.Context) ([]*DevcontainerConfig, error) {
	resp, err := c.grpcClient.List(ctx, &dcmproto.ListRequest{})
	if err != nil {
		return nil, fmt.Errorf("DCM List RPC failed: %w", err)
	}

	return resp.GetConfigs(), nil
}

// WaitForReady polls the container state until it reaches StateReady or fails.
func (c *RealClient) WaitForReady(ctx context.Context, containerID string, pollInterval time.Duration) (*DevcontainerConfig, error) {
	if pollInterval <= 0 {
		pollInterval = 1 * time.Second
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		configs, err := c.List(ctx)
		if err == nil {
			for _, cfg := range configs {
				if cfg.GetId() == containerID {
					switch cfg.GetState() {
					case StateReady:
						return cfg, nil
					case StateFailed, StateHardFailed:
						return cfg, fmt.Errorf("container %s reached failure state %v: %s", containerID, cfg.GetState(), cfg.GetErrorMessage())
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// HealthCheck verifies connectivity by performing a lightweight List RPC.
func (c *RealClient) HealthCheck(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := c.grpcClient.List(checkCtx, &dcmproto.ListRequest{})
	if err != nil {
		return fmt.Errorf("DCM health check failed: %w", err)
	}

	return nil
}

// Close closes the gRPC connection.
func (c *RealClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
