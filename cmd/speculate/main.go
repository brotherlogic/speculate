package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/brotherlogic/speculate/pkg/setup"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type DaemonConfig struct {
	HTTPPort       int
	GRPCPort       int
	OllamaEndpoint string
	TargetRepo     string
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

	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	fs.IntVar(&httpPort, "http-port", httpPort, "HTTP server port")
	fs.IntVar(&grpcPort, "grpc-port", grpcPort, "gRPC server port")
	fs.StringVar(&ollamaEndpoint, "ollama-endpoint", ollamaEndpoint, "Ollama LLM API endpoint")
	fs.StringVar(&targetRepo, "target-repo", targetRepo, "Target repository (owner/repo)")
	_ = fs.Parse(args)

	return &DaemonConfig{
		HTTPPort:       httpPort,
		GRPCPort:       grpcPort,
		OllamaEndpoint: ollamaEndpoint,
		TargetRepo:     targetRepo,
	}
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

	go func() {
		log.Printf("Starting gRPC server on port %d...", cfg.GRPCPort)
		if err := grpcServer.Serve(grpcLis); err != nil && err != grpc.ErrServerStopped {
			log.Fatalf("gRPC server error: %v", err)
		}
	}()

	// 2. Initialize HTTP server (metrics, healthz, status)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK\n"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "running",
			"target_repo":     cfg.TargetRepo,
			"ollama_endpoint": cfg.OllamaEndpoint,
			"timestamp":       time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.Handle("/metrics", promhttp.Handler())

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

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	repo := fs.String("repo", "", "Target GitHub repository (owner/name); inferred from git remote if empty")
	token := fs.String("token", "", "GitHub personal access token (defaults to GH_TOKEN / GITHUB_TOKEN)")
	collaborator := fs.String("collaborator", "brotherlogic-automation", "Automation collaborator to add")
	dir := fs.String("dir", ".", "Target project directory")
	force := fs.Bool("force", false, "Overwrite existing files without prompting")
	skipPush := fs.Bool("skip-push", false, "Skip git commit and push")

	if err := fs.Parse(args); err != nil {
		log.Fatalf("failed parsing flags: %v", err)
	}

	cfg := &setup.Config{
		RootDir:      *dir,
		Repo:         *repo,
		Token:        *token,
		Collaborator: *collaborator,
		Force:        *force,
		SkipPush:     *skipPush,
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
		case "help", "-h", "--help":
			fmt.Println("Usage: speculate <command> [options]")
			fmt.Println("Commands:")
			fmt.Println("  init    Initialize a target repository with speculate structure, workflows, and rulesets")
			fmt.Println("  daemon  Run the speculate orchestrator daemon (default)")
			return
		}
	}

	runDaemon(os.Args[1:])
}
