package parser

import (
	"path/filepath"
	"testing"
)

func TestParseSpecFile_ExampleKV(t *testing.T) {
	specPath := filepath.Join("..", "..", "example", "specs", "kv.md")
	spec, err := ParseSpecFile(specPath)
	if err != nil {
		t.Fatalf("ParseSpecFile(%q) failed: %v", specPath, err)
	}

	if spec.Title != "Key-Value Storage Service Specification" {
		t.Errorf("expected title 'Key-Value Storage Service Specification', got %q", spec.Title)
	}

	if len(spec.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(spec.Stages))
	}

	stage := spec.Stages[0]
	if stage.Name != "Core" {
		t.Errorf("expected stage name 'Core', got %q", stage.Name)
	}
	if stage.Description != "Basic Key-Value Storage" {
		t.Errorf("expected description 'Basic Key-Value Storage', got %q", stage.Description)
	}
	if len(stage.DependsOn) != 0 {
		t.Errorf("expected 0 dependencies for Core, got %v", stage.DependsOn)
	}

	if len(stage.Requirements) != 3 {
		t.Fatalf("expected 3 requirements in Core, got %d", len(stage.Requirements))
	}

	// Requirement 1: Put
	req1 := stage.Requirements[0]
	if req1.ID != "Core-1" {
		t.Errorf("expected ID 'Core-1', got %q", req1.ID)
	}
	if req1.TargetRPC != "Put" {
		t.Errorf("expected TargetRPC 'Put', got %q", req1.TargetRPC)
	}

	// Requirement 2: Get
	req2 := stage.Requirements[1]
	if req2.ID != "Core-2" {
		t.Errorf("expected ID 'Core-2', got %q", req2.ID)
	}
	if req2.TargetRPC != "Get" {
		t.Errorf("expected TargetRPC 'Get', got %q", req2.TargetRPC)
	}

	// Requirement 3: Get NotFound
	req3 := stage.Requirements[2]
	if req3.ID != "Core-3" {
		t.Errorf("expected ID 'Core-3', got %q", req3.ID)
	}
	if req3.TargetRPC != "Get" {
		t.Errorf("expected TargetRPC 'Get', got %q", req3.TargetRPC)
	}

	if spec.TotalRequirements() != 3 {
		t.Errorf("expected total requirements 3, got %d", spec.TotalRequirements())
	}
}

func TestParseSpec_MultipleStagesWithDependencies(t *testing.T) {
	input := `# Distributed KV Service

## [Stage: Core] Basic Operations
- Put: stores payload.
- Get: returns payload.

## [Stage: Expiry] (Depends on: Core) TTL and Eviction
- PutWithTTL: stores payload with duration.
- Expired keys return NOT_FOUND on Get.

## [Stage: Replication] (Depends on: Core, Expiry) Multi-node Sync
- Replicate: propagates writes to replica quorum.
`

	spec, err := ParseSpec(input)
	if err != nil {
		t.Fatalf("ParseSpec failed: %v", err)
	}

	if len(spec.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(spec.Stages))
	}

	// Stage 1: Core
	core := spec.StageByName("Core")
	if core == nil {
		t.Fatal("expected Core stage to be found")
	}
	if len(core.DependsOn) != 0 {
		t.Errorf("expected 0 dependencies for Core, got %v", core.DependsOn)
	}
	if len(core.Requirements) != 2 {
		t.Errorf("expected 2 requirements in Core, got %d", len(core.Requirements))
	}

	// Stage 2: Expiry
	expiry := spec.StageByName("Expiry")
	if expiry == nil {
		t.Fatal("expected Expiry stage to be found")
	}
	if len(expiry.DependsOn) != 1 || expiry.DependsOn[0] != "Core" {
		t.Errorf("expected Expiry depends on [Core], got %v", expiry.DependsOn)
	}
	if expiry.Description != "TTL and Eviction" {
		t.Errorf("expected description 'TTL and Eviction', got %q", expiry.Description)
	}

	// Stage 3: Replication
	repl := spec.StageByName("Replication")
	if repl == nil {
		t.Fatal("expected Replication stage to be found")
	}
	if len(repl.DependsOn) != 2 || repl.DependsOn[0] != "Core" || repl.DependsOn[1] != "Expiry" {
		t.Errorf("expected Replication depends on [Core, Expiry], got %v", repl.DependsOn)
	}
	if repl.Description != "Multi-node Sync" {
		t.Errorf("expected description 'Multi-node Sync', got %q", repl.Description)
	}

	if spec.TotalRequirements() != 5 {
		t.Errorf("expected total requirements 5, got %d", spec.TotalRequirements())
	}
}

func TestParseSpec_NumberedBulletsAndEdgeCases(t *testing.T) {
	input := `# Edge Case Spec

Some markdown introduction that should not be a stage or requirement.

## [Stage: Frontier]
1. ` + "`Put(k, v)`" + `: stores item.
2. ` + "`Delete(k)`" + `: removes item.
`

	spec, err := ParseSpec(input)
	if err != nil {
		t.Fatalf("ParseSpec failed: %v", err)
	}

	if len(spec.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(spec.Stages))
	}

	frontier := spec.Stages[0]
	if len(frontier.Requirements) != 2 {
		t.Fatalf("expected 2 requirements, got %d", len(frontier.Requirements))
	}

	if frontier.Requirements[0].TargetRPC != "Put" {
		t.Errorf("expected target RPC 'Put', got %q", frontier.Requirements[0].TargetRPC)
	}
	if frontier.Requirements[1].TargetRPC != "Delete" {
		t.Errorf("expected target RPC 'Delete', got %q", frontier.Requirements[1].TargetRPC)
	}
}
