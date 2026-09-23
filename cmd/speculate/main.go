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

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type Config struct {
	HTTPPort       int
	GRPCPort       int
	OllamaEndpoint string
	TargetRepo     string
}

func parseConfig() *Config {
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

	flag.IntVar(&httpPort, "http-port", httpPort, "HTTP server port")
	flag.IntVar(&grpcPort, "grpc-port", grpcPort, "gRPC server port")
	flag.StringVar(&ollamaEndpoint, "ollama-endpoint", ollamaEndpoint, "Ollama LLM API endpoint")
	flag.StringVar(&targetRepo, "target-repo", targetRepo, "Target repository (owner/repo)")
	flag.Parse()

	return &Config{
		HTTPPort:       httpPort,
		GRPCPort:       grpcPort,
		OllamaEndpoint: ollamaEndpoint,
		TargetRepo:     targetRepo,
	}
}

func main() {
	cfg := parseConfig()

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
