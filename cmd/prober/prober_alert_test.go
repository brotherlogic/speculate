package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brotherlogic/speculate/pkg/alerter"
)

type testGitHubClient struct {
	searchIssuesFunc func(ctx context.Context, owner, repo, query string) ([]*alerter.Issue, error)
	createIssueFunc  func(ctx context.Context, owner, repo string, req *alerter.IssueRequest) (*alerter.Issue, error)
}

func (m *testGitHubClient) SearchIssues(ctx context.Context, owner, repo, query string) ([]*alerter.Issue, error) {
	if m.searchIssuesFunc != nil {
		return m.searchIssuesFunc(ctx, owner, repo, query)
	}
	return nil, nil
}

func (m *testGitHubClient) CreateIssue(ctx context.Context, owner, repo string, req *alerter.IssueRequest) (*alerter.Issue, error) {
	if m.createIssueFunc != nil {
		return m.createIssueFunc(ctx, owner, repo, req)
	}
	return nil, nil
}

func TestFileProberIssue_Success(t *testing.T) {
	origFactory := issueClientFactory
	defer func() { issueClientFactory = origFactory }()

	var created bool
	issueClientFactory = func(token string) alerter.GitHubIssueClient {
		return &testGitHubClient{
			searchIssuesFunc: func(ctx context.Context, owner, repo, query string) ([]*alerter.Issue, error) {
				return []*alerter.Issue{}, nil
			},
			createIssueFunc: func(ctx context.Context, owner, repo string, req *alerter.IssueRequest) (*alerter.Issue, error) {
				created = true
				return &alerter.Issue{
					Number:  123,
					Title:   req.Title,
					State:   "open",
					HTMLURL: "https://github.com/brotherlogic/speculate/issues/123",
				}, nil
			},
		}
	}

	fileProberIssue("brotherlogic/speculate", "evaluator", "https://github.com/brotherlogic/speculate-kv", "/tmp/kv", "test-token", 2*time.Second, errors.New("simulated error"))

	if !created {
		t.Errorf("expected issue to be created")
	}
}

func TestFileProberIssue_Deduplicated(t *testing.T) {
	origFactory := issueClientFactory
	defer func() { issueClientFactory = origFactory }()

	var created bool
	issueClientFactory = func(token string) alerter.GitHubIssueClient {
		return &testGitHubClient{
			searchIssuesFunc: func(ctx context.Context, owner, repo, query string) ([]*alerter.Issue, error) {
				title := alerter.BuildAlertTitle("evaluator", "https://github.com/brotherlogic/speculate-kv")
				return []*alerter.Issue{
					{
						Number:  100,
						Title:   title,
						State:   "open",
						HTMLURL: "https://github.com/brotherlogic/speculate/issues/100",
					},
				}, nil
			},
			createIssueFunc: func(ctx context.Context, owner, repo string, req *alerter.IssueRequest) (*alerter.Issue, error) {
				created = true
				return nil, nil
			},
		}
	}

	fileProberIssue("brotherlogic/speculate", "evaluator", "https://github.com/brotherlogic/speculate-kv", "/tmp/kv", "test-token", 2*time.Second, errors.New("simulated error"))

	if created {
		t.Errorf("expected issue NOT to be created when deduplicated")
	}
}

func TestFileProberIssue_NoToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	// Should not panic or error out
	fileProberIssue("brotherlogic/speculate", "evaluator", "https://github.com/brotherlogic/speculate-kv", "/tmp/kv", "", 2*time.Second, errors.New("simulated error"))
}
