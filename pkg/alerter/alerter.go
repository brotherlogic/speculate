package alerter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Issue represents a GitHub issue response summary.
type Issue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
}

// IssueRequest represents the payload for creating a GitHub issue.
type IssueRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

// AlertResult represents the outcome of the failure alerting process.
type AlertResult struct {
	Deduplicated bool
	IssueNumber  int
	IssueURL     string
}

// GitHubIssueClient defines operations needed to query and create GitHub issues.
type GitHubIssueClient interface {
	SearchIssues(ctx context.Context, owner, repo, query string) ([]*Issue, error)
	CreateIssue(ctx context.Context, owner, repo string, req *IssueRequest) (*Issue, error)
}

// RealGitHubClient implements GitHubIssueClient via GitHub REST API over HTTPS.
type RealGitHubClient struct {
	token      string
	httpClient *http.Client
	baseURL    string
}

// NewRealGitHubClient constructs a new RealGitHubClient.
func NewRealGitHubClient(token string, baseURL ...string) *RealGitHubClient {
	url := "https://api.github.com"
	if len(baseURL) > 0 && baseURL[0] != "" {
		url = strings.TrimRight(baseURL[0], "/")
	}
	return &RealGitHubClient{
		token: token,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		baseURL: url,
	}
}

type searchIssuesResponse struct {
	TotalCount int      `json:"total_count"`
	Items      []*Issue `json:"items"`
}

// SearchIssues queries GitHub issue search for a specific repository.
func (c *RealGitHubClient) SearchIssues(ctx context.Context, owner, repo, query string) ([]*Issue, error) {
	fullQuery := fmt.Sprintf("repo:%s/%s %s", owner, repo, query)
	reqURL := fmt.Sprintf("%s/search/issues?q=%s", c.baseURL, url.QueryEscape(fullQuery))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating search request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing search request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("search issues failed (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var searchResp searchIssuesResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("decoding search response: %w", err)
	}

	return searchResp.Items, nil
}

// CreateIssue creates a new issue on GitHub via REST API.
func (c *RealGitHubClient) CreateIssue(ctx context.Context, owner, repo string, issueReq *IssueRequest) (*Issue, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/issues", c.baseURL, owner, repo)

	reqBytes, err := json.Marshal(issueReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling issue request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("creating post request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing create issue request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create issue failed (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var created Issue
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, fmt.Errorf("decoding created issue response: %w", err)
	}

	return &created, nil
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

// ProberAlertConfig holds parameters needed to report a prober failure.
type ProberAlertConfig struct {
	Mode       string
	TargetRepo string
	TargetDir  string
	IssueRepo  string
	Duration   time.Duration
	Err        error
	Time       time.Time
}

// BuildAlertTitle generates a deterministic title for prober alert issues.
func BuildAlertTitle(mode, targetRepo string) string {
	targetShort := targetRepo
	if owner, repo, err := ParseRepo(targetRepo); err == nil {
		targetShort = owner + "/" + repo
	}
	return fmt.Sprintf("[PROBER FAILURE] Speculate prober (%s) failed on %s", mode, targetShort)
}

// BuildDiagnosticReport generates the formatted Markdown diagnostic report for an issue.
func BuildDiagnosticReport(cfg ProberAlertConfig) string {
	timestamp := cfg.Time
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	timestampStr := timestamp.Format(time.RFC3339)

	var errDetails string
	if cfg.Err != nil {
		errDetails = cfg.Err.Error()
	} else {
		errDetails = "Unknown prober error"
	}

	targetRepoStr := cfg.TargetRepo
	if targetRepoStr == "" {
		targetRepoStr = "N/A"
	}

	return fmt.Sprintf(`## 🚨 Speculate Prober Failure Alert

The automated prober encountered a failure during execution.

### Diagnostic Report
- **Timestamp:** %s
- **Prober Mode:** `+"`%s`"+`
- **Target Repository:** %s
- **Execution Duration:** %s
- **Error Details:**
`+"```"+`
%s
`+"```"+`

### Operator Troubleshooting Steps
1. **Inspect Prober Logs:** In Kubernetes, check prober pod logs:
   `+"`kubectl logs -n speculate -l app=speculate-prober`"+`
2. **Verify Target Repository:** Ensure %s is accessible and matches the specification.
3. **Check Service Endpoints:** Verify any LLM endpoints (e.g. Ollama) or dependent services are operational.
4. **Resolution:** Once resolved, close this issue. The prober will not file duplicate alert issues while an issue with this title remains open.
`, timestampStr, cfg.Mode, targetRepoStr, cfg.Duration.Round(time.Millisecond).String(), errDetails, targetRepoStr)
}

// HandleProberFailure handles searching for duplicates and creating an issue on failure.
func HandleProberFailure(ctx context.Context, client GitHubIssueClient, cfg ProberAlertConfig) (*AlertResult, error) {
	if cfg.Err == nil {
		return nil, nil
	}

	repoFullName := cfg.IssueRepo
	if repoFullName == "" || repoFullName == "target" {
		repoFullName = cfg.TargetRepo
	}
	owner, repo, err := ParseRepo(repoFullName)
	if err != nil {
		return nil, fmt.Errorf("parsing alert issue repository: %w", err)
	}

	alertTitle := BuildAlertTitle(cfg.Mode, cfg.TargetRepo)

	// Search for existing open alert issues with the same title
	query := fmt.Sprintf("is:issue is:open in:title %q", alertTitle)
	issues, err := client.SearchIssues(ctx, owner, repo, query)
	if err != nil {
		return nil, fmt.Errorf("searching existing alert issues on %s/%s: %w", owner, repo, err)
	}

	for _, issue := range issues {
		if issue.State != "closed" && issue.Title == alertTitle {
			return &AlertResult{
				Deduplicated: true,
				IssueNumber:  issue.Number,
				IssueURL:     issue.HTMLURL,
			}, nil
		}
	}

	body := BuildDiagnosticReport(cfg)
	req := &IssueRequest{
		Title:  alertTitle,
		Body:   body,
		Labels: []string{"bug", "prober-failure"},
	}

	created, err := client.CreateIssue(ctx, owner, repo, req)
	if err != nil {
		return nil, fmt.Errorf("creating alert issue on %s/%s: %w", owner, repo, err)
	}

	return &AlertResult{
		Deduplicated: false,
		IssueNumber:  created.Number,
		IssueURL:     created.HTMLURL,
	}, nil
}

// ResolveToken retrieves a GitHub token from the provided value or environment variables.
func ResolveToken(token string) string {
	if token != "" {
		return token
	}
	if t := os.Getenv("GH_TOKEN"); t != "" {
		return t
	}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	// Fallback to gh auth token if available
	cmd := exec.Command("gh", "auth", "token")
	if out, err := cmd.Output(); err == nil {
		t := strings.TrimSpace(string(out))
		if t != "" {
			return t
		}
	}
	return ""
}
