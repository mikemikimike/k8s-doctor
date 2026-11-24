// Package config loads .k8s-doctor.yaml: rule toggles, severity overrides
// and (from #29) the overlay-drift allowlist, all in one file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// DefaultFileName is the config file looked up in the working directory.
const DefaultFileName = ".k8s-doctor.yaml"

// Config is the parsed config file. The zero value is the default config:
// every rule enabled at its default severity.
type Config struct {
	// Rules is keyed by rule ID.
	Rules map[string]RuleConfig `json:"rules,omitempty"`
	// Drift configures overlay-drift detection (#14).
	Drift Drift `json:"drift,omitzero"`
	// LLM configures the optional explanation provider. An empty Provider keeps
	// the command fully offline and uses rule descriptions.
	LLM LLM `json:"llm,omitzero"`
}

// LLM configures an OpenAI-compatible explanation provider. The endpoint and
// API key are supplied through process environment variables, not this file.
type LLM struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Retries  *int   `json:"retries,omitempty"`
}

// RuleConfig overrides one rule.
type RuleConfig struct {
	// Enabled defaults to true when unset.
	Enabled *bool `json:"enabled,omitempty"`
	// Severity replaces the rule's default severity when set.
	Severity string `json:"severity,omitempty"`
}

// Drift configures overlay-drift detection.
type Drift struct {
	// Allowlist holds field-path globs whose differences between overlays
	// are expected, e.g. "spec.replicas". Matching lands with #29.
	Allowlist []string `json:"allowlist,omitempty"`
}

// RuleEnabled reports whether the rule is enabled (the default).
func (c *Config) RuleEnabled(id string) bool {
	rc, ok := c.Rules[id]
	if !ok || rc.Enabled == nil {
		return true
	}
	return *rc.Enabled
}

// Parse parses config YAML. Unknown keys are errors, so a misspelled
// option is reported instead of silently ignored.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Load reads a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// LoadDefault loads DefaultFileName from dir, or returns the default config
// when the file does not exist.
func LoadDefault(dir string) (*Config, error) {
	c, err := Load(filepath.Join(dir, DefaultFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	return c, err
}
