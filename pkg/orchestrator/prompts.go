package orchestrator

import (
	"fmt"
	"strings"

	"github.com/brotherlogic/speculate/pkg/synthesizer"
)

// ScenarioBranchName normalizes a scenario identifier or title into a canonical feat/<slug> branch name.
func ScenarioBranchName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 50 {
		slug = strings.Trim(slug[:50], "-")
	}
	if slug == "" {
		slug = "scenario"
	}
	return "feat/" + slug
}

// BuildRedPrompt constructs the devcontainer prompt instructing the Red-phase agent
// to author a compiling-but-RED integration test in tests/ and open a PR to main.
func BuildRedPrompt(repo, branch string, card *synthesizer.ScenarioCard, diagnostic string) string {
	cardMD := ""
	if card != nil {
		cardMD = card.FormatMarkdown()
	}
	return BuildRedPromptFromMarkdown(repo, branch, cardMD, diagnostic)
}

// BuildRedPromptFromMarkdown constructs the Red-phase prompt from an existing Scenario Card Markdown body.
func BuildRedPromptFromMarkdown(repo, branch, scenarioMarkdown, diagnostic string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working in repository %s on branch %s.\n\n", repo, branch)
	b.WriteString("PHASE: RED TEST AUTHORING\n")
	b.WriteString("Your sole objective is to write a compiling-but-FAILING (RED) integration test in tests/ covering the Scenario Card below, and push your changes so a Pull Request is open against main.\n\n")
	b.WriteString("STRICT RULES:\n")
	b.WriteString("1. ONLY add or edit Go test files under tests/ (do NOT implement production code in internal/).\n")
	b.WriteString("2. Include `// Stage: <Stage>` and `// Scenario: <ID> <Title>` doc comments on the test function.\n")
	b.WriteString("3. Ensure `go build ./...`, `go vet ./...`, and `go test -c ./tests/...` succeed (`compile` check must pass).\n")
	b.WriteString("4. Ensure `go test -v ./...` FAILS (`test` check must be RED) against the current unimplemented server; do NOT write a passing/tautological test.\n")
	b.WriteString("5. Commit and push to branch `" + branch + "` and open a Pull Request targeting `main` if one is not already open.\n")

	if strings.TrimSpace(scenarioMarkdown) != "" {
		b.WriteString("\nSCENARIO CARD:\n")
		b.WriteString(strings.TrimSpace(scenarioMarkdown))
		b.WriteString("\n")
	}

	if strings.TrimSpace(diagnostic) != "" {
		b.WriteString("\nPREVIOUS ATTEMPT DIAGNOSTICS (MUST FIX):\n")
		b.WriteString(strings.TrimSpace(diagnostic))
		b.WriteString("\n")
	}

	return b.String()
}

// BuildGreenPrompt constructs the devcontainer prompt instructing the Green-phase agent
// to implement internal/** without modifying protected paths.
func BuildGreenPrompt(repo, branch, approvedSHA, diagnostic string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working in repository %s on branch %s.\n\n", repo, branch)
	b.WriteString("PHASE: GREEN IMPLEMENTATION ALIGNMENT\n")
	b.WriteString("The RED integration test on this PR has been approved by the repository owner")
	if strings.TrimSpace(approvedSHA) != "" {
		fmt.Fprintf(&b, " at commit %s", approvedSHA)
	}
	b.WriteString(".\nYour objective is to implement the missing functionality in internal/** so all CI checks (`compile`, `test`, and `review-gate`) pass.\n\n")
	b.WriteString("STRICT RULES:\n")
	b.WriteString("1. ONLY modify implementation files under `internal/**` (and `README.md` if necessary).\n")
	b.WriteString("2. NEVER modify protected paths: `tests/`, `specs/`, `proto/`, `api/`, `.github/`, `*_test.go`, or `*.proto`. Post-approval edits to protected paths will fail `review-gate` and cause the branch to be force-reset.\n")
	b.WriteString("3. Verify `go build ./...`, `go vet ./...`, and `go test -v ./...` pass locally before pushing.\n")
	b.WriteString("4. Commit and push your implementation commits to branch `" + branch + "`.\n")

	if strings.TrimSpace(diagnostic) != "" {
		b.WriteString("\nPREVIOUS ATTEMPT DIAGNOSTICS (MUST FIX):\n")
		b.WriteString(strings.TrimSpace(diagnostic))
		b.WriteString("\n")
	}

	return b.String()
}
