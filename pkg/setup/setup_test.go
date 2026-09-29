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





