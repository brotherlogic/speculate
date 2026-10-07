package github

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// MockClient provides an in-memory implementation of the GitHub Client interface.
type MockClient struct {
	mu           sync.RWMutex
	owner        string
	repo         string
	branches     map[string]string                 // branchName -> commitSHA
	labels       map[string]Label                  // labelName -> Label
	issues       map[int]*Issue                    // number -> Issue
	prs          map[int]*PullRequest              // number -> PullRequest
	files        map[string]map[string]*FileContent // ref -> path -> FileContent
	nextIssueNum int
	nextPRNum    int
}

// NewMockClient initializes a thread-safe MockClient.
func NewMockClient(owner, repo string) *MockClient {
	m := &MockClient{
		owner:        owner,
		repo:         repo,
		branches:     make(map[string]string),
		labels:       make(map[string]Label),
		issues:       make(map[int]*Issue),
		prs:          make(map[int]*PullRequest),
		files:        make(map[string]map[string]*FileContent),
		nextIssueNum: 1,
		nextPRNum:    1,
	}

	// Seed with default main branch
	mainSHA := "0123456789abcdef0123456789abcdef01234567"
	m.branches["main"] = mainSHA
	m.branches["refs/heads/main"] = mainSHA

	// Seed with initial README.md
	m.files["main"] = map[string]*FileContent{
		"README.md": {
			Content: "# " + repo + "\n\nInitial repository README.\n",
			SHA:     sha1String("initial-readme"),
		},
	}

	return m
}

func (m *MockClient) Owner() string { return m.owner }
func (m *MockClient) Repo() string  { return m.repo }

// GetBranch retrieves a branch.
func (m *MockClient) GetBranch(ctx context.Context, branch string) (*Branch, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sha, ok := m.branches[branch]
	if !ok {
		sha, ok = m.branches["refs/heads/"+branch]
	}
	if !ok {
		return nil, fmt.Errorf("branch %q not found", branch)
	}

	return &Branch{
		Name: branch,
		SHA:  sha,
	}, nil
}

// CreateBranch creates a branch pointing to baseBranchOrSHA.
func (m *MockClient) CreateBranch(ctx context.Context, branch, baseBranchOrSHA string) (*Branch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.branches[branch]; exists {
		return nil, fmt.Errorf("branch %q already exists", branch)
	}

	sha := baseBranchOrSHA
	if len(baseBranchOrSHA) != 40 {
		baseSHA, ok := m.branches[baseBranchOrSHA]
		if !ok {
			baseSHA, ok = m.branches["refs/heads/"+baseBranchOrSHA]
		}
		if !ok {
			return nil, fmt.Errorf("base branch %q not found", baseBranchOrSHA)
		}
		sha = baseSHA
	}

	m.branches[branch] = sha
	m.branches["refs/heads/"+branch] = sha

	// Copy files from base branch if available
	if baseFiles, ok := m.files[baseBranchOrSHA]; ok {
		branchFiles := make(map[string]*FileContent)
		for p, fc := range baseFiles {
			branchFiles[p] = &FileContent{
				Content: fc.Content,
				SHA:     fc.SHA,
			}
		}
		m.files[branch] = branchFiles
	} else if mainFiles, ok := m.files["main"]; ok {
		branchFiles := make(map[string]*FileContent)
		for p, fc := range mainFiles {
			branchFiles[p] = &FileContent{
				Content: fc.Content,
				SHA:     fc.SHA,
			}
		}
		m.files[branch] = branchFiles
	}

	return &Branch{
		Name: branch,
		SHA:  sha,
	}, nil
}

// DeleteBranch removes a branch.
func (m *MockClient) DeleteBranch(ctx context.Context, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.branches, branch)
	delete(m.branches, "refs/heads/"+branch)
	delete(m.files, branch)
	return nil
}

// EnsureLabels creates or updates labels in memory.
func (m *MockClient) EnsureLabels(ctx context.Context, labels []Label) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, l := range labels {
		m.labels[l.Name] = l
	}
	return nil
}

// CreateIssue adds an issue.
func (m *MockClient) CreateIssue(ctx context.Context, req *CreateIssueRequest) (*Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	num := m.nextIssueNum
	m.nextIssueNum++

	issue := &Issue{
		Number:  num,
		Title:   req.Title,
		Body:    req.Body,
		State:   "open",
		Labels:  req.Labels,
		HTMLURL: fmt.Sprintf("https://github.com/%s/%s/issues/%d", m.owner, m.repo, num),
	}

	m.issues[num] = issue
	return issue, nil
}

// GetIssue retrieves an issue by number.
func (m *MockClient) GetIssue(ctx context.Context, number int) (*Issue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	issue, ok := m.issues[number]
	if !ok {
		return nil, fmt.Errorf("issue #%d not found", number)
	}
	return issue, nil
}

// CloseIssue marks an issue closed.
func (m *MockClient) CloseIssue(ctx context.Context, number int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	issue, ok := m.issues[number]
	if !ok {
		return fmt.Errorf("issue #%d not found", number)
	}
	issue.State = "closed"
	return nil
}

// ListIssues filters in-memory issues matching state and all requested labels.
func (m *MockClient) ListIssues(ctx context.Context, state string, labels []string) ([]*Issue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*Issue, 0)
	for i := 1; i < m.nextIssueNum; i++ {
		iss, exists := m.issues[i]
		if !exists {
			continue
		}

		// State filter
		// GitHub API default for state is "open". "all" matches any state.
		if state != "all" {
			targetState := state
			if targetState == "" {
				targetState = "open"
			}
			if !strings.EqualFold(iss.State, targetState) {
				continue
			}
		}

		// Label filter: issue must contain ALL requested labels
		hasAllLabels := true
		for _, reqLabel := range labels {
			found := false
			for _, issLabel := range iss.Labels {
				if issLabel == reqLabel {
					found = true
					break
				}
			}
			if !found {
				hasAllLabels = false
				break
			}
		}
		if !hasAllLabels {
			continue
		}

		copiedLabels := make([]string, len(iss.Labels))
		copy(copiedLabels, iss.Labels)
		result = append(result, &Issue{
			Number:  iss.Number,
			Title:   iss.Title,
			Body:    iss.Body,
			State:   iss.State,
			Labels:  copiedLabels,
			HTMLURL: iss.HTMLURL,
		})
	}

	return result, nil
}

// FindOpenIssueByLabel finds the first open issue with the specified label, or nil if none found.
func (m *MockClient) FindOpenIssueByLabel(ctx context.Context, label string) (*Issue, error) {
	issues, err := m.ListIssues(ctx, "open", []string{label})
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		return nil, nil
	}
	return issues[0], nil
}

// CreatePullRequest opens a pull request.
func (m *MockClient) CreatePullRequest(ctx context.Context, req *CreatePRRequest) (*PullRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	num := m.nextPRNum
	m.nextPRNum++

	pr := &PullRequest{
		Number:  num,
		Title:   req.Title,
		Head:    req.Head,
		Base:    req.Base,
		State:   "open",
		Merged:  false,
		HTMLURL: fmt.Sprintf("https://github.com/%s/%s/pull/%d", m.owner, m.repo, num),
	}

	m.prs[num] = pr
	return pr, nil
}

// GetPullRequest retrieves a PR by number.
func (m *MockClient) GetPullRequest(ctx context.Context, number int) (*PullRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	pr, ok := m.prs[number]
	if !ok {
		return nil, fmt.Errorf("pull request #%d not found", number)
	}
	return pr, nil
}

// SquashMergePullRequest marks PR as merged.
func (m *MockClient) SquashMergePullRequest(ctx context.Context, prNumber int, commitTitle, commitMessage string) (*MergeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pr, ok := m.prs[prNumber]
	if !ok {
		return nil, fmt.Errorf("pull request #%d not found", prNumber)
	}
	if pr.Merged {
		return nil, fmt.Errorf("pull request #%d already merged", prNumber)
	}

	newSHA := sha1String(fmt.Sprintf("squash-pr-%d-%s", prNumber, commitTitle))
	pr.Merged = true
	pr.State = "closed"

	// Update base branch SHA to the merged SHA
	m.branches[pr.Base] = newSHA
	m.branches["refs/heads/"+pr.Base] = newSHA

	// Copy/merge files from head branch to base branch if available
	if headFiles, ok := m.files[pr.Head]; ok {
		if _, ok := m.files[pr.Base]; !ok {
			m.files[pr.Base] = make(map[string]*FileContent)
		}
		for path, fc := range headFiles {
			m.files[pr.Base][path] = &FileContent{
				Content: fc.Content,
				SHA:     fc.SHA,
			}
		}
	}

	return &MergeResult{
		SHA:     newSHA,
		Merged:  true,
		Message: "Pull request successfully merged",
	}, nil
}

// GetFileContent retrieves file content and SHA.
func (m *MockClient) GetFileContent(ctx context.Context, path, ref string) (*FileContent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	branchFiles, ok := m.files[ref]
	if !ok {
		branchFiles = m.files["main"]
	}
	if branchFiles == nil {
		return nil, fmt.Errorf("file %q not found on ref %q", path, ref)
	}

	fc, ok := branchFiles[path]
	if !ok {
		return nil, fmt.Errorf("file %q not found on ref %q", path, ref)
	}

	return &FileContent{
		Content: fc.Content,
		SHA:     fc.SHA,
	}, nil
}

// UpdateFile updates file content on a branch.
func (m *MockClient) UpdateFile(ctx context.Context, path, content, sha, branch, commitMessage string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	branchFiles, ok := m.files[branch]
	if !ok {
		branchFiles = make(map[string]*FileContent)
		m.files[branch] = branchFiles
	}

	newSHA := sha1String(content)
	branchFiles[path] = &FileContent{
		Content: content,
		SHA:     newSHA,
	}

	return nil
}

// UpdateReadmeBadge updates or inserts badge markdown on README.md.
func (m *MockClient) UpdateReadmeBadge(ctx context.Context, branch, badgeMarkdown string) error {
	file, _ := m.GetFileContent(ctx, "README.md", branch)
	var currentContent string
	var currentSHA string
	if file != nil {
		currentContent = file.Content
		currentSHA = file.SHA
	}

	updated := InjectOrUpdateBadge(currentContent, badgeMarkdown)
	return m.UpdateFile(ctx, "README.md", updated, currentSHA, branch, "chore: update spec alignment badge")
}

func sha1String(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}
