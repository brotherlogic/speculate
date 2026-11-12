package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	pb "github.com/brotherlogic/speculate/proto"
)

// EnrollmentLister provides read-only access to tracked repository enrollment records.
type EnrollmentLister interface {
	ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error)
}

// DashboardData holds template data for the read-only Speculate HTML dashboard.
type DashboardData struct {
	TotalCount     int
	AlignedCount   int
	AttentionCount int
	GeneratedAtUTC string
	Rows           []RepoRowView
}

// RepoRowView represents a single tracked repository row in the HTML dashboard.
type RepoRowView struct {
	Repository          string
	RepoURL             string
	StageLabel          string
	StageBadgeClass     string
	AlignmentPercentage int32
	ShowAlignment       bool
	IssueURL            string
	IssueLabel          string
	LastCheckedUTC      string
	LastCheckedRelative string
	StatusMessage       string
	LastError           string
}

var (
	githubIssueNumberRe = regexp.MustCompile(`/(?:issues|pull)/([0-9]+)(?:[/?#]|$)`)
	dashboardTmpl       = template.Must(template.New("dashboard").Parse(dashboardHTMLTemplate))
)

// NewDashboardHandler returns a self-contained, read-only HTTP handler rendering
// all tracked Speculate repositories at exact path "/".
func NewDashboardHandler(lister EnrollmentLister) http.Handler {
	return newDashboardHandlerWithClock(lister, time.Now)
}

func newDashboardHandlerWithClock(lister EnrollmentLister, nowFn func() time.Time) http.Handler {
	if nowFn == nil {
		nowFn = time.Now
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		if lister == nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		records, err := lister.ListEnrollments(r.Context())
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		now := nowFn().UTC()
		data := buildDashboardData(records, now)

		var buf bytes.Buffer
		if err := dashboardTmpl.Execute(&buf, data); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(buf.Bytes())
		}
	})
}

func buildDashboardData(records []*pb.EnrollmentRecord, now time.Time) DashboardData {
	data := DashboardData{
		GeneratedAtUTC: now.UTC().Format("2006-01-02 15:04:05 UTC"),
		Rows:           make([]RepoRowView, 0, len(records)),
	}

	for _, rec := range records {
		if rec == nil {
			continue
		}

		repo := strings.TrimSpace(rec.GetRepository())
		repoURL := ""
		if repo != "" {
			repoURL = "https://github.com/" + strings.TrimPrefix(repo, "/")
		}

		stage := rec.GetStage()
		stageLabel, stageClass := stagePresentation(stage)

		pct := rec.GetAlignmentPercentage()
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}

		issueURL, issueLabel := extractIssueLink(rec.GetIssueUrl())
		lastUTC, lastRel := formatLastChecked(rec.GetLastEvaluatedAtUnix(), now)
		lastErr := strings.TrimSpace(rec.GetLastError())
		statusMsg := strings.TrimSpace(rec.GetStatusMessage())

		if stage == pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED {
			data.AlignedCount++
		}
		if stage == pb.LifecycleStage_LIFECYCLE_STAGE_STALLED || lastErr != "" {
			data.AttentionCount++
		}

		data.Rows = append(data.Rows, RepoRowView{
			Repository:          repo,
			RepoURL:             repoURL,
			StageLabel:          stageLabel,
			StageBadgeClass:     stageClass,
			AlignmentPercentage: pct,
			ShowAlignment:       stage != pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
			IssueURL:            issueURL,
			IssueLabel:          issueLabel,
			LastCheckedUTC:      lastUTC,
			LastCheckedRelative: lastRel,
			StatusMessage:       statusMsg,
			LastError:           lastErr,
		})
	}

	data.TotalCount = len(data.Rows)
	return data
}

func stagePresentation(stage pb.LifecycleStage) (label, badgeClass string) {
	switch stage {
	case pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED:
		return "Awaiting Seed", "stage-seed"
	case pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING:
		return "Evaluating", "stage-evaluating"
	case pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW:
		return "Awaiting Test Review", "stage-review"
	case pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING:
		return "Aligning", "stage-aligning"
	case pb.LifecycleStage_LIFECYCLE_STAGE_STALLED:
		return "Stalled", "stage-stalled"
	case pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED:
		return "Aligned", "stage-aligned"
	default:
		return "Unspecified", "stage-unspecified"
	}
}

func extractIssueLink(rawURL string) (cleanURL, label string) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", ""
	}
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return "", ""
	}
	if matches := githubIssueNumberRe.FindStringSubmatch(trimmed); len(matches) == 2 {
		return trimmed, "#" + matches[1]
	}
	return trimmed, "View Issue"
}

func formatLastChecked(unixSec int64, now time.Time) (utcFormatted, relative string) {
	if unixSec <= 0 {
		return "", "Never"
	}

	checked := time.Unix(unixSec, 0).UTC()
	utcFormatted = checked.Format("2006-01-02 15:04:05 UTC")

	diff := now.UTC().Sub(checked)
	switch {
	case diff < time.Minute:
		return utcFormatted, "just now"
	case diff < time.Hour:
		return utcFormatted, fmt.Sprintf("%dm ago", int(diff/time.Minute))
	case diff < 24*time.Hour:
		return utcFormatted, fmt.Sprintf("%dh ago", int(diff/time.Hour))
	default:
		return utcFormatted, fmt.Sprintf("%dd ago", int(diff/(24*time.Hour)))
	}
}

const dashboardHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Speculate — Tracked Repositories</title>
  <style>
    :root {
      --bg-primary: #f8fafc;
      --bg-card: #ffffff;
      --bg-subtle: #f1f5f9;
      --text-primary: #0f172a;
      --text-muted: #64748b;
      --border-subtle: #e2e8f0;
      --link-color: #2563eb;
      --error-bg: #fef2f2;
      --error-border: #fecaca;
      --error-text: #991b1b;
    }

    @media (prefers-color-scheme: dark) {
      :root {
        --bg-primary: #0b1120;
        --bg-card: #111827;
        --bg-subtle: #1e293b;
        --text-primary: #f8fafc;
        --text-muted: #94a3b8;
        --border-subtle: #1f2937;
        --link-color: #60a5fa;
        --error-bg: rgba(220, 38, 38, 0.14);
        --error-border: rgba(220, 38, 38, 0.4);
        --error-text: #fca5a5;
      }
    }

    * {
      box-sizing: border-box;
    }

    body {
      margin: 0;
      padding: 2rem 1.25rem;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      background: var(--bg-primary);
      color: var(--text-primary);
      line-height: 1.5;
    }

    .container {
      max-width: 1180px;
      margin: 0 auto;
    }

    header {
      display: flex;
      flex-wrap: wrap;
      justify-content: space-between;
      align-items: baseline;
      gap: 0.75rem;
      margin-bottom: 1.5rem;
    }

    header h1 {
      margin: 0;
      font-size: 1.5rem;
      font-weight: 700;
      letter-spacing: -0.02em;
    }

    header .subtitle {
      color: var(--text-muted);
      font-size: 0.875rem;
    }

    .summary-bar {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
      gap: 1rem;
      margin-bottom: 1.5rem;
    }

    .summary-card {
      background: var(--bg-card);
      border: 1px solid var(--border-subtle);
      border-radius: 0.625rem;
      padding: 1rem 1.25rem;
    }

    .summary-label {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--text-muted);
    }

    .summary-value {
      margin-top: 0.25rem;
      font-size: 1.625rem;
      font-weight: 700;
    }

    .table-card {
      background: var(--bg-card);
      border: 1px solid var(--border-subtle);
      border-radius: 0.625rem;
      overflow-x: auto;
    }

    table {
      width: 100%;
      border-collapse: collapse;
      min-width: 720px;
    }

    th, td {
      padding: 0.875rem 1rem;
      text-align: left;
      border-bottom: 1px solid var(--border-subtle);
      vertical-align: top;
      font-size: 0.9rem;
    }

    th {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--text-muted);
      background: var(--bg-subtle);
    }

    tbody tr:last-child td {
      border-bottom: none;
    }

    a {
      color: var(--link-color);
      text-decoration: none;
      font-weight: 500;
    }

    a:hover {
      text-decoration: underline;
    }

    .stage-badge {
      display: inline-block;
      padding: 0.2rem 0.625rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 600;
      color: #ffffff;
      white-space: nowrap;
    }

    .stage-seed { background-color: #1d76db; }
    .stage-evaluating { background-color: #6366f1; }
    .stage-review { background-color: #d97706; }
    .stage-aligning { background-color: #8b5cf6; }
    .stage-stalled { background-color: #dc2626; }
    .stage-aligned { background-color: #059669; }
    .stage-unspecified { background-color: #6b7280; }

    .status-note {
      margin-top: 0.35rem;
      font-size: 0.8rem;
      color: var(--text-muted);
    }

    .error-callout {
      margin-top: 0.45rem;
      padding: 0.4rem 0.6rem;
      border-radius: 0.375rem;
      background: var(--error-bg);
      border: 1px solid var(--error-border);
      color: var(--error-text);
      font-size: 0.8rem;
      word-break: break-word;
    }

    .alignment-wrap {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      min-width: 120px;
    }

    .progress-track {
      flex: 1;
      height: 0.45rem;
      background: var(--bg-subtle);
      border-radius: 9999px;
      overflow: hidden;
      max-width: 90px;
    }

    .progress-fill {
      height: 100%;
      background: #059669;
      border-radius: 9999px;
    }

    .timestamp-rel {
      font-weight: 500;
    }

    .timestamp-utc {
      font-size: 0.775rem;
      color: var(--text-muted);
      white-space: nowrap;
    }

    .muted {
      color: var(--text-muted);
    }

    .empty-state {
      background: var(--bg-card);
      border: 1px solid var(--border-subtle);
      border-radius: 0.625rem;
      padding: 3rem 1.5rem;
      text-align: center;
    }

    .empty-state h2 {
      margin: 0 0 0.5rem;
      font-size: 1.125rem;
    }

    .empty-state p {
      margin: 0 0 1rem;
      color: var(--text-muted);
      font-size: 0.925rem;
    }

    .empty-state code {
      display: inline-block;
      padding: 0.4rem 0.75rem;
      border-radius: 0.375rem;
      background: var(--bg-subtle);
      border: 1px solid var(--border-subtle);
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 0.85rem;
    }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <div>
        <h1>Speculate Dashboard</h1>
        <div class="subtitle">Read-only lifecycle overview of tracked GitHub repositories</div>
      </div>
      <div class="subtitle">Rendered {{.GeneratedAtUTC}}</div>
    </header>

    <section class="summary-bar" aria-label="Repository Summary">
      <div class="summary-card">
        <div class="summary-label">Total Tracked Repositories</div>
        <div class="summary-value" data-metric="total">{{.TotalCount}}</div>
      </div>
      <div class="summary-card">
        <div class="summary-label">Aligned</div>
        <div class="summary-value" data-metric="aligned">{{.AlignedCount}}</div>
      </div>
      <div class="summary-card">
        <div class="summary-label">Attention Needed</div>
        <div class="summary-value" data-metric="attention">{{.AttentionCount}}</div>
      </div>
    </section>

    {{if .Rows}}
    <div class="table-card">
      <table>
        <thead>
          <tr>
            <th>Repository</th>
            <th>Lifecycle Stage</th>
            <th>Alignment</th>
            <th>Active Issue / PR</th>
            <th>Last Checked</th>
          </tr>
        </thead>
        <tbody>
          {{range .Rows}}
          <tr>
            <td>
              {{if .RepoURL}}<a href="{{.RepoURL}}" target="_blank" rel="noopener noreferrer">{{.Repository}}</a>{{else}}<span>—</span>{{end}}
            </td>
            <td>
              <span class="stage-badge {{.StageBadgeClass}}">{{.StageLabel}}</span>
              {{if .StatusMessage}}<div class="status-note">{{.StatusMessage}}</div>{{end}}
              {{if .LastError}}<div class="error-callout" role="alert">Warning: {{.LastError}}</div>{{end}}
            </td>
            <td>
              {{if .ShowAlignment}}
              <div class="alignment-wrap">
                <div class="progress-track" aria-hidden="true">
                  <div class="progress-fill" style="width: {{.AlignmentPercentage}}%;"></div>
                </div>
                <span>{{.AlignmentPercentage}}%</span>
              </div>
              {{else}}
              <span class="muted">—</span>
              {{end}}
            </td>
            <td>
              {{if .IssueURL}}<a href="{{.IssueURL}}" target="_blank" rel="noopener noreferrer">{{.IssueLabel}}</a>{{else}}<span class="muted">—</span>{{end}}
            </td>
            <td>
              <div class="timestamp-rel">{{.LastCheckedRelative}}</div>
              {{if .LastCheckedUTC}}<div class="timestamp-utc">{{.LastCheckedUTC}}</div>{{end}}
            </td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
    {{else}}
    <div class="empty-state">
      <h2>No repositories are currently tracked</h2>
      <p>Invite the Speculate collaborator to a repository to enroll it automatically:</p>
      <code>speculate init --repo owner/repo</code>
    </div>
    {{end}}
  </div>
</body>
</html>
`
