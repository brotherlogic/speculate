package main

import (
	"context"
	"testing"
)

func TestValidateDCMProtoSchema(t *testing.T) {
	if err := validateDCMProtoSchema(); err != nil {
		t.Fatalf("validateDCMProtoSchema failed: %v", err)
	}
}

func TestRunClientsProberDryRun(t *testing.T) {
	ctx := context.Background()
	err := runClientsProber(ctx, "brotherlogic/speculate-kv", true, "", "")
	if err != nil {
		t.Fatalf("runClientsProber in dry-run mode failed: %v", err)
	}
}
