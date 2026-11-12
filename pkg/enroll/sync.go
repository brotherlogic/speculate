package enroll

import (
	"context"
	"log"
	"reflect"
	"strings"
	"time"

	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/pstore"
	pb "github.com/brotherlogic/speculate/proto"
)

// DefaultSyncInterval is the default polling interval used by Syncer.Run when interval <= 0.
const DefaultSyncInterval = 5 * time.Minute

// EnrollmentRunner defines the narrow repository evaluation interface satisfied by *Pipeline.
type EnrollmentRunner interface {
	Enroll(ctx context.Context, repo string) (*pb.EnrollResponse, error)
}

var _ EnrollmentRunner = (*Pipeline)(nil)

// Syncer orchestrates startup and periodic GitHub collaboration invitation acceptance,
// active collaborator repository discovery, safe removal pruning, and lifecycle evaluation.
type Syncer struct {
	accountClient ghclient.AccountClient
	store         pstore.Store
	runner        EnrollmentRunner
}

// NewSyncer constructs a new background collaborator Syncer.
func NewSyncer(accountClient ghclient.AccountClient, store pstore.Store, runner EnrollmentRunner) *Syncer {
	return &Syncer{
		accountClient: accountClient,
		store:         store,
		runner:        runner,
	}
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

// SyncOnce executes a single collaborator discovery, safe pruning, and evaluation cycle.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	if s == nil || isNilInterface(s.accountClient) {
		log.Printf("Warning: Syncer has no GitHub AccountClient configured; skipping collaborator sync")
		return nil
	}

	accepted, err := s.accountClient.AcceptPendingInvitations(ctx)
	for _, repo := range accepted {
		log.Printf("Syncer accepted pending collaboration invitation for %s", repo)
	}
	if err != nil {
		log.Printf("Warning: failed accepting one or more pending collaboration invitations (continuing discovery): %v", err)
	}

	repos, err := s.accountClient.ListCollaboratorRepositories(ctx)
	if err != nil {
		log.Printf("Warning: failed to list collaborator repositories (aborting pruning and evaluation): %v", err)
		return err
	}

	if !isNilInterface(s.store) {
		stored, listErr := s.store.ListEnrollments(ctx)
		if listErr != nil {
			log.Printf("Warning: failed to list stored enrollments for collaborator pruning: %v", listErr)
		} else {
			activeSet := make(map[string]struct{}, len(repos))
			for _, r := range repos {
				norm := strings.ToLower(pstore.NormalizeRepo(r))
				if norm != "" {
					activeSet[norm] = struct{}{}
				}
			}

			for _, rec := range stored {
				if rec == nil {
					continue
				}
				normStored := strings.ToLower(pstore.NormalizeRepo(rec.GetRepository()))
				if _, active := activeSet[normStored]; !active {
					if delErr := s.store.DeleteEnrollment(ctx, rec.GetRepository()); delErr != nil {
						log.Printf("Warning: failed to delete pruned enrollment record for %s: %v", rec.GetRepository(), delErr)
					} else {
						log.Printf("Syncer pruned removed collaborator repository %s", rec.GetRepository())
					}
				}
			}
		}
	}

	if isNilInterface(s.runner) {
		log.Printf("Warning: Syncer has no EnrollmentRunner configured; skipping repository evaluation")
		return nil
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.TrimSpace(repo) == "" {
			continue
		}
		if _, evalErr := s.runner.Enroll(ctx, repo); evalErr != nil {
			log.Printf("Warning: failed to evaluate collaborator repository %s: %v", repo, evalErr)
		}
	}

	return nil
}

// Run executes SyncOnce immediately on startup and then periodically at interval until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultSyncInterval
	}

	if err := s.SyncOnce(ctx); err != nil && ctx.Err() == nil {
		log.Printf("Warning: initial collaborator sync completed with error: %v", err)
	}
	if ctx.Err() != nil {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			if err := s.SyncOnce(ctx); err != nil && ctx.Err() == nil {
				log.Printf("Warning: periodic collaborator sync completed with error: %v", err)
			}
		}
	}
}
