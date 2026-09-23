package synthesizer

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brotherlogic/speculate/pkg/evaluator"
	"github.com/brotherlogic/speculate/pkg/parser"
)

func TestFormatMarkdown(t *testing.T) {
	card := &ScenarioCard{
		ID:           "Core-1",
		Stage:        "Core",
		Requirement:  "Put(key, value) overwrites existing values",
		TargetRPC:    "Put",
		Title:        "Basic Put Overwrite",
		Given:        "An empty KV store",
		When:         "Put is called twice with the same key",
		Then:         "The second value replaces the first without error",
		TestFuncName: "TestPut_Overwrite",
	}

	md := card.FormatMarkdown()
	if !strings.Contains(md, "### Scenario Card: [Stage: Core] Basic Put Overwrite") {
		t.Errorf("expected title in markdown, got %s", md)
	}
	if !strings.Contains(md, "* **GIVEN:** An empty KV store") {
		t.Errorf("expected GIVEN in markdown, got %s", md)
	}
	if !strings.Contains(md, "* **WHEN:** Put is called twice with the same key") {
		t.Errorf("expected WHEN in markdown, got %s", md)
	}
	if !strings.Contains(md, "* **THEN:** The second value replaces the first without error") {
		t.Errorf("expected THEN in markdown, got %s", md)
	}
	if !strings.Contains(md, "* **Target Test Function:** `TestPut_Overwrite`") {
		t.Errorf("expected TestFuncName in markdown, got %s", md)
	}
}

func TestGenerateScenarioCard_WithMockLLM(t *testing.T) {
	mockLLM := &evaluator.MockLLMClient{
		Response: `{"id": "Core-1", "stage": "Core", "title": "Put Overwrite", "given": "Initialized store", "when": "Put is called", "then": "Stored successfully", "test_func_name": "TestPut_Overwrite"}`,
	}

	syn := NewSynthesizer(mockLLM)
	stage := &parser.Stage{Name: "Core"}
	req := &parser.Requirement{
		ID:          "Core-1",
		TargetRPC:   "Put",
		Description: "Put stores payload",
	}

	card, err := syn.GenerateScenarioCard(context.Background(), stage, req, "rpc Put(PutRequest) returns (PutResponse);")
	if err != nil {
		t.Fatalf("GenerateScenarioCard failed: %v", err)
	}

	if card.ID != "Core-1" {
		t.Errorf("expected ID 'Core-1', got %q", card.ID)
	}
	if card.Stage != "Core" {
		t.Errorf("expected Stage 'Core', got %q", card.Stage)
	}
	if card.Title != "Put Overwrite" {
		t.Errorf("expected Title 'Put Overwrite', got %q", card.Title)
	}
	if card.TestFuncName != "TestPut_Overwrite" {
		t.Errorf("expected TestFuncName 'TestPut_Overwrite', got %q", card.TestFuncName)
	}
}

func TestSynthesizeTest_WithMockLLM_ValidSyntax(t *testing.T) {
	mockTestCode := `package tests

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/brotherlogic/speculate/example/internal/server"
	pb "github.com/brotherlogic/speculate/example/proto/kv/v1"
)

// Stage: Core
// Scenario: Core-1 Basic Put Overwrite
func TestPut_Overwrite(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	pb.RegisterKVServer(s, server.New())
	go s.Serve(lis)
	t.Cleanup(func() { s.Stop() })

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	client := pb.NewKVClient(conn)
	_, err = client.Put(context.Background(), &pb.PutRequest{Key: "k", Value: []byte("v")})
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
}
`

	mockLLM := &evaluator.MockLLMClient{
		Response: "```go\n" + mockTestCode + "\n```",
	}

	syn := NewSynthesizer(mockLLM)
	card := &ScenarioCard{
		ID:           "Core-1",
		Stage:        "Core",
		Title:        "Basic Put Overwrite",
		TestFuncName: "TestPut_Overwrite",
	}

	code, err := syn.SynthesizeTest(context.Background(), card, "github.com/brotherlogic/speculate/example/internal/server", "github.com/brotherlogic/speculate/example/proto/kv/v1")
	if err != nil {
		t.Fatalf("SynthesizeTest failed: %v", err)
	}

	if err := ValidateGoCode(code); err != nil {
		t.Fatalf("synthesized code has invalid syntax: %v", err)
	}
}

func TestValidateGoCode_InvalidSyntax(t *testing.T) {
	invalidCode := `package tests
func IncompleteFunc( {
`
	if err := ValidateGoCode(invalidCode); err == nil {
		t.Error("expected error for invalid Go syntax, got nil")
	}
}

func TestMutationProbe_FailsAgainstUnimplementedServer(t *testing.T) {
	// A valid test asserting Put success against the current skeleton server
	// must fail with codes.Unimplemented, proving the test is RED.
	testCode := `package tests

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/brotherlogic/speculate/example/internal/server"
	pb "github.com/brotherlogic/speculate/example/proto/kv/v1"
)

// Stage: Core
// Scenario: Core-1 Basic Put Overwrite
func TestPut_Probe(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	pb.RegisterKVServer(s, server.New())
	go s.Serve(lis)
	t.Cleanup(func() { s.Stop() })

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	client := pb.NewKVClient(conn)
	_, err = client.Put(context.Background(), &pb.PutRequest{Key: "k", Value: []byte("v")})
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
}
`

	syn := NewSynthesizer(&evaluator.MockLLMClient{})
	targetDir := filepath.Join("..", "..", "example", "tests")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := syn.MutationProbe(ctx, testCode, targetDir)
	if err != nil {
		t.Fatalf("MutationProbe returned system error: %v", err)
	}

	if !result.IsRed {
		t.Errorf("expected probe to be RED (test failure against skeleton server), but passed! Output:\n%s", result.Output)
	}

	if !strings.Contains(result.Output, "method Put not implemented") && !strings.Contains(result.Output, "Unimplemented") {
		t.Errorf("expected output to mention Unimplemented, got:\n%s", result.Output)
	}
}

func TestSynthesizer_LiveOllamaIntegration(t *testing.T) {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get("http://192.168.68.112:11434/api/tags")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skip("local Ollama endpoint (http://192.168.68.112:11434) not reachable; skipping live test")
	}
	defer resp.Body.Close()

	syn := NewSynthesizer(evaluator.NewOllamaClient("", ""))
	stage := &parser.Stage{Name: "Core"}
	req := &parser.Requirement{
		ID:          "Core-1",
		TargetRPC:   "Put",
		Description: "Put(key, value): Stores a key and associated byte payload. Overwrites existing values if the key is already present.",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	card, err := syn.GenerateScenarioCard(ctx, stage, req, "service KV { rpc Put(PutRequest) returns (PutResponse); }")
	if err != nil {
		t.Fatalf("Live GenerateScenarioCard failed: %v", err)
	}

	if card.ID != "Core-1" {
		t.Errorf("expected ID 'Core-1', got %q", card.ID)
	}
	t.Logf("Generated Live Scenario Card:\n%s", card.FormatMarkdown())
}
