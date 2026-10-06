package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v50/github"
	"golang.org/x/oauth2"
)

// ClientOption allows customizing RealClient.
type ClientOption func(*RealClient)

// WithBaseURL sets a custom GitHub API base URL (useful for testing or GitHub Enterprise).
func WithBaseURL(rawURL string) ClientOption {
	return func(c *RealClient) {
		if rawURL != "" {
			if !strings.HasSuffix(rawURL, "/") {
				rawURL += "/"
			}
			if u, err := url.Parse(rawURL); err == nil {
				c.ghClient.BaseURL = u
			}
		}
	}
}

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *RealClient) {
		if client != nil {
			c.httpClient = client
			c.ghClient = github.NewClient(client)
		}
	}
}

// RealClient implements Client using the official go-github library.
type RealClient struct {
	owner      string
	repo       string
	token      string
	httpClient *http.Client
	ghClient   *github.Client
}

// NewRealClient constructs a GitHub Client adapter for the specified repository.
func NewRealClient(token, owner, repo string, opts ...ClientOption) *RealClient {
	var tc *http.Client
	if token != "" {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
		tc = oauth2.NewClient(context.Background(), ts)
	} else {
		tc = &http.Client{Timeout: 30 * time.Second}
	}

	c := &RealClient{
		owner:      owner,
		repo:       repo,
		token:      token,
		httpClient: tc,
		ghClient:   github.NewClient(tc),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

func (c *RealClient) Owner() string { return c.owner }
func (c *RealClient) Repo() string  { return c.repo }

// GetBranch retrieves branch information including the head commit SHA.
func (c *RealClient) GetBranch(ctx context.Context, branch string) (*Branch, error) {
	b, _, err := c.ghClient.Repositories.GetBranch(ctx, c.owner, c.repo, branch, true)
	if err != nil {
		return nil, fmt.Errorf("getting branch %q: %w", branch, err)
	}
	sha := ""
	if b.Commit != nil {
		sha = b.Commit.GetSHA()
	}
	return &Branch{
		Name: b.GetName(),
		SHA:  sha,
	}, nil
}

var shaRegex = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// CreateBranch creates a new branch pointing to baseBranchOrSHA.
func (c *RealClient) CreateBranch(ctx context.Context, branch, baseBranchOrSHA string) (*Branch, error) {
	sha := baseBranchOrSHA
	if !shaRegex.MatchString(baseBranchOrSHA) {
		baseBranch, err := c.GetBranch(ctx, baseBranchOrSHA)
		if err != nil {
			return nil, fmt.Errorf("resolving base ref %q: %w", baseBranchOrSHA, err)
		}
		sha = baseBranch.SHA
	}

	refName := "refs/heads/" + branch
	ref := &github.Reference{
		Ref: github.String(refName),
		Object: &github.GitObject{
			SHA: github.String(sha),
		},
	}

	_, _, err := c.ghClient.Git.CreateRef(ctx, c.owner, c.repo, ref)
	if err != nil {
		return nil, fmt.Errorf("creating branch %q pointing to %s: %w", branch, sha, err)
	}

	return &Branch{
		Name: branch,
		SHA:  sha,
	}, nil
}

// DeleteBranch deletes a branch reference on the remote repository.
func (c *RealClient) DeleteBranch(ctx context.Context, branch string) error {
	refName := "refs/heads/" + branch
	_, err := c.ghClient.Git.DeleteRef(ctx, c.owner, c.repo, refName)
	if err != nil {
		return fmt.Errorf("deleting branch %q: %w", branch, err)
	}
	return nil
}

// EnsureLabels verifies required labels exist on the repo and creates any that are missing.
func (c *RealClient) EnsureLabels(ctx context.Context, labels []Label) error {
	for _, l := range labels {
		existing, resp, err := c.ghClient.Issues.GetLabel(ctx, c.owner, c.repo, l.Name)
		if err == nil && existing != nil {
			// Label exists; ensure color/description match
			if existing.GetColor() != l.Color || existing.GetDescription() != l.Description {
				_, _, _ = c.ghClient.Issues.EditLabel(ctx, c.owner, c.repo, l.Name, &github.Label{
					Name:        github.String(l.Name),
					Color:       github.String(l.Color),
					Description: github.String(l.Description),
				})
			}
			continue
		}

		if resp != nil && resp.StatusCode == http.StatusNotFound {
			_, _, createErr := c.ghClient.Issues.CreateLabel(ctx, c.owner, c.repo, &github.Label{
				Name:        github.String(l.Name),
				Color:       github.String(l.Color),
				Description: github.String(l.Description),
			})
			if createErr != nil {
				return fmt.Errorf("creating label %q: %w", l.Name, createErr)
			}
			continue
		}

		if err != nil {
			return fmt.Errorf("checking label %q: %w", l.Name, err)
		}
	}
	return nil
}

// CreateIssue creates an issue with specified title, body, and labels.
func (c *RealClient) CreateIssue(ctx context.Context, req *CreateIssueRequest) (*Issue, error) {
	issueReq := &github.IssueRequest{
		Title: github.String(req.Title),
		Body:  github.String(req.Body),
	}
	if len(req.Labels) > 0 {
		issueReq.Labels = &req.Labels
	}
	if len(req.Assignees) > 0 {
		issueReq.Assignees = &req.Assignees
	}

	issue, _, err := c.ghClient.Issues.Create(ctx, c.owner, c.repo, issueReq)
	if err != nil {
		return nil, fmt.Errorf("creating issue: %w", err)
	}

	var labels []string
	for _, l := range issue.Labels {
		if l.Name != nil {
			labels = append(labels, *l.Name)
		}
	}

	return &Issue{
		Number:  issue.GetNumber(),
		Title:   issue.GetTitle(),
		Body:    issue.GetBody(),
		State:   issue.GetState(),
		Labels:  labels,
		HTMLURL: issue.GetHTMLURL(),
	}, nil
}

// GetIssue retrieves an issue by number.
func (c *RealClient) GetIssue(ctx context.Context, number int) (*Issue, error) {
	issue, _, err := c.ghClient.Issues.Get(ctx, c.owner, c.repo, number)
	if err != nil {
		return nil, fmt.Errorf("getting issue #%d: %w", number, err)
	}

	var labels []string
	for _, l := range issue.Labels {
		if l.Name != nil {
			labels = append(labels, *l.Name)
		}
	}

	return &Issue{
		Number:  issue.GetNumber(),
		Title:   issue.GetTitle(),
		Body:    issue.GetBody(),
		State:   issue.GetState(),
		Labels:  labels,
		HTMLURL: issue.GetHTMLURL(),
	}, nil
}

// CloseIssue updates the state of an issue to closed.
func (c *RealClient) CloseIssue(ctx context.Context, number int) error {
	state := "closed"
	_, _, err := c.ghClient.Issues.Edit(ctx, c.owner, c.repo, number, &github.IssueRequest{
		State: github.String(state),
	})
	if err != nil {
		return fmt.Errorf("closing issue #%d: %w", number, err)
	}
	return nil
}

// CreatePullRequest opens a pull request.
func (c *RealClient) CreatePullRequest(ctx context.Context, req *CreatePRRequest) (*PullRequest, error) {
	prReq := &github.NewPullRequest{
		Title: github.String(req.Title),
		Head:  github.String(req.Head),
		Base:  github.String(req.Base),
		Body:  github.String(req.Body),
	}

	pr, _, err := c.ghClient.PullRequests.Create(ctx, c.owner, c.repo, prReq)
	if err != nil {
		return nil, fmt.Errorf("creating PR %q (%s -> %s): %w", req.Title, req.Head, req.Base, err)
	}

	return &PullRequest{
		Number:  pr.GetNumber(),
		Title:   pr.GetTitle(),
		Head:    pr.GetHead().GetRef(),
		Base:    pr.GetBase().GetRef(),
		State:   pr.GetState(),
		Merged:  pr.GetMerged(),
		HTMLURL: pr.GetHTMLURL(),
	}, nil
}

// GetPullRequest fetches PR details.
func (c *RealClient) GetPullRequest(ctx context.Context, number int) (*PullRequest, error) {
	pr, _, err := c.ghClient.PullRequests.Get(ctx, c.owner, c.repo, number)
	if err != nil {
		return nil, fmt.Errorf("getting PR #%d: %w", number, err)
	}

	return &PullRequest{
		Number:  pr.GetNumber(),
		Title:   pr.GetTitle(),
		Head:    pr.GetHead().GetRef(),
		Base:    pr.GetBase().GetRef(),
		State:   pr.GetState(),
		Merged:  pr.GetMerged(),
		HTMLURL: pr.GetHTMLURL(),
	}, nil
}

// SquashMergePullRequest performs a squash merge on a pull request.
func (c *RealClient) SquashMergePullRequest(ctx context.Context, prNumber int, commitTitle, commitMessage string) (*MergeResult, error) {
	opts := &github.PullRequestOptions{
		MergeMethod: "squash",
	}
	if commitTitle != "" {
		opts.CommitTitle = commitTitle
	}

	res, _, err := c.ghClient.PullRequests.Merge(ctx, c.owner, c.repo, prNumber, commitMessage, opts)
	if err != nil {
		return nil, fmt.Errorf("squash merging PR #%d: %w", prNumber, err)
	}

	return &MergeResult{
		SHA:     res.GetSHA(),
		Merged:  res.GetMerged(),
		Message: res.GetMessage(),
	}, nil
}

// GetFileContent retrieves file content and its SHA from a given git ref.
func (c *RealClient) GetFileContent(ctx context.Context, path, ref string) (*FileContent, error) {
	opts := &github.RepositoryContentGetOptions{}
	if ref != "" {
		opts.Ref = ref
	}

	fileContent, _, resp, err := c.ghClient.Repositories.GetContents(ctx, c.owner, c.repo, path, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("file %q not found on ref %q: %w", path, ref, err)
		}
		return nil, fmt.Errorf("getting content for %q: %w", path, err)
	}

	if fileContent == nil {
		return nil, fmt.Errorf("path %q is a directory, not a file", path)
	}

	contentStr, err := fileContent.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decoding content for %q: %w", path, err)
	}

	return &FileContent{
		Content: contentStr,
		SHA:     fileContent.GetSHA(),
	}, nil
}

// UpdateFile commits updated file content to a branch.
func (c *RealClient) UpdateFile(ctx context.Context, path, content, sha, branch, commitMessage string) error {
	opts := &github.RepositoryContentFileOptions{
		Message: github.String(commitMessage),
		Content: []byte(content),
		Branch:  github.String(branch),
	}
	if sha != "" {
		opts.SHA = github.String(sha)
	}

	_, _, err := c.ghClient.Repositories.UpdateFile(ctx, c.owner, c.repo, path, opts)
	if err != nil {
		return fmt.Errorf("updating file %q on branch %q: %w", path, branch, err)
	}

	return nil
}

// UpdateReadmeBadge updates or inserts the Spec Alignment badge in README.md on the specified branch.
func (c *RealClient) UpdateReadmeBadge(ctx context.Context, branch, badgeMarkdown string) error {
	var currentContent string
	var currentSHA string

	file, err := c.GetFileContent(ctx, "README.md", branch)
	if err == nil && file != nil {
		currentContent = file.Content
		currentSHA = file.SHA
	}

	updated := InjectOrUpdateBadge(currentContent, badgeMarkdown)
	if updated == currentContent {
		// No modification needed
		return nil
	}

	commitMsg := "chore: update spec alignment badge"
	return c.UpdateFile(ctx, "README.md", updated, currentSHA, branch, commitMsg)
}
