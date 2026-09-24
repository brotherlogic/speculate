package setup

import (
	"context"
	"errors"
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

func TestRun_OrderOfOperations_PushBeforeRulesets(t *testing.T) {
	tempDir := t.TempDir()
	var executionOrder []string

	cfg := &Config{
		RootDir:      tempDir,
		Repo:         "brotherlogic/test-repo",
		Collaborator: "brotherlogic-automation",
		Force:        true,
		CheckPermissionsFunc: func(ctx context.Context, cfg *Config) (RepoPermissions, error) {
			return RepoPermissions{Admin: true, Push: true}, nil
		},
		ConfigureRepoFunc: func(ctx context.Context, cfg *Config) error {
			executionOrder = append(executionOrder, "repo_settings")
			return nil
		},
		ConfigureCollabFunc: func(ctx context.Context, cfg *Config) error {
			executionOrder = append(executionOrder, "collaborator")
			return nil
		},
		CommitAndPushFunc: func(ctx context.Context, cfg *Config) error {
			executionOrder = append(executionOrder, "git_push")
			return nil
		},
		ConfigureRulesetsFunc: func(ctx context.Context, cfg *Config) error {
			executionOrder = append(executionOrder, "rulesets")
			return nil
		},
	}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	pushIdx := -1
	rulesetIdx := -1
	for idx, op := range executionOrder {
		if op == "git_push" {
			pushIdx = idx
		}
		if op == "rulesets" {
			rulesetIdx = idx
		}
	}

	if pushIdx == -1 {
		t.Fatalf("git_push was not executed in Run")
	}
	if rulesetIdx == -1 {
		t.Fatalf("rulesets was not executed in Run")
	}

	// git_push MUST occur before rulesets, otherwise on first push the ruleset
	// blocks the push with GH013 repository rule violations.
	if pushIdx > rulesetIdx {
		t.Fatalf("git_push (index %d) occurred after rulesets (index %d); git_push must occur before rulesets to avoid GH013 push rejection on first push. Order: %v", pushIdx, rulesetIdx, executionOrder)
	}
}

func TestRun_ExistingActiveRuleset_PausedDuringPush(t *testing.T) {
	tempDir := t.TempDir()
	var events []string

	cfg := &Config{
		RootDir:      tempDir,
		Repo:         "brotherlogic/test-repo",
		Collaborator: "brotherlogic-automation",
		Force:        true,
		CheckPermissionsFunc: func(ctx context.Context, cfg *Config) (RepoPermissions, error) {
			return RepoPermissions{Admin: true, Push: true}, nil
		},
		ConfigureRepoFunc: func(ctx context.Context, cfg *Config) error {
			return nil
		},
		ConfigureCollabFunc: func(ctx context.Context, cfg *Config) error {
			return nil
		},
		CheckExistingRulesetFn: func(ctx context.Context, cfg *Config, name string) (int64, string, error) {
			return 12345, "active", nil
		},
		SetRulesetEnforceFn: func(ctx context.Context, cfg *Config, rulesetID int64, enforcement string) error {
			events = append(events, "enforce_"+enforcement)
			return nil
		},
		CommitAndPushFunc: func(ctx context.Context, cfg *Config) error {
			events = append(events, "git_push")
			return nil
		},
		ConfigureRulesetsFunc: func(ctx context.Context, cfg *Config) error {
			events = append(events, "ruleset_configured")
			return nil
		},
	}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	expectedOrder := []string{"enforce_disabled", "git_push", "ruleset_configured"}
	if len(events) != len(expectedOrder) {
		t.Fatalf("expected events %v, got %v", expectedOrder, events)
	}
	for i, exp := range expectedOrder {
		if events[i] != exp {
			t.Errorf("expected event[%d] to be %s, got %s (full events: %v)", i, exp, events[i], events)
		}
	}
}

func TestRun_ExistingActiveRuleset_RestoredOnPushError(t *testing.T) {
	tempDir := t.TempDir()
	var events []string

	cfg := &Config{
		RootDir:      tempDir,
		Repo:         "brotherlogic/test-repo",
		Collaborator: "brotherlogic-automation",
		Force:        true,
		CheckPermissionsFunc: func(ctx context.Context, cfg *Config) (RepoPermissions, error) {
			return RepoPermissions{Admin: true, Push: true}, nil
		},
		ConfigureRepoFunc: func(ctx context.Context, cfg *Config) error {
			return nil
		},
		ConfigureCollabFunc: func(ctx context.Context, cfg *Config) error {
			return nil
		},
		CheckExistingRulesetFn: func(ctx context.Context, cfg *Config, name string) (int64, string, error) {
			return 12345, "active", nil
		},
		SetRulesetEnforceFn: func(ctx context.Context, cfg *Config, rulesetID int64, enforcement string) error {
			events = append(events, "enforce_"+enforcement)
			return nil
		},
		CommitAndPushFunc: func(ctx context.Context, cfg *Config) error {
			events = append(events, "git_push")
			return errors.New("network failure during push")
		},
		ConfigureRulesetsFunc: func(ctx context.Context, cfg *Config) error {
			events = append(events, "ruleset_configured")
			return nil
		},
	}

	err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected Run to fail on push error, got nil")
	}

	// Should disable before push, attempt push, then restore to active via defer
	expectedOrder := []string{"enforce_disabled", "git_push", "enforce_active"}
	if len(events) != len(expectedOrder) {
		t.Fatalf("expected events %v, got %v", expectedOrder, events)
	}
	for i, exp := range expectedOrder {
		if events[i] != exp {
			t.Errorf("expected event[%d] to be %s, got %s (full events: %v)", i, exp, events[i], events)
		}
	}
}


