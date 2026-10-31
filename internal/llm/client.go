package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rbalukja15/k8s-doctor/internal/config"
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

var sensitive = regexp.MustCompile(`(?i)(api[_-]?key|token|password|secret|authorization|bearer)\s*[:=]\s*[^\s,;]+`)

// RedactText removes common credential-like key/value pairs before text is
// placed in a provider request. It deliberately preserves the surrounding
// explanation so prompts remain useful for diagnosis.
func RedactText(text string) string {
	return sensitive.ReplaceAllStringFunc(text, func(match string) string {
		key := match[:strings.IndexAny(match, ":=")]
		return key + "=[REDACTED]"
	})
}

// Prompt is the minimal context sent to an explanation provider.
type Prompt struct {
	Finding     rules.Finding  `json:"finding"`
	Description string         `json:"description"`
	Context     map[string]any `json:"context"`
}

// BuildPrompt keeps only the finding and a redacted snapshot. Snapshot fields
// that collectors never record (secret and literal env values) therefore
// cannot leak through this boundary.
func BuildPrompt(f rules.Finding, description string, snap snapshot.Snapshot) Prompt {
	f.Message = RedactText(f.Message)
	if f.Evidence != nil {
		copyEvidence := make(map[string]string, len(f.Evidence))
		for key, value := range f.Evidence {
			redactedKey := RedactText(key)
			redactedValue := RedactText(value)
			if sensitive.MatchString(redactedKey) || strings.Contains(strings.ToLower(redactedKey), "secret") || strings.Contains(strings.ToLower(redactedKey), "token") || strings.Contains(strings.ToLower(redactedKey), "password") || strings.Contains(strings.ToLower(redactedKey), "api_key") {
				redactedValue = "[REDACTED]"
			}
			copyEvidence[redactedKey] = redactedValue
		}
		f.Evidence = copyEvidence
	}
	return Prompt{Finding: f, Description: RedactText(description), Context: redactContext(f, snap)}
}

func redactContext(f rules.Finding, snap snapshot.Snapshot) map[string]any {
	context := map[string]any{"mode": snap.Mode, "resource": f.Resource}
	for i := range snap.Spec.Workloads {
		workload := &snap.Spec.Workloads[i]
		if workload.Kind == f.Resource.Kind && workload.Namespace == f.Resource.Namespace && workload.Name == f.Resource.Name {
			context["workload"] = workload
			break
		}
	}
	if snap.Runtime != nil {
		for i := range snap.Runtime.Pods {
			pod := &snap.Runtime.Pods[i]
			if pod.Ref() == f.Resource || (pod.Owner != nil && *pod.Owner == f.Resource) {
				context["pod"] = pod
				break
			}
		}
		for i := range snap.Runtime.Events {
			event := &snap.Runtime.Events[i]
			if event.InvolvedObject == f.Resource {
				context["event"] = event
				break
			}
		}
	}
	data, err := json.Marshal(context)
	if err != nil {
		return context
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return context
	}
	redactJSON(value)
	data, err = json.Marshal(value)
	if err != nil {
		return context
	}
	var redacted map[string]any
	if err := json.Unmarshal(data, &redacted); err != nil {
		return context
	}
	return redacted
}

func redactJSON(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") {
				v[key] = "[REDACTED]"
				continue
			}
			if text, ok := child.(string); ok {
				v[key] = RedactText(text)
			} else {
				redactJSON(child)
			}
		}
	case []any:
		for _, child := range v {
			redactJSON(child)
		}
	}
}

// Client explains one deterministic finding.
type Client interface {
	Explain(context.Context, Prompt) (string, error)
}

// StaticClient is the deterministic offline provider used by --no-llm and as
// the fallback for provider failures.
type StaticClient struct{}

func (StaticClient) Explain(_ context.Context, p Prompt) (string, error) {
	if p.Description == "" {
		return "", errors.New("empty rule description")
	}
	return p.Description, nil
}

// Cache memoizes successful explanations by finding identity and prompt hash.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]string
}

func NewCache() *Cache { return &Cache{entries: map[string]string{}} }

func (c *Cache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.entries[key]
	return v, ok
}

func (c *Cache) Put(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = value
}

func CacheKey(p Prompt) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// HTTPClient is a small OpenAI-compatible chat client. It retries transient
// transport and 5xx failures, while honoring the caller's context deadline.
type HTTPClient struct {
	Endpoint string
	Model    string
	APIKey   string
	Timeout  time.Duration
	Retries  int
	HTTP     *http.Client
	Cache    *Cache
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (c *HTTPClient) Explain(ctx context.Context, p Prompt) (string, error) {
	if c.Endpoint == "" || c.Model == "" {
		return "", errors.New("llm endpoint and model are required")
	}
	key := CacheKey(p)
	if c.Cache != nil {
		if value, ok := c.Cache.Get(key); ok {
			return value, nil
		}
	}
	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: []chatMessage{{Role: "user", Content: mustJSON(p)}}})
	if err != nil {
		return "", err
	}
	retries := c.Retries
	if retries < 0 {
		retries = 0
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		requestCtx := ctx
		if c.Timeout > 0 {
			var cancel context.CancelFunc
			requestCtx, cancel = context.WithTimeout(ctx, c.Timeout)
			defer cancel()
		}
		req, reqErr := http.NewRequestWithContext(requestCtx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
		if reqErr != nil {
			return "", reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		client := c.HTTP
		if client == nil {
			client = http.DefaultClient
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			last = doErr
		} else {
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				last = readErr
			} else if resp.StatusCode >= 500 {
				last = fmt.Errorf("llm provider returned %s", resp.Status)
			} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return "", fmt.Errorf("llm provider returned %s", resp.Status)
			} else {
				var decoded chatResponse
				if err := json.Unmarshal(data, &decoded); err != nil {
					return "", err
				}
				if len(decoded.Choices) == 0 || decoded.Choices[0].Message.Content == "" {
					return "", errors.New("llm provider returned no explanation")
				}
				value := RedactText(decoded.Choices[0].Message.Content)
				if c.Cache != nil {
					c.Cache.Put(key, value)
				}
				return value, nil
			}
		}
		if attempt < retries {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 25 * time.Millisecond):
			}
		}
	}
	return "", last
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// NewFromConfig creates a provider client only when a provider is configured.
// An empty provider deliberately selects the offline static client.
func NewFromConfig(cfg config.LLM, getenv func(string) string) (Client, error) {
	if cfg.Provider == "" {
		return StaticClient{}, nil
	}
	if !strings.EqualFold(cfg.Provider, "openai-compatible") && !strings.EqualFold(cfg.Provider, "openai") {
		return nil, fmt.Errorf("unsupported llm provider %q", cfg.Provider)
	}
	if cfg.Endpoint == "" || cfg.Model == "" {
		return nil, errors.New("llm endpoint and model are required when a provider is configured")
	}
	var timeout time.Duration
	var err error
	if cfg.Timeout != "" {
		timeout, err = time.ParseDuration(cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("llm timeout: %w", err)
		}
	}
	apiKey := ""
	if cfg.APIKeyEnv != "" && getenv != nil {
		apiKey = getenv(cfg.APIKeyEnv)
	}
	return &HTTPClient{Endpoint: cfg.Endpoint, Model: cfg.Model, APIKey: apiKey, Timeout: timeout, Retries: cfg.Retries, Cache: NewCache()}, nil
}
