package alerter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type mockGitHubClient struct {
	searchIssuesFunc func(ctx context.Context, owner, repo, query string) ([]*Issue, error)
	createIssueFunc  func(ctx context.Context, owner, repo string, req *IssueRequest) (*Issue, error)
}

func (m *mockGitHubClient) SearchIssues(ctx context.Context, owner, repo, query string) ([]*Issue, error) {
	if m.searchIssuesFunc != nil {
		return m.searchIssuesFunc(ctx, owner, repo, query)
	}
	return nil, nil
}

func (m *mockGitHubClient) CreateIssue(ctx context.Context, owner, repo string, req *IssueRequest) (*Issue, error) {
	if m.createIssueFunc != nil {
		return m.createIssueFunc(ctx, owner, repo, req)
	}
	return nil, nil
}

func TestParseRepo(t *testing.T) {
	tests := []struct {
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{
			input:     "brotherlogic/speculate",
			wantOwner: "brotherlogic",
			wantRepo:  "speculate",
		},
		{
			input:     "https://github.com/brotherlogic/speculate-kv",
			wantOwner: "brotherlogic",
			wantRepo:  "speculate-kv",
		},
		{
			input:     "https://github.com/brotherlogic/speculate-kv.git",
			wantOwner: "brotherlogic",
			wantRepo:  "speculate-kv",
		},
		{
			input:     "git@github.com:brotherlogic/speculate-kv.git",
			wantOwner: "brotherlogic",
			wantRepo:  "speculate-kv",
		},
		{
			input:   "invalid-url-format",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		owner, repo, err := ParseRepo(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseRepo(%q) expected error, got nil", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRepo(%q) unexpected error: %v", tt.input, err)
		}
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("ParseRepo(%q) = (%q, %q), want (%q, %q)", tt.input, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}
}

func TestBuildAlertTitle(t *testing.T) {
	title := BuildAlertTitle("evaluator", "https://github.com/brotherlogic/speculate-kv")
	want := "[PROBER FAILURE] Speculate prober (evaluator) failed on brotherlogic/speculate-kv"
	if title != want {
		t.Errorf("BuildAlertTitle got %q, want %q", title, want)
	}
}

func TestBuildDiagnosticReport(t *testing.T) {
	cfg := ProberAlertConfig{
		Mode:       "evaluator",
		TargetRepo: "brotherlogic/speculate-kv",
		Duration:   1234 * time.Millisecond,
		Err:        errors.New("mutation probe expected RED test failure"),
		Time:       time.Date(2026, 9, 24, 5, 0, 0, 0, time.UTC),
	}

	report := BuildDiagnosticReport(cfg)
	if !strings.Contains(report, "mutation probe expected RED test failure") {
		t.Errorf("expected report to contain error message, got: %s", report)
	}
	if !strings.Contains(report, "brotherlogic/speculate-kv") {
		t.Errorf("expected report to contain target repo, got: %s", report)
	}
	if !strings.Contains(report, "`evaluator`") {
		t.Errorf("expected report to contain prober mode, got: %s", report)
	}
	if !strings.Contains(report, "2026-09-24T05:00:00Z") {
		t.Errorf("expected report to contain timestamp, got: %s", report)
	}
}

func TestHandleProberFailure_NoError(t *testing.T) {
	client := &mockGitHubClient{}
	cfg := ProberAlertConfig{
		Err: nil,
	}

	res, err := HandleProberFailure(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result for nil error, got %+v", res)
	}
}

func TestHandleProberFailure_Deduplicated(t *testing.T) {
	expectedTitle := BuildAlertTitle("evaluator", "brotherlogic/speculate-kv")
	client := &mockGitHubClient{
		searchIssuesFunc: func(ctx context.Context, owner, repo, query string) ([]*Issue, error) {
			return []*Issue{
				{
					Number:  42,
					Title:   expectedTitle,
					State:   "open",
					HTMLURL: "https://github.com/brotherlogic/speculate/issues/42",
				},
			}, nil
		},
		createIssueFunc: func(ctx context.Context, owner, repo string, req *IssueRequest) (*Issue, error) {
			t.Fatal("CreateIssue should not be called when issue is deduplicated")
			return nil, nil
		},
	}

	cfg := ProberAlertConfig{
		Mode:       "evaluator",
		TargetRepo: "brotherlogic/speculate-kv",
		IssueRepo:  "brotherlogic/speculate",
		Err:        errors.New("something went wrong"),
	}

	res, err := HandleProberFailure(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Deduplicated {
		t.Errorf("expected Deduplicated to be true, got false")
	}
	if res.IssueNumber != 42 {
		t.Errorf("expected IssueNumber 42, got %d", res.IssueNumber)
	}
	if res.IssueURL != "https://github.com/brotherlogic/speculate/issues/42" {
		t.Errorf("expected IssueURL, got %s", res.IssueURL)
	}
}

func TestHandleProberFailure_CreatesIssue(t *testing.T) {
	var createdReq *IssueRequest
	var createdOwner, createdRepo string

	client := &mockGitHubClient{
		searchIssuesFunc: func(ctx context.Context, owner, repo, query string) ([]*Issue, error) {
			return []*Issue{}, nil
		},
		createIssueFunc: func(ctx context.Context, owner, repo string, req *IssueRequest) (*Issue, error) {
			createdOwner = owner
			createdRepo = repo
			createdReq = req
			return &Issue{
				Number:  99,
				Title:   req.Title,
				State:   "open",
				HTMLURL: "https://github.com/brotherlogic/speculate/issues/99",
			}, nil
		},
	}

	cfg := ProberAlertConfig{
		Mode:       "evaluator",
		TargetRepo: "brotherlogic/speculate-kv",
		IssueRepo:  "brotherlogic/speculate",
		Err:        errors.New("evaluation failure"),
	}

	res, err := HandleProberFailure(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Deduplicated {
		t.Errorf("expected Deduplicated to be false, got true")
	}
	if res.IssueNumber != 99 {
		t.Errorf("expected IssueNumber 99, got %d", res.IssueNumber)
	}
	if createdOwner != "brotherlogic" || createdRepo != "speculate" {
		t.Errorf("created on (%s/%s), want (brotherlogic/speculate)", createdOwner, createdRepo)
	}
	if createdReq == nil || !strings.Contains(createdReq.Body, "evaluation failure") {
		t.Errorf("expected created issue body to contain error details")
	}
}

func TestHandleProberFailure_SearchError(t *testing.T) {
	client := &mockGitHubClient{
		searchIssuesFunc: func(ctx context.Context, owner, repo, query string) ([]*Issue, error) {
			return nil, errors.New("search api error")
		},
	}

	cfg := ProberAlertConfig{
		Mode:       "evaluator",
		TargetRepo: "brotherlogic/speculate-kv",
		IssueRepo:  "brotherlogic/speculate",
		Err:        errors.New("eval error"),
	}

	_, err := HandleProberFailure(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "search api error") {
		t.Errorf("expected search api error, got: %v", err)
	}
}

func TestHandleProberFailure_InvalidIssueRepo(t *testing.T) {
	client := &mockGitHubClient{}
	cfg := ProberAlertConfig{
		Mode:       "evaluator",
		TargetRepo: "invalid-repo",
		IssueRepo:  "invalid-repo",
		Err:        errors.New("eval error"),
	}

	_, err := HandleProberFailure(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("expected error parsing invalid repo, got nil")
	}
}

func TestRealGitHubClient_SearchAndCreate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/search/issues") {
			resp := searchIssuesResponse{
				TotalCount: 1,
				Items: []*Issue{
					{
						Number:  10,
						Title:   "Existing Issue",
						State:   "open",
						HTMLURL: "https://github.com/brotherlogic/speculate/issues/10",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if r.Method == http.MethodPost && r.URL.Path == "/repos/brotherlogic/speculate/issues" {
			var req IssueRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Issue{
				Number:  11,
				Title:   req.Title,
				State:   "open",
				HTMLURL: "https://github.com/brotherlogic/speculate/issues/11",
			})
			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewRealGitHubClient("test-token", server.URL)

	// Test SearchIssues
	issues, err := client.SearchIssues(context.Background(), "brotherlogic", "speculate", "test")
	if err != nil {
		t.Fatalf("SearchIssues unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 10 {
		t.Fatalf("expected 1 issue with number 10, got %+v", issues)
	}

	// Test CreateIssue
	created, err := client.CreateIssue(context.Background(), "brotherlogic", "speculate", &IssueRequest{
		Title: "New Alert",
		Body:  "Alert details",
	})
	if err != nil {
		t.Fatalf("CreateIssue unexpected error: %v", err)
	}
	if created.Number != 11 || created.Title != "New Alert" {
		t.Fatalf("expected created issue number 11, got %+v", created)
	}
}

func TestRealGitHubClient_Errors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewRealGitHubClient("test-token", server.URL)

	_, err := client.SearchIssues(context.Background(), "brotherlogic", "speculate", "test")
	if err == nil {
		t.Fatal("expected SearchIssues error on 500, got nil")
	}

	_, err = client.CreateIssue(context.Background(), "brotherlogic", "speculate", &IssueRequest{Title: "fail"})
	if err == nil {
		t.Fatal("expected CreateIssue error on 500, got nil")
	}
}

func TestResolveToken(t *testing.T) {
	if tok := ResolveToken("explicit-token"); tok != "explicit-token" {
		t.Errorf("ResolveToken got %q, want explicit-token", tok)
	}

	t.Setenv("GH_TOKEN", "env-gh-token")
	if tok := ResolveToken(""); tok != "env-gh-token" {
		t.Errorf("ResolveToken with GH_TOKEN got %q, want env-gh-token", tok)
	}
}
