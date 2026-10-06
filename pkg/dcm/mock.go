package dcm

import (
	"context"
	"fmt"
	"sync"
	"time"

	dcmproto "github.com/brotherlogic/devcontainer-manager/proto"
)

// MockServer implements dcmproto.ManagerServiceServer in-memory for testing.
type MockServer struct {
	dcmproto.UnimplementedManagerServiceServer

	mu         sync.RWMutex
	containers map[string]*dcmproto.DevcontainerConfig
	prompts    map[string][]string
	nextID     int
}

// NewMockServer constructs an in-memory MockServer.
func NewMockServer() *MockServer {
	return &MockServer{
		containers: make(map[string]*dcmproto.DevcontainerConfig),
		prompts:    make(map[string][]string),
		nextID:     1,
	}
}

// Up implements ManagerServiceServer.Up.
func (s *MockServer) Up(ctx context.Context, req *dcmproto.UpRequest) (*dcmproto.UpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := fmt.Sprintf("dcm-container-%d", s.nextID)
	s.nextID++

	cfg := &dcmproto.DevcontainerConfig{
		Id:      id,
		Request: req,
		State:   dcmproto.State_DCM_READY,
	}

	s.containers[id] = cfg
	return &dcmproto.UpResponse{Config: cfg}, nil
}

// Down implements ManagerServiceServer.Down.
func (s *MockServer) Down(ctx context.Context, req *dcmproto.DownRequest) (*dcmproto.DownResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.containers, req.GetId())
	return &dcmproto.DownResponse{}, nil
}

// List implements ManagerServiceServer.List.
func (s *MockServer) List(ctx context.Context, req *dcmproto.ListRequest) (*dcmproto.ListResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var list []*dcmproto.DevcontainerConfig
	for _, c := range s.containers {
		list = append(list, c)
	}

	return &dcmproto.ListResponse{Configs: list}, nil
}

// PushPrompt implements ManagerServiceServer.PushPrompt.
func (s *MockServer) PushPrompt(ctx context.Context, req *dcmproto.PushPromptRequest) (*dcmproto.PushPromptResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.containers[req.GetId()]; !ok {
		return nil, fmt.Errorf("container %s not found", req.GetId())
	}

	s.prompts[req.GetId()] = append(s.prompts[req.GetId()], req.GetPrompt())
	return &dcmproto.PushPromptResponse{}, nil
}

// SetContainerState allows test scenarios to manipulate container state.
func (s *MockServer) SetContainerState(id string, state dcmproto.State, errorMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cfg, ok := s.containers[id]; ok {
		cfg.State = state
		cfg.ErrorMessage = errorMsg
	}
}

// GetPrompts returns the prompts received by a container.
func (s *MockServer) GetPrompts(id string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return append([]string(nil), s.prompts[id]...)
}

// MockClient provides an in-memory implementation of the Client interface.
type MockClient struct {
	mu         sync.RWMutex
	containers map[string]*DevcontainerConfig
	prompts    map[string][]string
	nextID     int
}

// NewMockClient constructs a MockClient.
func NewMockClient() *MockClient {
	return &MockClient{
		containers: make(map[string]*DevcontainerConfig),
		prompts:    make(map[string][]string),
		nextID:     1,
	}
}

// Up simulates devcontainer creation.
func (m *MockClient) Up(ctx context.Context, params *UpParams) (*DevcontainerConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := fmt.Sprintf("mock-dcm-%d", m.nextID)
	m.nextID++

	req := &dcmproto.UpRequest{
		Repo:    params.Repo,
		Branch:  params.Branch,
		Prompt:  params.Prompt,
		Model:   params.Model,
		Harness: params.Harness,
	}

	cfg := &DevcontainerConfig{
		Id:      id,
		Request: req,
		State:   StateReady,
	}

	m.containers[id] = cfg
	return cfg, nil
}

// PushPrompt records a prompt sent to a container.
func (m *MockClient) PushPrompt(ctx context.Context, containerID, prompt string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.containers[containerID]; !ok {
		return fmt.Errorf("container %s not found", containerID)
	}

	m.prompts[containerID] = append(m.prompts[containerID], prompt)
	return nil
}

// WaitForReady returns the container once ready.
func (m *MockClient) WaitForReady(ctx context.Context, containerID string, pollInterval time.Duration) (*DevcontainerConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cfg, ok := m.containers[containerID]
	if !ok {
		return nil, fmt.Errorf("container %s not found", containerID)
	}

	if cfg.State == StateReady {
		return cfg, nil
	}
	if cfg.State == StateFailed || cfg.State == StateHardFailed {
		return cfg, fmt.Errorf("container %s entered failure state %v", containerID, cfg.State)
	}

	return cfg, nil
}

// List returns all active configs.
func (m *MockClient) List(ctx context.Context) ([]*DevcontainerConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var res []*DevcontainerConfig
	for _, c := range m.containers {
		res = append(res, c)
	}
	return res, nil
}

// HealthCheck returns nil for mock.
func (m *MockClient) HealthCheck(ctx context.Context) error {
	return nil
}

// Close is a no-op for mock.
func (m *MockClient) Close() error {
	return nil
}
