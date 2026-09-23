package parser

// Requirement represents an atomic behavioral rule within a specification Stage.
type Requirement struct {
	ID          string   `json:"id"`
	RawText     string   `json:"raw_text"`
	TargetRPC   string   `json:"target_rpc,omitempty"`
	Description string   `json:"description"`
	LineNumber  int      `json:"line_number"`
	Tags        []string `json:"tags,omitempty"`
}

// Stage represents a cohesive milestone/frontier in the specification.
type Stage struct {
	Name         string         `json:"name"`
	RawHeader    string         `json:"raw_header"`
	Description  string         `json:"description"`
	DependsOn    []string       `json:"depends_on,omitempty"`
	Requirements []*Requirement `json:"requirements"`
	LineNumber   int            `json:"line_number"`
}

// Specification represents a parsed staged specification document.
type Specification struct {
	Title    string   `json:"title"`
	FilePath string   `json:"file_path,omitempty"`
	Stages   []*Stage `json:"stages"`
}

// TotalRequirements returns the total count of requirements across all stages.
func (s *Specification) TotalRequirements() int {
	count := 0
	for _, st := range s.Stages {
		count += len(st.Requirements)
	}
	return count
}

// StageByName retrieves a stage by its case-insensitive name.
func (s *Specification) StageByName(name string) *Stage {
	for _, st := range s.Stages {
		if st.Name == name {
			return st
		}
	}
	return nil
}

// TestScenario represents a test function or subtest found in tests/.
type TestScenario struct {
	Name              string `json:"name"`
	Package           string `json:"package"`
	FilePath          string `json:"file_path"`
	LineNumber        int    `json:"line_number"`
	Doc               string `json:"doc,omitempty"`
	TargetStage       string `json:"target_stage,omitempty"`
	TargetRequirement string `json:"target_requirement,omitempty"`
}

// TestFile represents a single parsed test file.
type TestFile struct {
	FilePath  string          `json:"file_path"`
	Package   string          `json:"package"`
	Scenarios []*TestScenario `json:"scenarios"`
}

// TestSuite represents the collection of parsed test files and scenarios in a directory.
type TestSuite struct {
	Directory string          `json:"directory"`
	Files     []*TestFile     `json:"files"`
	Scenarios []*TestScenario `json:"scenarios"`
}
