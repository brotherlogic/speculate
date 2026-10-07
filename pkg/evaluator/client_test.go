package evaluator

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOllamaClient_Complete_SuccessWithoutRetry(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"test response"}}]}`))
	}))
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model",
		WithBaseBackoff(1*time.Millisecond),
		WithMaxAttempts(3),
	)

	resp, err := client.Complete(context.Background(), "system prompt", "user prompt")
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if resp != "test response" {
		t.Errorf("expected 'test response', got %q", resp)
	}
	if count := atomic.LoadInt32(&requestCount); count != 1 {
		t.Errorf("expected exactly 1 request, got %d", count)
	}
}

func TestOllamaClient_Complete_RetryOnTransientStatus_Success(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		if count == 1 {
			w.WriteHeader(http.StatusBadGateway) // 502
			_, _ = w.Write([]byte("bad gateway"))
			return
		}
		if count == 2 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503
			_, _ = w.Write([]byte("service unavailable"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"success after retry"}}]}`))
	}))
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model",
		WithBaseBackoff(1*time.Millisecond),
		WithMaxAttempts(3),
	)

	resp, err := client.Complete(context.Background(), "", "hello")
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if resp != "success after retry" {
		t.Errorf("expected 'success after retry', got %q", resp)
	}
	if count := atomic.LoadInt32(&requestCount); count != 3 {
		t.Errorf("expected 3 requests before success, got %d", count)
	}
}

func TestOllamaClient_Complete_RetryOnTransientNetworkError_Success(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"network recovered"}}]}`))
	}))
	defer server.Close()

	customTransport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		count := atomic.AddInt32(&requestCount, 1)
		if count == 1 {
			// Simulate transient "no route to host"
			return nil, &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: syscall.EHOSTUNREACH,
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})

	httpClient := &http.Client{
		Transport: customTransport,
		Timeout:   5 * time.Second,
	}

	client := NewOllamaClient(server.URL, "test-model",
		WithHTTPClient(httpClient),
		WithBaseBackoff(1*time.Millisecond),
		WithMaxAttempts(3),
	)

	resp, err := client.Complete(context.Background(), "", "hello")
	if err != nil {
		t.Fatalf("expected success after network retry, got: %v", err)
	}
	if resp != "network recovered" {
		t.Errorf("expected 'network recovered', got %q", resp)
	}
	if count := atomic.LoadInt32(&requestCount); count != 2 {
		t.Errorf("expected 2 requests, got %d", count)
	}
}

func TestOllamaClient_Complete_PersistentFailure_TerminatesAtLimit(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.WriteHeader(http.StatusGatewayTimeout) // 504
		_, _ = w.Write([]byte("gateway timeout"))
	}))
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model",
		WithBaseBackoff(1*time.Millisecond),
		WithMaxAttempts(3),
	)

	_, err := client.Complete(context.Background(), "", "hello")
	if err == nil {
		t.Fatalf("expected error on persistent failure, got nil")
	}

	if count := atomic.LoadInt32(&requestCount); count != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", count)
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "3 attempts") {
		t.Errorf("expected error message to mention 3 attempts, got: %v", errMsg)
	}
	if !strings.Contains(errMsg, "attempt 1") || !strings.Contains(errMsg, "attempt 2") || !strings.Contains(errMsg, "attempt 3") {
		t.Errorf("expected error message to contain attempt details, got: %v", errMsg)
	}
}

func TestOllamaClient_Complete_NonRetryableStatus_NoRetry(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.WriteHeader(http.StatusBadRequest) // 400
		_, _ = w.Write([]byte("bad request"))
	}))
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model",
		WithBaseBackoff(1*time.Millisecond),
		WithMaxAttempts(3),
	)

	_, err := client.Complete(context.Background(), "", "hello")
	if err == nil {
		t.Fatalf("expected error on 400 Bad Request, got nil")
	}

	if count := atomic.LoadInt32(&requestCount); count != 1 {
		t.Errorf("expected exactly 1 attempt for non-retryable error, got %d", count)
	}
}

func TestOllamaClient_Complete_ContextCanceled_AbortsRetry(t *testing.T) {
	var requestCount int32
	ctx, cancel := context.WithCancel(context.Background())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		// Cancel context on first failure
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable"))
	}))
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model",
		WithBaseBackoff(50*time.Millisecond),
		WithMaxAttempts(3),
	)

	_, err := client.Complete(ctx, "", "hello")
	if err == nil {
		t.Fatalf("expected context canceled error, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
		t.Errorf("expected error to reflect cancellation, got: %v", err)
	}
	if count := atomic.LoadInt32(&requestCount); count != 1 {
		t.Errorf("expected exactly 1 attempt before context cancellation stopped it, got %d", count)
	}
}
