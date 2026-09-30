package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config encapsulates configuration for the project setup process.
type Config struct {
	RootDir      string
	Repo         string
	Owner        string
	Token        string
	Collaborator string
	Force        bool
	SkipPush     bool
	PromptFunc   func(path string) bool

	// Testing hooks
	CheckPermissionsFunc        func(ctx context.Context, cfg *Config) (RepoPermissions, error)
	CheckScaffoldingChangesFunc func(ctx context.Context, cfg *Config) (bool, error)
	CommitAndPushBranchFunc     func(ctx context.Context, cfg *Config, branchName string) error
	CreatePRFunc                func(ctx context.Context, cfg *Config, branchName, title, body string) (string, error)
	EnableAutoMergeFunc         func(ctx context.Context, cfg *Config, prURL string) error
	PollPRStatusFunc            func(ctx context.Context, cfg *Config, prURL string, timeout time.Duration) error
	PollInterval                time.Duration
	PollTimeout                 time.Duration
	BranchName                  string
	SyncMainBranchFunc          func(ctx context.Context, cfg *Config, branchName string) error
	ConfigureRepoFunc           func(ctx context.Context, cfg *Config) error
	ConfigureCollabFunc         func(ctx context.Context, cfg *Config) error
	CommitAndPushFunc           func(ctx context.Context, cfg *Config) error
	ConfigureRulesetsFunc       func(ctx context.Context, cfg *Config) error
	CheckToolchainFunc          func(ctx context.Context) error
}

// RepoPermissions captures repository access permissions from GitHub API.
type RepoPermissions struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Pull     bool `json:"pull"`
}

// ParseGitRepoFromURL extracts "owner/repo" from common git remote URLs.
func ParseGitRepoFromURL(rawURL string) (string, error) {
	u := strings.TrimSpace(rawURL)
	u = strings.TrimSuffix(u, ".git")

	if idx := strings.Index(u, "github.com/"); idx != -1 {
		part := u[idx+len("github.com/"):]
		parts := strings.Split(strings.Trim(part, "/"), "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1], nil
		}
	}

	if idx := strings.Index(u, "github.com:"); idx != -1 {
		part := u[idx+len("github.com:"):]
		parts := strings.Split(strings.Trim(part, "/"), "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1], nil
		}
	}

	return "", fmt.Errorf("unable to parse GitHub repository owner/name from URL: %s", rawURL)
}

// DetectRepo inspects git remote or gh CLI to detect the repository name.
func DetectRepo(ctx context.Context, dir string) (string, error) {
	// Try git remote get-url origin
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	if out, err := cmd.Output(); err == nil {
		if repo, err := ParseGitRepoFromURL(string(out)); err == nil {
			return repo, nil
		}
	}

	// Fallback to gh repo view
	cmd = exec.CommandContext(ctx, "gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		repo := strings.TrimSpace(string(out))
		if repo != "" && strings.Contains(repo, "/") {
			return repo, nil
		}
	}

	return "", errors.New("failed to automatically detect GitHub repository; specify via --repo=owner/name")
}

// CheckToolchain verifies that the go executable is installed and available on PATH.
func CheckToolchain(ctx context.Context, cfg *Config) error {
	if cfg != nil && cfg.CheckToolchainFunc != nil {
		return cfg.CheckToolchainFunc(ctx)
	}

	if _, err := exec.LookPath("go"); err != nil {
		return errors.New("go executable not found on PATH; please install Go or verify your PATH environment variable")
	}

	return nil
}

// SetupDirectories ensures specs/, proto/, tests/, internal/, and .github/workflows/ exist.
func SetupDirectories(cfg *Config) error {
	dirs := []string{
		"specs",
		"proto",
		"tests",
		"internal",
		filepath.Join(".github", "workflows"),
	}

	for _, d := range dirs {
		fullPath := filepath.Join(cfg.RootDir, d)
		if err := os.MkdirAll(fullPath, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", d, err)
		}

		// If specs, proto, tests, or internal is empty, add a .gitkeep so git tracks it
		if d == "specs" || d == "proto" || d == "tests" || d == "internal" {
			entries, err := os.ReadDir(fullPath)
			if err == nil && len(entries) == 0 {
				gitkeep := filepath.Join(fullPath, ".gitkeep")
				_ = os.WriteFile(gitkeep, []byte(""), 0644)
			}
		}
	}

	return nil
}

// SetupTemplateFiles renders and writes the required GitHub workflows and CODEOWNERS.
func SetupTemplateFiles(cfg *Config) ([]string, error) {
	tmplCtx := TemplateContext{
		Owner: cfg.Owner,
		Repo:  cfg.Repo,
	}

	reviewGateContent, err := renderTemplate(reviewGateTemplate, tmplCtx)
	if err != nil {
		return nil, fmt.Errorf("rendering review-gate template: %w", err)
	}

	codeownersContent, err := renderTemplate(codeownersTemplate, tmplCtx)
	if err != nil {
		return nil, fmt.Errorf("rendering codeowners template: %w", err)
	}

	templates := map[string]string{
		filepath.Join(".github", "CODEOWNERS"):                   codeownersContent,
		filepath.Join(".github", "workflows", "review-gate.yml"): reviewGateContent,
		filepath.Join(".github", "workflows", "auto-merge.yml"):  autoMergeTemplate,
		filepath.Join(".github", "workflows", "tests.yml"):       testsTemplate,
	}

	var written []string

	for relPath, content := range templates {
		fullPath := filepath.Join(cfg.RootDir, relPath)
		parentDir := filepath.Dir(fullPath)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			return nil, fmt.Errorf("creating parent directory %s: %w", parentDir, err)
		}

		if existingData, err := os.ReadFile(fullPath); err == nil {
			if string(existingData) == content {
				// File already exists and matches template exactly
				continue
			}

			// File exists with different content
			if !cfg.Force {
				shouldOverwrite := false
				if cfg.PromptFunc != nil {
					shouldOverwrite = cfg.PromptFunc(relPath)
				} else {
					shouldOverwrite = defaultPrompt(relPath)
				}

				if !shouldOverwrite {
					log.Printf("Skipping existing file: %s", relPath)
					continue
				}
			}
		}

		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return nil, fmt.Errorf("writing file %s: %w", fullPath, err)
		}
		written = append(written, relPath)
	}

	return written, nil
}

func defaultPrompt(path string) bool {
	fmt.Printf("File %s already exists with different content. Overwrite? [y/N]: ", path)
	var resp string
	_, _ = fmt.Scanln(&resp)
	resp = strings.TrimSpace(resp)
	return strings.EqualFold(resp, "y") || strings.EqualFold(resp, "yes")
}

// ghCmd executes a gh command with token in environment if configured.
func ghCmd(ctx context.Context, cfg *Config, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = cfg.RootDir
	if cfg.Token != "" {
		cmd.Env = append(os.Environ(), "GH_TOKEN="+cfg.Token, "GITHUB_TOKEN="+cfg.Token)
	}
	return cmd
}

// CheckPermissions inspects the user's permissions on the target repository.
func CheckPermissions(ctx context.Context, cfg *Config) (RepoPermissions, error) {
	if cfg.CheckPermissionsFunc != nil {
		return cfg.CheckPermissionsFunc(ctx, cfg)
	}

	cmd := ghCmd(ctx, cfg, "api", fmt.Sprintf("repos/%s", cfg.Repo), "--jq", ".permissions")
	out, err := cmd.Output()
	if err != nil {
		return RepoPermissions{}, fmt.Errorf("checking repo permissions: %s: %w", string(out), err)
	}
	var perms RepoPermissions
	if err := json.Unmarshal(out, &perms); err != nil {
		return RepoPermissions{}, fmt.Errorf("parsing permissions JSON: %w", err)
	}
	return perms, nil
}

// ConfigureRepoSettings enables auto-merge and delete-branch-on-merge.
func ConfigureRepoSettings(ctx context.Context, cfg *Config) error {
	if cfg.ConfigureRepoFunc != nil {
		return cfg.ConfigureRepoFunc(ctx, cfg)
	}

	cmd := ghCmd(ctx, cfg, "repo", "edit", cfg.Repo, "--enable-auto-merge", "--delete-branch-on-merge")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("configuring repository settings via gh: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ConfigureCollaborator adds or verifies collaborator permissions.
func ConfigureCollaborator(ctx context.Context, cfg *Config) error {
	if cfg.ConfigureCollabFunc != nil {
		return cfg.ConfigureCollabFunc(ctx, cfg)
	}

	if cfg.Collaborator == "" {
		return nil
	}

	endpoint := fmt.Sprintf("repos/%s/collaborators/%s", cfg.Repo, cfg.Collaborator)
	cmd := ghCmd(ctx, cfg, "api", endpoint, "-X", "PUT", "-f", "permission=push")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("configuring collaborator: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RulesetPayload represents the JSON body for the GitHub branch ruleset.
type RulesetPayload struct {
	Name        string            `json:"name"`
	Target      string            `json:"target"`
	Enforcement string            `json:"enforcement"`
	Conditions  RulesetConditions `json:"conditions"`
	Rules       []any             `json:"rules"`
}

type RulesetConditions struct {
	RefName RulesetRefName `json:"ref_name"`
}

type RulesetRefName struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// CheckExistingRuleset returns the ID and enforcement status of a ruleset by name, or 0 if not found.
func CheckExistingRuleset(ctx context.Context, cfg *Config, name string) (int64, string, error) {
	listCmd := ghCmd(ctx, cfg, "api", fmt.Sprintf("repos/%s/rulesets", cfg.Repo))
	listOut, err := listCmd.Output()
	if err != nil {
		return 0, "", fmt.Errorf("listing rulesets: %w", err)
	}
	var existingRulesets []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Enforcement string `json:"enforcement"`
	}
	if err := json.Unmarshal(listOut, &existingRulesets); err != nil {
		return 0, "", fmt.Errorf("parsing rulesets: %w", err)
	}
	for _, r := range existingRulesets {
		if r.Name == name {
			return r.ID, r.Enforcement, nil
		}
	}
	return 0, "", nil
}

// SetRulesetEnforcement updates the enforcement status of a ruleset (e.g., "active", "disabled").
func SetRulesetEnforcement(ctx context.Context, cfg *Config, rulesetID int64, enforcement string) error {
	payload := map[string]string{
		"enforcement": enforcement,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling enforcement payload: %w", err)
	}
	cmd := ghCmd(ctx, cfg, "api", fmt.Sprintf("repos/%s/rulesets/%d", cfg.Repo, rulesetID), "-X", "PATCH", "--input", "-")
	cmd.Stdin = bytes.NewReader(payloadBytes)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("setting ruleset enforcement to %s: %s: %w", enforcement, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ConfigureRulesets creates or updates the default branch ruleset.
func ConfigureRulesets(ctx context.Context, cfg *Config) error {
	if cfg.ConfigureRulesetsFunc != nil {
		return cfg.ConfigureRulesetsFunc(ctx, cfg)
	}

	ruleset := RulesetPayload{
		Name:        "main",
		Target:      "branch",
		Enforcement: "active",
		Conditions: RulesetConditions{
			RefName: RulesetRefName{
				Include: []string{"~DEFAULT_BRANCH"},
				Exclude: []string{},
			},
		},
		Rules: []any{
			map[string]string{"type": "deletion"},
			map[string]string{"type": "non_fast_forward"},
			map[string]string{"type": "required_linear_history"},
			map[string]any{
				"type": "pull_request",
				"parameters": map[string]any{
					"required_approving_review_count":                 0,
					"dismiss_stale_reviews_on_push":                   false,
					"required_reviewers":                              []string{},
					"require_code_owner_review":                       true,
					"require_last_push_approval":                      false,
					"required_review_thread_resolution":               false,
					"require_extra_approval_for_unattributed_changes": true,
					"allowed_merge_methods":                           []string{"squash"},
				},
			},
			map[string]any{
				"type": "required_status_checks",
				"parameters": map[string]any{
					"strict_required_status_checks_policy": false,
					"do_not_enforce_on_create":             false,
					"required_status_checks": []map[string]string{
						{"context": "test"},
						{"context": "review-gate"},
					},
				},
			},
		},
	}

	payloadBytes, err := json.Marshal(ruleset)
	if err != nil {
		return fmt.Errorf("marshaling ruleset payload: %w", err)
	}

	// Check if ruleset named "main" already exists
	existingID, _, _ := CheckExistingRuleset(ctx, cfg, "main")

	var applyCmd *exec.Cmd
	if existingID > 0 {
		applyCmd = ghCmd(ctx, cfg, "api", fmt.Sprintf("repos/%s/rulesets/%d", cfg.Repo, existingID), "-X", "PUT", "--input", "-")
	} else {
		applyCmd = ghCmd(ctx, cfg, "api", fmt.Sprintf("repos/%s/rulesets", cfg.Repo), "-X", "POST", "--input", "-")
	}

	applyCmd.Stdin = bytes.NewReader(payloadBytes)
	out, err := applyCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("applying ruleset via gh: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// CheckScaffoldingChanges stages scaffolding directories and checks if there are pending staged changes.
func CheckScaffoldingChanges(ctx context.Context, cfg *Config) (bool, error) {
	if cfg.CheckScaffoldingChangesFunc != nil {
		return cfg.CheckScaffoldingChangesFunc(ctx, cfg)
	}

	rootDir := cfg.RootDir
	if rootDir == "" {
		rootDir = "."
	}

	// 1. Stage scaffolding paths
	paths := []string{".github", "specs", "proto", "tests", "internal"}
	var existingPaths []string
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(rootDir, p)); err == nil {
			existingPaths = append(existingPaths, p)
		}
	}

	if len(existingPaths) > 0 {
		args := append([]string{"-C", rootDir, "add"}, existingPaths...)
		addCmd := exec.CommandContext(ctx, "git", args...)
		if out, err := addCmd.CombinedOutput(); err != nil {
			return false, fmt.Errorf("git add failed: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}

	// 2. Run git diff --cached --quiet to detect pending staged changes
	diffCmd := exec.CommandContext(ctx, "git", "-C", rootDir, "diff", "--cached", "--quiet")
	if err := diffCmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return true, nil
		}
		return false, fmt.Errorf("git diff --cached --quiet failed: %w", err)
	}

	// 3. Return false if tree is clean
	return false, nil
}

// GitCommitAndPush stages changes, commits, and performs a single push to remote.
func GitCommitAndPush(ctx context.Context, cfg *Config) error {
	// 1. Stage directories
	addCmd := exec.CommandContext(ctx, "git", "-C", cfg.RootDir, "add", ".github", "specs", "proto", "tests", "internal")
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 2. Check if anything is staged
	diffCmd := exec.CommandContext(ctx, "git", "-C", cfg.RootDir, "diff", "--cached", "--quiet")
	if err := diffCmd.Run(); err == nil {
		log.Println("Working tree has no new changes to commit.")
		return nil
	}

	// 3. Commit
	commitCmd := exec.CommandContext(ctx, "git", "-C", cfg.RootDir, "commit", "-m", "chore: initialize speculate project scaffolding")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 4. Single push to origin HEAD
	pushCmd := exec.CommandContext(ctx, "git", "-C", cfg.RootDir, "push", "origin", "HEAD")
	if out, err := pushCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// GitCommitAndPushBranch creates and switches to a dedicated branch, commits staged files, and pushes the branch to remote origin.
func GitCommitAndPushBranch(ctx context.Context, cfg *Config, branchName string) error {
	if cfg.CommitAndPushBranchFunc != nil {
		return cfg.CommitAndPushBranchFunc(ctx, cfg, branchName)
	}

	rootDir := cfg.RootDir
	if rootDir == "" {
		rootDir = "."
	}

	// 1. Switch and create branch: git checkout -b <branchName>
	checkoutCmd := exec.CommandContext(ctx, "git", "-C", rootDir, "checkout", "-b", branchName)
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout -b %s failed: %s: %w", branchName, strings.TrimSpace(string(out)), err)
	}

	// 2. Commit staged files: git commit -m "chore: initialize speculate project scaffolding"
	commitCmd := exec.CommandContext(ctx, "git", "-C", rootDir, "commit", "-m", "chore: initialize speculate project scaffolding")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 3. Push branch: git push origin <branchName>
	pushCmd := exec.CommandContext(ctx, "git", "-C", rootDir, "push", "origin", branchName)
	if out, err := pushCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s failed: %s: %w", branchName, strings.TrimSpace(string(out)), err)
	}

	return nil
}

// CreatePullRequest creates a pull request for the feature branch targeting main.
func CreatePullRequest(ctx context.Context, cfg *Config, branchName string) (string, error) {
	if branchName == "" {
		return "", errors.New("branchName cannot be empty")
	}

	title := "chore: initialize speculate project scaffolding"
	body := "Initial speculate scaffolding (directory structure, GitHub workflows, and CODEOWNERS)."

	if cfg.CreatePRFunc != nil {
		return cfg.CreatePRFunc(ctx, cfg, branchName, title, body)
	}

	cmd := ghCmd(ctx, cfg, "pr", "create", "--base", "main", "--head", branchName, "--title", title, "--body", body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var prURL string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
			prURL = trimmed
			break
		}
	}
	if prURL == "" {
		prURL = strings.TrimSpace(string(out))
	}
	if prURL == "" {
		return "", errors.New("gh pr create returned empty URL")
	}

	return prURL, nil
}

// EnableAutoMerge enables squash auto-merge on the specified pull request.
func EnableAutoMerge(ctx context.Context, cfg *Config, prURL string) error {
	if prURL == "" {
		return errors.New("prURL cannot be empty")
	}

	if cfg.EnableAutoMergeFunc != nil {
		return cfg.EnableAutoMergeFunc(ctx, cfg, prURL)
	}

	cmd := ghCmd(ctx, cfg, "pr", "merge", prURL, "--auto", "--squash")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr merge failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// PRCheckItem represents an individual check run or status context from GitHub GraphQL statusCheckRollup.
type PRCheckItem struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	State      string `json:"state"`
	Conclusion string `json:"conclusion"`
}

// PRViewStatus represents the JSON payload from gh pr view --json state,statusCheckRollup.
type PRViewStatus struct {
	State             string        `json:"state"`
	StatusCheckRollup []PRCheckItem `json:"statusCheckRollup"`
}

// PollPRStatus polls the status of a pull request and its CI checks until merged or failure.
func PollPRStatus(ctx context.Context, cfg *Config, prURL string, timeout time.Duration) error {
	if prURL == "" {
		return errors.New("prURL cannot be empty")
	}

	if cfg.PollPRStatusFunc != nil {
		return cfg.PollPRStatusFunc(ctx, cfg, prURL, timeout)
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	interval := cfg.PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}

	checkOnce := func() (bool, error) {
		cmd := ghCmd(ctx, cfg, "pr", "view", prURL, "--json", "state,statusCheckRollup")
		out, err := cmd.CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, fmt.Errorf("gh pr view failed: %s: %w", strings.TrimSpace(string(out)), err)
		}

		var viewStatus PRViewStatus
		if err := json.Unmarshal(out, &viewStatus); err != nil {
			return false, fmt.Errorf("parsing gh pr view JSON: %w", err)
		}

		// Check for failing status checks immediately
		for _, check := range viewStatus.StatusCheckRollup {
			conclusion := strings.ToUpper(check.Conclusion)
			state := strings.ToUpper(check.State)
			if conclusion == "FAILURE" || conclusion == "TIMED_OUT" || conclusion == "CANCELLED" || state == "FAILURE" || state == "ERROR" {
				checkName := check.Name
				if checkName == "" {
					checkName = check.Context
				}
				if checkName == "" {
					checkName = "unknown"
				}
				reason := check.Conclusion
				if reason == "" {
					reason = check.State
				}
				return false, fmt.Errorf("status check %q failed with conclusion/state %s", checkName, reason)
			}
		}

		// Check overall PR state
		switch strings.ToUpper(viewStatus.State) {
		case "MERGED":
			return true, nil
		case "CLOSED":
			return false, fmt.Errorf("pull request %s was closed without merging", prURL)
		default:
			return false, nil
		}
	}

	// Check immediately before waiting on ticker
	done, err := checkOnce()
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("polling PR %s timed out: %w", prURL, ctx.Err())
		case <-ticker.C:
			done, err := checkOnce()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// SyncMainBranch checks out the main branch, pulls latest changes from origin, and deletes the temporary feature branch.
func SyncMainBranch(ctx context.Context, cfg *Config, branchName string) error {
	if branchName == "" {
		return errors.New("branchName cannot be empty")
	}
	if branchName == "main" {
		return errors.New("cannot clean up main branch")
	}

	if cfg.SyncMainBranchFunc != nil {
		return cfg.SyncMainBranchFunc(ctx, cfg, branchName)
	}

	rootDir := cfg.RootDir
	if rootDir == "" {
		rootDir = "."
	}

	// 1. Switch to main: git checkout main
	checkoutCmd := exec.CommandContext(ctx, "git", "-C", rootDir, "checkout", "main")
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout main failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 2. Pull remote changes: git pull origin main
	pullCmd := exec.CommandContext(ctx, "git", "-C", rootDir, "pull", "origin", "main")
	if out, err := pullCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git pull origin main failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 3. Delete temporary local branch: git branch -D <branchName>
	deleteCmd := exec.CommandContext(ctx, "git", "-C", rootDir, "branch", "-D", branchName)
	if out, err := deleteCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -D %s failed: %s: %w", branchName, strings.TrimSpace(string(out)), err)
	}

	return nil
}

// Run executes the complete speculate project initialization.
func Run(ctx context.Context, cfg *Config) error {
	if cfg.RootDir == "" {
		cfg.RootDir = "."
	}

	if cfg.Repo == "" {
		detected, err := DetectRepo(ctx, cfg.RootDir)
		if err != nil {
			return err
		}
		cfg.Repo = detected
	}

	parts := strings.Split(cfg.Repo, "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid repository format %q; expected 'owner/name'", cfg.Repo)
	}
	cfg.Owner = parts[0]

	fmt.Printf("🚀 Initializing Speculate setup for repository: %s\n", cfg.Repo)
	fmt.Printf("📁 Target directory: %s\n\n", cfg.RootDir)

	// 1. Directories
	fmt.Print("1. Setting up directory structure (specs/, proto/, tests/, internal/)... ")
	if err := SetupDirectories(cfg); err != nil {
		return fmt.Errorf("failed creating directories: %w", err)
	}
	fmt.Println("✓ Done")

	// 2. Templates
	fmt.Print("2. Setting up workflows and CODEOWNERS... ")
	written, err := SetupTemplateFiles(cfg)
	if err != nil {
		return fmt.Errorf("failed creating template files: %w", err)
	}
	if len(written) > 0 {
		fmt.Printf("✓ Created/updated: %s\n", strings.Join(written, ", "))
	} else {
		fmt.Println("✓ Up-to-date (no changes needed)")
	}

	// Check permissions on the target repository
	perms, err := CheckPermissions(ctx, cfg)
	if err != nil {
		fmt.Printf("⚠️  Could not determine repository permissions: %v\n", err)
	}

	if perms.Admin {
		// 3. Repo Settings
		fmt.Printf("3. Configuring repository settings on GitHub (%s)... ", cfg.Repo)
		if err := ConfigureRepoSettings(ctx, cfg); err != nil {
			fmt.Printf("⚠️  Warning: %v\n", err)
		} else {
			fmt.Println("✓ Auto-merge & branch deletion enabled")
		}

		// 4. Collaborator
		if cfg.Collaborator != "" {
			fmt.Printf("4. Ensuring collaborator access for @%s... ", cfg.Collaborator)
			if err := ConfigureCollaborator(ctx, cfg); err != nil {
				fmt.Printf("⚠️  Warning: %v\n", err)
			} else {
				fmt.Println("✓ Done")
			}
		}
	} else {
		fmt.Printf("3-4. Note: Admin permissions required for repository settings and collaborators on %s.\n", cfg.Repo)
	}

	// 5. Idempotency Check
	fmt.Print("5. Checking for scaffolding changes... ")
	hasChanges, err := CheckScaffoldingChanges(ctx, cfg)
	if err != nil {
		return fmt.Errorf("checking scaffolding changes: %w", err)
	}
	if !hasChanges {
		fmt.Println("✓ Repository scaffolding is already up to date (no changes detected)")
		fmt.Printf("\n🎉 Repository %s initialization completed!\n", cfg.Repo)
		return nil
	}
	fmt.Println("✓ Changes detected")

	if cfg.SkipPush {
		fmt.Println("6. Skipping git branch, push, and PR creation (--skip-push specified)")
		if perms.Admin {
			fmt.Print("7. Configuring GitHub Ruleset for default branch... ")
			if err := ConfigureRulesets(ctx, cfg); err != nil {
				return fmt.Errorf("failed configuring rulesets: %w", err)
			}
			fmt.Println("✓ Ruleset 'main' enforced (CODEOWNERS review, required checks, squash merge)")
		}
		fmt.Printf("\n🎉 Repository %s initialization completed!\n", cfg.Repo)
		return nil
	}

	branchName := cfg.BranchName
	if branchName == "" {
		branchName = "feature/speculate-scaffolding"
	}

	// 6. Branch creation & push
	fmt.Printf("6. Creating branch %s and pushing scaffolding changes... ", branchName)
	if err := GitCommitAndPushBranch(ctx, cfg, branchName); err != nil {
		return fmt.Errorf("failed committing and pushing branch: %w", err)
	}
	fmt.Println("✓ Pushed to origin")

	// 7. PR creation & auto-merge
	fmt.Printf("7. Creating pull request for %s... ", branchName)
	prURL, err := CreatePullRequest(ctx, cfg, branchName)
	if err != nil {
		return fmt.Errorf("failed creating pull request: %w", err)
	}
	fmt.Printf("✓ Created: %s\n", prURL)

	fmt.Print("8. Enabling auto-merge on pull request... ")
	if err := EnableAutoMerge(ctx, cfg, prURL); err != nil {
		return fmt.Errorf("failed enabling auto-merge: %w", err)
	}
	fmt.Println("✓ Auto-merge enabled")

	// 8. Poll PR status
	fmt.Printf("9. Polling pull request status until merge (%s)... ", prURL)
	timeout := cfg.PollTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if err := PollPRStatus(ctx, cfg, prURL, timeout); err != nil {
		return fmt.Errorf("pull request polling failed: %w", err)
	}
	fmt.Println("✓ Pull request merged")

	// 9. Sync main branch
	fmt.Printf("10. Synchronizing local main branch and cleaning up %s... ", branchName)
	if err := SyncMainBranch(ctx, cfg, branchName); err != nil {
		return fmt.Errorf("failed synchronizing main branch: %w", err)
	}
	fmt.Println("✓ Local main synchronized and branch cleaned up")

	// 10. Ruleset enforcement
	if perms.Admin {
		fmt.Print("11. Configuring GitHub Ruleset for default branch... ")
		if err := ConfigureRulesets(ctx, cfg); err != nil {
			return fmt.Errorf("failed configuring rulesets: %w", err)
		}
		fmt.Println("✓ Ruleset 'main' enforced (CODEOWNERS review, required checks, squash merge)")
	} else {
		fmt.Printf("11. Note: Admin permissions required to configure branch ruleset on %s.\n", cfg.Repo)
		fmt.Printf("    Current token has push=%v, admin=%v. Run 'speculate init --token=<admin-token>' as repository owner to apply rulesets.\n", perms.Push, perms.Admin)
	}

	fmt.Printf("\n🎉 Repository %s initialization completed!\n", cfg.Repo)
	return nil
}
