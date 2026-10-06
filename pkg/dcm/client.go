package dcm

import (
	"context"
	"time"

	dcmproto "github.com/brotherlogic/devcontainer-manager/proto"
)

// Harness alias for DCM harness enum.
type Harness = dcmproto.Harness

const (
	HarnessUnspecified = dcmproto.Harness_HARNESS_UNSPECIFIED
	HarnessAntigravity = dcmproto.Harness_HARNESS_ANTIGRAVITY
	HarnessPi          = dcmproto.Harness_HARNESS_PI
)

// State alias for DCM devcontainer lifecycle states.
type State = dcmproto.State

const (
	StateUnknown    = dcmproto.State_UNKNOWN_STATE
	StateReceived   = dcmproto.State_DCM_RECEIVED
	StateCreating   = dcmproto.State_DCM_CREATING
	StateBranching  = dcmproto.State_DCM_BRANCHING
	StateHarness    = dcmproto.State_DCM_HARNESS
	StateReady      = dcmproto.State_DCM_READY
	StateFailed     = dcmproto.State_DCM_FAILED
	StateHardFailed = dcmproto.State_DCM_HARD_FAILED
)

// DevcontainerConfig aliases DCM's protobuf configuration definition.
type DevcontainerConfig = dcmproto.DevcontainerConfig

// UpParams defines the parameters to provision a devcontainer environment.
type UpParams struct {
	Repo        string
	Branch      string
	Harness     Harness
	IssueNumber int32
	PRNumber    int32
	Prompt      string
	Model       string
}

// Client defines operations for interacting with devcontainer-manager (DCM).
type Client interface {
	// Up requests devcontainer-manager to spin up a container environment for a branch.
	Up(ctx context.Context, params *UpParams) (*DevcontainerConfig, error)

	// PushPrompt delivers a prompt instruction to an active devcontainer session.
	PushPrompt(ctx context.Context, containerID, prompt string) error

	// WaitForReady polls the container state until it reaches StateReady, fails, or context cancels.
	WaitForReady(ctx context.Context, containerID string, pollInterval time.Duration) (*DevcontainerConfig, error)

	// List queries currently tracked devcontainers.
	List(ctx context.Context) ([]*DevcontainerConfig, error)

	// HealthCheck validates connectivity to the devcontainer-manager service.
	HealthCheck(ctx context.Context) error

	// Close releases the underlying gRPC connection.
	Close() error
}
