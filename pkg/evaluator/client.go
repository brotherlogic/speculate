package evaluator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultLocalEndpoint points to the local Ollama LLM endpoint on the network.
	DefaultLocalEndpoint = "http://192.168.68.112:11434/v1"
	// DefaultLocalModel is the default coder model deployed on the llama machine.
	DefaultLocalModel = "deepseek-coder-v2:latest"
	// DefaultMaxAttempts is the default number of attempts for Ollama requests.
	DefaultMaxAttempts = 3
	// DefaultBaseBackoff is the default base backoff duration between retry attempts.
	DefaultBaseBackoff = 500 * time.Millisecond
)

// LLMClient provides text completion capabilities.
type LLMClient interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// OllamaClient interfaces with the OpenAI-compatible API exposed by Ollama.
type OllamaClient struct {
	endpoint    string
	model       string
	httpClient  *http.Client
	maxAttempts int
	baseBackoff time.Duration
}

// OllamaOption allows customizing OllamaClient behavior.
type OllamaOption func(*OllamaClient)

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(client *http.Client) OllamaOption {
	return func(c *OllamaClient) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithMaxAttempts sets the maximum number of attempts (including retries).
func WithMaxAttempts(attempts int) OllamaOption {
	return func(c *OllamaClient) {
		if attempts > 0 {
			c.maxAttempts = attempts
		}
	}
}

// WithBaseBackoff sets the base duration for backoff between retries.
func WithBaseBackoff(backoff time.Duration) OllamaOption {
	return func(c *OllamaClient) {
		c.baseBackoff = backoff
	}
}

// NewOllamaClient creates a new OllamaClient with endpoint and model overrides or environment defaults.
func NewOllamaClient(endpoint, model string, opts ...OllamaOption) *OllamaClient {
	if endpoint == "" {
		endpoint = os.Getenv("OLLAMA_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = DefaultLocalEndpoint
	}
	endpoint = strings.TrimSuffix(endpoint, "/")

	if model == "" {
		model = os.Getenv("OLLAMA_MODEL")
	}
	if model == "" {
		model = DefaultLocalModel
	}

	client := &OllamaClient{
		endpoint:    endpoint,
		model:       model,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		maxAttempts: DefaultMaxAttempts,
		baseBackoff: DefaultBaseBackoff,
	}

	for _, opt := range opts {
		opt(client)
	}

	return client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *OllamaClient) backoffDuration(attempt int) time.Duration {
	if c.baseBackoff <= 0 {
		return 0
	}
	// Exponential backoff: base * 2^attempt
	multiplier := 1 << attempt
	backoff := c.baseBackoff * time.Duration(multiplier)
	if backoff <= 0 {
		return 0
	}
	// Add jitter up to 25% of backoff
	jitterLimit := int64(backoff / 4)
	if jitterLimit > 0 {
		backoff += time.Duration(rand.Int64N(jitterLimit))
	}
	return backoff
}

func isRetryableStatusCode(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout ||
		code == http.StatusTooManyRequests
}

func isRetryableError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		switch syscallErr {
		case syscall.ECONNREFUSED, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ETIMEDOUT, syscall.ECONNRESET:
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof")
}

// Complete sends a prompt to the local LLM and returns the text response.
func (c *OllamaClient) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	messages := []chatMessage{}
	if systemPrompt != "" {
		messages = append(messages, chatMessage{
			Role:    "system",
			Content: systemPrompt,
		})
	}
	messages = append(messages, chatMessage{
		Role:    "user",
		Content: userPrompt,
	})

	reqBody := chatCompletionRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: 0.0,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling LLM request: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", c.endpoint)
	maxAttempts := c.maxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}

	var attemptDetails []string
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if len(attemptDetails) > 0 {
				return "", fmt.Errorf("request context canceled after %d attempts (%s): %w", attempt, strings.Join(attemptDetails, "; "), err)
			}
			return "", err
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return "", fmt.Errorf("creating http request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("sending request to LLM at %s: %w", url, err)
			attemptDetails = append(attemptDetails, fmt.Sprintf("attempt %d: %v", attempt+1, lastErr))

			if !isRetryableError(ctx, err) || attempt == maxAttempts-1 {
				break
			}

			delay := c.backoffDuration(attempt)
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("request context canceled after %d attempts (%s): %w", attempt+1, strings.Join(attemptDetails, "; "), ctx.Err())
			case <-time.After(delay):
			}
			continue
		}

		respBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if readErr != nil {
			lastErr = fmt.Errorf("reading LLM response: %w", readErr)
			attemptDetails = append(attemptDetails, fmt.Sprintf("attempt %d: %v", attempt+1, lastErr))

			if !isRetryableError(ctx, readErr) || attempt == maxAttempts-1 {
				break
			}

			delay := c.backoffDuration(attempt)
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("request context canceled after %d attempts (%s): %w", attempt+1, strings.Join(attemptDetails, "; "), ctx.Err())
			case <-time.After(delay):
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("LLM returned status %d: %s", resp.StatusCode, string(respBytes))
			attemptDetails = append(attemptDetails, fmt.Sprintf("attempt %d: %v", attempt+1, lastErr))

			if isRetryableStatusCode(resp.StatusCode) && attempt < maxAttempts-1 {
				delay := c.backoffDuration(attempt)
				select {
				case <-ctx.Done():
					return "", fmt.Errorf("request context canceled after %d attempts (%s): %w", attempt+1, strings.Join(attemptDetails, "; "), ctx.Err())
				case <-time.After(delay):
				}
				continue
			}
			break
		}

		var chatResp chatCompletionResponse
		if err := json.Unmarshal(respBytes, &chatResp); err != nil {
			return "", fmt.Errorf("parsing LLM response JSON: %w", err)
		}

		if chatResp.Error != nil && chatResp.Error.Message != "" {
			return "", fmt.Errorf("LLM error: %s", chatResp.Error.Message)
		}

		if len(chatResp.Choices) == 0 {
			return "", fmt.Errorf("no completion returned by LLM")
		}

		return strings.TrimSpace(chatResp.Choices[0].Message.Content), nil
	}

	if len(attemptDetails) > 1 {
		return "", fmt.Errorf("LLM request to %s failed after %d attempts (%s): %w", url, len(attemptDetails), strings.Join(attemptDetails, "; "), lastErr)
	}
	return "", lastErr
}

// MockLLMClient is used for hermetic unit testing.
type MockLLMClient struct {
	Response string
	Err      error
}

func (m *MockLLMClient) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	return m.Response, nil
}
