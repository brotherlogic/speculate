package setup

import (
	"bytes"
	"text/template"
)

type TemplateContext struct {
	Owner string
	Repo  string
}

const codeownersTemplate = `# CODEOWNERS for Speculate-managed repository
# Requires human approval for changes to specs, schemas, tests, and workflows.
/specs/ @[[.Owner]]
/proto/ @[[.Owner]]
/api/ @[[.Owner]]
/tests/ @[[.Owner]]
/.github/ @[[.Owner]]
`

const reviewGateTemplate = `name: Review Gate

on:
  pull_request:
    branches:
      - main
    types:
      - opened
      - synchronize
      - reopened
  pull_request_review:
    types:
      - submitted
      - edited
      - dismissed

jobs:
  review-gate:
    name: review-gate
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pull-requests: write
      actions: write
    steps:
      - name: Checkout code
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Check review requirements
        env:
          GH_TOKEN: ${{ secrets.PERSONAL_TOKEN || secrets.GITHUB_TOKEN }}
        run: |
          PR_NUMBER="${{ github.event.pull_request.number }}"
          if [ -z "$PR_NUMBER" ] || [ "$PR_NUMBER" = "null" ]; then
            echo "No PR number found, skipping check."
            exit 0
          fi

          echo "Inspecting files changed in PR #${PR_NUMBER}..."
          CHANGED_FILES=$(gh pr diff "$PR_NUMBER" --name-only)
          echo "Changed files:"
          echo "$CHANGED_FILES"

          PROTECTED=false
          while IFS= read -r file; do
            [ -z "$file" ] && continue
            case "$file" in
              *.proto|*_test.go|specs/*|*/specs/*|tests/*|*/tests/*|proto/*|*/proto/*|api/*|*/api/*)
                echo "Protected path matched: $file"
                PROTECTED=true
                ;;
            esac
          done <<< "$CHANGED_FILES"

          if [ "$PROTECTED" = "false" ]; then
            echo "✓ No protected files modified. Human review not required."
            exit 0
          fi

          PR_AUTHOR=$(gh pr view "$PR_NUMBER" --json author --jq '.author.login')
          if [ "$PR_AUTHOR" = "[[.Owner]]" ]; then
            echo "✓ PR #${PR_NUMBER} is authored by @[[.Owner]]. Human review requirement satisfied by author."
            exit 0
          fi

          echo "Protected files modified. Ensuring @[[.Owner]] is requested for review..."
          REVIEWER_REQUESTED=$(gh pr view "$PR_NUMBER" --json reviewRequests --jq '[.reviewRequests[] | select(.login == "[[.Owner]]" or .name == "[[.Owner]]")] | length')
          if [ "$REVIEWER_REQUESTED" -eq 0 ]; then
            echo "Requesting review from @[[.Owner]]..."
            gh pr edit "$PR_NUMBER" --add-reviewer [[.Owner]] 2>/dev/null || echo "Note: Could not add [[.Owner]] as reviewer (e.g. if author)"
          fi

          echo "Checking for approval from @[[.Owner]]..."
          APPROVALS=$(gh pr view "$PR_NUMBER" --json reviews --jq '[.reviews[] | select(.author.login == "[[.Owner]]" and .state == "APPROVED")] | length')
          echo "Found $APPROVALS approval(s) from @[[.Owner]]"

          if [ "$APPROVALS" -gt 0 ]; then
            echo "✓ PR #${PR_NUMBER} has been approved by @[[.Owner]]."
            if [ "${{ github.event_name }}" = "pull_request_review" ]; then
              HEAD_SHA="${{ github.event.pull_request.head.sha }}"
              echo "Review approved on pull_request_review event. Checking for previous failed pull_request runs on ${HEAD_SHA}..."
              FAILED_RUNS=$(gh run list --workflow=review-gate.yml --event=pull_request --commit="$HEAD_SHA" --status=failure --json databaseId --jq '.[].databaseId')
              for run_id in $FAILED_RUNS; do
                echo "Triggering rerun of failed pull_request run $run_id..."
                gh run rerun "$run_id" || true
              done
            fi
            exit 0
          else
            echo "::error::PR #${PR_NUMBER} modifies protected contracts/specs/tests and requires an approving review from @[[.Owner]]."
            exit 1
          fi
`

const autoMergeTemplate = `name: Auto-merge PR

on:
  pull_request:
    types:
      - opened
      - reopened
      - synchronize
  status:
  check_suite:
    types:
      - completed

jobs:
  enable-auto-merge:
    runs-on: ubuntu-latest
    permissions:
      contents: write
      pull-requests: write
    steps:
      - name: Enable auto-merge
        run: |
          if [ "${{ github.event_name }}" == "pull_request" ]; then
            PR_URL="${{ github.event.pull_request.html_url }}"
          else
            PR_URL=$(gh pr list --head "${{ github.sha }}" --json url --jq '.[0].url')
          fi
          
          if [ -n "$PR_URL" ]; then
            gh pr merge --auto --squash "$PR_URL"
          else
            echo "No associated PR found for this event."
          fi
        env:
          GH_TOKEN: ${{ secrets.PERSONAL_TOKEN || secrets.GITHUB_TOKEN }}
`

const testsTemplate = `name: Tests

on:
  push:
    branches:
      - main
      - 'feat/**'
  pull_request:
    branches:
      - main
      - 'feat/**'

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout code
        uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version-file: 'go.mod'

      - name: Install dependencies
        run: go mod download

      - name: Run tests
        run: go test -v ./...

      - name: Run build
        run: go build ./...

      - name: Run lint
        run: go vet ./...
`

func renderTemplate(tmplStr string, ctx TemplateContext) (string, error) {
	tmpl, err := template.New("tmpl").Delims("[[", "]]").Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", err
	}
	return buf.String(), nil
}
