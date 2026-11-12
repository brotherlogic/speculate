package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	pb "github.com/brotherlogic/speculate/proto"
)

type mockEnrollmentLister struct {
	listFn func(ctx context.Context) ([]*pb.EnrollmentRecord, error)
}

func (m *mockEnrollmentLister) ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
	if m.listFn != nil {
		return m.listFn(ctx)
	}
	return nil, nil
}

func assertSummaryMetric(t *testing.T, htmlBody, metricID, expectedValue string) {
	t.Helper()
	pattern := `data-metric="` + regexp.QuoteMeta(metricID) + `"[^>]*>\s*` + regexp.QuoteMeta(expectedValue) + `\s*<`
	matched, err := regexp.MatchString(pattern, htmlBody)
	if err != nil {
		t.Fatalf("invalid regex %q: %v", pattern, err)
	}
	if !matched {
		t.Errorf("expected summary metric %q to equal %q in rendered HTML", metricID, expectedValue)
	}
}

func TestDashboardHandler_EmptyState(t *testing.T) {
	lister := &mockEnrollmentLister{
		listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
			return []*pb.EnrollmentRecord{}, nil
		},
	}

	handler := NewDashboardHandler(lister)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want prefix %q", ct, "text/html")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "No repositories are currently tracked") {
		t.Errorf("expected empty state message in body, got:\n%s", body)
	}
	if !strings.Contains(body, "speculate init") {
		t.Errorf("expected 'speculate init' quickstart hint in empty state, got:\n%s", body)
	}

	assertSummaryMetric(t, body, "total", "0")
	assertSummaryMetric(t, body, "aligned", "0")
	assertSummaryMetric(t, body, "attention", "0")
}

func TestDashboardHandler_PopulatedAllStages(t *testing.T) {
	fixedNow := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)

	records := []*pb.EnrollmentRecord{
		{
			Repository:          "brotherlogic/seed-repo",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
			AlignmentPercentage: 0,
			IssueUrl:            "https://github.com/brotherlogic/seed-repo/issues/1",
			LastEvaluatedAtUnix: fixedNow.Add(-30 * time.Second).Unix(),
			StatusMessage:       "waiting for seed files",
		},
		{
			Repository:          "brotherlogic/eval-repo",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING,
			AlignmentPercentage: 25,
			IssueUrl:            "",
			LastEvaluatedAtUnix: fixedNow.Add(-4 * time.Minute).Unix(),
			StatusMessage:       "evaluating specification coverage",
		},
		{
			Repository:          "brotherlogic/review-repo",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
			AlignmentPercentage: 50,
			IssueUrl:            "https://github.com/brotherlogic/review-repo/pull/18",
			LastEvaluatedAtUnix: fixedNow.Add(-2 * time.Hour).Unix(),
			StatusMessage:       "awaiting scenario card review",
		},
		{
			Repository:          "brotherlogic/align-repo",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
			AlignmentPercentage: 75,
			IssueUrl:            "https://github.com/brotherlogic/align-repo/issues/42",
			LastEvaluatedAtUnix: fixedNow.Add(-3 * 24 * time.Hour).Unix(),
			StatusMessage:       "autonomous alignment in progress",
		},
		{
			Repository:          "brotherlogic/stalled-repo",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_STALLED,
			AlignmentPercentage: 60,
			IssueUrl:            "https://github.com/brotherlogic/stalled-repo/issues/99",
			LastEvaluatedAtUnix: 0,
			StatusMessage:       "stalled after max retries",
		},
		{
			Repository:          "brotherlogic/aligned-repo",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
			AlignmentPercentage: 100,
			IssueUrl:            "https://github.com/brotherlogic/aligned-repo/actions",
			LastEvaluatedAtUnix: fixedNow.Unix(),
			StatusMessage:       "100% aligned",
		},
		{
			Repository:          "brotherlogic/unspecified-repo",
			Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_UNSPECIFIED,
			AlignmentPercentage: 15,
			IssueUrl:            "",
			LastEvaluatedAtUnix: -10,
		},
	}

	lister := &mockEnrollmentLister{
		listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
			return records, nil
		},
	}

	handler := newDashboardHandlerWithClock(lister, func() time.Time { return fixedNow })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}

	body := rec.Body.String()

	if strings.Contains(body, "No repositories are currently tracked") {
		t.Errorf("did not expect empty state message when records are populated")
	}

	// Verify summary counts: 7 total, 1 aligned, 1 attention needed (stalled-repo)
	assertSummaryMetric(t, body, "total", "7")
	assertSummaryMetric(t, body, "aligned", "1")
	assertSummaryMetric(t, body, "attention", "1")

	// Verify GitHub links, stage labels, badge classes, colors, and responsive/theme CSS
	expectedSubstrings := []string{
		`href="https://github.com/brotherlogic/seed-repo"`,
		`href="https://github.com/brotherlogic/eval-repo"`,
		`href="https://github.com/brotherlogic/review-repo"`,
		`href="https://github.com/brotherlogic/align-repo"`,
		`href="https://github.com/brotherlogic/stalled-repo"`,
		`href="https://github.com/brotherlogic/aligned-repo"`,
		"Awaiting Seed",
		"stage-seed",
		"#1d76db",
		"Evaluating",
		"stage-evaluating",
		"#6366f1",
		"Awaiting Test Review",
		"stage-review",
		"#d97706",
		"Aligning",
		"stage-aligning",
		"#8b5cf6",
		"Stalled",
		"stage-stalled",
		"#dc2626",
		"Aligned",
		"stage-aligned",
		"#059669",
		"Unspecified",
		"stage-unspecified",
		// Extracted issue/PR labels & fallback
		`href="https://github.com/brotherlogic/seed-repo/issues/1"`,
		"#1",
		`href="https://github.com/brotherlogic/review-repo/pull/18"`,
		"#18",
		`href="https://github.com/brotherlogic/align-repo/issues/42"`,
		"#42",
		`href="https://github.com/brotherlogic/stalled-repo/issues/99"`,
		"#99",
		`href="https://github.com/brotherlogic/aligned-repo/actions"`,
		"View Issue",
		// Alignment percentages
		"25%",
		"50%",
		"75%",
		"60%",
		"100%",
		"15%",
		// Deterministic UTC & relative timestamps
		"2026-11-11 15:59:30 UTC",
		"just now",
		"2026-11-11 15:56:00 UTC",
		"4m ago",
		"2026-11-11 14:00:00 UTC",
		"2h ago",
		"2026-11-08 16:00:00 UTC",
		"3d ago",
		"2026-11-11 16:00:00 UTC",
		"Never",
		// Responsive & dark mode styling
		"overflow-x: auto",
		"prefers-color-scheme: dark",
	}

	for _, want := range expectedSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("rendered HTML missing expected substring %q", want)
		}
	}

	// Verify AWAITING_SEED renders "—" rather than "0%" for alignment
	if strings.Contains(body, ">0%<") || strings.Contains(body, "width: 0%") {
		t.Errorf("expected AWAITING_SEED row to render '—' instead of '0%%'")
	}
	seedRowRe := regexp.MustCompile(`(?s)<tr>\s*<td>[^<]*<a href="https://github\.com/brotherlogic/seed-repo".*?</tr>`)
	seedRow := seedRowRe.FindString(body)
	if seedRow == "" {
		t.Fatalf("could not locate seed-repo table row in HTML")
	}
	if !strings.Contains(seedRow, "—") || strings.Contains(seedRow, "progress-fill") {
		t.Errorf("expected seed-repo row to render '—' without progress bar, got:\n%s", seedRow)
	}
}

func TestDashboardHandler_ErrorPreservationAndXSSEscaping(t *testing.T) {
	fixedNow := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)

	lister := &mockEnrollmentLister{
		listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
			return []*pb.EnrollmentRecord{
				{
					Repository:          "brotherlogic/<script>repo</script>",
					Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
					AlignmentPercentage: 80,
					IssueUrl:            "https://github.com/brotherlogic/repo/issues/12",
					LastEvaluatedAtUnix: fixedNow.Add(-1 * time.Minute).Unix(),
					StatusMessage:       `status <img src=x onerror=alert("status")>`,
					LastError:           `github rate limit: <script>alert("xss")</script>`,
				},
				{
					Repository:          "brotherlogic/stalled-and-errored",
					Stage:               pb.LifecycleStage_LIFECYCLE_STAGE_STALLED,
					AlignmentPercentage: 40,
					LastEvaluatedAtUnix: fixedNow.Add(-5 * time.Minute).Unix(),
					LastError:           "ollama connection refused",
				},
			}, nil
		},
	}

	handler := newDashboardHandlerWithClock(lister, func() time.Time { return fixedNow })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()

	// Preserves stage badge ("Aligning") AND renders escaped LastError callout
	if !strings.Contains(body, "Aligning") || !strings.Contains(body, "stage-aligning") {
		t.Errorf("expected preserved 'Aligning' stage badge when LastError is set")
	}
	if !strings.Contains(body, "github rate limit: &lt;script&gt;alert(") {
		t.Errorf("expected HTML-escaped LastError message in body, got:\n%s", body)
	}
	if !strings.Contains(body, "&lt;img src=x onerror=alert(") {
		t.Errorf("expected HTML-escaped StatusMessage in body, got:\n%s", body)
	}
	if !strings.Contains(body, "brotherlogic/&lt;script&gt;repo&lt;/script&gt;") {
		t.Errorf("expected HTML-escaped Repository name in body, got:\n%s", body)
	}

	// Ensure raw unescaped malicious tags never appear
	if strings.Contains(body, "<script>alert(") || strings.Contains(body, "<script>repo</script>") || strings.Contains(body, "<img src=x") {
		t.Fatalf("XSS vulnerability: unescaped HTML tags found in rendered output:\n%s", body)
	}

	// Both rows require attention (row 1 via LastError != "", row 2 via STALLED + LastError without double-counting)
	assertSummaryMetric(t, body, "total", "2")
	assertSummaryMetric(t, body, "aligned", "0")
	assertSummaryMetric(t, body, "attention", "2")
}

func TestDashboardHandler_RoutingAndMethodGuards(t *testing.T) {
	lister := &mockEnrollmentLister{
		listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
			return []*pb.EnrollmentRecord{}, nil
		},
	}

	mux := http.NewServeMux()
	mux.Handle("/", NewDashboardHandler(lister))

	t.Run("HEAD / returns 200 OK with empty body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("HEAD / status = %d, want %d", rec.Code, http.StatusOK)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("HEAD / Content-Type = %q, want prefix %q", ct, "text/html")
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD / body length = %d, want 0", rec.Body.Len())
		}
	})

	t.Run("GET /unknown-path returns 404 Not Found", func(t *testing.T) {
		for _, path := range []string{"/unknown-path", "/dashboard", "/api/repos"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s status = %d, want %d", path, rec.Code, http.StatusNotFound)
			}
		}
	})

	t.Run("non-GET/HEAD methods return 405 Method Not Allowed", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req := httptest.NewRequest(method, "/", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s / status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("%s / Allow header = %q, want %q", method, allow, "GET, HEAD")
			}
		}
	})
}

func TestDashboardHandler_ListerError(t *testing.T) {
	t.Run("ListEnrollments error returns 500", func(t *testing.T) {
		lister := &mockEnrollmentLister{
			listFn: func(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
				return nil, errors.New("pstore transport failure")
			},
		}

		handler := NewDashboardHandler(lister)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("nil lister returns 500 without panicking", func(t *testing.T) {
		handler := NewDashboardHandler(nil)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}
