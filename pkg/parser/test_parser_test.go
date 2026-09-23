package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTestDir_EmptyDirectory(t *testing.T) {
	// example/tests currently has 0 test files (only .gitkeep)
	exampleTestsDir := filepath.Join("..", "..", "example", "tests")
	suite, err := ParseTestDir(exampleTestsDir)
	if err != nil {
		t.Fatalf("ParseTestDir(%q) failed: %v", exampleTestsDir, err)
	}

	if len(suite.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(suite.Files))
	}
	if len(suite.Scenarios) != 0 {
		t.Errorf("expected 0 scenarios in empty test dir, got %d", len(suite.Scenarios))
	}
}

func TestParseTestDir_NonExistentDirectory(t *testing.T) {
	suite, err := ParseTestDir("path/does/not/exist")
	if err != nil {
		t.Fatalf("expected no error for non-existent directory, got %v", err)
	}
	if len(suite.Scenarios) != 0 {
		t.Errorf("expected 0 scenarios, got %d", len(suite.Scenarios))
	}
}

func TestParseTestFile_ScenariosAndSubtests(t *testing.T) {
	tempDir := t.TempDir()
	sampleTestFile := filepath.Join(tempDir, "sample_test.go")

	content := `package sample_test

import "testing"

// Stage: Core
// Scenario: Core-1 Put overwrites existing values
func TestPut_Overwrite(t *testing.T) {
	// test logic
}

// Stage: Core
// Scenario: Core-2 Get retrieves stored value
func TestGet_TableDriven(t *testing.T) {
	t.Run("ExistingKey", func(t *testing.T) {
		// subtest 1
	})

	t.Run("NotFoundKey", func(t *testing.T) {
		// subtest 2
	})
}

func helperFunc() {
	// Should be ignored
}
`

	if err := os.WriteFile(sampleTestFile, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp test file: %v", err)
	}

	testFile, err := ParseTestFile(sampleTestFile)
	if err != nil {
		t.Fatalf("ParseTestFile failed: %v", err)
	}

	if testFile.Package != "sample_test" {
		t.Errorf("expected package 'sample_test', got %q", testFile.Package)
	}

	// Should extract 1 standalone scenario + 2 subtests = 3 scenarios total
	if len(testFile.Scenarios) != 3 {
		t.Fatalf("expected 3 scenarios, got %d", len(testFile.Scenarios))
	}

	s1 := testFile.Scenarios[0]
	if s1.Name != "TestPut_Overwrite" {
		t.Errorf("expected scenario name 'TestPut_Overwrite', got %q", s1.Name)
	}
	if s1.TargetStage != "Core" {
		t.Errorf("expected target stage 'Core', got %q", s1.TargetStage)
	}
	if s1.TargetRequirement != "Core-1 Put overwrites existing values" {
		t.Errorf("expected target requirement 'Core-1 Put overwrites existing values', got %q", s1.TargetRequirement)
	}

	s2 := testFile.Scenarios[1]
	if s2.Name != "TestGet_TableDriven/ExistingKey" {
		t.Errorf("expected scenario name 'TestGet_TableDriven/ExistingKey', got %q", s2.Name)
	}
	if s2.TargetStage != "Core" {
		t.Errorf("expected target stage 'Core', got %q", s2.TargetStage)
	}

	s3 := testFile.Scenarios[2]
	if s3.Name != "TestGet_TableDriven/NotFoundKey" {
		t.Errorf("expected scenario name 'TestGet_TableDriven/NotFoundKey', got %q", s3.Name)
	}
	if s3.TargetStage != "Core" {
		t.Errorf("expected target stage 'Core', got %q", s3.TargetStage)
	}

	// Verify ParseTestDir picks up the file
	suite, err := ParseTestDir(tempDir)
	if err != nil {
		t.Fatalf("ParseTestDir failed: %v", err)
	}
	if len(suite.Files) != 1 {
		t.Errorf("expected 1 file in suite, got %d", len(suite.Files))
	}
	if len(suite.Scenarios) != 3 {
		t.Errorf("expected 3 scenarios in suite, got %d", len(suite.Scenarios))
	}
}
