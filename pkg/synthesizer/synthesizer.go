package synthesizer

import (
	"context"
	"encoding/json"
	"fmt"
	goParser "go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/brotherlogic/speculate/pkg/evaluator"
	"github.com/brotherlogic/speculate/pkg/parser"
)

var (
	jsonCodeFenceRegex = regexp.MustCompile("(?s)```(?:json)?\\s*(.+?)\\s*```")
	goCodeFenceRegex   = regexp.MustCompile("(?s)```(?:go)?\\s*(.+?)\\s*```")
)

// Synthesizer generates Scenario Cards and executable integration tests using an LLM.
type Synthesizer struct {
	client evaluator.LLMClient
}

// NewSynthesizer creates a new Synthesizer with the given LLM client.
func NewSynthesizer(client evaluator.LLMClient) *Synthesizer {
	if client == nil {
		client = evaluator.NewOllamaClient("", "")
	}
	return &Synthesizer{
		client: client,
	}
}

// GenerateScenarioCard produces a structured GIVEN / WHEN / THEN card for an unexercised requirement.
func (s *Synthesizer) GenerateScenarioCard(ctx context.Context, stage *parser.Stage, req *parser.Requirement, protoContract string) (*ScenarioCard, error) {
	if stage == nil || req == nil {
		return nil, fmt.Errorf("stage and requirement cannot be nil")
	}

	systemPrompt := `You are an expert test scenario designer in a Spec-Driven Autonomous Verification system.
Your job is to formulate a structured GIVEN / WHEN / THEN Scenario Card for a specification requirement.
Always respond strictly with valid JSON only matching the schema.`

	userPrompt := fmt.Sprintf(`Stage Name: %s
Requirement ID: %s
Target RPC: %s
Requirement Text: %s
Protobuf Contract / IDL:
%s

Formulate a Scenario Card for this requirement.
Respond with JSON matching this exact schema:
{
  "id": "%s",
  "stage": "%s",
  "requirement": "%s",
  "target_rpc": "%s",
  "title": "Short descriptive scenario title",
  "given": "Preconditions and initial state",
  "when": "Action or RPC invocation performed",
  "then": "Expected outcome and assertions",
  "test_func_name": "Test%s_DescriptiveName"
}`, stage.Name, req.ID, req.TargetRPC, req.Description, protoContract,
		req.ID, stage.Name, req.Description, req.TargetRPC, req.TargetRPC)

	rawResp, err := s.client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("calling LLM for scenario card: %w", err)
	}

	cleanJSON := extractFencedContent(rawResp, jsonCodeFenceRegex)
	var card ScenarioCard
	if err := json.Unmarshal([]byte(cleanJSON), &card); err != nil {
		return nil, fmt.Errorf("parsing scenario card JSON (%s): %w", cleanJSON, err)
	}

	// Guarantee key fields are filled even if LLM modified them
	if card.ID == "" {
		card.ID = req.ID
	}
	if card.Stage == "" {
		card.Stage = stage.Name
	}
	if card.Requirement == "" {
		card.Requirement = req.Description
	}
	if card.TargetRPC == "" {
		card.TargetRPC = req.TargetRPC
	}
	if card.TestFuncName == "" {
		card.TestFuncName = fmt.Sprintf("Test%s_%s", req.TargetRPC, strings.ReplaceAll(card.ID, "-", "_"))
	}

	return &card, nil
}

// SynthesizeTest generates an executable Go integration test file using in-memory bufconn.
func (s *Synthesizer) SynthesizeTest(ctx context.Context, card *ScenarioCard, serverPackage, protoPackage string) (string, error) {
	if card == nil {
		return "", fmt.Errorf("scenario card cannot be nil")
	}

	systemPrompt := `You are an expert Go systems engineer writing hermetic integration tests.
Generate a complete, self-contained Go test file that tests the given scenario card against an in-memory gRPC server using google.golang.org/grpc/test/bufconn.
Include doc comments in the test function with // Stage: <Stage> and // Scenario: <ID> <Title>.
Output ONLY valid Go source code.`

	userPrompt := fmt.Sprintf(`Scenario Card:
%s

Server Package: %s
Proto Package: %s

Requirements for the generated Go test:
1. Package must be package tests.
2. Setup in-memory server using bufconn.Listen(1024*1024), register with server.New(), and connect client via grpc.NewClient or grpc.Dial.
3. Include test function named %s.
4. Include doc comments:
// Stage: %s
// Scenario: %s %s
5. Exercise the WHEN condition and assert the THEN condition.

Respond ONLY with the complete Go source code.`,
		card.FormatMarkdown(), serverPackage, protoPackage,
		card.TestFuncName, card.Stage, card.ID, card.Title)

	rawResp, err := s.client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return "", fmt.Errorf("calling LLM for test synthesis: %w", err)
	}

	cleanGo := extractFencedContent(rawResp, goCodeFenceRegex)

	// Validate syntax
	if err := ValidateGoCode(cleanGo); err != nil {
		return "", fmt.Errorf("synthesized test code has invalid Go syntax: %w\nCode:\n%s", err, cleanGo)
	}

	return cleanGo, nil
}

// ValidateGoCode checks that the Go source string parses cleanly without syntax errors.
func ValidateGoCode(code string) error {
	fset := token.NewFileSet()
	_, err := goParser.ParseFile(fset, "", code, goParser.AllErrors)
	return err
}

// ProbeResult details the execution outcome of a mutation probe.
type ProbeResult struct {
	Passed bool   `json:"passed"`
	IsRed  bool   `json:"is_red"`
	Output string `json:"output"`
}

// MutationProbe writes the test code to a temporary file in targetDir, runs `go test -v ./...`,
// verifies that the test properly fails (RED) against the un-implemented server, and cleans up.
func (s *Synthesizer) MutationProbe(ctx context.Context, testCode string, targetDir string) (*ProbeResult, error) {
	probeFileName := filepath.Join(targetDir, "mutation_probe_test.go")

	if err := os.WriteFile(probeFileName, []byte(testCode), 0644); err != nil {
		return nil, fmt.Errorf("writing probe test file: %w", err)
	}
	defer os.Remove(probeFileName)

	cmd := exec.CommandContext(ctx, "go", "test", "-v", "./...")
	cmd.Dir = targetDir

	outputBytes, err := cmd.CombinedOutput()
	outputStr := string(outputBytes)

	// In a mutation probe, exit error (non-zero exit) is expected (RED), indicating
	// the test properly fails against the un-implemented server.
	isRed := (err != nil)
	passed := (err == nil)

	return &ProbeResult{
		Passed: passed,
		IsRed:  isRed,
		Output: outputStr,
	}, nil
}

func extractFencedContent(input string, regex *regexp.Regexp) string {
	input = strings.TrimSpace(input)
	if matches := regex.FindStringSubmatch(input); matches != nil {
		return strings.TrimSpace(matches[1])
	}
	return input
}
