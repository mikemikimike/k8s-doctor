package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rbalukja15/k8s-doctor/internal/config"
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

func TestRedactAndBuildPrompt(t *testing.T) {
	f := rules.Finding{
		Severity: rules.SeverityHigh,
		RuleID:   "example-rule",
		Message:  "token=abc123 Authorization: Bearer auth-secret postgres://dbuser:dbpass@db.example/app redis://session-token@cache.example/0",
		Evidence: map[string]string{"api_key": "secret-value", "safe": "ok", "connection": "postgres://user:url-secret@db.example/app"},
	}
	p := BuildPrompt(f, "Use password: hunter2", snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Mode:          snapshot.ModeCluster,
		Runtime:       &snapshot.Runtime{Events: []snapshot.Event{{Message: "authorization: Bearer bearer-secret"}}},
	})
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, leaked := range []string{"abc123", "secret-value", "hunter2", "bearer-secret", "auth-secret", "dbuser", "dbpass", "url-secret", "session-token", "user:"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("prompt leaked %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("prompt was not redacted: %s", out)
	}
}

func TestBuildPromptIncludesMatchingEventsAndBoundsContext(t *testing.T) {
	ref := snapshot.ResourceRef{Kind: "Deployment", Namespace: "apps", Name: "web"}
	events := []snapshot.Event{
		{InvolvedObject: ref, Reason: "first", Message: "first matching event"},
		{InvolvedObject: ref, Reason: "second", Message: "second matching event"},
		{InvolvedObject: snapshot.ResourceRef{Kind: "Pod", Namespace: "apps", Name: "other"}, Reason: "unrelated"},
	}
	prompt := BuildPrompt(rules.Finding{Resource: ref}, "description", snapshot.Snapshot{
		Mode:    snapshot.ModeCluster,
		Runtime: &snapshot.Runtime{Events: events},
	})
	matching, ok := prompt.Context["events"].([]any)
	if !ok || len(matching) != 2 {
		t.Fatalf("matching events = %#v, want both matching events", prompt.Context["events"])
	}
	if !strings.Contains(mustJSON(prompt.Context), "second matching event") {
		t.Fatal("newest matching event is missing from context")
	}

	largeEvents := make([]snapshot.Event, 20)
	for i := range largeEvents {
		largeEvents[i] = snapshot.Event{InvolvedObject: ref, Reason: "large", Message: strings.Repeat("x", 8*1024)}
	}
	largeEvents[5].Message = "later small event"
	bounded := BuildPrompt(rules.Finding{Resource: ref}, "description", snapshot.Snapshot{
		Mode:    snapshot.ModeCluster,
		Runtime: &snapshot.Runtime{Events: largeEvents},
	})
	data, err := json.Marshal(bounded.Context)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxContextBytes {
		t.Fatalf("context is %d bytes, limit is %d", len(data), maxContextBytes)
	}
	if !strings.Contains(string(data), "later small event") {
		t.Fatal("a smaller event after oversized context entries was not included")
	}
	if truncated, ok := bounded.Context["truncated"].(bool); !ok || !truncated {
		t.Fatalf("context truncation marker = %#v, want true", bounded.Context["truncated"])
	}
}

func TestHTTPClientRetriesAndRedactsResponse(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(request.Messages) != 2 || request.Messages[0].Role != "system" || !strings.Contains(request.Messages[0].Content, "untrusted Kubernetes data") {
			t.Errorf("request did not mark cluster content untrusted: %+v", request.Messages)
		}
		if calls == 1 {
			http.Error(w, "temporary", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"token: leaked"}}]}`))
	}))
	defer srv.Close()
	c := &HTTPClient{Endpoint: srv.URL, Model: "test", Retries: 1}
	p := Prompt{Description: "desc"}
	got, err := c.Explain(context.Background(), p)
	if err != nil || got != "token=[REDACTED]" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if calls != 2 {
		t.Fatalf("server calls %d, want 2 (one retry)", calls)
	}
}

func TestHTTPClientRejectsNonRetryableStatus(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := &HTTPClient{Endpoint: srv.URL, Model: "test", Retries: 3}
	_, err := client.Explain(context.Background(), Prompt{Description: "desc"})
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("Explain error = %v, want 401 status", err)
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want one non-retried 4xx request", calls)
	}
}

func TestHTTPClientTimesOutAndCancelsRequest(t *testing.T) {
	finished := make(chan struct{})
	transport := requestContextTransport{finished: finished}
	client := &HTTPClient{
		Endpoint: "http://provider.invalid",
		Model:    "test",
		Timeout:  20 * time.Millisecond,
		HTTP:     &http.Client{Transport: transport},
	}
	start := time.Now()
	_, err := client.Explain(context.Background(), Prompt{Description: "desc"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Explain error = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("request did not respect its short timeout")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("request context was not canceled")
	}
}

func TestHTTPClientCancelsContextAfterResponseBody(t *testing.T) {
	transport := &responseContextTransport{}
	client := &HTTPClient{
		Endpoint: "http://provider.invalid",
		Model:    "test",
		HTTP:     &http.Client{Transport: transport},
	}
	_, err := client.Explain(context.Background(), Prompt{Description: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if transport.requestContext == nil {
		t.Fatal("request context was not recorded")
	}
	select {
	case <-transport.requestContext.Done():
	default:
		t.Fatal("request context remained live after response body was read")
	}
}

type requestContextTransport struct {
	finished chan struct{}
}

func (transport requestContextTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	close(transport.finished)
	return nil, request.Context().Err()
}

type responseContextTransport struct {
	requestContext context.Context
}

func (transport *responseContextTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.requestContext = request.Context()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
		Request:    request,
	}, nil
}

func TestNewFromConfig(t *testing.T) {
	if client, err := NewFromConfig(config.LLM{}, nil); err == nil || client != nil {
		t.Fatalf("empty provider returned client %T and err %v, want configuration error", client, err)
	}
	client, err := NewFromConfig(config.LLM{Provider: "openai-compatible", Endpoint: "http://localhost", Model: "m", Timeout: "2s", APIKeyEnv: "KEY"}, func(string) string { return "value" })
	if err != nil {
		t.Fatal(err)
	}
	httpClient, ok := client.(*HTTPClient)
	if !ok || httpClient.Timeout != 2*time.Second || httpClient.APIKey != "value" {
		t.Fatalf("unexpected client %#v", client)
	}
	client, err = NewFromConfig(config.LLM{Provider: "openai", Endpoint: "http://localhost", Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpClient, ok = client.(*HTTPClient)
	if !ok || httpClient.Timeout != DefaultRequestTimeout {
		t.Fatalf("default timeout client = %#v, want %s", client, DefaultRequestTimeout)
	}
	if _, err := NewFromConfig(config.LLM{Provider: "openai-compatible", Endpoint: "http://localhost", Model: "m", Timeout: "0s"}, nil); err == nil {
		t.Fatal("zero timeout was accepted")
	}
}
