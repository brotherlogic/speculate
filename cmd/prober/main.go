package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/brotherlogic/speculate/pkg/evaluator"
	"github.com/brotherlogic/speculate/pkg/parser"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	proberRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "speculate_prober_runs_total",
			Help: "Total count of speculate prober execution runs.",
		},
		[]string{"repo", "prober", "status"},
	)
	proberDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "speculate_prober_duration_seconds",
			Help:    "Duration of prober execution runs in seconds.",
			Buckets: []float64{0.5, 1.0, 2.5, 5.0, 10.0, 15.0, 20.0, 30.0, 45.0, 60.0, 90.0, 120.0},
		},
		[]string{"repo", "prober"},
	)
	proberLastDurationSeconds = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "speculate_prober_last_duration_seconds",
			Help: "Duration of the most recent prober execution run in seconds.",
		},
		[]string{"repo", "prober"},
	)
	proberStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "speculate_prober_status",
			Help: "Status code of the prober run (1 = success, 0 = failure).",
		},
		[]string{"repo", "prober"},
	)
	specAlignmentScore = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "speculate_alignment_score",
			Help: "Calculated alignment percentage of the target repository.",
		},
		[]string{"repo"},
	)
)

func main() {
	defaultMode := os.Getenv("PROBER_MODE")
	if defaultMode == "" {
		defaultMode = "evaluator"
	}
	defaultTargetDir := os.Getenv("PROBER_TARGET_DIR")
	defaultTargetRepo := os.Getenv("PROBER_TARGET_REPO")
	if defaultTargetRepo == "" {
		defaultTargetRepo = "https://github.com/brotherlogic/speculate-kv"
	}
	defaultOllamaEndpoint := os.Getenv("OLLAMA_ENDPOINT")
	if defaultOllamaEndpoint == "" {
		defaultOllamaEndpoint = "http://192.168.68.112:11434/v1"
	}
	defaultMetricsAddr := os.Getenv("PROBER_METRICS_ADDR")

	mode := flag.String("mode", defaultMode, "Prober execution mode: evaluator, simulation, cluster")
	targetDir := flag.String("target-dir", defaultTargetDir, "Local target directory of the repository to probe (e.g. /tmp/speculate-kv)")
	targetRepo := flag.String("target-repo", defaultTargetRepo, "Target repository git URL")
	ollamaEndpoint := flag.String("ollama-endpoint", defaultOllamaEndpoint, "Ollama API endpoint")
	metricsAddr := flag.String("metrics-addr", defaultMetricsAddr, "Optional address to serve Prometheus metrics until scraped (e.g. :8081)")
	metricsHoldTimeout := flag.Duration("metrics-hold-timeout", 30*time.Second, "Hold duration to wait for Prometheus scrape")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	var runErr error

	resolvedDir, cleanup, err := resolveTargetDir(ctx, *targetDir, *targetRepo)
	if err != nil {
		log.Fatalf("❌ Failed to resolve target repo: %v", err)
	}
	defer cleanup()

	switch *mode {
	case "evaluator":
		runErr = runEvaluatorProber(ctx, resolvedDir, *targetRepo, *ollamaEndpoint)
		if runErr != nil {
			log.Printf("❌ Prober Failed: %v", runErr)
		} else {
			fmt.Println("✅ [PROBER PASS] Target repository evaluated and synthesized successfully!")
		}
	default:
		runErr = fmt.Errorf("unknown prober mode: %s", *mode)
		log.Printf("❌ %v", runErr)
	}

	duration := time.Since(start)
	recordProberResult(*targetRepo, *mode, duration, runErr)

	if *metricsAddr != "" {
		log.Printf("Serving prober metrics on %s (hold timeout: %v)...", *metricsAddr, *metricsHoldTimeout)
		if err := serveMetricsUntilScraped(ctx, *metricsAddr, *metricsHoldTimeout); err != nil {
			log.Printf("Warning: failed serving metrics: %v", err)
		}
	}

	if runErr != nil {
		os.Exit(1)
	}
}

func resolveTargetDir(ctx context.Context, dir, repoURL string) (string, func(), error) {
	noop := func() {}

	if dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return dir, noop, nil
		}
	}

	// Check if /tmp/speculate-kv exists locally
	candidate := "/tmp/speculate-kv"
	if _, err := os.Stat(candidate); err == nil {
		return candidate, noop, nil
	}

	// Fallback to testdata if local
	if _, err := os.Stat("testdata/specs/kv.md"); err == nil {
		return "testdata", noop, nil
	}

	// Otherwise clone target repo into a temp directory
	tempDir, err := os.MkdirTemp("", "speculate-target-*")
	if err != nil {
		return "", noop, fmt.Errorf("creating temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tempDir) }

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", repoURL, tempDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("cloning %s: %s: %w", repoURL, string(out), err)
	}

	return tempDir, cleanup, nil
}

func runEvaluatorProber(ctx context.Context, repoDir, targetRepo, ollamaEndpoint string) error {
	specPath := filepath.Join(repoDir, "specs", "kv.md")
	testsDir := filepath.Join(repoDir, "tests")

	fmt.Printf("🔍 Step 1: Parsing specification at %s...\n", specPath)
	spec, err := parser.ParseSpecFile(specPath)
	if err != nil {
		return fmt.Errorf("parsing spec %q: %w", specPath, err)
	}

	testSuite, err := parser.ParseTestDir(testsDir)
	if err != nil {
		return fmt.Errorf("parsing tests %q: %w", testsDir, err)
	}

	fmt.Printf("✓ Target Spec: %s (%d stages, %d requirements)\n", spec.Title, len(spec.Stages), spec.TotalRequirements())
	fmt.Printf("✓ Discovered tests: %d scenarios\n\n", len(testSuite.Scenarios))

	fmt.Println("🔍 Step 2: Running Spec Evaluator against target...")
	llmClient := evaluator.NewOllamaClient(ollamaEndpoint, "")
	eval := evaluator.NewEvaluator(llmClient)

	evalResult, err := eval.Evaluate(ctx, spec, testSuite)
	if err != nil {
		return fmt.Errorf("evaluating spec alignment: %w", err)
	}

	recordAlignmentScore(targetRepo, evalResult.Percentage)
	fmt.Printf("✓ Alignment Score: %d%% (%s)\n", evalResult.Percentage, evalResult.BadgeMarkdown)
	if evalResult.ActiveFrontierStage == nil {
		return fmt.Errorf("expected active frontier stage, got nil")
	}
	if evalResult.NextRequirement == nil {
		return fmt.Errorf("expected next unexercised requirement, got nil")
	}

	fmt.Printf("✓ Active Frontier Stage: %s\n", evalResult.ActiveFrontierStage.Name)
	fmt.Printf("✓ Next Requirement to Exercise: [%s] %s\n\n", evalResult.NextRequirement.ID, evalResult.NextRequirement.Description)

	fmt.Println("🔍 Step 3: Synthesizing Scenario Card via local LLM...")
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

	testCode := fmt.Sprintf(`package tests

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/brotherlogic/speculate-kv/internal/server"
	pb "github.com/brotherlogic/speculate-kv/proto/kv/v1"
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

	fmt.Println("✓ Mutation Probe successfully confirmed RED failure against target repo skeleton server:")
	for _, line := range osLines(probeResult.Output) {
		if len(line) > 0 {
			fmt.Printf("   | %s\n", line)
		}
	}
	fmt.Println()

	return nil
}

func serveMetricsUntilScraped(ctx context.Context, addr string, timeout time.Duration) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	scrapedCh := make(chan struct{})
	var once sync.Once

	promHandler := promhttp.Handler()
	scrapeDetector := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		promHandler.ServeHTTP(w, r)
		once.Do(func() {
			close(scrapedCh)
		})
	})

	mux := http.NewServeMux()
	mux.Handle("/metrics", scrapeDetector)

	server := &http.Server{Handler: mux}
	serverErrCh := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			serverErrCh <- serveErr
		}
		close(serverErrCh)
	}()

	var timer *time.Timer
	var timerCh <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timerCh = timer.C
	}

	select {
	case err := <-serverErrCh:
		return err
	case <-scrapedCh:
	case <-timerCh:
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func recordProberResult(repo, proberMode string, duration time.Duration, runErr error) {
	statusStr := "success"
	statusVal := 1.0
	if runErr != nil {
		statusStr = "failure"
		statusVal = 0.0
	}
	proberRunsTotal.WithLabelValues(repo, proberMode, statusStr).Inc()
	proberDurationSeconds.WithLabelValues(repo, proberMode).Observe(duration.Seconds())
	proberLastDurationSeconds.WithLabelValues(repo, proberMode).Set(duration.Seconds())
	proberStatus.WithLabelValues(repo, proberMode).Set(statusVal)
}

func recordAlignmentScore(repo string, score int) {
	specAlignmentScore.WithLabelValues(repo).Set(float64(score))
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
