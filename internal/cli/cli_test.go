package cli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbalukja15/k8s-doctor/internal/cli"
	"github.com/rbalukja15/k8s-doctor/internal/llm"
	"github.com/rbalukja15/k8s-doctor/internal/report"
)

const (
	clean     = "../../fixtures/clean/well-configured/snapshot.json"
	highFix   = "../../fixtures/image-pull-policy-stale/positive/snapshot.json"   // worst: high
	mediumFix = "../../fixtures/deployment-single-replica/positive/snapshot.json" // worst: medium
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = cli.RunEnv(args, cli.Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }})
	return code, out.String(), errOut.String()
}

func TestUsage(t *testing.T) {
	code, out, _ := run()
	if code != cli.ExitOK || !strings.Contains(out, "Usage:") {
		t.Fatalf("no-arg run: code %d, output %q", code, out)
	}
	code, _, errOut := run("frobnicate")
	if code != cli.ExitUsage || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown command: code %d, stderr %q", code, errOut)
	}
	code, _, errOut = run("check", "-h")
	if code != cli.ExitOK || !strings.Contains(errOut, "Exit codes:") {
		t.Fatalf("check -h: code %d, stderr %q", code, errOut)
	}
}

func TestRulesAndSchema(t *testing.T) {
	code, out, _ := run("rules")
	if code != cli.ExitOK || !strings.Contains(out, "probe-readiness-missing") {
		t.Fatalf("rules: code %d, output %q", code, out)
	}
	code, out, _ = run("schema")
	if code != cli.ExitOK || !json.Valid([]byte(out)) || out != string(report.Schema) {
		t.Fatalf("schema: code %d, output is not the embedded schema", code)
	}
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"clean", []string{"--snapshot", clean}, cli.ExitOK},
		{"high at default threshold", []string{"--snapshot", highFix}, cli.ExitFindingHigh},
		{"medium below default threshold", []string{"--snapshot", mediumFix}, cli.ExitOK},
		{"medium with --fail-on medium", []string{"--snapshot", mediumFix, "--fail-on", "medium"}, cli.ExitFindingMedium},
		{"high with --fail-on critical", []string{"--snapshot", highFix, "--fail-on", "critical"}, cli.ExitOK},
		{"high with --fail-on none", []string{"--snapshot", highFix, "--fail-on", "none"}, cli.ExitOK},
		{"high rule disabled", []string{"--snapshot", highFix, "--disable-rule", "image-pull-policy-stale", "--fail-on", "low"}, cli.ExitFindingMedium},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errOut := run(append([]string{"check"}, c.args...)...)
			if code != c.want {
				t.Errorf("code %d, want %d (stderr %q)", code, c.want, errOut)
			}
		})
	}
}

func TestBareFlagsRunCheck(t *testing.T) {
	code, out, _ := run("--snapshot", highFix, "--color", "never")
	if code != cli.ExitFindingHigh || !strings.Contains(out, "image-pull-policy-stale") {
		t.Fatalf("code %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "\033[") {
		t.Error("--color never still printed ANSI codes")
	}
}

func TestJSONOutputAndReportFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	code, out, errOut := run("check", "--snapshot", highFix, "--format", "json", "--report", path)
	if code != cli.ExitFindingHigh {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	var stdoutReport, fileReport report.Report
	if err := json.Unmarshal([]byte(out), &stdoutReport); err != nil {
		t.Fatalf("stdout is not a JSON report: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fileReport); err != nil {
		t.Fatalf("--report file is not a JSON report: %v", err)
	}
	if stdoutReport.SchemaVersion != report.SchemaVersion || stdoutReport.Summary.Worst != "high" {
		t.Errorf("unexpected report: %+v", stdoutReport.Summary)
	}
	if len(fileReport.Findings) != len(stdoutReport.Findings) {
		t.Error("--report file and stdout disagree")
	}
	if stdoutReport.Input.LLM {
		t.Error("report claims the LLM ran")
	}
}

func TestTextReportFileStillJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	code, out, _ := run("check", "--snapshot", clean, "--report", path)
	if code != cli.ExitOK || !strings.Contains(out, "No findings.") {
		t.Fatalf("code %d, output %q", code, out)
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		t.Fatalf("--report with text format should still write JSON: %v", err)
	}
}

func TestInputAndFlagErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    int
		wantErr string
	}{
		{"no input", []string{"check"}, cli.ExitUsage, "exactly one of"},
		{"two inputs", []string{"check", "--snapshot", clean, "--cluster"}, cli.ExitUsage, "exactly one of"},
		{"cluster bad kubeconfig", []string{"check", "--cluster", "--kubeconfig", "no-such-kubeconfig"}, cli.ExitError, "kubeconfig"},
		{"cluster flags without --cluster", []string{"check", "--snapshot", clean, "--namespace", "shop"}, cli.ExitUsage, "only apply to --cluster"},
		{"manifests missing dir", []string{"check", "--manifests", "no-such-dir"}, cli.ExitError, "no-such-dir"},
		{"missing file", []string{"check", "--snapshot", "nope.json"}, cli.ExitError, "nope.json"},
		{"bad format", []string{"check", "--snapshot", clean, "--format", "yaml"}, cli.ExitUsage, "--format"},
		{"bad fail-on", []string{"check", "--snapshot", clean, "--fail-on", "urgent"}, cli.ExitUsage, "--fail-on"},
		{"bad color", []string{"check", "--snapshot", clean, "--color", "rainbow"}, cli.ExitUsage, "--color"},
		{"unknown rule", []string{"check", "--snapshot", clean, "--disable-rule", "no-such-rule"}, cli.ExitError, "no-such-rule"},
		{"rule enabled and disabled", []string{"check", "--snapshot", clean, "--enable-rule", "image-tag-mutable", "--disable-rule", "image-tag-mutable"}, cli.ExitUsage, "both"},
		{"stray argument", []string{"check", "--snapshot", clean, "extra"}, cli.ExitUsage, "extra"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errOut := run(c.args...)
			if code != c.want || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("code %d (want %d), stderr %q (want %q)", code, c.want, errOut, c.wantErr)
			}
		})
	}
}

func TestNoColorEnv(t *testing.T) {
	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &bytes.Buffer{}, Getenv: func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	}}
	cli.RunEnv([]string{"check", "--snapshot", highFix}, env)
	if strings.Contains(out.String(), "\033[") {
		t.Error("NO_COLOR was not honored")
	}
}

func TestManifests(t *testing.T) {
	code, out, errOut := run("check", "--manifests", "../../fixtures/kustomize/prod-overlay/manifests", "--color", "never")
	if code != cli.ExitFindingHigh {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"Deployment/shop-prod/prod-checkout", "deployment-single-replica", "manifest mode"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestSnapshotCommand(t *testing.T) {
	code, out, errOut := run("snapshot", "--manifests", "../../fixtures/kustomize/prod-overlay/manifests")
	if code != cli.ExitOK {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(out), &s); err != nil || s["mode"] != "manifest" {
		t.Fatalf("not a manifest snapshot: %v\n%s", err, out)
	}
	if code, _, errOut := run("snapshot"); code != cli.ExitUsage || !strings.Contains(errOut, "--cluster or --manifests") {
		t.Errorf("no input: code %d, stderr %q", code, errOut)
	}
	if code, _, _ := run("snapshot", "--snapshot", clean); code != cli.ExitUsage {
		t.Errorf("--snapshot should not be a snapshot flag, code %d", code)
	}
}

func TestLLMFailureWarnsAndFallsBackToRuleExplanation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	configPath := writeLLMConfig(t)

	var out, errOut bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errOut, Getenv: llmTestEnv(server.URL)}
	code := cli.RunEnv([]string{"check", "--snapshot", highFix, "--config", configPath, "--format", "json", "--llm"}, env)
	if code != cli.ExitFindingHigh {
		t.Fatalf("code %d, want finding exit code; stderr %q", code, errOut.String())
	}
	var got report.Report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if got.Input.LLM {
		t.Error("report claims LLM was used although all requests failed")
	}
	if len(got.Findings) == 0 || got.Findings[0].Explanation.Source != report.SourceRule {
		t.Fatalf("explanation did not fall back to rule text: %+v", got.Findings)
	}
	if !strings.Contains(errOut.String(), "warning: LLM explanations failed for ") || !strings.Contains(errOut.String(), "first failure: llm provider returned 503 Service Unavailable") || !strings.Contains(errOut.String(), "using rule descriptions") {
		t.Fatalf("missing fallback warning: %q", errOut.String())
	}
	if strings.Contains(errOut.String(), "unavailable") {
		t.Fatal("warning exposed the provider response body")
	}
}

func TestLLMSuccessMarksReportUsed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"A generated explanation."}}]}`))
	}))
	defer server.Close()
	configPath := writeLLMConfig(t)

	var out, errOut bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errOut, Getenv: llmTestEnv(server.URL)}
	code := cli.RunEnv([]string{"check", "--snapshot", highFix, "--config", configPath, "--format", "json", "--llm"}, env)
	if code != cli.ExitFindingHigh {
		t.Fatalf("code %d, want finding exit code; stderr %q", code, errOut.String())
	}
	var got report.Report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !got.Input.LLM || len(got.Findings) == 0 || got.Findings[0].Explanation.Source != report.SourceLLM {
		t.Fatalf("successful explanation not reflected in report: input=%+v findings=%+v", got.Input, got.Findings)
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected warning on success: %q", errOut.String())
	}
}

func TestLLMConfigRequiresExplicitOptIn(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected call", http.StatusInternalServerError)
	}))
	defer server.Close()
	configPath := writeLLMConfig(t)

	var out, errOut bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errOut, Getenv: llmTestEnv(server.URL)}
	code := cli.RunEnv([]string{"check", "--snapshot", highFix, "--config", configPath, "--format", "json"}, env)
	if code != cli.ExitFindingHigh {
		t.Fatalf("code %d, want finding exit code; stderr %q", code, errOut.String())
	}
	var got report.Report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if got.Input.LLM || calls.Load() != 0 || errOut.Len() != 0 {
		t.Fatalf("LLM was not kept opt-in: used=%t calls=%d stderr=%q", got.Input.LLM, calls.Load(), errOut.String())
	}
}

func TestLLMFlagRequiresConfiguredProvider(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("llm:\n  model: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }}
	code := cli.RunEnv([]string{"check", "--snapshot", highFix, "--config", configPath, "--llm"}, env)
	if code != cli.ExitUsage {
		t.Fatalf("code %d, want usage error; stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "--llm requires llm.provider") {
		t.Fatalf("missing provider error: %q", errOut.String())
	}
}

func writeLLMConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("llm:\n  provider: openai-compatible\n  model: test\n  retries: 0\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func llmTestEnv(endpoint string) func(string) string {
	return func(name string) string {
		if name == llm.EndpointEnv {
			return endpoint
		}
		return ""
	}
}
