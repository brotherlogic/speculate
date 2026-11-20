package main

import (
	"context"
	"testing"
	"time"

	ghclient "github.com/brotherlogic/speculate/pkg/github"
	pb "github.com/brotherlogic/speculate/proto"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRunSimulationProber_Success(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const targetRepo = "brotherlogic/speculate-kv"
	if err := runSimulationProber(ctx, targetRepo); err != nil {
		t.Fatalf("runSimulationProber failed: %v", err)
	}

	score := testutil.ToFloat64(specAlignmentScore.WithLabelValues(targetRepo))
	if score != 100 {
		t.Errorf("expected specAlignmentScore=100 for %s, got %v", targetRepo, score)
	}
}

func TestRunSimulationProber_InvalidRepo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := runSimulationProber(ctx, "invalid-repo"); err == nil {
		t.Fatal("expected error for invalid repository format, got nil")
	}
}

func TestRunSimulationProber_FailurePropagation(t *testing.T) {
	const targetRepo = "brotherlogic/speculate-kv"

	tests := []struct {
		name string
		hook *simulationHook
	}{
		{
			name: "red phase missing container",
			hook: &simulationHook{
				afterRedUp: func(ctx context.Context, h *simulationHarness) {
					rec, _ := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
					if rec != nil {
						rec.Stage = pb.LifecycleStage_LIFECYCLE_STAGE_STALLED
						_ = h.mockStore.SaveEnrollment(ctx, rec)
					}
				},
			},
		},
		{
			name: "red PR not awaiting test review",
			hook: &simulationHook{
				afterRedPR: func(ctx context.Context, h *simulationHarness) {
					rec, _ := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
					if rec != nil {
						rec.Stage = pb.LifecycleStage_LIFECYCLE_STAGE_EVALUATING
						_ = h.mockStore.SaveEnrollment(ctx, rec)
					}
				},
			},
		},
		{
			name: "approval did not launch fresh green container",
			hook: &simulationHook{
				afterApproval: func(ctx context.Context, h *simulationHarness) {
					rec, _ := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
					if rec != nil {
						rec.ActiveContainerId = ""
						_ = h.mockStore.SaveEnrollment(ctx, rec)
					}
				},
			},
		},
		{
			name: "green merge did not reach 100% aligned",
			hook: &simulationHook{
				afterGreenMerge: func(ctx context.Context, h *simulationHarness) {
					rec, _ := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
					if rec != nil {
						rec.AlignmentPercentage = 50
						_ = h.mockStore.SaveEnrollment(ctx, rec)
					}
				},
			},
		},
		{
			name: "tamper guard did not reset branch",
			hook: &simulationHook{
				afterTamperReset: func(ctx context.Context, h *simulationHarness) {
					rec, _ := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
					if rec != nil && rec.GetActiveBranch() != "" {
						h.mockGH.SetBranchSHA(rec.GetActiveBranch(), "badbadbadbadbadbadbadbadbadbadbadbadbadb")
					}
				},
			},
		},
		{
			name: "strike 3 missing stalled label",
			hook: &simulationHook{
				afterStrike3Stall: func(ctx context.Context, h *simulationHarness) {
					rec, _ := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
					if rec != nil && rec.GetActiveIssueNumber() > 0 {
						_ = h.mockGH.RemoveIssueLabel(ctx, int(rec.GetActiveIssueNumber()), ghclient.LabelStalled)
					}
				},
			},
		},
		{
			name: "unstall did not reset attempt count",
			hook: &simulationHook{
				afterUnstallResume: func(ctx context.Context, h *simulationHarness) {
					rec, _ := h.mockStore.GetEnrollment(ctx, h.canonicalRepo)
					if rec != nil {
						rec.AttemptCount = 3
						_ = h.mockStore.SaveEnrollment(ctx, rec)
					}
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := runSimulationProberWithHook(ctx, targetRepo, tc.hook); err == nil {
				t.Fatalf("expected failure propagation for %q, got nil", tc.name)
			}
		})
	}
}
