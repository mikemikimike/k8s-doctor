package fixtures_test

import (
	"fmt"
	"testing"

	"github.com/rbalukja15/k8s-doctor/internal/fixtures"
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/rules/checks"
)

const root = "../../fixtures"

// TestFixtures runs every fixture through the engine and diffs actual
// against expected findings.
func TestFixtures(t *testing.T) {
	reg := checks.Registry()
	engine, err := rules.NewEngine(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	all, err := fixtures.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, f := range all {
		t.Run(f.Name, func(t *testing.T) {
			if err := f.Validate(reg); err != nil {
				t.Fatal(err)
			}
			snap, err := f.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			d := f.Compare(engine.Run(snap))
			for _, k := range d.Missing {
				t.Errorf("missing finding (false negative): %s", fmtKey(k))
			}
			for _, k := range d.Unexpected {
				t.Errorf("unexpected finding (false positive): %s", fmtKey(k))
			}
		})
	}
}

// TestEveryRuleHasFixtures enforces that each rule ships with at least one
// positive, one clean and one near-miss fixture.
func TestEveryRuleHasFixtures(t *testing.T) {
	all, err := fixtures.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range checks.All() {
		kinds := map[string]bool{}
		for _, f := range all {
			if !f.InScope(r.ID()) {
				continue
			}
			// A positive fixture only counts if it expects a finding from this rule.
			if f.Expected.Kind == fixtures.KindPositive && !expectsRule(f, r.ID()) {
				continue
			}
			kinds[f.Expected.Kind] = true
		}
		for _, k := range []string{fixtures.KindPositive, fixtures.KindClean, fixtures.KindNearMiss} {
			if !kinds[k] {
				t.Errorf("rule %s has no %s fixture", r.ID(), k)
			}
		}
	}
}

func expectsRule(f fixtures.Fixture, id string) bool {
	for _, ef := range f.Expected.Findings {
		if ef.Rule == id {
			return true
		}
	}
	return false
}

func fmtKey(k rules.Key) string {
	return fmt.Sprintf("%s on %s at %s", k.RuleID, k.Resource, k.FieldPath)
}
