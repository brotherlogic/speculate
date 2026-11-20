package enroll

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/brotherlogic/speculate/pkg/dcm"
	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/orchestrator"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
)

// Pipeline orchestrates repository enrollment and lifecycle reconciliation by delegating
// to the Phase 4 orchestrator.Reconciler while preserving backward-compatible options.
type Pipeline struct {
	reconciler *orchestrator.Reconciler
	cloneFn    func(ctx context.Context, repoURL, targetDir string) error
}

// NewPipeline constructs an enrollment Pipeline orchestrator backed by orchestrator.Reconciler.
func NewPipeline(
	token string,
	githubClientFactory func(token, owner, repo string) ghclient.Client,
	eval *evaluator.Evaluator,
	synth *synthesizer.Synthesizer,
	store pstore.Store,
) *Pipeline {
	p := &Pipeline{}
	rec := orchestrator.NewReconciler(token, githubClientFactory, nil, eval, synth, store).
		WithLegacyMainBadge(true)
	rec.WithCloneFn(func(ctx context.Context, repoURL, targetDir string) error {
		if p.cloneFn != nil {
			return p.cloneFn(ctx, repoURL, targetDir)
		}
		return defaultClone(ctx, repoURL, targetDir)
	})
	p.reconciler = rec
	return p
}

// Reconciler returns the underlying Phase 4 Reconciler.
func (p *Pipeline) Reconciler() *orchestrator.Reconciler {
	return p.reconciler
}

// WithCloneFn configures a custom clone function, useful for hermetic testing.
func (p *Pipeline) WithCloneFn(fn func(ctx context.Context, repoURL, targetDir string) error) *Pipeline {
	p.cloneFn = fn
	return p
}

// WithDCMClient configures the devcontainer-manager client on the underlying Reconciler
// and switches README badge updates to PR-only mode when non-nil.
func (p *Pipeline) WithDCMClient(client dcm.Client) *Pipeline {
	p.reconciler.WithDCMClient(client)
	p.reconciler.WithLegacyMainBadge(client == nil)
	return p
}

// WithHarness configures the DCM harness on the underlying Reconciler.
func (p *Pipeline) WithHarness(harness dcm.Harness) *Pipeline {
	p.reconciler.WithHarness(harness)
	return p
}

// WithModel configures the DCM model on the underlying Reconciler.
func (p *Pipeline) WithModel(model string) *Pipeline {
	p.reconciler.WithModel(model)
	return p
}

// WithAttemptTimeout configures the per-attempt timeout on the underlying Reconciler.
func (p *Pipeline) WithAttemptTimeout(d time.Duration) *Pipeline {
	p.reconciler.WithAttemptTimeout(d)
	return p
}

// WithNowFn configures a custom clock function on the underlying Reconciler.
func (p *Pipeline) WithNowFn(fn func() time.Time) *Pipeline {
	p.reconciler.WithNowFn(fn)
	return p
}

// ListEnrollments returns all tracked repository enrollment records from storage.
func (p *Pipeline) ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
	return p.reconciler.ListEnrollments(ctx)
}

// Enroll orchestrates enrollment and Phase 4 reconciliation of a repository.
func (p *Pipeline) Enroll(ctx context.Context, repo string) (*pb.EnrollResponse, error) {
	return p.reconciler.Reconcile(ctx, repo)
}

func defaultClone(ctx context.Context, repoURL, targetDir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", repoURL, targetDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
