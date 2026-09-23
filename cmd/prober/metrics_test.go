package main

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordProberResult_Success(t *testing.T) {
	repo := "brotherlogic/test-metrics-success"
	mode := "evaluator"
	duration := 2500 * time.Millisecond

	initialRuns := testutil.ToFloat64(proberRunsTotal.WithLabelValues(repo, mode, "success"))

	recordProberResult(repo, mode, duration, nil)

	newRuns := testutil.ToFloat64(proberRunsTotal.WithLabelValues(repo, mode, "success"))
	if newRuns != initialRuns+1 {
		t.Errorf("expected runs to increment by 1, got %f -> %f", initialRuns, newRuns)
	}

	status := testutil.ToFloat64(proberStatus.WithLabelValues(repo, mode))
	if status != 1.0 {
		t.Errorf("expected status 1.0 on success, got %f", status)
	}

	lastDuration := testutil.ToFloat64(proberLastDurationSeconds.WithLabelValues(repo, mode))
	if lastDuration != duration.Seconds() {
		t.Errorf("expected last duration %f, got %f", duration.Seconds(), lastDuration)
	}
}

func TestRecordProberResult_Failure(t *testing.T) {
	repo := "brotherlogic/test-metrics-failure"
	mode := "evaluator"
	duration := 1200 * time.Millisecond
	simulatedErr := errors.New("simulated probe failure")

	initialRuns := testutil.ToFloat64(proberRunsTotal.WithLabelValues(repo, mode, "failure"))

	recordProberResult(repo, mode, duration, simulatedErr)

	newRuns := testutil.ToFloat64(proberRunsTotal.WithLabelValues(repo, mode, "failure"))
	if newRuns != initialRuns+1 {
		t.Errorf("expected runs to increment by 1, got %f -> %f", initialRuns, newRuns)
	}

	status := testutil.ToFloat64(proberStatus.WithLabelValues(repo, mode))
	if status != 0.0 {
		t.Errorf("expected status 0.0 on failure, got %f", status)
	}

	lastDuration := testutil.ToFloat64(proberLastDurationSeconds.WithLabelValues(repo, mode))
	if lastDuration != duration.Seconds() {
		t.Errorf("expected last duration %f, got %f", duration.Seconds(), lastDuration)
	}
}

func TestRecordAlignmentScore(t *testing.T) {
	repo := "brotherlogic/test-metrics-alignment"
	score := 75

	recordAlignmentScore(repo, score)

	val := testutil.ToFloat64(specAlignmentScore.WithLabelValues(repo))
	if val != float64(score) {
		t.Errorf("expected alignment score %f, got %f", float64(score), val)
	}
}
