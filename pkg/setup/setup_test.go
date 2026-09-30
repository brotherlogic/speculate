package setup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRun_SuccessWithPRWorkflow(t *testing.T) {
	tempDir := t.TempDir()
	var executionOrder []string
	var branchCreated, prCreated, autoMergeEnabled, prPolled, branchSynced string

	cfg := &Config{
		RootDir:      tempDir,
		Repo:         "brotherlogic/test-repo",
		Collaborator: "brotherlogic-automation",
		Force:        true,
		CheckPermissionsFunc: func(ctx context.Context, cfg *Config) (RepoPermissions, error) {
			executionOrder = append(executionOrder, "check_permissions")
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
		CheckScaffoldingChangesFunc: func(ctx context.Context, cfg *Config) (bool, error) {
			executionOrder = append(executionOrder, "check_scaffolding")
			return true, nil
		},
		CommitAndPushBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			executionOrder = append(executionOrder, "commit_and_push_branch")
			branchCreated = branchName
			return nil
		},
		CreatePRFunc: func(ctx context.Context, cfg *Config, branchName, title, body string) (string, error) {
			executionOrder = append(executionOrder, "create_pr")
			prCreated = branchName
			return "https://github.com/brotherlogic/test-repo/pull/42", nil
		},
		EnableAutoMergeFunc: func(ctx context.Context, cfg *Config, prURL string) error {
			executionOrder = append(executionOrder, "enable_auto_merge")
			autoMergeEnabled = prURL
			return nil
		},
		PollPRStatusFunc: func(ctx context.Context, cfg *Config, prURL string, timeout time.Duration) error {
			executionOrder = append(executionOrder, "poll_pr_status")
			prPolled = prURL
			return nil
		},
		SyncMainBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			executionOrder = append(executionOrder, "sync_main")
			branchSynced = branchName
			return nil
		},
		ConfigureRulesetsFunc: func(ctx context.Context, cfg *Config) error {
			executionOrder = append(executionOrder, "configure_rulesets")
			return nil
		},
	}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	expectedOrder := []string{
		"check_permissions",
		"repo_settings",
		"collaborator",
		"check_scaffolding",
		"commit_and_push_branch",
		"create_pr",
		"enable_auto_merge",
		"poll_pr_status",
		"sync_main",
		"configure_rulesets",
	}

	if len(executionOrder) != len(expectedOrder) {
		t.Fatalf("expected execution order %v, got %v", expectedOrder, executionOrder)
	}
	for i, expected := range expectedOrder {
		if executionOrder[i] != expected {
			t.Errorf("expected step %d to be %q, got %q (order: %v)", i, expected, executionOrder[i], executionOrder)
		}
	}

	expectedBranch := "feature/speculate-scaffolding"
	if branchCreated != expectedBranch {
		t.Errorf("expected branchCreated %q, got %q", expectedBranch, branchCreated)
	}
	if prCreated != expectedBranch {
		t.Errorf("expected prCreated %q, got %q", expectedBranch, prCreated)
	}
	if branchSynced != expectedBranch {
		t.Errorf("expected branchSynced %q, got %q", expectedBranch, branchSynced)
	}

	expectedPRURL := "https://github.com/brotherlogic/test-repo/pull/42"
	if autoMergeEnabled != expectedPRURL {
		t.Errorf("expected autoMergeEnabled %q, got %q", expectedPRURL, autoMergeEnabled)
	}
	if prPolled != expectedPRURL {
		t.Errorf("expected prPolled %q, got %q", expectedPRURL, prPolled)
	}
}

func TestRun_IdempotentCleanTree_NoOp(t *testing.T) {
	tempDir := t.TempDir()
	branchCreated := false
	prCreated := false

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
		CheckScaffoldingChangesFunc: func(ctx context.Context, cfg *Config) (bool, error) {
			// Clean tree: no staged changes
			return false, nil
		},
		CommitAndPushBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			branchCreated = true
			return nil
		},
		CreatePRFunc: func(ctx context.Context, cfg *Config, branchName, title, body string) (string, error) {
			prCreated = true
			return "https://github.com/brotherlogic/test-repo/pull/42", nil
		},
		ConfigureRulesetsFunc: func(ctx context.Context, cfg *Config) error {
			return nil
		},
	}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if branchCreated {
		t.Errorf("expected no branch creation on clean tree")
	}
	if prCreated {
		t.Errorf("expected no PR creation on clean tree")
	}
}

func TestRun_AbortionOnCICheckFailure(t *testing.T) {
	tempDir := t.TempDir()
	synced := false
	rulesetConfigured := false

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
		CheckScaffoldingChangesFunc: func(ctx context.Context, cfg *Config) (bool, error) {
			return true, nil
		},
		CommitAndPushBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			return nil
		},
		CreatePRFunc: func(ctx context.Context, cfg *Config, branchName, title, body string) (string, error) {
			return "https://github.com/brotherlogic/test-repo/pull/42", nil
		},
		EnableAutoMergeFunc: func(ctx context.Context, cfg *Config, prURL string) error {
			return nil
		},
		PollPRStatusFunc: func(ctx context.Context, cfg *Config, prURL string, timeout time.Duration) error {
			return errors.New("status check \"review-gate\" failed with conclusion/state FAILURE")
		},
		SyncMainBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			synced = true
			return nil
		},
		ConfigureRulesetsFunc: func(ctx context.Context, cfg *Config) error {
			rulesetConfigured = true
			return nil
		},
	}

	err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected Run to fail on CI check failure, got nil")
	}
	if !strings.Contains(err.Error(), "review-gate") {
		t.Errorf("expected error to mention 'review-gate', got: %v", err)
	}
	if synced {
		t.Errorf("SyncMainBranch should not be called when PR polling fails")
	}
	if rulesetConfigured {
		t.Errorf("ConfigureRulesets should not be called when PR polling fails")
	}
}

func TestRun_PollingTimeout(t *testing.T) {
	tempDir := t.TempDir()
	synced := false
	rulesetConfigured := false

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
		CheckScaffoldingChangesFunc: func(ctx context.Context, cfg *Config) (bool, error) {
			return true, nil
		},
		CommitAndPushBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			return nil
		},
		CreatePRFunc: func(ctx context.Context, cfg *Config, branchName, title, body string) (string, error) {
			return "https://github.com/brotherlogic/test-repo/pull/42", nil
		},
		EnableAutoMergeFunc: func(ctx context.Context, cfg *Config, prURL string) error {
			return nil
		},
		PollPRStatusFunc: func(ctx context.Context, cfg *Config, prURL string, timeout time.Duration) error {
			return errors.New("polling PR timed out: context deadline exceeded")
		},
		SyncMainBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			synced = true
			return nil
		},
		ConfigureRulesetsFunc: func(ctx context.Context, cfg *Config) error {
			rulesetConfigured = true
			return nil
		},
	}

	err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected Run to fail on polling timeout, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected error to mention 'timed out', got: %v", err)
	}
	if synced {
		t.Errorf("SyncMainBranch should not be called when PR polling times out")
	}
	if rulesetConfigured {
		t.Errorf("ConfigureRulesets should not be called when PR polling times out")
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.name", "Test User"},
		{"git", "config", "user.email", "test@example.com"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed running %v in %s: %s: %v", args, dir, string(out), err)
		}
	}
}

func TestCheckScaffoldingChanges_CleanTree(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	cfg := &Config{
		RootDir: tempDir,
		Repo:    "brotherlogic/test-repo",
		Owner:   "brotherlogic",
	}

	if err := SetupDirectories(cfg); err != nil {
		t.Fatalf("SetupDirectories failed: %v", err)
	}
	if _, err := SetupTemplateFiles(cfg); err != nil {
		t.Fatalf("SetupTemplateFiles failed: %v", err)
	}

	// Commit initial scaffolding
	cmdAdd := exec.Command("git", "add", ".")
	cmdAdd.Dir = tempDir
	if out, err := cmdAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %s: %v", string(out), err)
	}
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	if out, err := cmdCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %s: %v", string(out), err)
	}

	hasChanges, err := CheckScaffoldingChanges(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CheckScaffoldingChanges returned unexpected error: %v", err)
	}
	if hasChanges {
		t.Errorf("expected CheckScaffoldingChanges to return false on clean tree, got true")
	}
}

func TestCheckScaffoldingChanges_WithChanges(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	cfg := &Config{
		RootDir: tempDir,
		Repo:    "brotherlogic/test-repo",
		Owner:   "brotherlogic",
	}

	if err := SetupDirectories(cfg); err != nil {
		t.Fatalf("SetupDirectories failed: %v", err)
	}
	if _, err := SetupTemplateFiles(cfg); err != nil {
		t.Fatalf("SetupTemplateFiles failed: %v", err)
	}

	// Commit initial scaffolding
	cmdAdd := exec.Command("git", "add", ".")
	cmdAdd.Dir = tempDir
	if out, err := cmdAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %s: %v", string(out), err)
	}
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	if out, err := cmdCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %s: %v", string(out), err)
	}

	// Add a new file to specs/
	newSpecPath := filepath.Join(tempDir, "specs", "feature.md")
	if err := os.WriteFile(newSpecPath, []byte("# Feature Spec"), 0644); err != nil {
		t.Fatalf("writing new spec file: %v", err)
	}

	hasChanges, err := CheckScaffoldingChanges(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CheckScaffoldingChanges returned unexpected error: %v", err)
	}
	if !hasChanges {
		t.Errorf("expected CheckScaffoldingChanges to return true when changes exist, got false")
	}
}

func TestCheckScaffoldingChanges_IgnoresNonScaffolding(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	cfg := &Config{
		RootDir: tempDir,
		Repo:    "brotherlogic/test-repo",
		Owner:   "brotherlogic",
	}

	if err := SetupDirectories(cfg); err != nil {
		t.Fatalf("SetupDirectories failed: %v", err)
	}
	if _, err := SetupTemplateFiles(cfg); err != nil {
		t.Fatalf("SetupTemplateFiles failed: %v", err)
	}

	// Commit initial scaffolding
	cmdAdd := exec.Command("git", "add", ".")
	cmdAdd.Dir = tempDir
	if out, err := cmdAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %s: %v", string(out), err)
	}
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	if out, err := cmdCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %s: %v", string(out), err)
	}

	// Create an untracked file outside the scaffolding directories
	unrelatedFile := filepath.Join(tempDir, "unrelated.txt")
	if err := os.WriteFile(unrelatedFile, []byte("unrelated data"), 0644); err != nil {
		t.Fatalf("writing unrelated file: %v", err)
	}

	hasChanges, err := CheckScaffoldingChanges(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CheckScaffoldingChanges returned unexpected error: %v", err)
	}
	if hasChanges {
		t.Errorf("expected CheckScaffoldingChanges to return false when only non-scaffolding changes exist, got true")
	}
}

func TestConfig_CheckScaffoldingChangesHook(t *testing.T) {
	hookCalled := false
	cfg := &Config{
		CheckScaffoldingChangesFunc: func(ctx context.Context, cfg *Config) (bool, error) {
			hookCalled = true
			return true, nil
		},
	}
	if cfg.CheckScaffoldingChangesFunc != nil {
		res, err := cfg.CheckScaffoldingChangesFunc(context.Background(), cfg)
		if err != nil || !res || !hookCalled {
			t.Fatalf("expected hook to be called successfully")
		}
	}
}

func TestGitCommitAndPushBranch_Success(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	// Set up a bare remote repository to act as origin
	bareRemote := t.TempDir()
	cmdInitBare := exec.Command("git", "init", "--bare")
	cmdInitBare.Dir = bareRemote
	if out, err := cmdInitBare.CombinedOutput(); err != nil {
		t.Fatalf("failed initializing bare remote: %s: %v", string(out), err)
	}

	cmdRemote := exec.Command("git", "remote", "add", "origin", bareRemote)
	cmdRemote.Dir = tempDir
	if out, err := cmdRemote.CombinedOutput(); err != nil {
		t.Fatalf("failed adding remote origin: %s: %v", string(out), err)
	}

	// Create an initial commit on main so branch creation from HEAD works
	initialFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed writing initial file: %v", err)
	}
	cmdAdd := exec.Command("git", "add", "README.md")
	cmdAdd.Dir = tempDir
	if out, err := cmdAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %s: %v", string(out), err)
	}
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	if out, err := cmdCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %s: %v", string(out), err)
	}

	// Now stage a new scaffolding file
	scaffoldFile := filepath.Join(tempDir, "specs.md")
	if err := os.WriteFile(scaffoldFile, []byte("# Spec"), 0644); err != nil {
		t.Fatalf("failed writing scaffold file: %v", err)
	}
	cmdAdd2 := exec.Command("git", "add", "specs.md")
	cmdAdd2.Dir = tempDir
	if out, err := cmdAdd2.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %s: %v", string(out), err)
	}

	cfg := &Config{
		RootDir: tempDir,
		Repo:    "brotherlogic/test-repo",
		Owner:   "brotherlogic",
	}

	branchName := "feature/speculate-scaffolding"
	if err := GitCommitAndPushBranch(context.Background(), cfg, branchName); err != nil {
		t.Fatalf("GitCommitAndPushBranch failed unexpectedly: %v", err)
	}

	// Verify current branch is branchName
	cmdBranch := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmdBranch.Dir = tempDir
	outBranch, err := cmdBranch.Output()
	if err != nil {
		t.Fatalf("failed getting current branch: %v", err)
	}
	if strings.TrimSpace(string(outBranch)) != branchName {
		t.Errorf("expected branch %q, got %q", branchName, strings.TrimSpace(string(outBranch)))
	}

	// Verify commit message
	cmdLog := exec.Command("git", "log", "-1", "--pretty=%s")
	cmdLog.Dir = tempDir
	outLog, err := cmdLog.Output()
	if err != nil {
		t.Fatalf("failed getting commit message: %v", err)
	}
	expectedMsg := "chore: initialize speculate project scaffolding"
	if strings.TrimSpace(string(outLog)) != expectedMsg {
		t.Errorf("expected commit message %q, got %q", expectedMsg, strings.TrimSpace(string(outLog)))
	}

	// Verify remote has the branch
	cmdRemoteRef := exec.Command("git", "--git-dir", bareRemote, "rev-parse", "--verify", "refs/heads/"+branchName)
	if out, err := cmdRemoteRef.CombinedOutput(); err != nil {
		t.Errorf("expected remote to have branch %s: %s: %v", branchName, string(out), err)
	}
}

func TestGitCommitAndPushBranch_InvalidBranch(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	cfg := &Config{RootDir: tempDir}
	// Invalid branch name should fail checkout -b
	err := GitCommitAndPushBranch(context.Background(), cfg, "invalid branch name with spaces and ..")
	if err == nil {
		t.Errorf("expected error for invalid branch name, got nil")
	}
}

func TestGitCommitAndPushBranch_CommitFailsWhenNothingStaged(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	// Make an initial commit
	initialFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed writing initial file: %v", err)
	}
	cmdAdd := exec.Command("git", "add", "README.md")
	cmdAdd.Dir = tempDir
	_ = cmdAdd.Run()
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	_ = cmdCommit.Run()

	cfg := &Config{RootDir: tempDir}
	// Nothing staged, so commit should fail
	err := GitCommitAndPushBranch(context.Background(), cfg, "feature/empty-branch")
	if err == nil {
		t.Errorf("expected error when nothing is staged to commit, got nil")
	}
}

func TestGitCommitAndPushBranch_PushFailsNoRemote(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	initialFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed writing initial file: %v", err)
	}
	cmdAdd := exec.Command("git", "add", "README.md")
	cmdAdd.Dir = tempDir
	_ = cmdAdd.Run()
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	_ = cmdCommit.Run()

	// Stage a new file
	newFile := filepath.Join(tempDir, "specs.md")
	if err := os.WriteFile(newFile, []byte("# Specs"), 0644); err != nil {
		t.Fatalf("failed writing file: %v", err)
	}
	cmdAdd2 := exec.Command("git", "add", "specs.md")
	cmdAdd2.Dir = tempDir
	_ = cmdAdd2.Run()

	cfg := &Config{RootDir: tempDir}
	// No origin remote configured, push must fail
	err := GitCommitAndPushBranch(context.Background(), cfg, "feature/no-remote")
	if err == nil {
		t.Errorf("expected error when push fails without remote, got nil")
	}
}

func TestConfig_CommitAndPushBranchHook(t *testing.T) {
	hookCalled := false
	var passedBranch string
	cfg := &Config{
		CommitAndPushBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			hookCalled = true
			passedBranch = branchName
			return nil
		},
	}
	if cfg.CommitAndPushBranchFunc != nil {
		err := cfg.CommitAndPushBranchFunc(context.Background(), cfg, "test-branch")
		if err != nil || !hookCalled || passedBranch != "test-branch" {
			t.Fatalf("expected hook to be called with test-branch")
		}
	}
}

func setupMockGH(t *testing.T, scriptBody string) (string, string) {
	tempBin := t.TempDir()
	logFile := filepath.Join(tempBin, "gh_args.log")
	ghPath := filepath.Join(tempBin, "gh")
	script := "#!/bin/sh\necho \"$@\" >> \"" + logFile + "\"\n" + scriptBody + "\n"
	if err := os.WriteFile(ghPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to create mock gh binary: %v", err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tempBin+string(os.PathListSeparator)+origPath)
	return ghPath, logFile
}

func TestCreatePullRequest_HookInvocation(t *testing.T) {
	hookCalled := false
	var gotBranch, gotTitle, gotBody string

	cfg := &Config{
		CreatePRFunc: func(ctx context.Context, cfg *Config, branchName, title, body string) (string, error) {
			hookCalled = true
			gotBranch = branchName
			gotTitle = title
			gotBody = body
			return "https://github.com/brotherlogic/test-repo/pull/99", nil
		},
	}

	url, err := CreatePullRequest(context.Background(), cfg, "feature/my-branch")
	if err != nil {
		t.Fatalf("unexpected error from CreatePullRequest: %v", err)
	}
	if !hookCalled {
		t.Errorf("expected CreatePRFunc hook to be called")
	}
	if gotBranch != "feature/my-branch" {
		t.Errorf("expected branch %q, got %q", "feature/my-branch", gotBranch)
	}
	if gotTitle != "chore: initialize speculate project scaffolding" {
		t.Errorf("expected title %q, got %q", "chore: initialize speculate project scaffolding", gotTitle)
	}
	if gotBody != "Initial speculate scaffolding (directory structure, GitHub workflows, and CODEOWNERS)." {
		t.Errorf("expected body to match default scaffolding description, got %q", gotBody)
	}
	if url != "https://github.com/brotherlogic/test-repo/pull/99" {
		t.Errorf("expected URL %q, got %q", "https://github.com/brotherlogic/test-repo/pull/99", url)
	}
}

func TestCreatePullRequest_EmptyBranch(t *testing.T) {
	cfg := &Config{}
	_, err := CreatePullRequest(context.Background(), cfg, "")
	if err == nil {
		t.Errorf("expected error when branchName is empty, got nil")
	}
}

func TestCreatePullRequest_CommandExecutionAndURLParsing(t *testing.T) {
	_, logFile := setupMockGH(t, "echo 'https://github.com/brotherlogic/test-repo/pull/42'")

	cfg := &Config{RootDir: t.TempDir()}
	url, err := CreatePullRequest(context.Background(), cfg, "feature/init-branch")
	if err != nil {
		t.Fatalf("unexpected error from CreatePullRequest: %v", err)
	}
	if url != "https://github.com/brotherlogic/test-repo/pull/42" {
		t.Errorf("expected url %q, got %q", "https://github.com/brotherlogic/test-repo/pull/42", url)
	}

	loggedArgsBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed reading mock gh args log: %v", err)
	}
	loggedArgs := strings.TrimSpace(string(loggedArgsBytes))
	expectedArgs := "pr create --base main --head feature/init-branch --title chore: initialize speculate project scaffolding --body Initial speculate scaffolding (directory structure, GitHub workflows, and CODEOWNERS)."
	if loggedArgs != expectedArgs {
		t.Errorf("expected gh command args:\n%q\ngot:\n%q", expectedArgs, loggedArgs)
	}
}

func TestCreatePullRequest_MultilineOutputURLParsing(t *testing.T) {
	multilineScript := "echo 'Creating pull request for feature/init-branch into main in brotherlogic/test-repo'\necho 'https://github.com/brotherlogic/test-repo/pull/88'\necho ''"
	setupMockGH(t, multilineScript)

	cfg := &Config{RootDir: t.TempDir()}
	url, err := CreatePullRequest(context.Background(), cfg, "feature/init-branch")
	if err != nil {
		t.Fatalf("unexpected error from CreatePullRequest: %v", err)
	}
	if url != "https://github.com/brotherlogic/test-repo/pull/88" {
		t.Errorf("expected url %q, got %q", "https://github.com/brotherlogic/test-repo/pull/88", url)
	}
}

func TestCreatePullRequest_CommandFailure(t *testing.T) {
	setupMockGH(t, "echo 'GraphQL error: A pull request already exists' >&2\nexit 1")

	cfg := &Config{RootDir: t.TempDir()}
	_, err := CreatePullRequest(context.Background(), cfg, "feature/init-branch")
	if err == nil {
		t.Fatalf("expected error when gh pr create fails, got nil")
	}
	if !strings.Contains(err.Error(), "gh pr create failed") {
		t.Errorf("expected error message to contain 'gh pr create failed', got %v", err)
	}
}

func TestEnableAutoMerge_HookInvocation(t *testing.T) {
	hookCalled := false
	var gotURL string

	cfg := &Config{
		EnableAutoMergeFunc: func(ctx context.Context, cfg *Config, prURL string) error {
			hookCalled = true
			gotURL = prURL
			return nil
		},
	}

	err := EnableAutoMerge(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42")
	if err != nil {
		t.Fatalf("unexpected error from EnableAutoMerge: %v", err)
	}
	if !hookCalled {
		t.Errorf("expected EnableAutoMergeFunc hook to be called")
	}
	if gotURL != "https://github.com/brotherlogic/test-repo/pull/42" {
		t.Errorf("expected URL %q, got %q", "https://github.com/brotherlogic/test-repo/pull/42", gotURL)
	}
}

func TestEnableAutoMerge_EmptyURL(t *testing.T) {
	cfg := &Config{}
	err := EnableAutoMerge(context.Background(), cfg, "")
	if err == nil {
		t.Errorf("expected error when prURL is empty, got nil")
	}
}

func TestEnableAutoMerge_CommandExecution(t *testing.T) {
	_, logFile := setupMockGH(t, "exit 0")

	cfg := &Config{RootDir: t.TempDir()}
	err := EnableAutoMerge(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42")
	if err != nil {
		t.Fatalf("unexpected error from EnableAutoMerge: %v", err)
	}

	loggedArgsBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed reading mock gh args log: %v", err)
	}
	loggedArgs := strings.TrimSpace(string(loggedArgsBytes))
	expectedArgs := "pr merge https://github.com/brotherlogic/test-repo/pull/42 --auto --squash"
	if loggedArgs != expectedArgs {
		t.Errorf("expected gh command args:\n%q\ngot:\n%q", expectedArgs, loggedArgs)
	}
}

func TestEnableAutoMerge_CommandFailure(t *testing.T) {
	setupMockGH(t, "echo 'GraphQL error: Auto-merge is not allowed for this repository' >&2\nexit 1")

	cfg := &Config{RootDir: t.TempDir()}
	err := EnableAutoMerge(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42")
	if err == nil {
		t.Fatalf("expected error when gh pr merge fails, got nil")
	}
	if !strings.Contains(err.Error(), "gh pr merge failed") {
		t.Errorf("expected error message to contain 'gh pr merge failed', got %v", err)
	}
}

func TestPollPRStatus_HookInvocation(t *testing.T) {
	hookCalled := false
	var gotURL string
	var gotTimeout time.Duration

	cfg := &Config{
		PollPRStatusFunc: func(ctx context.Context, cfg *Config, prURL string, timeout time.Duration) error {
			hookCalled = true
			gotURL = prURL
			gotTimeout = timeout
			return nil
		},
	}

	err := PollPRStatus(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hookCalled {
		t.Errorf("expected PollPRStatusFunc hook to be called")
	}
	if gotURL != "https://github.com/brotherlogic/test-repo/pull/42" {
		t.Errorf("expected URL %q, got %q", "https://github.com/brotherlogic/test-repo/pull/42", gotURL)
	}
	if gotTimeout != 5*time.Minute {
		t.Errorf("expected timeout %v, got %v", 5*time.Minute, gotTimeout)
	}
}

func TestPollPRStatus_EmptyURL(t *testing.T) {
	cfg := &Config{}
	err := PollPRStatus(context.Background(), cfg, "", time.Minute)
	if err == nil {
		t.Errorf("expected error when prURL is empty, got nil")
	}
}

func TestPollPRStatus_SuccessOnMerge(t *testing.T) {
	_, logFile := setupMockGH(t, `echo '{"state":"MERGED","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}'`)

	cfg := &Config{RootDir: t.TempDir()}
	err := PollPRStatus(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42", time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loggedArgsBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed reading mock gh args log: %v", err)
	}
	loggedArgs := strings.TrimSpace(string(loggedArgsBytes))
	expectedArgs := "pr view https://github.com/brotherlogic/test-repo/pull/42 --json state,statusCheckRollup"
	if loggedArgs != expectedArgs {
		t.Errorf("expected gh command args %q, got %q", expectedArgs, loggedArgs)
	}
}

func TestPollPRStatus_LoopUntilMerge(t *testing.T) {
	tempDir := t.TempDir()
	counterFile := filepath.Join(tempDir, "poll_count")
	script := `
COUNT=0
if [ -f "` + counterFile + `" ]; then
  COUNT=$(cat "` + counterFile + `")
fi
COUNT=$((COUNT + 1))
echo "$COUNT" > "` + counterFile + `"
if [ "$COUNT" -eq 1 ]; then
  echo '{"state":"OPEN","statusCheckRollup":[{"name":"test","status":"IN_PROGRESS","conclusion":""}]}'
else
  echo '{"state":"MERGED","statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}'
fi
`
	setupMockGH(t, script)

	cfg := &Config{
		RootDir:      t.TempDir(),
		PollInterval: 10 * time.Millisecond,
	}
	err := PollPRStatus(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42", 5*time.Second)
	if err != nil {
		t.Fatalf("expected loop to succeed on merge, got: %v", err)
	}
}

func TestPollPRStatus_AbortOnCheckFailure(t *testing.T) {
	tests := []struct {
		name       string
		conclusion string
		state      string
	}{
		{name: "failure conclusion", conclusion: "FAILURE"},
		{name: "timed_out conclusion", conclusion: "TIMED_OUT"},
		{name: "cancelled conclusion", conclusion: "CANCELLED"},
		{name: "failure state", state: "FAILURE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jsonOutput := `{"state":"OPEN","statusCheckRollup":[{"name":"test","conclusion":"` + tc.conclusion + `","state":"` + tc.state + `"}]}`
			setupMockGH(t, `echo '`+jsonOutput+`'`)

			cfg := &Config{
				RootDir:      t.TempDir(),
				PollInterval: 10 * time.Millisecond,
			}
			err := PollPRStatus(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42", 5*time.Second)
			if err == nil {
				t.Fatalf("expected error on check failure, got nil")
			}
			if !strings.Contains(err.Error(), "test") {
				t.Errorf("expected error message to mention failing check name 'test', got %v", err)
			}
		})
	}
}

func TestPollPRStatus_ClosedPRFailure(t *testing.T) {
	setupMockGH(t, `echo '{"state":"CLOSED","statusCheckRollup":[]}'`)

	cfg := &Config{
		RootDir:      t.TempDir(),
		PollInterval: 10 * time.Millisecond,
	}
	err := PollPRStatus(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42", 5*time.Second)
	if err == nil {
		t.Fatalf("expected error when PR is closed, got nil")
	}
	if !strings.Contains(err.Error(), "closed without merging") {
		t.Errorf("expected error message to contain 'closed without merging', got %v", err)
	}
}

func TestPollPRStatus_TimeoutHandling(t *testing.T) {
	setupMockGH(t, `echo '{"state":"OPEN","statusCheckRollup":[]}'`)

	cfg := &Config{
		RootDir:      t.TempDir(),
		PollInterval: 10 * time.Millisecond,
	}
	err := PollPRStatus(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42", 50*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected timeout error, got %v", err)
	}
}

func TestPollPRStatus_CommandFailure(t *testing.T) {
	setupMockGH(t, `echo "GraphQL error: Could not resolve to a PullRequest" >&2; exit 1`)

	cfg := &Config{
		RootDir:      t.TempDir(),
		PollInterval: 10 * time.Millisecond,
	}
	err := PollPRStatus(context.Background(), cfg, "https://github.com/brotherlogic/test-repo/pull/42", 5*time.Second)
	if err == nil {
		t.Fatalf("expected error on command failure, got nil")
	}
	if !strings.Contains(err.Error(), "gh pr view failed") {
		t.Errorf("expected error to mention 'gh pr view failed', got %v", err)
	}
}

func TestSyncMainBranch_HookInvocation(t *testing.T) {
	hookCalled := false
	var passedBranch string
	cfg := &Config{
		SyncMainBranchFunc: func(ctx context.Context, cfg *Config, branchName string) error {
			hookCalled = true
			passedBranch = branchName
			return nil
		},
	}

	err := SyncMainBranch(context.Background(), cfg, "feature/my-branch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hookCalled {
		t.Errorf("expected SyncMainBranchFunc to be called")
	}
	if passedBranch != "feature/my-branch" {
		t.Errorf("expected branch %q, got %q", "feature/my-branch", passedBranch)
	}
}

func TestSyncMainBranch_EmptyBranch(t *testing.T) {
	cfg := &Config{}
	err := SyncMainBranch(context.Background(), cfg, "")
	if err == nil {
		t.Errorf("expected error when branchName is empty, got nil")
	}
}

func TestSyncMainBranch_MainBranch(t *testing.T) {
	cfg := &Config{}
	err := SyncMainBranch(context.Background(), cfg, "main")
	if err == nil {
		t.Errorf("expected error when branchName is main, got nil")
	}
}

func TestSyncMainBranch_Success(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	// Set up a bare remote repository to act as origin
	bareRemote := t.TempDir()
	cmdInitBare := exec.Command("git", "init", "--bare")
	cmdInitBare.Dir = bareRemote
	if out, err := cmdInitBare.CombinedOutput(); err != nil {
		t.Fatalf("failed initializing bare remote: %s: %v", string(out), err)
	}
	cmdHead := exec.Command("git", "--git-dir", bareRemote, "symbolic-ref", "HEAD", "refs/heads/main")
	if out, err := cmdHead.CombinedOutput(); err != nil {
		t.Fatalf("failed setting bare remote HEAD: %s: %v", string(out), err)
	}

	cmdRemote := exec.Command("git", "remote", "add", "origin", bareRemote)
	cmdRemote.Dir = tempDir
	if out, err := cmdRemote.CombinedOutput(); err != nil {
		t.Fatalf("failed adding remote origin: %s: %v", string(out), err)
	}

	// Create initial commit on main and push to bare remote
	initialFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed writing initial file: %v", err)
	}
	cmdAdd := exec.Command("git", "add", "README.md")
	cmdAdd.Dir = tempDir
	if out, err := cmdAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %s: %v", string(out), err)
	}
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	if out, err := cmdCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %s: %v", string(out), err)
	}
	cmdBranchM := exec.Command("git", "branch", "-M", "main")
	cmdBranchM.Dir = tempDir
	if out, err := cmdBranchM.CombinedOutput(); err != nil {
		t.Fatalf("git branch -M main failed: %s: %v", string(out), err)
	}
	cmdPushMain := exec.Command("git", "push", "-u", "origin", "main")
	cmdPushMain.Dir = tempDir
	if out, err := cmdPushMain.CombinedOutput(); err != nil {
		t.Fatalf("git push origin main failed: %s: %v", string(out), err)
	}

	// Create and checkout feature branch in tempDir
	featureBranch := "feature/init-scaffolding"
	cmdCheckoutB := exec.Command("git", "checkout", "-b", featureBranch)
	cmdCheckoutB.Dir = tempDir
	if out, err := cmdCheckoutB.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b failed: %s: %v", string(out), err)
	}

	// In another clone, simulate PR merge onto main
	secondaryClone := t.TempDir()
	cmdClone := exec.Command("git", "clone", bareRemote, secondaryClone)
	if out, err := cmdClone.CombinedOutput(); err != nil {
		t.Fatalf("git clone failed: %s: %v", string(out), err)
	}
	_ = exec.Command("git", "-C", secondaryClone, "config", "user.name", "Test User").Run()
	_ = exec.Command("git", "-C", secondaryClone, "config", "user.email", "test@example.com").Run()
	mergedFile := filepath.Join(secondaryClone, "merged.txt")
	if err := os.WriteFile(mergedFile, []byte("merged remote content"), 0644); err != nil {
		t.Fatalf("writing merged file: %v", err)
	}
	cmdAdd2 := exec.Command("git", "add", "merged.txt")
	cmdAdd2.Dir = secondaryClone
	_ = cmdAdd2.Run()
	cmdCommit2 := exec.Command("git", "commit", "-m", "merge pull request #1 into main")
	cmdCommit2.Dir = secondaryClone
	_ = cmdCommit2.Run()
	cmdPush2 := exec.Command("git", "push", "origin", "main")
	cmdPush2.Dir = secondaryClone
	if out, err := cmdPush2.CombinedOutput(); err != nil {
		t.Fatalf("push from secondary clone failed: %s: %v", string(out), err)
	}

	cfg := &Config{
		RootDir: tempDir,
		Repo:    "brotherlogic/test-repo",
		Owner:   "brotherlogic",
	}

	// Execute SyncMainBranch
	if err := SyncMainBranch(context.Background(), cfg, featureBranch); err != nil {
		t.Fatalf("SyncMainBranch failed unexpectedly: %v", err)
	}

	// Verify current branch is main
	cmdBranch := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmdBranch.Dir = tempDir
	outBranch, err := cmdBranch.Output()
	if err != nil {
		t.Fatalf("failed checking current branch: %v", err)
	}
	if strings.TrimSpace(string(outBranch)) != "main" {
		t.Errorf("expected branch main, got %q", strings.TrimSpace(string(outBranch)))
	}

	// Verify merged.txt was pulled
	pulledFile := filepath.Join(tempDir, "merged.txt")
	if _, err := os.Stat(pulledFile); os.IsNotExist(err) {
		t.Errorf("expected merged.txt to be pulled from origin main, but it does not exist")
	}

	// Verify feature branch was deleted locally
	cmdCheckBranch := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+featureBranch)
	cmdCheckBranch.Dir = tempDir
	if err := cmdCheckBranch.Run(); err == nil {
		t.Errorf("expected local feature branch %s to be deleted, but it still exists", featureBranch)
	}
}

func TestSyncMainBranch_CheckoutError(t *testing.T) {
	tempDir := t.TempDir()
	// Repo not initialized with git, so checkout must fail
	cfg := &Config{RootDir: tempDir}
	err := SyncMainBranch(context.Background(), cfg, "feature/foo")
	if err == nil {
		t.Errorf("expected error when checkout fails in non-git dir, got nil")
	}
}

func TestSyncMainBranch_PullError(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	// Make initial commit on main so checkout main works, but there is no remote
	initialFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Test"), 0644); err != nil {
		t.Fatalf("failed writing initial file: %v", err)
	}
	cmdAdd := exec.Command("git", "add", "README.md")
	cmdAdd.Dir = tempDir
	_ = cmdAdd.Run()
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	_ = cmdCommit.Run()
	cmdBranchM := exec.Command("git", "branch", "-M", "main")
	cmdBranchM.Dir = tempDir
	_ = cmdBranchM.Run()

	cfg := &Config{RootDir: tempDir}
	// Pull from origin main should fail because origin does not exist
	err := SyncMainBranch(context.Background(), cfg, "feature/foo")
	if err == nil {
		t.Errorf("expected error when pull fails, got nil")
	}
}

func TestSyncMainBranch_BranchDeleteError(t *testing.T) {
	tempDir := t.TempDir()
	initGitRepo(t, tempDir)

	// Set up bare remote
	bareRemote := t.TempDir()
	cmdInitBare := exec.Command("git", "init", "--bare")
	cmdInitBare.Dir = bareRemote
	_ = cmdInitBare.Run()
	cmdRemote := exec.Command("git", "remote", "add", "origin", bareRemote)
	cmdRemote.Dir = tempDir
	_ = cmdRemote.Run()

	initialFile := filepath.Join(tempDir, "README.md")
	_ = os.WriteFile(initialFile, []byte("# Test"), 0644)
	cmdAdd := exec.Command("git", "add", "README.md")
	cmdAdd.Dir = tempDir
	_ = cmdAdd.Run()
	cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
	cmdCommit.Dir = tempDir
	_ = cmdCommit.Run()
	cmdBranchM2 := exec.Command("git", "branch", "-M", "main")
	cmdBranchM2.Dir = tempDir
	_ = cmdBranchM2.Run()
	cmdPush := exec.Command("git", "push", "-u", "origin", "main")
	cmdPush.Dir = tempDir
	_ = cmdPush.Run()

	cfg := &Config{RootDir: tempDir}
	// Feature branch does not exist, so deleting it should fail
	err := SyncMainBranch(context.Background(), cfg, "feature/nonexistent-branch")
	if err == nil {
		t.Errorf("expected error when deleting nonexistent branch, got nil")
	}
}

func TestEnsurePassingTests_ScaffoldsWhenNoTests(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &Config{RootDir: tempDir}

	if err := EnsurePassingTests(cfg); err != nil {
		t.Fatalf("expected EnsurePassingTests to succeed, got error: %v", err)
	}

	initTestPath := filepath.Join(tempDir, "tests", "init_test.go")
	content, err := os.ReadFile(initTestPath)
	if err != nil {
		t.Fatalf("expected %s to exist, got error: %v", initTestPath, err)
	}

	if !strings.Contains(string(content), "func TestInit(t *testing.T)") {
		t.Errorf("expected init_test.go to contain 'func TestInit(t *testing.T)', got:\n%s", string(content))
	}
	if !strings.Contains(string(content), "package tests") {
		t.Errorf("expected init_test.go to have package tests, got:\n%s", string(content))
	}
}

func TestEnsurePassingTests_PreservesExistingTests(t *testing.T) {
	tempDir := t.TempDir()
	existingTestDir := filepath.Join(tempDir, "pkg", "mypkg")
	if err := os.MkdirAll(existingTestDir, 0755); err != nil {
		t.Fatalf("failed to create existing test dir: %v", err)
	}
	existingTestFile := filepath.Join(existingTestDir, "mypkg_test.go")
	if err := os.WriteFile(existingTestFile, []byte("package mypkg\n"), 0644); err != nil {
		t.Fatalf("failed to write existing test file: %v", err)
	}

	cfg := &Config{RootDir: tempDir}
	if err := EnsurePassingTests(cfg); err != nil {
		t.Fatalf("expected EnsurePassingTests to succeed, got error: %v", err)
	}

	initTestPath := filepath.Join(tempDir, "tests", "init_test.go")
	if _, err := os.Stat(initTestPath); !os.IsNotExist(err) {
		t.Errorf("expected init_test.go not to be created when existing tests are present")
	}
}

func TestHasExistingTests(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Empty dir: should return false
	hasTests, err := HasExistingTests(tempDir)
	if err != nil {
		t.Fatalf("unexpected error checking empty dir: %v", err)
	}
	if hasTests {
		t.Errorf("expected false for empty dir, got true")
	}

	// 2. Hidden dirs and .git should be ignored
	gitDir := filepath.Join(tempDir, ".git")
	_ = os.MkdirAll(gitDir, 0755)
	_ = os.WriteFile(filepath.Join(gitDir, "ignored_test.go"), []byte("package git\n"), 0644)

	hiddenDir := filepath.Join(tempDir, ".hidden")
	_ = os.MkdirAll(hiddenDir, 0755)
	_ = os.WriteFile(filepath.Join(hiddenDir, "ignored_test.go"), []byte("package hidden\n"), 0644)

	hasTests, err = HasExistingTests(tempDir)
	if err != nil {
		t.Fatalf("unexpected error checking dir with only hidden tests: %v", err)
	}
	if hasTests {
		t.Errorf("expected false when tests are only in .git or hidden dirs, got true")
	}

	// 3. Test in normal subdirectory: should return true
	subDir := filepath.Join(tempDir, "pkg", "sample")
	_ = os.MkdirAll(subDir, 0755)
	_ = os.WriteFile(filepath.Join(subDir, "sample_test.go"), []byte("package sample\n"), 0644)

	hasTests, err = HasExistingTests(tempDir)
	if err != nil {
		t.Fatalf("unexpected error checking dir with tests: %v", err)
	}
	if !hasTests {
		t.Errorf("expected true when tests are present, got false")
	}
}

func TestEnsurePassingTests_HookInvocation(t *testing.T) {
	hookCalled := false
	expectedErr := errors.New("hook error")
	cfg := &Config{
		EnsurePassingTestsFunc: func(cfg *Config) error {
			hookCalled = true
			return expectedErr
		},
	}

	err := EnsurePassingTests(cfg)
	if !hookCalled {
		t.Fatalf("expected EnsurePassingTestsFunc hook to be called")
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}
}
