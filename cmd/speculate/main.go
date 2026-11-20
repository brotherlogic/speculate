package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	ghwebhookpb "github.com/brotherlogic/ghwebhook/proto/ghwebhook/v1"
	"github.com/brotherlogic/speculate/pkg/dcm"
	"github.com/brotherlogic/speculate/pkg/enroll"
	"github.com/brotherlogic/speculate/pkg/evaluator"
	ghclient "github.com/brotherlogic/speculate/pkg/github"
	"github.com/brotherlogic/speculate/pkg/pstore"
	"github.com/brotherlogic/speculate/pkg/setup"
	"github.com/brotherlogic/speculate/pkg/synthesizer"
	pb "github.com/brotherlogic/speculate/proto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type DaemonConfig struct {
	HTTPPort       int
	GRPCPort       int
	OllamaEndpoint string
	TargetRepo     string
	SyncInterval   time.Duration
	DCMAddr        string
	DCMHarness     string
	DCMModel       string
	GHWebhookAddr  string
	SelfGRPCAddr   string
}

func parseDaemonConfig(args []string) *DaemonConfig {
	httpPort := 8080
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			httpPort = p
		}
	} else if envPort := os.Getenv("HTTP_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			httpPort = p
		}
	}

	grpcPort := 50051
	if envPort := os.Getenv("GRPC_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			grpcPort = p
		}
	}

	ollamaEndpoint := os.Getenv("OLLAMA_ENDPOINT")
	if ollamaEndpoint == "" {
		ollamaEndpoint = "http://192.168.68.112:11434/v1"
	}

	targetRepo := os.Getenv("TARGET_REPO")
	if targetRepo == "" {
		targetRepo = "brotherlogic/speculate-kv"
	}

	syncInterval := enroll.DefaultSyncInterval
	if envSync := os.Getenv("SYNC_INTERVAL"); envSync != "" {
		if d, err := time.ParseDuration(envSync); err == nil && d > 0 {
			syncInterval = d
		}
	}

	dcmAddr := os.Getenv("DCM_ADDR")

	dcmHarness := os.Getenv("DCM_HARNESS")
	if strings.TrimSpace(dcmHarness) == "" {
		dcmHarness = "antigravity"
	}

	dcmModel := os.Getenv("DCM_MODEL")

	ghWebhookAddr := os.Getenv("GHWEBHOOK_ADDR")

	selfGRPCAddr := os.Getenv("SELF_GRPC_ADDR")
	if strings.TrimSpace(selfGRPCAddr) == "" {
		selfGRPCAddr = "speculate.speculate.svc:50051"
	}

	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.IntVar(&httpPort, "http-port", httpPort, "HTTP server port")
	fs.IntVar(&grpcPort, "grpc-port", grpcPort, "gRPC server port")
	fs.StringVar(&ollamaEndpoint, "ollama-endpoint", ollamaEndpoint, "Ollama LLM API endpoint")
	fs.StringVar(&targetRepo, "target-repo", targetRepo, "Target repository (owner/repo)")
	fs.DurationVar(&syncInterval, "sync-interval", syncInterval, "Interval between collaborator discovery sync cycles")
	fs.StringVar(&dcmAddr, "dcm-addr", dcmAddr, "devcontainer-manager (DCM) gRPC address")
	fs.StringVar(&dcmHarness, "dcm-harness", dcmHarness, "DCM agent harness (antigravity or pi)")
	fs.StringVar(&dcmModel, "dcm-model", dcmModel, "Optional DCM LLM model override")
	fs.StringVar(&ghWebhookAddr, "ghwebhook-addr", ghWebhookAddr, "ghwebhook gRPC address for automatic repository registration")
	fs.StringVar(&selfGRPCAddr, "self-grpc-addr", selfGRPCAddr, "Speculate gRPC service address advertised to ghwebhook")
	_ = fs.Parse(args)
	if syncInterval <= 0 {
		syncInterval = enroll.DefaultSyncInterval
	}
	if strings.TrimSpace(dcmHarness) == "" {
		dcmHarness = "antigravity"
	}
	if strings.TrimSpace(selfGRPCAddr) == "" {
		selfGRPCAddr = "speculate.speculate.svc:50051"
	}

	return &DaemonConfig{
		HTTPPort:       httpPort,
		GRPCPort:       grpcPort,
		OllamaEndpoint: ollamaEndpoint,
		TargetRepo:     targetRepo,
		SyncInterval:   syncInterval,
		DCMAddr:        dcmAddr,
		DCMHarness:     dcmHarness,
		DCMModel:       dcmModel,
		GHWebhookAddr:  ghWebhookAddr,
		SelfGRPCAddr:   selfGRPCAddr,
	}
}

func initHTTPMux(cfg *DaemonConfig, lister EnrollmentLister) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", NewDashboardHandler(lister))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var targetRepo, ollamaEndpoint string
		if cfg != nil {
			targetRepo = cfg.TargetRepo
			ollamaEndpoint = cfg.OllamaEndpoint
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "running",
			"target_repo":     targetRepo,
			"ollama_endpoint": ollamaEndpoint,
			"timestamp":       time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

func runDaemon(args []string) {
	cfg := parseDaemonConfig(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Starting speculate daemon for target: %s", cfg.TargetRepo)
	log.Printf("Ollama endpoint: %s", cfg.OllamaEndpoint)

	// 1. Initialize gRPC server
	grpcLis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		log.Fatalf("Failed to listen on gRPC port %d: %v", cfg.GRPCPort, err)
	}

	grpcServer := grpc.NewServer()
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("speculate", grpc_health_v1.HealthCheckResponse_SERVING)

	// Initialize pstore
	var store pstore.Store
	if pstoreAddr := os.Getenv("PSTORE_ADDR"); pstoreAddr != "" {
		pstoreClient, err := pstore.Dial(ctx, pstoreAddr)
		if err != nil {
			log.Printf("Warning: failed to connect to pstore at %s: %v; falling back to in-memory store", pstoreAddr, err)
			store = pstore.NewMockStore()
		} else {
			defer pstoreClient.Close()
			store = pstore.NewFallbackStore(pstoreClient, pstore.NewMockStore())
		}
	} else {
		store = pstore.NewMockStore()
	}

	ghToken := os.Getenv("GH_TOKEN")
	if ghToken == "" {
		ghToken = os.Getenv("GITHUB_TOKEN")
	}

	evalClient := evaluator.NewOllamaClient(cfg.OllamaEndpoint, "")
	eval := evaluator.NewEvaluator(evalClient)
	synth := synthesizer.NewSynthesizer(evalClient)

	harness, err := dcm.ParseHarness(cfg.DCMHarness)
	if err != nil {
		log.Printf("Warning: invalid DCM harness %q (%v); defaulting to antigravity", cfg.DCMHarness, err)
		harness = dcm.HarnessAntigravity
	}

	pipeline := enroll.NewPipeline(ghToken, nil, eval, synth, store).
		WithHarness(harness).
		WithModel(cfg.DCMModel)

	if strings.TrimSpace(cfg.DCMAddr) != "" {
		dcmClient, err := dcm.NewRealClient(ctx, cfg.DCMAddr)
		if err != nil {
			log.Printf("Warning: failed to connect to DCM at %s: %v; continuing in card-only mode", cfg.DCMAddr, err)
		} else {
			defer dcmClient.Close()
			pipeline.WithDCMClient(dcmClient)
			log.Printf("DCM client configured at %s (harness=%s)", cfg.DCMAddr, cfg.DCMHarness)
		}
	}

	speculateServer := NewSpeculateServer(pipeline)
	pb.RegisterSpeculateServiceServer(grpcServer, speculateServer)
	ghwebhookpb.RegisterWebhookHandlerServer(grpcServer, NewWebhookServer(pipeline))

	if ghToken != "" {
		accountClient := ghclient.NewRealAccountClient(ghToken)
		syncer := enroll.NewSyncer(accountClient, store, pipeline)
		if strings.TrimSpace(cfg.GHWebhookAddr) != "" {
			ghWebhookConn, err := grpc.NewClient(cfg.GHWebhookAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				log.Printf("Warning: failed to dial ghwebhook at %s: %v; automatic webhook registration is disabled", cfg.GHWebhookAddr, err)
			} else {
				defer ghWebhookConn.Close()
				regClient := ghwebhookpb.NewRegistrationServiceClient(ghWebhookConn)
				syncer.WithWebhookRegistrar(enroll.NewGRPCWebhookRegistrar(regClient), cfg.SelfGRPCAddr)
				log.Printf("ghwebhook registration enabled (addr=%s, self=%s)", cfg.GHWebhookAddr, cfg.SelfGRPCAddr)
			}
		}
		go syncer.Run(ctx, cfg.SyncInterval)
	} else {
		log.Printf("Warning: GH_TOKEN / GITHUB_TOKEN not set; automatic collaborator discovery is disabled")
	}

	go func() {
		log.Printf("Starting gRPC server on port %d...", cfg.GRPCPort)
		if err := grpcServer.Serve(grpcLis); err != nil && err != grpc.ErrServerStopped {
			log.Fatalf("gRPC server error: %v", err)
		}
	}()

	// 2. Initialize HTTP server (dashboard, metrics, healthz, status)
	mux := initHTTPMux(cfg, pipeline)

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler: mux,
	}

	go func() {
		log.Printf("Starting HTTP server on port %d...", cfg.HTTPPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	log.Println("Shutting down speculate daemon...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	grpcServer.GracefulStop()
	_ = httpServer.Shutdown(shutdownCtx)
	log.Println("Speculate daemon stopped.")
}

func parseInitConfig(args []string) (*setup.Config, error) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := fs.String("repo", "", "Target GitHub repository (owner/name); inferred from git remote if empty")
	token := fs.String("token", "", "GitHub personal access token (defaults to GH_TOKEN / GITHUB_TOKEN)")
	collaborator := fs.String("collaborator", setup.DefaultCollaborator, "Automation collaborator to add")
	dir := fs.String("dir", ".", "Target project directory")
	force := fs.Bool("force", false, "Overwrite existing files without prompting")
	skipPush := fs.Bool("skip-push", false, "Skip git commit and push")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	return &setup.Config{
		RootDir:      *dir,
		Repo:         *repo,
		Token:        *token,
		Collaborator: *collaborator,
		Force:        *force,
		SkipPush:     *skipPush,
	}, nil
}

func runInit(args []string) {
	cfg, err := parseInitConfig(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatalf("failed parsing flags: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := setup.Run(ctx, cfg); err != nil {
		log.Fatalf("❌ speculate init failed: %v", err)
	}
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			runInit(os.Args[2:])
			return
		case "daemon":
			runDaemon(os.Args[2:])
			return
		case "enroll":
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := runEnroll(ctx, os.Args[2:], os.Stdout, os.Stderr); err != nil {
				os.Exit(1)
			}
			return
		case "help", "-h", "--help":
			fmt.Println("Usage: speculate <command> [options]")
			fmt.Println("Commands:")
			fmt.Println("  init    Initialize a target repository with speculate structure, workflows, and rulesets")
			fmt.Println("  daemon  Run the speculate orchestrator daemon (default)")
			fmt.Println("  enroll  Enroll a target repository and dispatch the first scenario card")
			return
		}
	}

	runDaemon(os.Args[1:])
}
