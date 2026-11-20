package main

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"

	ghwebhookpb "github.com/brotherlogic/ghwebhook/proto/ghwebhook/v1"
	"github.com/brotherlogic/speculate/pkg/enroll"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/pstore"
)

// WebhookOption configures optional behavior on WebhookServer.
type WebhookOption func(*WebhookServer)

// WithEnrollmentLister configures an EnrollmentLister used to filter incoming webhooks to tracked repositories.
func WithEnrollmentLister(lister EnrollmentLister) WebhookOption {
	return func(s *WebhookServer) {
		s.lister = lister
	}
}

// WebhookServer implements ghwebhook.v1.WebhookHandlerServer to trigger asynchronous
// per-repository reconciliation when GitHub webhook events arrive.
type WebhookServer struct {
	ghwebhookpb.UnimplementedWebhookHandlerServer
	runner enroll.EnrollmentRunner
	lister EnrollmentLister
}

// NewWebhookServer creates a new WebhookServer backed by an EnrollmentRunner.
func NewWebhookServer(runner enroll.EnrollmentRunner, opts ...WebhookOption) *WebhookServer {
	s := &WebhookServer{runner: runner}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return rv.IsNil()
	default:
		return false
	}
}

// ReceiveWebhook handles incoming webhook events from ghwebhook, ignoring unrecognized or
// empty repositories gracefully while dispatching asynchronous reconciliation for tracked repos.
func (s *WebhookServer) ReceiveWebhook(ctx context.Context, event *ghwebhookpb.WebhookEvent) (*ghwebhookpb.WebhookResponse, error) {
	rawRepo := strings.TrimSpace(extractWebhookRepo(event))
	if rawRepo == "" {
		return &ghwebhookpb.WebhookResponse{
			Success: true,
			Message: "ignored webhook without repository",
		}, nil
	}

	if !strings.Contains(rawRepo, "github.com") && strings.Count(strings.Trim(rawRepo, "/"), "/") != 1 {
		return &ghwebhookpb.WebhookResponse{
			Success: true,
			Message: fmt.Sprintf("ignored invalid repository %q", rawRepo),
		}, nil
	}

	owner, repoName, err := ghclient.ParseRepo(rawRepo)
	if err != nil || strings.ContainsAny(owner, " \t\r\n") || strings.ContainsAny(repoName, " \t\r\n") {
		return &ghwebhookpb.WebhookResponse{
			Success: true,
			Message: fmt.Sprintf("ignored invalid repository %q", rawRepo),
		}, nil
	}
	canonicalRepo := fmt.Sprintf("%s/%s", owner, repoName)

	if s == nil || isNilInterface(s.runner) {
		return &ghwebhookpb.WebhookResponse{
			Success: true,
			Message: "ignored webhook: enrollment runner not configured",
		}, nil
	}

	lister := s.lister
	if isNilInterface(lister) {
		if inferred, ok := s.runner.(EnrollmentLister); ok {
			lister = inferred
		}
	}

	if !isNilInterface(lister) {
		records, listErr := lister.ListEnrollments(ctx)
		if listErr != nil {
			log.Printf("Warning: failed checking tracked enrollments for webhook %s: %v", canonicalRepo, listErr)
		} else if len(records) > 0 {
			tracked := false
			for _, rec := range records {
				if rec == nil {
					continue
				}
				if strings.EqualFold(pstore.NormalizeRepo(rec.GetRepository()), canonicalRepo) {
					tracked = true
					break
				}
			}
			if !tracked {
				return &ghwebhookpb.WebhookResponse{
					Success: true,
					Message: fmt.Sprintf("ignored untracked repository %s", canonicalRepo),
				}, nil
			}
		}
	}

	bgCtx := context.WithoutCancel(ctx)
	go func(target string) {
		if _, err := s.runner.Enroll(bgCtx, target); err != nil {
			log.Printf("Warning: webhook-triggered reconciliation for %s failed: %v", target, err)
		}
	}(canonicalRepo)

	return &ghwebhookpb.WebhookResponse{
		Success: true,
		Message: fmt.Sprintf("dispatched reconciliation for %s", canonicalRepo),
	}, nil
}

func extractWebhookRepo(event *ghwebhookpb.WebhookEvent) string {
	if event == nil {
		return ""
	}
	switch p := event.GetPayload().(type) {
	case *ghwebhookpb.WebhookEvent_PullRequest:
		return p.PullRequest.GetRepository().GetFullName()
	case *ghwebhookpb.WebhookEvent_Issue:
		return p.Issue.GetRepository().GetFullName()
	case *ghwebhookpb.WebhookEvent_Push:
		return p.Push.GetRepository().GetFullName()
	case *ghwebhookpb.WebhookEvent_PullRequestReview:
		return p.PullRequestReview.GetRepository().GetFullName()
	case *ghwebhookpb.WebhookEvent_CheckSuite:
		return p.CheckSuite.GetRepository().GetFullName()
	default:
		return ""
	}
}
