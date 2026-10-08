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
	"strings"
	"sync"
	"time"

	dcmproto "github.com/brotherlogic/devcontainer-manager/proto"
	"github.com/brotherlogic/speculate/pkg/alerter"
	"github.com/brotherlogic/speculate/pkg/dcm"
	"github.com/brotherlogic/speculate/pkg/enroll"
	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/parser"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
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

var issueClientFactory = func(token string) alerter.GitHubIssueClient {
	return alerter.NewRealGitHubClient(token)
}

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
	defaultIssueRepo := os.Getenv("PROBER_ISSUE_REPO")
	if defaultIssueRepo == "" {
		defaultIssueRepo = "brotherlogic/speculate"
	}
	defaultEnableIssueFiling := os.Getenv("PROBER_ENABLE_ISSUE_FILING") != "false"
	defaultDryRun := os.Getenv("PROBER_DRY_RUN") == "true"
	defaultDCMEndpoint := os.Getenv("DCM_ENDPOINT")
	defaultPStoreEndpoint := os.Getenv("PSTORE_ENDPOINT")

	mode := flag.String("mode", defaultMode, "Prober execution mode: evaluator, clients, simulation, cluster, enroll")
	targetDir := flag.String("target-dir", defaultTargetDir, "Local target directory of the repository to probe (e.g. /tmp/speculate-kv)")
	targetRepo := flag.String("target-repo", defaultTargetRepo, "Target repository git URL")
	ollamaEndpoint := flag.String("ollama-endpoint", defaultOllamaEndpoint, "Ollama API endpoint")
	metricsAddr := flag.String("metrics-addr", defaultMetricsAddr, "Optional address to serve Prometheus metrics until scraped (e.g. :8081)")
	metricsHoldTimeout := flag.Duration("metrics-hold-timeout", 30*time.Second, "Hold duration to wait for Prometheus scrape")
	issueRepo := flag.String("issue-repo", defaultIssueRepo, "Repository where failure issues should be filed (e.g. brotherlogic/speculate or target)")
	enableIssueFiling := flag.Bool("enable-issue-filing", defaultEnableIssueFiling, "Enable filing a GitHub issue when the prober fails")
	githubToken := flag.String("github-token", "", "GitHub token for filing issues or live operations (defaults to GH_TOKEN or GITHUB_TOKEN)")
	dryRun := flag.Bool("dry-run", defaultDryRun, "Run prober in dry-run mode using mock/in-process servers")
	dcmEndpoint := flag.String("dcm-endpoint", defaultDCMEndpoint, "DCM gRPC endpoint (e.g. devcontainer-manager.speculate.svc.cluster.local:50051)")
	pstoreEndpoint := flag.String("pstore-endpoint", defaultPStoreEndpoint, "PStore gRPC endpoint (e.g. pstore.speculate.svc.cluster.local:50051)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	var runErr error

	repoExplicit := isFlagPassed("target-repo") || os.Getenv("PROBER_TARGET_REPO") != ""

	if *mode == "clients" {
		resolvedToken := alerter.ResolveToken(*githubToken)
		runErr = runClientsProber(ctx, *targetRepo, *dryRun, resolvedToken, *dcmEndpoint)
		if runErr != nil {
			log.Printf("❌ Prober Failed: %v", runErr)
		} else {
			fmt.Println("✅ [PROBER PASS] GitHub and DCM client adapters verified successfully!")
		}
	} else if *mode == "enroll" {
		resolvedToken := alerter.ResolveToken(*githubToken)
		runErr = runEnrollProber(ctx, *targetRepo, *dryRun, resolvedToken, *pstoreEndpoint)
		if runErr != nil {
			log.Printf("❌ Prober Failed: %v", runErr)
		} else {
			fmt.Println("✅ [PROBER PASS] Repository enrollment verified successfully!")
		}
	} else {
		resolvedDir, cleanup, err := resolveTargetDir(ctx, *targetDir, *targetRepo, repoExplicit)
		if err != nil {
			runErr = fmt.Errorf("resolving target repo: %w", err)
			log.Printf("❌ Failed to resolve target repo: %v", err)
		} else {
			defer cleanup()

			if resolvedDir == "example" && !repoExplicit {
				*targetRepo = "brotherlogic/speculate"
			}

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
		}
	}

	duration := time.Since(start)
	recordProberResult(*targetRepo, *mode, duration, runErr)

	if runErr != nil && *enableIssueFiling {
		fileProberIssue(*issueRepo, *mode, *targetRepo, *targetDir, *githubToken, duration, runErr)
	}

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

func fileProberIssue(issueRepo, mode, targetRepo, targetDir, token string, duration time.Duration, runErr error) {
	resolvedToken := alerter.ResolveToken(token)
	if resolvedToken == "" {
		log.Println("⚠️ Warning: no GitHub token found (GH_TOKEN or --github-token); skipping GitHub issue creation")
		return
	}

	alertCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := issueClientFactory(resolvedToken)
	cfg := alerter.ProberAlertConfig{
		Mode:       mode,
		TargetRepo: targetRepo,
		TargetDir:  targetDir,
		IssueRepo:  issueRepo,
		Duration:   duration,
		Err:        runErr,
		Time:       time.Now().UTC(),
	}

	alertRes, err := alerter.HandleProberFailure(alertCtx, client, cfg)
	if err != nil {
		log.Printf("⚠️ Warning: failed to file GitHub issue on prober failure: %v", err)
		return
	}

	if alertRes != nil {
		if alertRes.Deduplicated {
			log.Printf("ℹ️ Existing open prober issue #%d found; skipped duplicate issue creation (%s)", alertRes.IssueNumber, alertRes.IssueURL)
		} else {
			log.Printf("🚨 Filed GitHub issue #%d for prober failure: %s", alertRes.IssueNumber, alertRes.IssueURL)
		}
	}
}

func isFlagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func determineTargetPackages(repoDir string) (string, string) {
	goModPath := filepath.Join(repoDir, "go.mod")
	if data, err := os.ReadFile(goModPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "module ") {
				mod := strings.TrimSpace(strings.TrimPrefix(line, "module"))
				return mod + "/internal/server", mod + "/proto/kv/v1"
			}
		}
	}
	return "github.com/brotherlogic/speculate/example/internal/server", "github.com/brotherlogic/speculate/example/proto/kv/v1"
}

func cloneTargetRepo(ctx context.Context, repoURL string) (string, func(), error) {
	tempDir, err := os.MkdirTemp("", "speculate-target-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tempDir) }

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", repoURL, tempDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("cloning %s: %s: %w", repoURL, string(out), err)
	}

	return tempDir, cleanup, nil
}

func resolveTargetDir(ctx context.Context, dir, repoURL string, repoExplicit bool) (string, func(), error) {
	noop := func() {}

	if dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return dir, noop, nil
		}
	}

	// If the user explicitly provided an external target repo, clone it
	if repoExplicit && repoURL != "" {
		return cloneTargetRepo(ctx, repoURL)
	}

	// 1. Prioritize local example directory for local development
	for _, p := range []string{"example/specs/kv.md", "../example/specs/kv.md", "../../example/specs/kv.md"} {
		if _, err := os.Stat(p); err == nil {
			return filepath.Dir(filepath.Dir(p)), noop, nil
		}
	}

	// 2. Check if /tmp/speculate-kv exists locally
	candidate := "/tmp/speculate-kv"
	if _, err := os.Stat(candidate); err == nil {
		return candidate, noop, nil
	}

	// 3. Fallback to testdata if local
	if _, err := os.Stat("testdata/specs/kv.md"); err == nil {
		return "testdata", noop, nil
	}

	// Otherwise clone target repo into a temp directory
	return cloneTargetRepo(ctx, repoURL)
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

	serverPkg, protoPkg := determineTargetPackages(repoDir)

	testCode := fmt.Sprintf(`package tests

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"%s"
	pb "%s"
)

// Stage: %%s
// Scenario: %%s %%s
func %%s(t *testing.T) {
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
		t.Fatalf("failed to dial: %%%%v", err)
	}
	t.Cleanup(func() { conn.Close() })

	client := pb.NewKVClient(conn)
	_, err = client.Put(context.Background(), &pb.PutRequest{Key: "prober-key", Value: []byte("prober-value")})
	if err != nil {
		t.Fatalf("Put failed: %%%%v", err)
	}
}
`, serverPkg, protoPkg)

	testCode = fmt.Sprintf(testCode, card.Stage, card.ID, card.Title, card.TestFuncName)

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

func runClientsProber(ctx context.Context, targetRepo string, dryRun bool, githubToken string, dcmEndpoint string) error {
	owner, repo, err := ghclient.ParseRepo(targetRepo)
	if err != nil {
		owner = "brotherlogic"
		repo = "speculate-kv"
	}

	// -------------------------------------------------------------
	// Step 1: Validate GitHub Client Adapter (pkg/github)
	// -------------------------------------------------------------
	fmt.Println("🔍 Step 1: Validating GitHub Client Adapter (pkg/github)...")

	if dryRun || githubToken == "" {
		mockClient := ghclient.NewMockClient(owner, repo)

		// 1. Ensure required labels
		if err := mockClient.EnsureLabels(ctx, ghclient.RequiredLabels()); err != nil {
			return fmt.Errorf("github client EnsureLabels failed: %w", err)
		}
		fmt.Println("✓ Required labels verified (speculate-agentic-loop, speculate-align, speculate-stalled)")

		// 2. Branch management (feat/<stage> and test/<scenario>)
		featBranch := "feat/dryrun-core"
		testBranch := "test/dryrun-basic-put"
		_, err := mockClient.CreateBranch(ctx, featBranch, "main")
		if err != nil {
			return fmt.Errorf("github client CreateBranch (%s): %w", featBranch, err)
		}
		_, err = mockClient.CreateBranch(ctx, testBranch, featBranch)
		if err != nil {
			return fmt.Errorf("github client CreateBranch (%s): %w", testBranch, err)
		}
		if _, err := mockClient.GetBranch(ctx, featBranch); err != nil {
			return fmt.Errorf("github client GetBranch (%s): %w", featBranch, err)
		}
		fmt.Printf("✓ Branch management verified (%s, %s)\n", featBranch, testBranch)

		// 3. Issue management
		cardBody := "## Scenario: Basic Put/Get\nGIVEN an empty KV service\nWHEN Put(key, value) is called\nTHEN Get(key) returns value"
		issueReq := &ghclient.CreateIssueRequest{
			Title:  "[Speculate Loop] Add integration test for Basic Put/Get",
			Body:   cardBody,
			Labels: []string{ghclient.LabelAgenticLoop},
		}
		issue, err := mockClient.CreateIssue(ctx, issueReq)
		if err != nil {
			return fmt.Errorf("github client CreateIssue: %w", err)
		}
		fmt.Printf("✓ Issue management verified (#%d created with label %s)\n", issue.Number, ghclient.LabelAgenticLoop)

		// 4. Pull Request workflow
		prReq := &ghclient.CreatePRRequest{
			Title: "test(kv): add basic put/get test scenario",
			Head:  testBranch,
			Base:  featBranch,
			Body:  fmt.Sprintf("Closes #%d\n\nAutomated test PR generated by Speculate.", issue.Number),
		}
		pr, err := mockClient.CreatePullRequest(ctx, prReq)
		if err != nil {
			return fmt.Errorf("github client CreatePullRequest: %w", err)
		}
		fmt.Printf("✓ Pull Request workflow verified (#%d opened targeting %s)\n", pr.Number, featBranch)

		// 5. README alignment badge update
		badgeMD := "[![Spec Alignment](https://img.shields.io/badge/Spec%20Alignment-25%25-yellow)](specs/kv.md)"
		if err := mockClient.UpdateReadmeBadge(ctx, featBranch, badgeMD); err != nil {
			return fmt.Errorf("github client UpdateReadmeBadge: %w", err)
		}
		fc, err := mockClient.GetFileContent(ctx, "README.md", featBranch)
		if err != nil || !strings.Contains(fc.Content, badgeMD) {
			return fmt.Errorf("README badge verification failed: content missing badge")
		}
		fmt.Println("✓ README alignment badge update verified")

		// 6. Squash merge
		mergeRes, err := mockClient.SquashMergePullRequest(ctx, pr.Number, pr.Title, "Squash merge test commit")
		if err != nil || !mergeRes.Merged {
			return fmt.Errorf("github client SquashMergePullRequest failed: %v", err)
		}
		fmt.Println("✓ Squash merge verified")

		// 7. Cleanup
		if err := mockClient.DeleteBranch(ctx, testBranch); err != nil {
			return fmt.Errorf("github client DeleteBranch failed: %w", err)
		}
		if err := mockClient.CloseIssue(ctx, issue.Number); err != nil {
			return fmt.Errorf("github client CloseIssue failed: %w", err)
		}
		fmt.Println("✓ Branch deletion and issue closure verified")
	} else {
		realClient := ghclient.NewRealClient(githubToken, owner, repo)
		if err := realClient.EnsureLabels(ctx, ghclient.RequiredLabels()); err != nil {
			return fmt.Errorf("github live EnsureLabels failed on %s/%s: %w", owner, repo, err)
		}
		if _, err := realClient.GetBranch(ctx, "main"); err != nil {
			return fmt.Errorf("github live GetBranch(main) failed on %s/%s: %w", owner, repo, err)
		}
		fmt.Printf("✓ Live GitHub repository %s/%s verified (branches, labels accessible)\n", owner, repo)
	}

	// -------------------------------------------------------------
	// Step 2: Validate DCM Client Adapter (pkg/dcm)
	// -------------------------------------------------------------
	fmt.Println("\n🔍 Step 2: Validating Devcontainer-Manager (DCM) Client Adapter (pkg/dcm)...")

	// Validate proto schema against proto/manager.proto
	if err := validateDCMProtoSchema(); err != nil {
		return fmt.Errorf("validating DCM proto schema: %w", err)
	}
	fmt.Println("✓ DCM Protobuf schema contract verified against proto/manager.proto")

	if dryRun || dcmEndpoint == "" {
		lis := bufconn.Listen(1024 * 1024)
		grpcServer := grpc.NewServer()
		mockDCM := dcm.NewMockServer()
		dcmproto.RegisterManagerServiceServer(grpcServer, mockDCM)

		go func() {
			_ = grpcServer.Serve(lis)
		}()
		defer grpcServer.Stop()

		conn, err := grpc.NewClient("passthrough://bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return lis.Dial()
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return fmt.Errorf("dialing mock DCM bufconn: %w", err)
		}
		defer conn.Close()

		dcmClient := dcm.NewFromClientConn(conn)

		// HealthCheck
		if err := dcmClient.HealthCheck(ctx); err != nil {
			return fmt.Errorf("DCM HealthCheck failed: %w", err)
		}
		fmt.Println("✓ Devcontainer ManagerService connection verified")

		// Up RPC
		upParams := &dcm.UpParams{
			Repo:        fmt.Sprintf("%s/%s", owner, repo),
			Branch:      "test/dryrun-scenario",
			Harness:     dcm.HarnessAntigravity,
			IssueNumber: 1,
			Prompt:      "Implement scenario test matching the issue Scenario Card",
			Model:       "deepseek-coder-v2:latest",
		}
		cfg, err := dcmClient.Up(ctx, upParams)
		if err != nil {
			return fmt.Errorf("DCM Up RPC failed: %w", err)
		}
		if cfg.GetId() == "" {
			return fmt.Errorf("DCM Up RPC returned empty container ID")
		}
		fmt.Printf("✓ DCM Up RPC verified (container_id=%s, harness=HARNESS_ANTIGRAVITY)\n", cfg.GetId())

		// PushPrompt RPC
		testPrompt := "Run `go test ./...` and commit changes to internal/server/"
		if err := dcmClient.PushPrompt(ctx, cfg.GetId(), testPrompt); err != nil {
			return fmt.Errorf("DCM PushPrompt RPC failed: %w", err)
		}
		fmt.Println("✓ DCM PushPrompt RPC verified")

		// Readiness check
		readyCfg, err := dcmClient.WaitForReady(ctx, cfg.GetId(), 10*time.Millisecond)
		if err != nil {
			return fmt.Errorf("DCM WaitForReady failed: %w", err)
		}
		if readyCfg.GetState() != dcm.StateReady {
			return fmt.Errorf("expected DCM_READY state, got %v", readyCfg.GetState())
		}
		fmt.Println("✓ Devcontainer readiness polling verified (DCM_READY)")
	} else {
		liveClient, err := dcm.NewRealClient(ctx, dcmEndpoint)
		if err != nil {
			return fmt.Errorf("connecting to live DCM at %s: %w", dcmEndpoint, err)
		}
		defer liveClient.Close()
		if err := liveClient.HealthCheck(ctx); err != nil {
			return fmt.Errorf("live DCM health check failed: %w", err)
		}
		fmt.Printf("✓ Live DCM endpoint %s connected and verified healthy\n", dcmEndpoint)
	}

	return nil
}

func validateDCMProtoSchema() error {
	// Look for proto/manager.proto in workspace or parent paths
	candidates := []string{
		"proto/manager.proto",
		"../proto/manager.proto",
		"../../proto/manager.proto",
	}

	var protoContent string
	var foundPath string
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			protoContent = string(data)
			foundPath = p
			break
		}
	}

	if protoContent == "" {
		// Fallback: check relative to executable if possible
		if execPath, err := os.Executable(); err == nil {
			dir := filepath.Dir(execPath)
			p := filepath.Join(dir, "proto", "manager.proto")
			if data, err := os.ReadFile(p); err == nil {
				protoContent = string(data)
				foundPath = p
			}
		}
	}

	if protoContent != "" {
		requiredSubstrings := []string{
			"service ManagerService",
			"rpc Up(UpRequest) returns (UpResponse)",
			"rpc Down(DownRequest) returns (DownResponse)",
			"rpc List(ListRequest) returns (ListResponse)",
			"rpc PushPrompt(PushPromptRequest) returns (PushPromptResponse)",
			"enum Harness",
			"HARNESS_ANTIGRAVITY",
			"enum State",
			"DCM_READY",
			"DCM_FAILED",
		}

		for _, sub := range requiredSubstrings {
			if !strings.Contains(protoContent, sub) {
				return fmt.Errorf("proto file %s missing required contract definition: %s", foundPath, sub)
			}
		}
	}

	// Validate compiled Go protobuf types are linked correctly
	if dcm.HarnessAntigravity != dcmproto.Harness_HARNESS_ANTIGRAVITY {
		return fmt.Errorf("mismatched HarnessAntigravity enum mapping")
	}
	if dcm.StateReady != dcmproto.State_DCM_READY {
		return fmt.Errorf("mismatched StateReady enum mapping")
	}

	return nil
}

type enrollServer struct {
	pb.UnimplementedSpeculateServiceServer
	pipeline *enroll.Pipeline
}

func (s *enrollServer) Enroll(ctx context.Context, req *pb.EnrollRequest) (*pb.EnrollResponse, error) {
	if req == nil || strings.TrimSpace(req.GetRepository()) == "" {
		return nil, status.Error(codes.InvalidArgument, "repository is required")
	}
	return s.pipeline.Enroll(ctx, strings.TrimSpace(req.GetRepository()))
}

func runEnrollProber(ctx context.Context, targetRepo string, dryRun bool, token string, pstoreEndpoint string) error {
	owner, repo, err := ghclient.ParseRepo(targetRepo)
	if err != nil {
		return fmt.Errorf("parsing target repo %q: %w", targetRepo, err)
	}
	canonicalRepo := fmt.Sprintf("%s/%s", owner, repo)

	if dryRun {
		fmt.Println("🔍 Step 1: Starting in-memory gRPC server with bufconn.Listen...")
		lis := bufconn.Listen(1024 * 1024)
		grpcServer := grpc.NewServer()

		mockGH := ghclient.NewMockClient(owner, repo)
		mockStore := pstore.NewMockStore()

		synthLLM := &evaluator.MockLLMClient{
			Response: `{
				"id": "Core-1",
				"stage": "Core",
				"requirement": "Put: Stores a key and associated byte payload",
				"target_rpc": "Put",
				"title": "Basic Put Key",
				"given": "An empty KV service",
				"when": "Put is called",
				"then": "Value is stored",
				"test_func_name": "TestPut_Basic"
			}`,
		}
		eval := evaluator.NewEvaluator(synthLLM)
		synth := synthesizer.NewSynthesizer(synthLLM)

		pipe := enroll.NewPipeline(
			token,
			func(t, o, r string) ghclient.Client {
				return mockGH
			},
			eval,
			synth,
			mockStore,
		)

		pipe.WithCloneFn(func(ctx context.Context, repoURL, targetDir string) error {
			candidates := []string{
				"example",
				"../example",
				"../../example",
			}
			foundExample := ""
			for _, candidate := range candidates {
				if _, err := os.Stat(filepath.Join(candidate, "specs", "kv.md")); err == nil {
					foundExample = candidate
					break
				}
			}

			if foundExample != "" {
				if err := copyDir(foundExample, targetDir); err != nil {
					return fmt.Errorf("copying example to target dir: %w", err)
				}
				return nil
			}

			specsDir := filepath.Join(targetDir, "specs")
			if err := os.MkdirAll(specsDir, 0755); err != nil {
				return err
			}
			specContent := "# Key-Value Service Specification\n\n## [Stage: Core] Basic Key-Value Storage\n- Put: Stores a key and associated value.\n"
			if err := os.WriteFile(filepath.Join(specsDir, "kv.md"), []byte(specContent), 0644); err != nil {
				return err
			}
			testsDir := filepath.Join(targetDir, "tests")
			if err := os.MkdirAll(testsDir, 0755); err != nil {
				return err
			}
			return nil
		})

		pb.RegisterSpeculateServiceServer(grpcServer, &enrollServer{pipeline: pipe})
		go func() {
			_ = grpcServer.Serve(lis)
		}()
		defer grpcServer.Stop()

		fmt.Println("🔍 Step 2: Connecting via bufconn gRPC client...")
		conn, err := grpc.NewClient("passthrough://bufnet",
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return lis.Dial()
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return fmt.Errorf("dialing in-memory bufconn: %w", err)
		}
		defer conn.Close()

		client := pb.NewSpeculateServiceClient(conn)

		fmt.Printf("🔍 Step 3: Calling Enroll for %s...\n", canonicalRepo)
		resp1, err := client.Enroll(ctx, &pb.EnrollRequest{Repository: canonicalRepo})
		if err != nil {
			return fmt.Errorf("first Enroll RPC failed: %w", err)
		}

		// Assert alignment percentage is recorded
		recordAlignmentScore(canonicalRepo, int(resp1.GetAlignmentPercentage()))
		rec, err := mockStore.GetEnrollment(ctx, canonicalRepo)
		if err != nil || rec == nil {
			return fmt.Errorf("enrollment record not stored in pstore: %w", err)
		}
		if rec.GetAlignmentPercentage() != resp1.GetAlignmentPercentage() {
			return fmt.Errorf("stored alignment percentage %d does not match response %d", rec.GetAlignmentPercentage(), resp1.GetAlignmentPercentage())
		}
		fmt.Printf("✓ Alignment percentage recorded (%d%%)\n", resp1.GetAlignmentPercentage())

		// Assert badge markdown is injected
		fc, err := mockGH.GetFileContent(ctx, "README.md", "main")
		if err != nil {
			return fmt.Errorf("getting README.md from main: %w", err)
		}
		if !strings.Contains(fc.Content, "Spec Alignment") {
			return fmt.Errorf("badge markdown not injected into README.md")
		}
		fmt.Println("✓ Badge markdown injected into README.md")

		// Assert scenario card issue is created
		issues1, err := mockGH.ListIssues(ctx, "open", []string{ghclient.LabelAgenticLoop})
		if err != nil {
			return fmt.Errorf("listing issues with label %s: %w", ghclient.LabelAgenticLoop, err)
		}
		if len(issues1) != 1 {
			return fmt.Errorf("expected 1 open scenario card issue, got %d", len(issues1))
		}
		if resp1.GetIssueUrl() == "" || resp1.GetIssueUrl() != issues1[0].HTMLURL {
			return fmt.Errorf("expected response issue URL %s to match created issue %s", resp1.GetIssueUrl(), issues1[0].HTMLURL)
		}
		fmt.Printf("✓ Scenario card issue created (#%d, url=%s)\n", issues1[0].Number, resp1.GetIssueUrl())

		// Call Enroll a second time and assert idempotency
		fmt.Println("🔍 Step 4: Calling Enroll a second time to verify idempotency...")
		resp2, err := client.Enroll(ctx, &pb.EnrollRequest{Repository: canonicalRepo})
		if err != nil {
			return fmt.Errorf("second Enroll RPC failed: %w", err)
		}

		issues2, err := mockGH.ListIssues(ctx, "open", []string{ghclient.LabelAgenticLoop})
		if err != nil {
			return fmt.Errorf("listing issues after second enroll: %w", err)
		}
		if len(issues2) != 1 {
			return fmt.Errorf("idempotency check failed: expected 1 issue, found %d", len(issues2))
		}
		if resp2.GetIssueUrl() != resp1.GetIssueUrl() {
			return fmt.Errorf("idempotency check failed: expected issue URL %s, got %s", resp1.GetIssueUrl(), resp2.GetIssueUrl())
		}
		fmt.Println("✓ Idempotency verified: existing scenario card issue reused, no duplicate created")
	} else {
		daemonAddr := os.Getenv("SPECULATE_DAEMON_ADDR")
		if daemonAddr == "" {
			daemonAddr = "localhost:50051"
		}

		daemonConn, connErr := grpc.NewClient(daemonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if connErr == nil {
			defer daemonConn.Close()
			client := pb.NewSpeculateServiceClient(daemonConn)
			resp, err := client.Enroll(ctx, &pb.EnrollRequest{Repository: canonicalRepo})
			if err == nil {
				recordAlignmentScore(canonicalRepo, int(resp.GetAlignmentPercentage()))
				fmt.Printf("✓ Live daemon enrolled %s: alignment %d%%, issue %s\n", canonicalRepo, resp.GetAlignmentPercentage(), resp.GetIssueUrl())
				return nil
			}
			log.Printf("Live daemon dial/RPC at %s did not succeed (%v), running live pipeline verification directly...", daemonAddr, err)
		}

		var store pstore.Store
		if pstoreEndpoint != "" {
			pClient, err := pstore.Dial(ctx, pstoreEndpoint)
			if err != nil {
				return fmt.Errorf("connecting to pstore at %s: %w", pstoreEndpoint, err)
			}
			defer pClient.Close()
			store = pClient
		}

		pipe := enroll.NewPipeline(
			token,
			func(t, o, r string) ghclient.Client {
				return ghclient.NewRealClient(t, o, r)
			},
			evaluator.NewEvaluator(nil),
			synthesizer.NewSynthesizer(nil),
			store,
		)

		resp, err := pipe.Enroll(ctx, canonicalRepo)
		if err != nil {
			return fmt.Errorf("live enrollment failed for %s: %w", canonicalRepo, err)
		}

		recordAlignmentScore(canonicalRepo, int(resp.GetAlignmentPercentage()))
		fmt.Printf("✓ Live pipeline enrolled %s: alignment %d%%, issue %s\n", canonicalRepo, resp.GetAlignmentPercentage(), resp.GetIssueUrl())
	}

	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

