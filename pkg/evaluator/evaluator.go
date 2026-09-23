package evaluator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/brotherlogic/speculate/pkg/parser"
)

var jsonCodeFenceRegex = regexp.MustCompile("(?s)```(?:json)?\\s*(.+?)\\s*```")

// RequirementStatus details whether a specific requirement is covered by a test scenario.
type RequirementStatus struct {
	Requirement   *parser.Requirement `json:"requirement"`
	Covered       bool                `json:"covered"`
	CoveredByTest string              `json:"covered_by_test,omitempty"`
	Reasoning     string              `json:"reasoning,omitempty"`
}

// StageCoverage summarizes the alignment status of an individual stage.
type StageCoverage struct {
	Stage               *parser.Stage `json:"stage"`
	TotalRequirements   int           `json:"total_requirements"`
	CoveredRequirements int           `json:"covered_requirements"`
	Fraction            float64       `json:"fraction"`
	Percentage          int           `json:"percentage"`
	IsFrontier          bool          `json:"is_frontier"`
	IsCompleted         bool          `json:"is_completed"`
}

// EvaluationResult represents the full spec alignment evaluation.
type EvaluationResult struct {
	SpecTitle           string                        `json:"spec_title"`
	TotalRequirements   int                           `json:"total_requirements"`
	CoveredRequirements int                           `json:"covered_requirements"`
	Fraction            float64                       `json:"fraction"`
	Percentage          int                           `json:"percentage"`
	BadgeURL            string                        `json:"badge_url"`
	BadgeMarkdown       string                        `json:"badge_markdown"`
	ActiveFrontierStage *parser.Stage                 `json:"active_frontier_stage,omitempty"`
	NextRequirement     *parser.Requirement           `json:"next_requirement,omitempty"`
	RequirementStatuses map[string]*RequirementStatus `json:"requirement_statuses"`
	StageCoverages      []*StageCoverage              `json:"stage_coverages"`
}

// Evaluator compares specifications against discovered test suites using an LLM.
type Evaluator struct {
	client LLMClient
}

// NewEvaluator creates a new Evaluator with the provided LLM client.
func NewEvaluator(client LLMClient) *Evaluator {
	if client == nil {
		client = NewOllamaClient("", "")
	}
	return &Evaluator{
		client: client,
	}
}

// Evaluate evaluates alignment between a specification and a test suite.
func (e *Evaluator) Evaluate(ctx context.Context, spec *parser.Specification, suite *parser.TestSuite) (*EvaluationResult, error) {
	if spec == nil {
		return nil, fmt.Errorf("specification cannot be nil")
	}

	result := &EvaluationResult{
		SpecTitle:           spec.Title,
		TotalRequirements:   spec.TotalRequirements(),
		RequirementStatuses: make(map[string]*RequirementStatus),
		StageCoverages:      []*StageCoverage{},
	}

	// Initialize all requirements as unexercised
	for _, stage := range spec.Stages {
		for _, req := range stage.Requirements {
			result.RequirementStatuses[req.ID] = &RequirementStatus{
				Requirement: req,
				Covered:     false,
			}
		}
	}

	// If there are 0 test scenarios, evaluation is deterministically 0% without needing an LLM call.
	if suite == nil || len(suite.Scenarios) == 0 {
		e.populateMetrics(spec, result)
		return result, nil
	}

	// Ask the LLM to map existing test scenarios against the specification requirements
	llmCoverage, err := e.queryLLMCoverage(ctx, spec, suite)
	if err != nil {
		return nil, fmt.Errorf("evaluating coverage with LLM: %w", err)
	}

	for _, cov := range llmCoverage {
		if status, exists := result.RequirementStatuses[cov.ID]; exists {
			status.Covered = cov.Covered
			status.CoveredByTest = cov.Test
			status.Reasoning = cov.Reasoning
		}
	}

	e.populateMetrics(spec, result)
	return result, nil
}

type llmReqItem struct {
	ID        string `json:"id"`
	Stage     string `json:"stage"`
	TargetRPC string `json:"target_rpc,omitempty"`
	Desc      string `json:"description"`
}

type llmTestItem struct {
	Name  string `json:"name"`
	Stage string `json:"stage,omitempty"`
	Doc   string `json:"doc,omitempty"`
}

type llmCoverageItem struct {
	ID        string `json:"id"`
	Covered   bool   `json:"covered"`
	Test      string `json:"test,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

type llmCoverageResponse struct {
	Coverage []llmCoverageItem `json:"coverage"`
}

func (e *Evaluator) queryLLMCoverage(ctx context.Context, spec *parser.Specification, suite *parser.TestSuite) ([]llmCoverageItem, error) {
	var reqItems []llmReqItem
	for _, stage := range spec.Stages {
		for _, req := range stage.Requirements {
			reqItems = append(reqItems, llmReqItem{
				ID:        req.ID,
				Stage:     stage.Name,
				TargetRPC: req.TargetRPC,
				Desc:      req.Description,
			})
		}
	}

	var testItems []llmTestItem
	for _, sc := range suite.Scenarios {
		testItems = append(testItems, llmTestItem{
			Name:  sc.Name,
			Stage: sc.TargetStage,
			Doc:   sc.Doc,
		})
	}

	reqsJSON, _ := json.MarshalIndent(reqItems, "", "  ")
	testsJSON, _ := json.MarshalIndent(testItems, "", "  ")

	systemPrompt := `You are an automated software specification and testing alignment evaluator.
Your job is to determine which specification requirements are currently covered by the existing test suite.
A requirement is considered covered if an existing test scenario explicitly asserts its behavioral contract.
Return ONLY valid JSON with no conversational prefix or suffix.`

	userPrompt := fmt.Sprintf(`Given the following specification requirements:
%s

And the following existing test scenarios:
%s

Determine whether each requirement is covered by at least one test scenario.
Respond strictly in JSON format matching this schema:
{
  "coverage": [
    {
      "id": "Stage-1",
      "covered": true,
      "test": "TestFunction_Name",
      "reasoning": "Brief explanation of how the test verifies this requirement"
    }
  ]
}`, string(reqsJSON), string(testsJSON))

	rawResp, err := e.client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, err
	}

	cleanJSON := extractJSON(rawResp)
	var resp llmCoverageResponse
	if err := json.Unmarshal([]byte(cleanJSON), &resp); err != nil {
		return nil, fmt.Errorf("parsing LLM JSON response (%s): %w", cleanJSON, err)
	}

	return resp.Coverage, nil
}

func (e *Evaluator) populateMetrics(spec *parser.Specification, result *EvaluationResult) {
	coveredCount := 0
	stageCompletion := make(map[string]bool)

	for _, stage := range spec.Stages {
		stageTotal := len(stage.Requirements)
		stageCovered := 0

		for _, req := range stage.Requirements {
			if status, ok := result.RequirementStatuses[req.ID]; ok && status.Covered {
				stageCovered++
				coveredCount++
			}
		}

		fraction := 0.0
		percentage := 0
		if stageTotal > 0 {
			fraction = float64(stageCovered) / float64(stageTotal)
			percentage = int(fraction * 100)
		}

		isCompleted := (stageTotal > 0 && stageCovered == stageTotal)
		stageCompletion[stage.Name] = isCompleted

		result.StageCoverages = append(result.StageCoverages, &StageCoverage{
			Stage:               stage,
			TotalRequirements:   stageTotal,
			CoveredRequirements: stageCovered,
			Fraction:            fraction,
			Percentage:          percentage,
			IsCompleted:         isCompleted,
		})
	}

	result.CoveredRequirements = coveredCount
	if result.TotalRequirements > 0 {
		result.Fraction = float64(result.CoveredRequirements) / float64(result.TotalRequirements)
		result.Percentage = int(result.Fraction * 100)
	}

	result.BadgeURL, result.BadgeMarkdown = FormatBadge(result.Percentage)

	// Determine Active Frontier Stage and Next Requirement
	for _, sc := range result.StageCoverages {
		if sc.IsCompleted {
			continue
		}

		// Check if dependencies are satisfied
		depsSatisfied := true
		for _, dep := range sc.Stage.DependsOn {
			if completed, exists := stageCompletion[dep]; !exists || !completed {
				depsSatisfied = false
				break
			}
		}

		if depsSatisfied {
			sc.IsFrontier = true
			result.ActiveFrontierStage = sc.Stage

			// Find first unexercised requirement in this frontier stage
			for _, req := range sc.Stage.Requirements {
				if status, ok := result.RequirementStatuses[req.ID]; ok && !status.Covered {
					result.NextRequirement = req
					break
				}
			}
			break
		}
	}
}

// FormatBadge returns the shields.io badge URL and markdown for the given alignment percentage.
func FormatBadge(percentage int) (badgeURL string, badgeMarkdown string) {
	color := "red"
	switch {
	case percentage == 100:
		color = "brightgreen"
	case percentage >= 75:
		color = "green"
	case percentage >= 50:
		color = "yellow"
	case percentage > 0:
		color = "orange"
	}

	badgeURL = fmt.Sprintf("https://img.shields.io/badge/Spec%%20Alignment-%d%%25-%s", percentage, color)
	badgeMarkdown = fmt.Sprintf("![Spec Alignment](%s)", badgeURL)
	return badgeURL, badgeMarkdown
}

// extractJSON handles cleaning code fences (```json ... ```) from LLM output.
func extractJSON(input string) string {
	input = strings.TrimSpace(input)
	if matches := jsonCodeFenceRegex.FindStringSubmatch(input); matches != nil {
		return strings.TrimSpace(matches[1])
	}
	return input
}
