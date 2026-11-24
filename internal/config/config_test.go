package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rbalukja15/k8s-doctor/internal/config"
)

func TestParse(t *testing.T) {
	c, err := config.Parse([]byte(`
rules:
  probe-liveness-missing:
    enabled: false
  image-tag-mutable:
    severity: high
drift:
  allowlist:
    - spec.replicas
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.RuleEnabled("probe-liveness-missing") {
		t.Error("probe-liveness-missing should be disabled")
	}
	if !c.RuleEnabled("image-tag-mutable") || !c.RuleEnabled("not-mentioned") {
		t.Error("rules are enabled unless disabled")
	}
	if c.Rules["image-tag-mutable"].Severity != "high" {
		t.Error("severity override not parsed")
	}
	if len(c.Drift.Allowlist) != 1 {
		t.Error("drift allowlist not parsed")
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	if _, err := config.Parse([]byte("rulez: {}\n")); err == nil {
		t.Error("expected an error for an unknown top-level key")
	}
	if _, err := config.Parse([]byte("rules:\n  x:\n    enable: false\n")); err == nil {
		t.Error("expected an error for a misspelled rule option")
	}
	for _, field := range []string{"endpoint", "apiKeyEnv"} {
		data := []byte("llm:\n  provider: openai-compatible\n  " + field + ": value\n")
		if _, err := config.Parse(data); err == nil {
			t.Errorf("expected repository config field %q to be rejected", field)
		}
	}
}

func TestLoadDefault(t *testing.T) {
	dir := t.TempDir()
	c, err := config.LoadDefault(dir)
	if err != nil || c == nil || len(c.Rules) != 0 {
		t.Fatalf("missing file should give the default config, got %+v, %v", c, err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.DefaultFileName), []byte("rules:\n  x:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = config.LoadDefault(dir)
	if err != nil || c.RuleEnabled("x") {
		t.Fatalf("config file not loaded: %+v, %v", c, err)
	}
}
