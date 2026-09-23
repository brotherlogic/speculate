package synthesizer

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ScenarioCard represents a GIVEN / WHEN / THEN behavioral test specification.
type ScenarioCard struct {
	ID           string `json:"id"`
	Stage        string `json:"stage"`
	Requirement  string `json:"requirement"`
	TargetRPC    string `json:"target_rpc,omitempty"`
	Title        string `json:"title"`
	Given        string `json:"given"`
	When         string `json:"when"`
	Then         string `json:"then"`
	TestFuncName string `json:"test_func_name"`
}

type rawCard struct {
	ID           string          `json:"id"`
	Stage        string          `json:"stage"`
	Requirement  string          `json:"requirement"`
	TargetRPC    string          `json:"target_rpc,omitempty"`
	Title        string          `json:"title"`
	Given        json.RawMessage `json:"given"`
	When         json.RawMessage `json:"when"`
	Then         json.RawMessage `json:"then"`
	TestFuncName string          `json:"test_func_name"`
}

// UnmarshalJSON safely unmarshals ScenarioCard even if LLM returns complex objects for given/when/then.
func (c *ScenarioCard) UnmarshalJSON(data []byte) error {
	var raw rawCard
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.ID = raw.ID
	c.Stage = raw.Stage
	c.Requirement = raw.Requirement
	c.TargetRPC = raw.TargetRPC
	c.Title = raw.Title
	c.TestFuncName = raw.TestFuncName
	c.Given = parseFlexibleString(raw.Given)
	c.When = parseFlexibleString(raw.When)
	c.Then = parseFlexibleString(raw.Then)
	return nil
}

func parseFlexibleString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if outcome, ok := obj["expected_outcome"].(string); ok {
			return outcome
		}
		if desc, ok := obj["description"].(string); ok {
			return desc
		}
		if outcome, ok := obj["then"].(string); ok {
			return outcome
		}
		// Compact JSON representation of object
		b, err := json.Marshal(obj)
		if err == nil {
			return string(b)
		}
	}

	return string(raw)
}

// FormatMarkdown formats the scenario card into human-readable GitHub issue / PR documentation.
func (c *ScenarioCard) FormatMarkdown() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("### Scenario Card: [Stage: %s] %s\n\n", c.Stage, c.Title))
	b.WriteString(fmt.Sprintf("* **Scenario ID:** `%s`\n", c.ID))
	if c.TargetRPC != "" {
		b.WriteString(fmt.Sprintf("* **Target RPC:** `%s`\n", c.TargetRPC))
	}
	b.WriteString(fmt.Sprintf("* **Specification Requirement:** %s\n", c.Requirement))
	b.WriteString(fmt.Sprintf("* **GIVEN:** %s\n", c.Given))
	b.WriteString(fmt.Sprintf("* **WHEN:** %s\n", c.When))
	b.WriteString(fmt.Sprintf("* **THEN:** %s\n", c.Then))
	b.WriteString(fmt.Sprintf("* **Target Test Function:** `%s`\n", c.TestFuncName))
	return b.String()
}
