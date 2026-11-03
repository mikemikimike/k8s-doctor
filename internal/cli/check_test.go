package cli

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbalukja15/k8s-doctor/internal/llm"
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/rules/checks"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

type deadlineClient struct {
	calls   atomic.Int32
	started chan struct{}
}

func (c *deadlineClient) Explain(ctx context.Context, _ llm.Prompt) (string, error) {
	c.calls.Add(1)
	c.started <- struct{}{}
	<-ctx.Done()
	return "", ctx.Err()
}

type boundedClient struct {
	active    atomic.Int32
	calls     atomic.Int32
	maxActive atomic.Int32
	started   chan struct{}
	release   <-chan struct{}
}

func (c *boundedClient) Explain(ctx context.Context, _ llm.Prompt) (string, error) {
	active := c.active.Add(1)
	defer c.active.Add(-1)
	c.calls.Add(1)
	for previous := c.maxActive.Load(); active > previous; previous = c.maxActive.Load() {
		if c.maxActive.CompareAndSwap(previous, active) {
			break
		}
	}
	c.started <- struct{}{}
	select {
	case <-c.release:
		return "explanation", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestExplainFindingsStopsAtOverallDeadline(t *testing.T) {
	rule := checks.Registry().All()[0]
	findings := make([]rules.Finding, llmExplanationWorkers+2)
	for i := range findings {
		findings[i] = rules.Finding{ID: "finding-" + strconv.Itoa(i), RuleID: rule.ID(), Severity: rules.SeverityHigh}
	}
	client := &deadlineClient{started: make(chan struct{}, llmExplanationWorkers)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var explanations map[string]string
	var failures int
	var firstFailure error
	go func() {
		explanations, failures, firstFailure = explainFindings(ctx, client, findings, checks.Registry(), snapshot.Snapshot{})
		close(done)
	}()
	for range llmExplanationWorkers {
		select {
		case <-client.started:
		case <-time.After(time.Second):
			t.Fatal("worker pool did not start the expected concurrent requests")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("explanation workers did not stop after cancellation")
	}
	if calls := client.calls.Load(); calls != llmExplanationWorkers {
		t.Fatalf("provider calls %d, want %d before cancellation", calls, llmExplanationWorkers)
	}
	if len(explanations) != 0 || failures != len(findings) {
		t.Fatalf("got explanations %v and %d failures", explanations, failures)
	}
	if !errors.Is(firstFailure, context.Canceled) {
		t.Fatalf("first failure = %v, want %v", firstFailure, context.Canceled)
	}
}

func TestExplainFindingsBoundsConcurrency(t *testing.T) {
	rule := checks.Registry().All()[0]
	findings := make([]rules.Finding, llmExplanationWorkers*2)
	for i := range findings {
		findings[i] = rules.Finding{ID: "finding-" + strconv.Itoa(i), RuleID: rule.ID(), Severity: rules.SeverityHigh}
	}
	release := make(chan struct{})
	client := &boundedClient{started: make(chan struct{}, len(findings)), release: release}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		explanations map[string]string
		failures     int
		firstFailure error
	}
	done := make(chan result, 1)
	go func() {
		explanations, failures, firstFailure := explainFindings(ctx, client, findings, checks.Registry(), snapshot.Snapshot{})
		done <- result{explanations: explanations, failures: failures, firstFailure: firstFailure}
	}()
	for range llmExplanationWorkers {
		select {
		case <-client.started:
		case <-time.After(time.Second):
			t.Fatal("worker pool did not start the expected concurrent requests")
		}
	}
	close(release)
	select {
	case got := <-done:
		if len(got.explanations) != len(findings) || got.failures != 0 || got.firstFailure != nil {
			t.Fatalf("got explanations %v and %d failures", got.explanations, got.failures)
		}
	case <-time.After(time.Second):
		t.Fatal("worker pool did not finish after requests were released")
	}
	if calls := client.calls.Load(); calls != int32(len(findings)) {
		t.Fatalf("provider calls %d, want %d", calls, len(findings))
	}
	if active := client.maxActive.Load(); active != llmExplanationWorkers {
		t.Fatalf("maximum concurrent requests %d, want %d", active, llmExplanationWorkers)
	}
}
