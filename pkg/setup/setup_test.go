package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGitRepoFromURL(t *testing.T) {
	tests := []struct {
		url      string
		expected string
		wantErr  bool
	}{
		{
			url:      "git@github.com:brotherlogic/speculate-kv.git",
			expected: "brotherlogic/speculate-kv",
		},
		{
			url:      "git@github.com:brotherlogic/speculate-kv",
			expected: "brotherlogic/speculate-kv",
		},
		{
			url:      "https://github.com/brotherlogic/speculate-kv.git",
			expected: "brotherlogic/speculate-kv",
		},
		{
			url:      "https://github.com/brotherlogic/speculate-kv",
			expected: "brotherlogic/speculate-kv",
		},
		{
			url:      "ssh://git@github.com/brotherlogic/speculate-kv.git",
			expected: "brotherlogic/speculate-kv",
		},
		{
			url:     "invalid-url-without-github",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, err := ParseGitRepoFromURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for URL %q, got nil", tt.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for URL %q: %v", tt.url, err)
			}
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestSetupDirectories(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &Config{RootDir: tempDir}

	if err := SetupDirectories(cfg); err != nil {
		t.Fatalf("SetupDirectories failed: %v", err)
	}

	for _, d := range []string{"specs", "proto", "tests", "internal", filepath.Join(".github", "workflows")} {
		p := filepath.Join(tempDir, d)
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("expected directory %s to exist, err: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be directory", d)
		}
	}

	// Verify .gitkeep was created in empty dirs
	for _, d := range []string{"specs", "proto", "tests", "internal"} {
		gitkeep := filepath.Join(tempDir, d, ".gitkeep")
		if _, err := os.Stat(gitkeep); err != nil {
			t.Errorf("expected %s to exist in empty dir %s", gitkeep, d)
		}
	}
}

func TestSetupTemplateFiles_FreshAndOverwrite(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &Config{
		RootDir: tempDir,
		Repo:    "brotherlogic/sample-repo",
		Owner:   "brotherlogic",
		Force:   false,
	}

	// 1. First run writes all files
	written, err := SetupTemplateFiles(cfg)
	if err != nil {
		t.Fatalf("SetupTemplateFiles failed: %v", err)
	}
	if len(written) != 4 {
		t.Fatalf("expected 4 written files, got %d: %v", len(written), written)
	}

	// Verify CODEOWNERS has @brotherlogic
	codeownersBytes, err := os.ReadFile(filepath.Join(tempDir, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatalf("reading CODEOWNERS failed: %v", err)
	}
	if !strings.Contains(string(codeownersBytes), "@brotherlogic") {
		t.Errorf("expected CODEOWNERS to contain @brotherlogic, got:\n%s", string(codeownersBytes))
	}

	// Verify review-gate.yml has brotherlogic interpolated
	reviewGateBytes, err := os.ReadFile(filepath.Join(tempDir, ".github", "workflows", "review-gate.yml"))
	if err != nil {
		t.Fatalf("reading review-gate.yml failed: %v", err)
	}
	if !strings.Contains(string(reviewGateBytes), `PR_AUTHOR" = "brotherlogic"`) {
		t.Errorf("expected review-gate.yml to contain brotherlogic author check, got:\n%s", string(reviewGateBytes))
	}

	// 2. Second run without changes writes 0 files
	writtenSecond, err := SetupTemplateFiles(cfg)
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if len(writtenSecond) != 0 {
		t.Fatalf("expected 0 written files on identical second run, got %d: %v", len(writtenSecond), writtenSecond)
	}

	// 3. Modify one file and verify PromptFunc is invoked
	testYml := filepath.Join(tempDir, ".github", "workflows", "tests.yml")
	if err := os.WriteFile(testYml, []byte("custom content"), 0644); err != nil {
		t.Fatalf("modifying tests.yml: %v", err)
	}

	promptCalled := false
	cfg.PromptFunc = func(path string) bool {
		if path == filepath.Join(".github", "workflows", "tests.yml") {
			promptCalled = true
			return false // reject overwrite
		}
		return false
	}

	writtenThird, err := SetupTemplateFiles(cfg)
	if err != nil {
		t.Fatalf("third run failed: %v", err)
	}
	if !promptCalled {
		t.Errorf("expected PromptFunc to be called for modified tests.yml")
	}
	if len(writtenThird) != 0 {
		t.Errorf("expected 0 written files when overwrite declined, got %v", writtenThird)
	}

	// Content should still be custom content
	contentAfter, _ := os.ReadFile(testYml)
	if string(contentAfter) != "custom content" {
		t.Errorf("expected custom content to be preserved, got %s", string(contentAfter))
	}

	// 4. Overwrite when PromptFunc returns true
	cfg.PromptFunc = func(path string) bool {
		return true // allow overwrite
	}
	writtenFourth, err := SetupTemplateFiles(cfg)
	if err != nil {
		t.Fatalf("fourth run failed: %v", err)
	}
	if len(writtenFourth) != 1 {
		t.Errorf("expected 1 file overwritten, got %v", writtenFourth)
	}
}
