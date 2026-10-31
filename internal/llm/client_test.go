package llm

import (
	"context"
	"encoding/json"
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
	f := rules.Finding{Severity: rules.SeverityHigh, RuleID: "example-rule", Message: "token=abc123", Evidence: map[string]string{"api_key": "secret-value", "safe": "ok"}}
	p := BuildPrompt(f, "Use password: hunter2", snapshot.Snapshot{SchemaVersion: snapshot.SchemaVersion, Mode: snapshot.ModeCluster, Runtime: &snapshot.Runtime{Events: []snapshot.Event{{Message: "authorization: bearer-secret"}}}})
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, leaked := range []string{"abc123", "secret-value", "hunter2", "bearer-secret"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("prompt leaked %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("prompt was not redacted: %s", out)
	}
}

func TestHTTPClientRetriesAndCaches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "temporary", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"token: leaked"}}]}`))
	}))
	defer srv.Close()
	c := &HTTPClient{Endpoint: srv.URL, Model: "test", Retries: 1, Cache: NewCache()}
	p := Prompt{Description: "desc"}
	got, err := c.Explain(context.Background(), p)
	if err != nil || got != "token=[REDACTED]" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if _, err := c.Explain(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("server calls %d, want 2 (retry then cache)", calls)
	}
}

func TestNewFromConfig(t *testing.T) {
	client, err := NewFromConfig(config.LLM{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(StaticClient); !ok {
		t.Fatalf("default client %T, want StaticClient", client)
	}
	client, err = NewFromConfig(config.LLM{Provider: "openai-compatible", Endpoint: "http://localhost", Model: "m", Timeout: "2s", APIKeyEnv: "KEY"}, func(string) string { return "value" })
	if err != nil {
		t.Fatal(err)
	}
	httpClient, ok := client.(*HTTPClient)
	if !ok || httpClient.Timeout != 2*time.Second || httpClient.APIKey != "value" {
		t.Fatalf("unexpected client %#v", client)
	}
}
