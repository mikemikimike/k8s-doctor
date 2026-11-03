package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		Message:  "token=abc123 Authorization: Bearer auth-secret postgres://dbuser:dbpass@db.example/app redis://session-token@cache.example/0 ghp_abcdefghijklmnopqrstuvwxyz1234567890 AKIA1234567890ABCDEF eyJhbGciOiJub25lIn0.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature",
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
	for _, leaked := range []string{"abc123", "secret-value", "hunter2", "bearer-secret", "auth-secret", "dbuser", "dbpass", "url-secret", "session-token", "user:", "ghp_abcdefghijklmnopqrstuvwxyz1234567890", "AKIA1234567890ABCDEF", "eyJhbGciOiJub25lIn0.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("prompt leaked %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("prompt was not redacted: %s", out)
	}
}

func TestBuildPromptKeepsDeploymentContextAndPodEventsWhenTrimmingLabels(t *testing.T) {
	deployment := snapshot.ResourceRef{Kind: "Deployment", Namespace: "apps", Name: "web"}
	podRef := snapshot.ResourceRef{Kind: "Pod", Namespace: "apps", Name: "web-7d9f-x2k9p"}
	replicaSetRef := snapshot.ResourceRef{Kind: "ReplicaSet", Namespace: "apps", Name: "web-7d9f"}
	replicas := int32(3)
	workload := snapshot.Workload{
		Kind:       deployment.Kind,
		APIVersion: "apps/v1",
		ObjectMeta: snapshot.ObjectMeta{
			Name:      deployment.Name,
			Namespace: deployment.Namespace,
			Labels:    map[string]string{"large": strings.Repeat("label", 8*1024)},
		},
		Replicas: &replicas,
		Selector: map[string]string{"large": strings.Repeat("selector", 4*1024)},
		Template: snapshot.PodTemplate{
			Labels: map[string]string{"large": strings.Repeat("template-label", 2*1024)},
			Spec: snapshot.PodSpec{
				Volumes: []snapshot.Volume{{Name: "large", Type: "hostPath", HostPath: strings.Repeat("/volume", 8*1024)}},
				Containers: []snapshot.Container{{
					Name:  "web",
					Image: "example/web:v1",
					Resources: snapshot.Resources{
						Requests: map[string]string{"cpu": "100m"},
						Limits:   map[string]string{"memory": "256Mi"},
					},
					ReadinessProbe: &snapshot.Probe{Handler: "httpGet", Path: "/readyz", Port: "http"},
				}},
			},
		},
	}
	pod := snapshot.Pod{ObjectMeta: snapshot.ObjectMeta{Name: podRef.Name, Namespace: podRef.Namespace}, Owner: &deployment, Phase: "Running"}
	prompt := BuildPrompt(rules.Finding{Resource: deployment}, "description", snapshot.Snapshot{
		Mode: snapshot.ModeCluster,
		Spec: snapshot.Spec{Workloads: []snapshot.Workload{workload}},
		Runtime: &snapshot.Runtime{
			Pods: []snapshot.Pod{pod},
			Events: []snapshot.Event{
				{InvolvedObject: deployment, Reason: "Scaling", Message: "deployment event"},
				{InvolvedObject: replicaSetRef, Owner: &deployment, Reason: "FailedCreate", Message: "replicaset event"},
				{InvolvedObject: podRef, Reason: "BackOff", Message: "pod restart event"},
				{InvolvedObject: snapshot.ResourceRef{Kind: "Pod", Namespace: "apps", Name: "unrelated"}, Reason: "Unrelated"},
			},
		},
	})

	data, err := json.Marshal(prompt.Context)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxContextBytes {
		t.Fatalf("context is %d bytes, limit is %d", len(data), maxContextBytes)
	}
	if truncated, ok := prompt.Context["truncated"].(bool); !ok || !truncated {
		t.Fatalf("context truncation marker = %#v, want true", prompt.Context["truncated"])
	}

	gotWorkload, ok := prompt.Context["workload"].(map[string]any)
	if !ok {
		t.Fatalf("deployment workload was dropped: %#v", prompt.Context["workload"])
	}
	if _, ok := gotWorkload["labels"]; ok {
		t.Fatal("oversized workload labels were not trimmed")
	}
	if _, ok := gotWorkload["selector"]; ok {
		t.Fatal("oversized workload selector was not trimmed")
	}
	template, ok := gotWorkload["template"].(map[string]any)
	if !ok {
		t.Fatalf("workload template missing: %#v", gotWorkload["template"])
	}
	if _, ok := template["labels"]; ok {
		t.Fatal("oversized pod template labels were not trimmed")
	}
	spec, ok := template["spec"].(map[string]any)
	if !ok {
		t.Fatalf("pod spec missing: %#v", template["spec"])
	}
	if _, ok := spec["volumes"]; ok {
		t.Fatal("oversized volumes were not trimmed")
	}
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) != 1 {
		t.Fatalf("workload containers = %#v, want the primary container", spec["containers"])
	}
	container, ok := containers[0].(map[string]any)
	if !ok || container["name"] != "web" {
		t.Fatalf("primary container = %#v", containers[0])
	}
	if _, ok := container["readinessProbe"]; !ok {
		t.Fatal("readiness probe was dropped")
	}
	resources, ok := container["resources"].(map[string]any)
	if !ok || resources["requests"] == nil || resources["limits"] == nil {
		t.Fatalf("container resources were dropped: %#v", container["resources"])
	}

	pods, ok := prompt.Context["pods"].([]any)
	if !ok || len(pods) != 1 {
		t.Fatalf("deployment pods = %#v, want the pod resolved through its ReplicaSet owner", prompt.Context["pods"])
	}
	events, ok := prompt.Context["events"].([]any)
	if !ok || len(events) != 3 {
		t.Fatalf("deployment events = %#v, want deployment, owned ReplicaSet, and owned-pod events", prompt.Context["events"])
	}
	encodedEvents := string(data)
	for _, want := range []string{"deployment event", "replicaset event", "pod restart event"} {
		if !strings.Contains(encodedEvents, want) {
			t.Errorf("deployment context is missing %q", want)
		}
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

func TestHTTPClientDoesNotFollowRedirects(t *testing.T) {
	targetRequests := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			targetRequests <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/redirect" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := &HTTPClient{
		Endpoint: srv.URL + "/redirect",
		Model:    "test",
		Retries:  0,
		HTTP:     &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }},
	}
	_, err := client.Explain(context.Background(), Prompt{Description: "desc"})
	if err == nil || !strings.Contains(err.Error(), "307 Temporary Redirect") {
		t.Fatalf("Explain error = %v, want the original redirect response", err)
	}
	select {
	case <-targetRequests:
		t.Fatal("client followed a redirect")
	default:
	}
}

func TestHTTPClientRetriesRateLimitAndHonorsRetryAfter(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"recovered"}}]}`))
	}))
	defer srv.Close()

	client := &HTTPClient{Endpoint: srv.URL, Model: "test", Retries: 1}
	start := time.Now()
	got, err := client.Explain(context.Background(), Prompt{Description: "desc"})
	if err != nil || got != "recovered" {
		t.Fatalf("Explain = %q, %v; want successful retry", got, err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retry after 429 took %s, want at least one second", elapsed)
	}
	if calls != 2 {
		t.Fatalf("requests = %d, want retry after 429", calls)
	}
}

func TestRetryDelay(t *testing.T) {
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		after   string
		attempt int
		want    time.Duration
		ok      bool
	}{
		{attempt: 0, want: time.Second, ok: true},
		{attempt: 1, want: 2 * time.Second, ok: true},
		{attempt: 2, want: 4 * time.Second, ok: true},
		{attempt: 10, want: maxRetryBackoff, ok: true},
		{after: "5", attempt: 0, want: 5 * time.Second, ok: true},
		{after: now.Add(7 * time.Second).Format(http.TimeFormat), want: 7 * time.Second, ok: true},
		{after: now.Add(-time.Second).Format(http.TimeFormat), want: 0, ok: true},
		{after: strconv.FormatInt(int64(maxRetryAfter/time.Second)+1, 10), ok: false},
	}
	for _, test := range tests {
		got, ok := retryDelay(test.attempt, test.after, now)
		if got != test.want || ok != test.ok {
			t.Errorf("retryDelay(%d, %q) = (%s, %t), want (%s, %t)", test.attempt, test.after, got, ok, test.want, test.ok)
		}
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
	retries := 0
	env := map[string]string{EndpointEnv: "http://localhost:8080/v1/chat/completions", APIKeyEnv: "value"}
	client, err := NewFromConfig(config.LLM{Provider: "openai-compatible", Model: "m", Timeout: "2s", Retries: &retries}, func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	httpClient, ok := client.(*HTTPClient)
	if !ok || httpClient.Endpoint != env[EndpointEnv] || httpClient.Timeout != 2*time.Second || httpClient.APIKey != "value" || httpClient.Retries != 0 {
		t.Fatalf("unexpected client %#v", client)
	}
	client, err = NewFromConfig(config.LLM{Provider: "openai", Model: "m"}, func(name string) string {
		if name == EndpointEnv {
			return "https://provider.example/v1/chat/completions"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	httpClient, ok = client.(*HTTPClient)
	if !ok || httpClient.Timeout != DefaultRequestTimeout || httpClient.Retries != DefaultRetries {
		t.Fatalf("default client = %#v, want timeout %s and %d retries", client, DefaultRequestTimeout, DefaultRetries)
	}
	if _, err := NewFromConfig(config.LLM{Provider: "openai-compatible", Model: "m", Timeout: "0s"}, func(string) string { return "https://provider.example" }); err == nil {
		t.Fatal("zero timeout was accepted")
	}
	if _, err := NewFromConfig(config.LLM{Provider: "openai-compatible", Model: "m"}, func(string) string { return "http://provider.example" }); err == nil {
		t.Fatal("non-loopback HTTP endpoint was accepted")
	}
	negativeRetries := -1
	if _, err := NewFromConfig(config.LLM{Provider: "openai-compatible", Model: "m", Retries: &negativeRetries}, func(string) string { return "https://provider.example" }); err == nil {
		t.Fatal("negative retry count was accepted")
	}
}
