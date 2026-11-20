package enroll

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	ghwebhookpb "github.com/brotherlogic/ghwebhook/proto/ghwebhook/v1"
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

// WebhookRegistrar registers and unregisters tracked repositories with ghwebhook.
type WebhookRegistrar interface {
	Register(ctx context.Context, repo, serviceAddr string) error
	Unregister(ctx context.Context, repo, serviceAddr string) error
}

// GRPCWebhookRegistrar backs WebhookRegistrar using a ghwebhook RegistrationServiceClient.
type GRPCWebhookRegistrar struct {
	client ghwebhookpb.RegistrationServiceClient
}

// NewGRPCWebhookRegistrar constructs a WebhookRegistrar backed by a gRPC RegistrationServiceClient.
func NewGRPCWebhookRegistrar(client ghwebhookpb.RegistrationServiceClient) *GRPCWebhookRegistrar {
	return &GRPCWebhookRegistrar{client: client}
}

// Register registers repo with ghwebhook for delivery to serviceAddr.
func (r *GRPCWebhookRegistrar) Register(ctx context.Context, repo, serviceAddr string) error {
	if r == nil || isNilInterface(r.client) {
		return fmt.Errorf("ghwebhook registration client is not configured")
	}
	resp, err := r.client.Register(ctx, &ghwebhookpb.RegistrationRequest{
		RepoFullName:   repo,
		ServiceAddress: serviceAddr,
	})
	if err != nil {
		return fmt.Errorf("registering %s with ghwebhook: %w", repo, err)
	}
	if resp != nil && !resp.GetSuccess() {
		return fmt.Errorf("ghwebhook rejected registration for %s: %s", repo, resp.GetMessage())
	}
	return nil
}

// Unregister removes repo registration from ghwebhook for serviceAddr.
func (r *GRPCWebhookRegistrar) Unregister(ctx context.Context, repo, serviceAddr string) error {
	if r == nil || isNilInterface(r.client) {
		return fmt.Errorf("ghwebhook registration client is not configured")
	}
	resp, err := r.client.Unregister(ctx, &ghwebhookpb.UnregisterRequest{
		RepoFullName:   repo,
		ServiceAddress: serviceAddr,
	})
	if err != nil {
		return fmt.Errorf("unregistering %s from ghwebhook: %w", repo, err)
	}
	if resp != nil && !resp.GetSuccess() {
		return fmt.Errorf("ghwebhook rejected unregistration for %s: %s", repo, resp.GetMessage())
	}
	return nil
}

// Syncer orchestrates startup and periodic GitHub collaboration invitation acceptance,
// active collaborator repository discovery, safe removal pruning, and lifecycle evaluation.
type Syncer struct {
	accountClient ghclient.AccountClient
	store         pstore.Store
	runner        EnrollmentRunner
	registrar     WebhookRegistrar
	serviceAddr   string
}

// NewSyncer constructs a new background collaborator Syncer.
func NewSyncer(accountClient ghclient.AccountClient, store pstore.Store, runner EnrollmentRunner) *Syncer {
	return &Syncer{
		accountClient: accountClient,
		store:         store,
		runner:        runner,
	}
}

// WithWebhookRegistrar configures automatic ghwebhook repository registration/unregistration on Syncer.
func (s *Syncer) WithWebhookRegistrar(registrar WebhookRegistrar, serviceAddr string) *Syncer {
	if s == nil {
		return nil
	}
	s.registrar = registrar
	s.serviceAddr = strings.TrimSpace(serviceAddr)
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
					if !isNilInterface(s.registrar) && strings.TrimSpace(rec.GetRepository()) != "" {
						if unregErr := s.registrar.Unregister(ctx, rec.GetRepository(), s.serviceAddr); unregErr != nil {
							log.Printf("Warning: failed to unregister pruned repository %s from ghwebhook: %v", rec.GetRepository(), unregErr)
						}
					}
				}
			}
		}
	}

	runnerMissing := isNilInterface(s.runner)
	if runnerMissing {
		log.Printf("Warning: Syncer has no EnrollmentRunner configured; skipping repository evaluation")
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.TrimSpace(repo) == "" {
			continue
		}
		if !isNilInterface(s.registrar) {
			if regErr := s.registrar.Register(ctx, repo, s.serviceAddr); regErr != nil {
				log.Printf("Warning: failed to register active repository %s with ghwebhook: %v", repo, regErr)
			}
		}
		if !runnerMissing {
			if _, evalErr := s.runner.Enroll(ctx, repo); evalErr != nil {
				log.Printf("Warning: failed to evaluate collaborator repository %s: %v", repo, evalErr)
			}
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
