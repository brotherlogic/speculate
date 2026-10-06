package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Standard labels used in the Speculate autonomous alignment loop.
const (
	LabelAgenticLoop = "speculate-agentic-loop"
	LabelAlign       = "speculate-align"
	LabelStalled     = "speculate-stalled"
)

// Branch represents a Git branch reference.
type Branch struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// Label defines a GitHub label with name, color, and description.
type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// RequiredLabels returns the set of labels required for the Speculate lifecycle.
func RequiredLabels() []Label {
	return []Label{
		{
			Name:        LabelAgenticLoop,
			Color:       "0e8a16",
			Description: "Active agentic loop generating tests",
		},
		{
			Name:        LabelAlign,
			Color:       "fbca04",
			Description: "Autonomous scaffolding alignment in progress",
		},
		{
			Name:        LabelStalled,
			Color:       "d93f0b",
			Description: "Agentic alignment stalled; human attention required",
		},
	}
}

// CreateIssueRequest holds parameters for opening a GitHub issue.
type CreateIssueRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

// Issue represents a GitHub issue.
type Issue struct {
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	State   string   `json:"state"`
	Labels  []string `json:"labels"`
	HTMLURL string   `json:"html_url"`
}

// CreatePRRequest holds parameters for creating a Pull Request.
type CreatePRRequest struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}

// PullRequest represents a GitHub Pull Request.
type PullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
}

// MergeResult represents the outcome of a Pull Request merge operation.
type MergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// FileContent represents a file retrieved from GitHub contents API.
type FileContent struct {
	Content string `json:"content"`
	SHA     string `json:"sha"`
}

// Client defines the interface for interacting with GitHub on target repositories.
type Client interface {
	Owner() string
	Repo() string

	// Branch management
	GetBranch(ctx context.Context, branch string) (*Branch, error)
	CreateBranch(ctx context.Context, branch, baseBranchOrSHA string) (*Branch, error)
	DeleteBranch(ctx context.Context, branch string) error

	// Label management
	EnsureLabels(ctx context.Context, labels []Label) error

	// Issue management
	CreateIssue(ctx context.Context, req *CreateIssueRequest) (*Issue, error)
	GetIssue(ctx context.Context, number int) (*Issue, error)
	CloseIssue(ctx context.Context, number int) error

	// Pull Request management
	CreatePullRequest(ctx context.Context, req *CreatePRRequest) (*PullRequest, error)
	GetPullRequest(ctx context.Context, number int) (*PullRequest, error)
	SquashMergePullRequest(ctx context.Context, prNumber int, commitTitle, commitMessage string) (*MergeResult, error)

	// File and README badge operations
	GetFileContent(ctx context.Context, path, ref string) (*FileContent, error)
	UpdateFile(ctx context.Context, path, content, sha, branch, commitMessage string) error
	UpdateReadmeBadge(ctx context.Context, branch, badgeMarkdown string) error
}

// ParseRepo extracts owner and repo name from GitHub URL or "owner/repo" string.
func ParseRepo(raw string) (string, string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".git")
	if idx := strings.Index(s, "github.com/"); idx != -1 {
		s = s[idx+len("github.com/"):]
	} else if idx := strings.Index(s, "github.com:"); idx != -1 {
		s = s[idx+len("github.com:"):]
	}
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], nil
	}
	return "", "", fmt.Errorf("unable to parse repository owner and name from %q", raw)
}

var badgePattern = regexp.MustCompile(`(?m)(?:\[?!\[(?:Spec Alignment|spec-alignment)\]\([^)]+\)\]?\([^)]+\)?|!\[(?:Spec Alignment|spec-alignment)\]\([^)]+\))`)

// InjectOrUpdateBadge replaces an existing Spec Alignment badge in README markdown,
// or inserts it under the top-level header if absent.
func InjectOrUpdateBadge(readmeContent, badgeMarkdown string) string {
	cleanBadge := strings.TrimSpace(badgeMarkdown)

	// Case 1: Existing badge found -> replace it
	if badgePattern.MatchString(readmeContent) {
		return badgePattern.ReplaceAllString(readmeContent, cleanBadge)
	}

	// Case 2: No badge yet -> place after top-level H1 header if present
	h1Pattern := regexp.MustCompile(`(?m)^(#\s+.*)$`)
	if loc := h1Pattern.FindStringIndex(readmeContent); loc != nil {
		headerEnd := loc[1]
		prefix := readmeContent[:headerEnd]
		suffix := readmeContent[headerEnd:]
		return prefix + "\n\n" + cleanBadge + suffix
	}

	// Case 3: No H1 header found -> prepend to document
	if strings.TrimSpace(readmeContent) == "" {
		return cleanBadge + "\n"
	}
	return cleanBadge + "\n\n" + readmeContent
}
