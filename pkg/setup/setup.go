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
	CheckPermissionsFunc   func(ctx context.Context, cfg *Config) (RepoPermissions, error)
	ConfigureRepoFunc      func(ctx context.Context, cfg *Config) error
	ConfigureCollabFunc    func(ctx context.Context, cfg *Config) error
	CommitAndPushFunc      func(ctx context.Context, cfg *Config) error
	ConfigureRulesetsFunc  func(ctx context.Context, cfg *Config) error
	CheckExistingRulesetFn func(ctx context.Context, cfg *Config, name string) (int64, string, error)
	SetRulesetEnforceFn    func(ctx context.Context, cfg *Config, rulesetID int64, enforcement string) error
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
	cmd := ghCmd(ctx, cfg, "repo", "edit", cfg.Repo, "--enable-auto-merge", "--delete-branch-on-merge")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("configuring repository settings via gh: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ConfigureCollaborator adds or verifies collaborator permissions.
func ConfigureCollaborator(ctx context.Context, cfg *Config) error {
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
	checkFn := CheckExistingRuleset
	if cfg.CheckExistingRulesetFn != nil {
		checkFn = cfg.CheckExistingRulesetFn
	}
	existingID, _, _ := checkFn(ctx, cfg, "main")

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
	checkPerms := CheckPermissions
	if cfg.CheckPermissionsFunc != nil {
		checkPerms = cfg.CheckPermissionsFunc
	}
	perms, err := checkPerms(ctx, cfg)
	if err != nil {
		fmt.Printf("⚠️  Could not determine repository permissions: %v\n", err)
	}

	var pausedRulesetID int64
	if perms.Admin {
		// 3. Repo Settings
		fmt.Printf("3. Configuring repository settings on GitHub (%s)... ", cfg.Repo)
		cfgRepo := ConfigureRepoSettings
		if cfg.ConfigureRepoFunc != nil {
			cfgRepo = cfg.ConfigureRepoFunc
		}
		if err := cfgRepo(ctx, cfg); err != nil {
			fmt.Printf("⚠️  Warning: %v\n", err)
		} else {
			fmt.Println("✓ Auto-merge & branch deletion enabled")
		}

		// 4. Collaborator
		if cfg.Collaborator != "" {
			fmt.Printf("4. Ensuring collaborator access for @%s... ", cfg.Collaborator)
			cfgCollab := ConfigureCollaborator
			if cfg.ConfigureCollabFunc != nil {
				cfgCollab = cfg.ConfigureCollabFunc
			}
			if err := cfgCollab(ctx, cfg); err != nil {
				fmt.Printf("⚠️  Warning: %v\n", err)
			} else {
				fmt.Println("✓ Done")
			}
		}

		// If a ruleset named "main" already exists and is active, temporarily pause it
		// during git push so the push is not rejected by GH013.
		checkRulesFn := CheckExistingRuleset
		if cfg.CheckExistingRulesetFn != nil {
			checkRulesFn = cfg.CheckExistingRulesetFn
		}
		existingID, enforcement, err := checkRulesFn(ctx, cfg, "main")
		if err == nil && existingID > 0 && enforcement == "active" {
			setEnforceFn := SetRulesetEnforcement
			if cfg.SetRulesetEnforceFn != nil {
				setEnforceFn = cfg.SetRulesetEnforceFn
			}
			if err := setEnforceFn(ctx, cfg, existingID, "disabled"); err == nil {
				pausedRulesetID = existingID
			}
		}
	} else {
		fmt.Printf("3-4. Note: Admin permissions required for repository settings and collaborators on %s.\n", cfg.Repo)
	}

	// Ensure any paused ruleset is restored if an error occurs before ConfigureRulesets
	if pausedRulesetID > 0 {
		defer func() {
			if pausedRulesetID > 0 {
				setEnforceFn := SetRulesetEnforcement
				if cfg.SetRulesetEnforceFn != nil {
					setEnforceFn = cfg.SetRulesetEnforceFn
				}
				_ = setEnforceFn(ctx, cfg, pausedRulesetID, "active")
			}
		}()
	}

	// 5. Git Push (executed BEFORE ruleset enforcement to prevent GH013 push rejection)
	if !cfg.SkipPush {
		fmt.Print("5. Committing and pushing scaffolding changes to remote... ")
		pushFn := GitCommitAndPush
		if cfg.CommitAndPushFunc != nil {
			pushFn = cfg.CommitAndPushFunc
		}
		if err := pushFn(ctx, cfg); err != nil {
			return fmt.Errorf("failed git commit/push: %w", err)
		}
		fmt.Println("✓ Pushed to origin")
	} else {
		fmt.Println("5. Skipping git push (--skip-push specified)")
	}

	// 6. Ruleset (configured AFTER git push has populated the branch)
	if perms.Admin {
		fmt.Print("6. Configuring GitHub Ruleset for default branch... ")
		cfgRules := ConfigureRulesets
		if cfg.ConfigureRulesetsFunc != nil {
			cfgRules = cfg.ConfigureRulesetsFunc
		}
		if err := cfgRules(ctx, cfg); err != nil {
			return fmt.Errorf("failed configuring rulesets: %w", err)
		}
		pausedRulesetID = 0 // Successfully configured and enforced
		fmt.Println("✓ Ruleset 'main' enforced (CODEOWNERS review, required checks, squash merge)")
	} else {
		fmt.Printf("6. Note: Admin permissions required to configure branch ruleset on %s.\n", cfg.Repo)
		fmt.Printf("   Current token has push=%v, admin=%v. Run 'speculate init --token=<admin-token>' as repository owner to apply rulesets.\n", perms.Push, perms.Admin)
	}

	fmt.Printf("\n🎉 Repository %s initialization completed!\n", cfg.Repo)
	return nil
}
