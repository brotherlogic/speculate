package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v50/github"
)

func TestParseRepo(t *testing.T) {
	tests := []struct {
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"brotherlogic/speculate-kv", "brotherlogic", "speculate-kv", false},
		{"https://github.com/brotherlogic/speculate-kv", "brotherlogic", "speculate-kv", false},
		{"https://github.com/brotherlogic/speculate-kv.git", "brotherlogic", "speculate-kv", false},
		{"git@github.com:brotherlogic/speculate-kv.git", "brotherlogic", "speculate-kv", false},
		{"invalid-format", "", "", true},
		{"", "", "", true},
	}

	for _, tt := range tests {
		owner, repo, err := ParseRepo(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRepo(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("ParseRepo(%q) = (%q, %q), want (%q, %q)", tt.input, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}
}

func TestInjectOrUpdateBadge(t *testing.T) {
	badge := "[![Spec Alignment](https://img.shields.io/badge/Spec%20Alignment-40%25-yellow)](specs/kv.md)"

	t.Run("Insert when missing with H1", func(t *testing.T) {
		initial := "# speculate-kv\n\nA simple key-value service.\n"
		result := InjectOrUpdateBadge(initial, badge)
		expected := "# speculate-kv\n\n" + badge + "\n\nA simple key-value service.\n"
		if result != expected {
			t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
		}
	})

	t.Run("Replace existing badge", func(t *testing.T) {
		initial := "# speculate-kv\n\n[![Spec Alignment](https://img.shields.io/badge/Spec%20Alignment-0%25-red)](specs/kv.md)\n\nA simple key-value service.\n"
		result := InjectOrUpdateBadge(initial, badge)
		expected := "# speculate-kv\n\n" + badge + "\n\nA simple key-value service.\n"
		if result != expected {
			t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
		}
	})

	t.Run("Insert when missing and empty content", func(t *testing.T) {
		result := InjectOrUpdateBadge("", badge)
		expected := badge + "\n"
		if result != expected {
			t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
		}
	})
}

func TestMockClientWorkflow(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient("brotherlogic", "speculate-kv")

	// 1. Label management
	err := client.EnsureLabels(ctx, RequiredLabels())
	if err != nil {
		t.Fatalf("EnsureLabels failed: %v", err)
	}

	// 2. Branch management: feat/<stage> off main
	featBranch, err := client.CreateBranch(ctx, "feat/core", "main")
	if err != nil {
		t.Fatalf("CreateBranch feat/core failed: %v", err)
	}
	if featBranch.Name != "feat/core" || featBranch.SHA == "" {
		t.Errorf("unexpected featBranch: %+v", featBranch)
	}

	// Create test branch off feat/core
	testBranch, err := client.CreateBranch(ctx, "test/basic-put-get", "feat/core")
	if err != nil {
		t.Fatalf("CreateBranch test/basic-put-get failed: %v", err)
	}
	if testBranch.Name != "test/basic-put-get" {
		t.Errorf("unexpected testBranch: %+v", testBranch)
	}

	// Verify GetBranch
	b, err := client.GetBranch(ctx, "feat/core")
	if err != nil || b.SHA != featBranch.SHA {
		t.Fatalf("GetBranch feat/core failed: %v", err)
	}

	// 3. Issue creation
	iss, err := client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "[Speculate Loop] Add test for basic Put/Get",
		Body:   "Scenario card details...",
		Labels: []string{LabelAgenticLoop},
	})
	if err != nil {
		t.Fatalf("CreateIssue failed: %v", err)
	}
	if iss.Number != 1 || iss.State != "open" {
		t.Errorf("unexpected issue: %+v", iss)
	}

	// 4. Pull Request creation
	pr, err := client.CreatePullRequest(ctx, &CreatePRRequest{
		Title: "test(kv): add basic put get test",
		Head:  "test/basic-put-get",
		Base:  "feat/core",
		Body:  "Closes #1",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest failed: %v", err)
	}
	if pr.Number != 1 || pr.Merged {
		t.Errorf("unexpected PR: %+v", pr)
	}

	// 5. Update README badge
	badge := "[![Spec Alignment](https://img.shields.io/badge/Spec%20Alignment-25%25-yellow)](specs/kv.md)"
	err = client.UpdateReadmeBadge(ctx, "feat/core", badge)
	if err != nil {
		t.Fatalf("UpdateReadmeBadge failed: %v", err)
	}

	fc, err := client.GetFileContent(ctx, "README.md", "feat/core")
	if err != nil {
		t.Fatalf("GetFileContent failed: %v", err)
	}
	if !strings.Contains(fc.Content, badge) {
		t.Errorf("expected README to contain badge, got:\n%s", fc.Content)
	}

	// 6. Squash merge PR
	mergeRes, err := client.SquashMergePullRequest(ctx, pr.Number, "test(kv): add basic put get test", "Merge commit message")
	if err != nil {
		t.Fatalf("SquashMergePullRequest failed: %v", err)
	}
	if !mergeRes.Merged {
		t.Errorf("expected PR to be merged, got %+v", mergeRes)
	}

	// 7. Delete test branch
	err = client.DeleteBranch(ctx, "test/basic-put-get")
	if err != nil {
		t.Fatalf("DeleteBranch failed: %v", err)
	}
	_, err = client.GetBranch(ctx, "test/basic-put-get")
	if err == nil {
		t.Errorf("expected error getting deleted branch, got nil")
	}

	// 8. Close issue
	err = client.CloseIssue(ctx, iss.Number)
	if err != nil {
		t.Fatalf("CloseIssue failed: %v", err)
	}
	issGot, err := client.GetIssue(ctx, iss.Number)
	if err != nil || issGot.State != "closed" {
		t.Errorf("expected closed issue, got %+v (err: %v)", issGot, err)
	}
}

func TestRealClientWithHttpServer(t *testing.T) {
	mux := http.NewServeMux()

	// Mock branch endpoint
	mux.HandleFunc("/repos/brotherlogic/speculate-kv/branches/main", func(w http.ResponseWriter, r *http.Request) {
		branch := &gh.Branch{
			Name: gh.String("main"),
			Commit: &gh.RepositoryCommit{
				SHA: gh.String("aaaa1111bbbb2222cccc3333dddd4444eeee5555"),
			},
		}
		json.NewEncoder(w).Encode(branch)
	})

	// Mock git ref creation
	mux.HandleFunc("/repos/brotherlogic/speculate-kv/git/refs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ref := &gh.Reference{
				Ref: gh.String("refs/heads/feat/core"),
				Object: &gh.GitObject{
					SHA: gh.String("aaaa1111bbbb2222cccc3333dddd4444eeee5555"),
				},
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(ref)
			return
		}
		http.NotFound(w, r)
	})

	// Mock labels endpoint
	mux.HandleFunc("/repos/brotherlogic/speculate-kv/labels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var l gh.Label
			json.NewDecoder(r.Body).Decode(&l)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(l)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/brotherlogic/speculate-kv/labels/", func(w http.ResponseWriter, r *http.Request) {
		// Return 404 so that CreateLabel is triggered in EnsureLabels
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
	})

	// Mock issue creation and listing
	mux.HandleFunc("/repos/brotherlogic/speculate-kv/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req gh.IssueRequest
			json.NewDecoder(r.Body).Decode(&req)
			iss := &gh.Issue{
				Number:  gh.Int(42),
				Title:   req.Title,
				Body:    req.Body,
				State:   gh.String("open"),
				HTMLURL: gh.String("https://github.com/brotherlogic/speculate-kv/issues/42"),
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(iss)
			return
		}
		if r.Method == http.MethodGet {
			iss := []*gh.Issue{
				{
					Number:  gh.Int(42),
					Title:   gh.String("Test Issue"),
					Body:    gh.String("Body"),
					State:   gh.String("open"),
					Labels:  []*gh.Label{{Name: gh.String(LabelAgenticLoop)}},
					HTMLURL: gh.String("https://github.com/brotherlogic/speculate-kv/issues/42"),
				},
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(iss)
			return
		}
		http.NotFound(w, r)
	})

	// Mock PR creation and merge
	mux.HandleFunc("/repos/brotherlogic/speculate-kv/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var newPR gh.NewPullRequest
			json.NewDecoder(r.Body).Decode(&newPR)
			pr := &gh.PullRequest{
				Number:  gh.Int(10),
				Title:   newPR.Title,
				Head:    &gh.PullRequestBranch{Ref: newPR.Head},
				Base:    &gh.PullRequestBranch{Ref: newPR.Base},
				State:   gh.String("open"),
				Merged:  gh.Bool(false),
				HTMLURL: gh.String("https://github.com/brotherlogic/speculate-kv/pull/10"),
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(pr)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/repos/brotherlogic/speculate-kv/pulls/10/merge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			res := &gh.PullRequestMergeResult{
				SHA:     gh.String("merge-sha-12345"),
				Merged:  gh.Bool(true),
				Message: gh.String("Pull Request successfully merged"),
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(res)
			return
		}
		http.NotFound(w, r)
	})

	// Mock contents endpoint
	readmeBody := "# speculate-kv\n\nInitial repository README.\n"
	encodedReadme := base64.StdEncoding.EncodeToString([]byte(readmeBody))
	mux.HandleFunc("/repos/brotherlogic/speculate-kv/contents/README.md", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			content := &gh.RepositoryContent{
				Name:    gh.String("README.md"),
				Path:    gh.String("README.md"),
				SHA:     gh.String("readme-sha-1234"),
				Content: gh.String(encodedReadme),
			}
			json.NewEncoder(w).Encode(content)
			return
		}
		if r.Method == http.MethodPut {
			resp := &gh.RepositoryContentResponse{
				Commit: gh.Commit{
					SHA: gh.String("new-readme-commit-sha"),
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewRealClient("fake-token", "brotherlogic", "speculate-kv", WithBaseURL(server.URL))

	ctx := context.Background()

	// Test GetBranch
	mainBranch, err := client.GetBranch(ctx, "main")
	if err != nil {
		t.Fatalf("GetBranch(main) failed: %v", err)
	}
	if mainBranch.SHA != "aaaa1111bbbb2222cccc3333dddd4444eeee5555" {
		t.Errorf("unexpected SHA: %s", mainBranch.SHA)
	}

	// Test CreateBranch
	featBranch, err := client.CreateBranch(ctx, "feat/core", "main")
	if err != nil {
		t.Fatalf("CreateBranch(feat/core) failed: %v", err)
	}
	if featBranch.Name != "feat/core" {
		t.Errorf("unexpected branch name: %s", featBranch.Name)
	}

	// Test EnsureLabels
	err = client.EnsureLabels(ctx, RequiredLabels())
	if err != nil {
		t.Fatalf("EnsureLabels failed: %v", err)
	}

	// Test CreateIssue
	issue, err := client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "Test Issue",
		Body:   "Body",
		Labels: []string{LabelAgenticLoop},
	})
	if err != nil {
		t.Fatalf("CreateIssue failed: %v", err)
	}
	if issue.Number != 42 {
		t.Errorf("expected issue number 42, got %d", issue.Number)
	}

	// Test CreatePullRequest
	pr, err := client.CreatePullRequest(ctx, &CreatePRRequest{
		Title: "Test PR",
		Head:  "test/basic-put-get",
		Base:  "feat/core",
		Body:  "PR body",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest failed: %v", err)
	}
	if pr.Number != 10 {
		t.Errorf("expected PR number 10, got %d", pr.Number)
	}

	// Test SquashMergePullRequest
	mergeRes, err := client.SquashMergePullRequest(ctx, pr.Number, "test PR title", "test PR body")
	if err != nil {
		t.Fatalf("SquashMergePullRequest failed: %v", err)
	}
	if !mergeRes.Merged || mergeRes.SHA != "merge-sha-12345" {
		t.Errorf("unexpected merge result: %+v", mergeRes)
	}

	// Test UpdateReadmeBadge
	err = client.UpdateReadmeBadge(ctx, "main", "[![Spec Alignment](https://img.shields.io/badge/Spec%20Alignment-50%25-green)](specs/kv.md)")
	if err != nil {
		t.Fatalf("UpdateReadmeBadge failed: %v", err)
	}

	// Test ListIssues on RealClient
	issues, err := client.ListIssues(ctx, "open", []string{LabelAgenticLoop})
	if err != nil {
		t.Fatalf("RealClient ListIssues failed: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 42 {
		t.Errorf("expected 1 issue (#42), got %+v", issues)
	}

	// Test FindOpenIssueByLabel on RealClient
	foundIssue, err := client.FindOpenIssueByLabel(ctx, LabelAgenticLoop)
	if err != nil {
		t.Fatalf("RealClient FindOpenIssueByLabel failed: %v", err)
	}
	if foundIssue == nil || foundIssue.Number != 42 {
		t.Errorf("expected found issue #42, got %+v", foundIssue)
	}
}

func TestMockClient_ListIssues(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient("brotherlogic", "speculate-kv")

	// 1. Empty states: no issues exist yet
	t.Run("Empty state - no issues in repository", func(t *testing.T) {
		issues, err := client.ListIssues(ctx, "open", nil)
		if err != nil {
			t.Fatalf("ListIssues failed on empty repo: %v", err)
		}
		if len(issues) != 0 {
			t.Errorf("expected 0 issues on empty repo, got %d", len(issues))
		}

		issues, err = client.ListIssues(ctx, "", nil)
		if err != nil {
			t.Fatalf("ListIssues failed on empty repo with empty state: %v", err)
		}
		if len(issues) != 0 {
			t.Errorf("expected 0 issues, got %d", len(issues))
		}
	})

	// Seed issues
	// Issue 1: Open, labels: ["speculate-agentic-loop", "backend"]
	iss1, err := client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "Issue 1",
		Body:   "Body 1",
		Labels: []string{"speculate-agentic-loop", "backend"},
	})
	if err != nil {
		t.Fatalf("CreateIssue 1 failed: %v", err)
	}

	// Issue 2: Open, labels: ["speculate-agentic-loop"]
	iss2, err := client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "Issue 2",
		Body:   "Body 2",
		Labels: []string{"speculate-agentic-loop"},
	})
	if err != nil {
		t.Fatalf("CreateIssue 2 failed: %v", err)
	}

	// Issue 3: Closed, labels: ["speculate-agentic-loop", "backend"]
	iss3, err := client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "Issue 3",
		Body:   "Body 3",
		Labels: []string{"speculate-agentic-loop", "backend"},
	})
	if err != nil {
		t.Fatalf("CreateIssue 3 failed: %v", err)
	}
	if err := client.CloseIssue(ctx, iss3.Number); err != nil {
		t.Fatalf("CloseIssue 3 failed: %v", err)
	}

	// Issue 4: Closed, labels: ["frontend"]
	iss4, err := client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "Issue 4",
		Body:   "Body 4",
		Labels: []string{"frontend"},
	})
	if err != nil {
		t.Fatalf("CreateIssue 4 failed: %v", err)
	}
	if err := client.CloseIssue(ctx, iss4.Number); err != nil {
		t.Fatalf("CloseIssue 4 failed: %v", err)
	}

	// Issue 5: Open, labels: ["frontend"]
	_, err = client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "Issue 5",
		Body:   "Body 5",
		Labels: []string{"frontend"},
	})
	if err != nil {
		t.Fatalf("CreateIssue 5 failed: %v", err)
	}

	// 2. Matching labels
	t.Run("Matching labels", func(t *testing.T) {
		// Single matching label
		issues, err := client.ListIssues(ctx, "open", []string{"speculate-agentic-loop"})
		if err != nil {
			t.Fatalf("ListIssues failed: %v", err)
		}
		if len(issues) != 2 || issues[0].Number != iss1.Number || issues[1].Number != iss2.Number {
			t.Errorf("expected issues [1, 2], got: %+v", issues)
		}

		// Multiple matching labels (both required)
		issues, err = client.ListIssues(ctx, "open", []string{"speculate-agentic-loop", "backend"})
		if err != nil {
			t.Fatalf("ListIssues failed: %v", err)
		}
		if len(issues) != 1 || issues[0].Number != iss1.Number {
			t.Errorf("expected issue 1, got: %+v", issues)
		}
	})

	// 3. Non-matching labels
	t.Run("Non-matching labels", func(t *testing.T) {
		issues, err := client.ListIssues(ctx, "open", []string{"non-existent-label"})
		if err != nil {
			t.Fatalf("ListIssues failed: %v", err)
		}
		if len(issues) != 0 {
			t.Errorf("expected 0 issues for non-matching label, got %d", len(issues))
		}

		// Partial match should not return (must match all requested labels)
		issues, err = client.ListIssues(ctx, "open", []string{"speculate-agentic-loop", "non-existent-label"})
		if err != nil {
			t.Fatalf("ListIssues failed: %v", err)
		}
		if len(issues) != 0 {
			t.Errorf("expected 0 issues for partial match, got %d", len(issues))
		}
	})

	// 4. Closed vs open states
	t.Run("Closed vs open states", func(t *testing.T) {
		// Open issues with label
		openIssues, err := client.ListIssues(ctx, "open", []string{"speculate-agentic-loop"})
		if err != nil {
			t.Fatalf("ListIssues open failed: %v", err)
		}
		if len(openIssues) != 2 {
			t.Errorf("expected 2 open issues, got %d", len(openIssues))
		}

		// Closed issues with label
		closedIssues, err := client.ListIssues(ctx, "closed", []string{"speculate-agentic-loop"})
		if err != nil {
			t.Fatalf("ListIssues closed failed: %v", err)
		}
		if len(closedIssues) != 1 || closedIssues[0].Number != iss3.Number {
			t.Errorf("expected closed issue 3, got %+v", closedIssues)
		}

		// All issues with label
		allIssues, err := client.ListIssues(ctx, "all", []string{"speculate-agentic-loop"})
		if err != nil {
			t.Fatalf("ListIssues all failed: %v", err)
		}
		if len(allIssues) != 3 {
			t.Errorf("expected 3 issues with state 'all', got %d", len(allIssues))
		}
	})

	// 5. Empty states (empty label filter, empty state string)
	t.Run("Empty states filter", func(t *testing.T) {
		// Empty labels slice returns all open issues
		openAll, err := client.ListIssues(ctx, "open", nil)
		if err != nil {
			t.Fatalf("ListIssues failed: %v", err)
		}
		if len(openAll) != 3 { // iss1, iss2, iss5
			t.Errorf("expected 3 open issues, got %d", len(openAll))
		}

		// Empty state string defaults to open
		defaultStateIssues, err := client.ListIssues(ctx, "", nil)
		if err != nil {
			t.Fatalf("ListIssues failed with empty state: %v", err)
		}
		if len(defaultStateIssues) != 3 {
			t.Errorf("expected 3 issues for empty state (defaults to open), got %d", len(defaultStateIssues))
		}

		// All issues across all states with nil labels
		allStateIssues, err := client.ListIssues(ctx, "all", []string{})
		if err != nil {
			t.Fatalf("ListIssues all states failed: %v", err)
		}
		if len(allStateIssues) != 5 {
			t.Errorf("expected 5 issues total, got %d", len(allStateIssues))
		}
	})
}

func TestMockClient_FindOpenIssueByLabel(t *testing.T) {
	ctx := context.Background()
	client := NewMockClient("brotherlogic", "speculate-kv")

	// 1. Empty state - no issues
	t.Run("Empty repository", func(t *testing.T) {
		found, err := client.FindOpenIssueByLabel(ctx, "speculate-agentic-loop")
		if err != nil {
			t.Fatalf("FindOpenIssueByLabel failed: %v", err)
		}
		if found != nil {
			t.Errorf("expected nil found issue, got %+v", found)
		}
	})

	// 2. Non-matching label
	iss1, err := client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "First Open Issue",
		Body:   "Body",
		Labels: []string{"speculate-agentic-loop"},
	})
	if err != nil {
		t.Fatalf("CreateIssue failed: %v", err)
	}

	t.Run("Non-matching label", func(t *testing.T) {
		found, err := client.FindOpenIssueByLabel(ctx, "non-existent")
		if err != nil {
			t.Fatalf("FindOpenIssueByLabel failed: %v", err)
		}
		if found != nil {
			t.Errorf("expected nil for non-matching label, got %+v", found)
		}
	})

	// 3. Closed issue with label does not match FindOpenIssueByLabel
	issClosed, err := client.CreateIssue(ctx, &CreateIssueRequest{
		Title:  "Closed Issue",
		Body:   "Body",
		Labels: []string{"only-closed-label"},
	})
	if err != nil {
		t.Fatalf("CreateIssue failed: %v", err)
	}
	if err := client.CloseIssue(ctx, issClosed.Number); err != nil {
		t.Fatalf("CloseIssue failed: %v", err)
	}

	t.Run("Closed issue with label", func(t *testing.T) {
		found, err := client.FindOpenIssueByLabel(ctx, "only-closed-label")
		if err != nil {
			t.Fatalf("FindOpenIssueByLabel failed: %v", err)
		}
		if found != nil {
			t.Errorf("expected nil for closed issue label, got %+v", found)
		}
	})

	// 4. Matching open label returns the open issue
	t.Run("Matching open issue", func(t *testing.T) {
		found, err := client.FindOpenIssueByLabel(ctx, "speculate-agentic-loop")
		if err != nil {
			t.Fatalf("FindOpenIssueByLabel failed: %v", err)
		}
		if found == nil || found.Number != iss1.Number {
			t.Errorf("expected issue #%d, got %+v", iss1.Number, found)
		}
	})
}
