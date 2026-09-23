package evaluator

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/brotherlogic/speculate/pkg/parser"
)

func TestEvaluate_EmptyTestSuite_ZeroPercent(t *testing.T) {
	specPath := filepath.Join("..", "..", "testdata", "specs", "kv.md")
	spec, err := parser.ParseSpecFile(specPath)
	if err != nil {
		t.Fatalf("ParseSpecFile failed: %v", err)
	}

	eval := NewEvaluator(&MockLLMClient{})
	emptySuite := &parser.TestSuite{
		Directory: t.TempDir(),
		Scenarios: []*parser.TestScenario{},
	}

	res, err := eval.Evaluate(context.Background(), spec, emptySuite)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}

	if res.TotalRequirements != 3 {
		t.Errorf("expected 3 total requirements, got %d", res.TotalRequirements)
	}
	if res.CoveredRequirements != 0 {
		t.Errorf("expected 0 covered requirements, got %d", res.CoveredRequirements)
	}
	if res.Fraction != 0.0 {
		t.Errorf("expected 0.0 fraction, got %f", res.Fraction)
	}
	if res.Percentage != 0 {
		t.Errorf("expected 0 percentage, got %d", res.Percentage)
	}

	expectedBadge := "![Spec Alignment](https://img.shields.io/badge/Spec%20Alignment-0%25-red)"
	if res.BadgeMarkdown != expectedBadge {
		t.Errorf("expected badge %q, got %q", expectedBadge, res.BadgeMarkdown)
	}

	if res.ActiveFrontierStage == nil || res.ActiveFrontierStage.Name != "Core" {
		t.Errorf("expected active frontier stage 'Core', got %v", res.ActiveFrontierStage)
	}

	if res.NextRequirement == nil || res.NextRequirement.ID != "Core-1" {
		t.Errorf("expected next requirement 'Core-1', got %v", res.NextRequirement)
	}
}

func TestEvaluate_PartialCoverage_WithMockLLM(t *testing.T) {
	specContent := `# Service
## [Stage: Core] Basic Operations
- Put: stores payload.
- Get: retrieves payload.
- Delete: removes payload.
`
	spec, err := parser.ParseSpec(specContent)
	if err != nil {
		t.Fatalf("ParseSpec failed: %v", err)
	}

	mockLLM := &MockLLMClient{
		Response: `{"coverage": [
			{"id": "Core-1", "covered": true, "test": "TestPut_Success", "reasoning": "Explicitly verifies payload storage"},
			{"id": "Core-2", "covered": false},
			{"id": "Core-3", "covered": false}
		]}`,
	}

	suite := &parser.TestSuite{
		Scenarios: []*parser.TestScenario{
			{Name: "TestPut_Success"},
		},
	}

	eval := NewEvaluator(mockLLM)
	res, err := eval.Evaluate(context.Background(), spec, suite)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}

	if res.TotalRequirements != 3 {
		t.Errorf("expected 3 total, got %d", res.TotalRequirements)
	}
	if res.CoveredRequirements != 1 {
		t.Errorf("expected 1 covered, got %d", res.CoveredRequirements)
	}
	if res.Percentage != 33 {
		t.Errorf("expected 33%%, got %d%%", res.Percentage)
	}

	expectedBadge := "![Spec Alignment](https://img.shields.io/badge/Spec%20Alignment-33%25-orange)"
	if res.BadgeMarkdown != expectedBadge {
		t.Errorf("expected badge %q, got %q", expectedBadge, res.BadgeMarkdown)
	}

	if res.NextRequirement == nil || res.NextRequirement.ID != "Core-2" {
		t.Errorf("expected next requirement 'Core-2', got %v", res.NextRequirement)
	}
}

func TestEvaluate_MultiStageDependencyProgression(t *testing.T) {
	specContent := `# Service
## [Stage: Core]
- Put: stores payload.

## [Stage: Expiry] (Depends on: Core)
- TTL: auto-expires after duration.
`
	spec, err := parser.ParseSpec(specContent)
	if err != nil {
		t.Fatalf("ParseSpec failed: %v", err)
	}

	// 1. When Core is NOT covered, Core is the active frontier
	evalNotCovered := NewEvaluator(&MockLLMClient{
		Response: `{"coverage": [{"id": "Core-1", "covered": false}, {"id": "Expiry-1", "covered": false}]}`,
	})
	res1, err := evalNotCovered.Evaluate(context.Background(), spec, &parser.TestSuite{
		Scenarios: []*parser.TestScenario{{Name: "TestDummy"}},
	})
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if res1.ActiveFrontierStage.Name != "Core" {
		t.Errorf("expected active stage 'Core', got %q", res1.ActiveFrontierStage.Name)
	}
	if res1.NextRequirement.ID != "Core-1" {
		t.Errorf("expected next req 'Core-1', got %q", res1.NextRequirement.ID)
	}

	// 2. When Core IS 100% covered, frontier advances to Expiry
	evalCoreCovered := NewEvaluator(&MockLLMClient{
		Response: `{"coverage": [{"id": "Core-1", "covered": true, "test": "TestPut"}, {"id": "Expiry-1", "covered": false}]}`,
	})
	res2, err := evalCoreCovered.Evaluate(context.Background(), spec, &parser.TestSuite{
		Scenarios: []*parser.TestScenario{{Name: "TestPut"}},
	})
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if res2.ActiveFrontierStage.Name != "Expiry" {
		t.Errorf("expected active stage 'Expiry', got %q", res2.ActiveFrontierStage.Name)
	}
	if res2.NextRequirement.ID != "Expiry-1" {
		t.Errorf("expected next req 'Expiry-1', got %q", res2.NextRequirement.ID)
	}

	// 3. When both are covered: 100% alignment
	evalAllCovered := NewEvaluator(&MockLLMClient{
		Response: `{"coverage": [{"id": "Core-1", "covered": true, "test": "TestPut"}, {"id": "Expiry-1", "covered": true, "test": "TestTTL"}]}`,
	})
	res3, err := evalAllCovered.Evaluate(context.Background(), spec, &parser.TestSuite{
		Scenarios: []*parser.TestScenario{{Name: "TestPut"}, {Name: "TestTTL"}},
	})
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if res3.Percentage != 100 {
		t.Errorf("expected 100%%, got %d%%", res3.Percentage)
	}
	if res3.ActiveFrontierStage != nil {
		t.Errorf("expected nil active frontier when 100%% aligned, got %v", res3.ActiveFrontierStage)
	}
	if res3.NextRequirement != nil {
		t.Errorf("expected nil next requirement when 100%% aligned, got %v", res3.NextRequirement)
	}
}

func TestFormatBadge_Colors(t *testing.T) {
	tests := []struct {
		percentage    int
		expectedColor string
	}{
		{100, "brightgreen"},
		{85, "green"},
		{75, "green"},
		{60, "yellow"},
		{50, "yellow"},
		{25, "orange"},
		{1, "orange"},
		{0, "red"},
	}

	for _, tt := range tests {
		url, md := FormatBadge(tt.percentage)
		expectedURL := "https://img.shields.io/badge/Spec%20Alignment-" + itoa(tt.percentage) + "%25-" + tt.expectedColor
		if url != expectedURL {
			t.Errorf("for %d%% expected URL %q, got %q", tt.percentage, expectedURL, url)
		}
		if md != "![Spec Alignment]("+expectedURL+")" {
			t.Errorf("for %d%% expected markdown with %q, got %q", tt.percentage, expectedURL, md)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var res string
	for i > 0 {
		res = string(rune('0'+(i%10))) + res
		i /= 10
	}
	return res
}

func TestEvaluate_LiveOllamaIntegration(t *testing.T) {
	// Skip in CI or hermetic runs if Ollama is unreachable
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get("http://192.168.68.112:11434/api/tags")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skip("local Ollama endpoint (http://192.168.68.112:11434) not reachable; skipping live test")
	}
	defer resp.Body.Close()

	specContent := `# Test Spec
## [Stage: Core]
- Put(k, v): stores value
`
	spec, err := parser.ParseSpec(specContent)
	if err != nil {
		t.Fatalf("ParseSpec failed: %v", err)
	}

	eval := NewEvaluator(NewOllamaClient("", ""))
	suite := &parser.TestSuite{
		Scenarios: []*parser.TestScenario{
			{Name: "TestPut_StoresValue", TargetStage: "Core"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := eval.Evaluate(ctx, spec, suite)
	if err != nil {
		t.Fatalf("Live Evaluate failed: %v", err)
	}

	if res.TotalRequirements != 1 {
		t.Errorf("expected 1 total req, got %d", res.TotalRequirements)
	}
	t.Logf("Live Ollama evaluation result: %d%% (%s)", res.Percentage, res.BadgeMarkdown)
}
