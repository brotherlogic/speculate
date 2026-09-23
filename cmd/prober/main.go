package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/brotherlogic/speculate/pkg/evaluator"
	"github.com/brotherlogic/speculate/pkg/parser"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
)

func main() {
	mode := flag.String("mode", "evaluator", "Prober execution mode: evaluator, harness, simulation")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch *mode {
	case "evaluator":
		if err := runEvaluatorProber(ctx); err != nil {
			log.Fatalf("❌ Prober 2 Failed: %v", err)
		}
		fmt.Println("✅ [PROBER 2 PASS] Evaluator and Synthesizer verified successfully!")
	default:
		log.Fatalf("Unknown prober mode: %s", *mode)
	}
}

func runEvaluatorProber(ctx context.Context) error {
	specPath := filepath.Join("example", "specs", "kv.md")
	testsDir := filepath.Join("example", "tests")

	fmt.Println("🔍 Step 1: Parsing specification and existing tests...")
	spec, err := parser.ParseSpecFile(specPath)
	if err != nil {
		return fmt.Errorf("parsing spec %q: %w", specPath, err)
	}

	testSuite, err := parser.ParseTestDir(testsDir)
	if err != nil {
		return fmt.Errorf("parsing tests %q: %w", testsDir, err)
	}

	fmt.Printf("✓ Spec: %s (%d stages, %d requirements)\n", spec.Title, len(spec.Stages), spec.TotalRequirements())
	fmt.Printf("✓ Discovered tests: %d scenarios\n\n", len(testSuite.Scenarios))

	fmt.Println("🔍 Step 2: Running Spec Evaluator...")
	llmClient := evaluator.NewOllamaClient("", "")
	eval := evaluator.NewEvaluator(llmClient)

	evalResult, err := eval.Evaluate(ctx, spec, testSuite)
	if err != nil {
		return fmt.Errorf("evaluating spec alignment: %w", err)
	}

	fmt.Printf("✓ Alignment Score: %d%% (%s)\n", evalResult.Percentage, evalResult.BadgeMarkdown)
	if evalResult.Percentage != 0 {
		return fmt.Errorf("expected 0%% alignment on empty test baseline, got %d%%", evalResult.Percentage)
	}
	if evalResult.ActiveFrontierStage == nil {
		return fmt.Errorf("expected active frontier stage, got nil")
	}
	if evalResult.NextRequirement == nil {
		return fmt.Errorf("expected next unexercised requirement, got nil")
	}

	fmt.Printf("✓ Active Frontier Stage: %s\n", evalResult.ActiveFrontierStage.Name)
	fmt.Printf("✓ Next Requirement to Exercise: [%s] %s\n\n", evalResult.NextRequirement.ID, evalResult.NextRequirement.Description)

	fmt.Println("🔍 Step 3: Synthesizing Scenario Card...")
	syn := synthesizer.NewSynthesizer(llmClient)

	protoContract := `service KV {
  rpc Put(PutRequest) returns (PutResponse);
  rpc Get(GetRequest) returns (GetResponse);
}`

	card, err := syn.GenerateScenarioCard(ctx, evalResult.ActiveFrontierStage, evalResult.NextRequirement, protoContract)
	if err != nil {
		return fmt.Errorf("generating scenario card: %w", err)
	}

	fmt.Println("✓ Generated Scenario Card:")
	fmt.Println(card.FormatMarkdown())

	fmt.Println("🔍 Step 4: Validating executable test code and running Mutation Probe (Red Phase)...")

	// Canonical test template exercising the scenario against the skeleton server
	testCode := fmt.Sprintf(`package tests

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/brotherlogic/speculate/example/internal/server"
	pb "github.com/brotherlogic/speculate/example/proto/kv/v1"
)

// Stage: %s
// Scenario: %s %s
func %s(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	pb.RegisterKVServer(s, server.New())
	go s.Serve(lis)
	t.Cleanup(func() { s.Stop() })

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial: %%v", err)
	}
	t.Cleanup(func() { conn.Close() })

	client := pb.NewKVClient(conn)
	_, err = client.Put(context.Background(), &pb.PutRequest{Key: "prober-key", Value: []byte("prober-value")})
	if err != nil {
		t.Fatalf("Put failed: %%v", err)
	}
}
`, card.Stage, card.ID, card.Title, card.TestFuncName)

	if err := synthesizer.ValidateGoCode(testCode); err != nil {
		return fmt.Errorf("test code failed syntax validation: %w", err)
	}
	fmt.Println("✓ Synthesized test passes Go syntax validation.")

	probeResult, err := syn.MutationProbe(ctx, testCode, testsDir)
	if err != nil {
		return fmt.Errorf("mutation probe execution error: %w", err)
	}

	if !probeResult.IsRed {
		return fmt.Errorf("mutation probe expected RED test failure against skeleton server, but test passed unexpectedly!\nOutput:\n%s", probeResult.Output)
	}

	fmt.Println("✓ Mutation Probe successfully confirmed RED failure against skeleton server:")
	for _, line := range osLines(probeResult.Output) {
		if len(line) > 0 {
			fmt.Printf("   | %s\n", line)
		}
	}
	fmt.Println()

	return nil
}

func osLines(s string) []string {
	var lines []string
	curr := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, curr)
			curr = ""
		} else if r != '\r' {
			curr += string(r)
		}
	}
	if curr != "" {
		lines = append(lines, curr)
	}
	return lines
}
