// Package fixtures loads the labeled fixtures under fixtures/ and compares
// rule engine output against their expected findings. The format is
// documented in fixtures/README.md.
package fixtures

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/rbalukja15/k8s-doctor/internal/collector"
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// File names inside a fixture directory.
const (
	ExpectedFile = "expected.yaml"
	SnapshotFile = "snapshot.json"
	ManifestsDir = "manifests"
)

// Kinds of fixture.
const (
	KindPositive = "positive"
	KindClean    = "clean"
	KindNearMiss = "near-miss"
)

// Expected is the parsed expected.yaml of a fixture.
type Expected struct {
	Description string `json:"description"`
	// Kind is positive, clean or near-miss.
	Kind string `json:"kind"`
	// Rules limits the comparison to these rule IDs. Empty means every rule,
	// which is what clean fixtures should normally use.
	Rules    []string          `json:"rules,omitempty"`
	Findings []ExpectedFinding `json:"findings"`
}

// ExpectedFinding identifies a finding by rule, resource and field path:
// the same inputs as the stable finding ID.
type ExpectedFinding struct {
	Rule     string `json:"rule"`
	Resource string `json:"resource"`
	Field    string `json:"field"`
}

// Key returns the comparison key.
func (e ExpectedFinding) Key() rules.Key {
	return rules.Key{RuleID: e.Rule, Resource: e.Resource, FieldPath: e.Field}
}

// Fixture is one fixture directory.
type Fixture struct {
	// Name is the directory path relative to the fixtures root.
	Name     string
	Dir      string
	Expected Expected
}

// Discover finds every directory under root that contains an expected.yaml,
// sorted by name.
func Discover(root string) ([]Fixture, error) {
	var out []Fixture
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != ExpectedFile {
			return nil
		}
		dir := filepath.Dir(path)
		name, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		exp, err := loadExpected(path)
		if err != nil {
			return err
		}
		out = append(out, Fixture{Name: filepath.ToSlash(name), Dir: dir, Expected: *exp})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func loadExpected(path string) (*Expected, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e Expected
	if err := yaml.UnmarshalStrict(data, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &e, nil
}

// Validate checks the fixture against the registry: known kind, known rule
// IDs, findings within the declared rule scope, and finding counts that
// match the kind (clean has none, positive has at least one).
func (f *Fixture) Validate(reg *rules.Registry) error {
	var errs []error
	e := f.Expected
	switch e.Kind {
	case KindPositive:
		if len(e.Findings) == 0 {
			errs = append(errs, errors.New("positive fixture expects no findings"))
		}
	case KindClean:
		if len(e.Findings) != 0 {
			errs = append(errs, errors.New("clean fixture must expect no findings"))
		}
	case KindNearMiss:
	default:
		errs = append(errs, fmt.Errorf("kind %q is not positive, clean or near-miss", e.Kind))
	}
	if e.Description == "" {
		errs = append(errs, errors.New("description is required"))
	}
	for _, id := range e.Rules {
		if _, ok := reg.Get(id); !ok {
			errs = append(errs, fmt.Errorf("rules: unknown rule %q", id))
		}
	}
	for _, ef := range e.Findings {
		if _, ok := reg.Get(ef.Rule); !ok {
			errs = append(errs, fmt.Errorf("findings: unknown rule %q", ef.Rule))
		}
		if !f.InScope(ef.Rule) {
			errs = append(errs, fmt.Errorf("findings: rule %q is outside the fixture's rules scope", ef.Rule))
		}
		if _, err := snapshot.ParseResourceRef(ef.Resource); err != nil {
			errs = append(errs, fmt.Errorf("findings: %w", err))
		}
		if ef.Field == "" {
			errs = append(errs, fmt.Errorf("findings: rule %q on %s has no field", ef.Rule, ef.Resource))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	return nil
}

// InScope reports whether findings of rule id are compared for this fixture.
func (f *Fixture) InScope(id string) bool {
	return len(f.Expected.Rules) == 0 || slices.Contains(f.Expected.Rules, id)
}

// Snapshot loads the fixture's input: manifests/ through the manifest
// collector when present, otherwise snapshot.json.
func (f *Fixture) Snapshot() (*snapshot.Snapshot, error) {
	dir := filepath.Join(f.Dir, ManifestsDir)
	if _, err := os.Stat(dir); err == nil {
		return collector.Manifests(dir)
	}
	return snapshot.Load(filepath.Join(f.Dir, SnapshotFile))
}

// Diff is the difference between expected and actual findings.
type Diff struct {
	Missing    []rules.Key // expected but not produced (false negatives)
	Unexpected []rules.Key // produced but not expected (false positives)
}

// Empty reports whether actual matched expected exactly.
func (d Diff) Empty() bool { return len(d.Missing) == 0 && len(d.Unexpected) == 0 }

// Compare diffs the engine's findings against the fixture's expectations,
// restricted to the fixture's rule scope.
func (f *Fixture) Compare(res rules.Result) Diff {
	want := map[rules.Key]bool{}
	for _, ef := range f.Expected.Findings {
		want[ef.Key()] = true
	}
	got := map[rules.Key]bool{}
	for i := range res.Findings {
		if f.InScope(res.Findings[i].RuleID) {
			got[res.Findings[i].Key()] = true
		}
	}
	var d Diff
	for k := range want {
		if !got[k] {
			d.Missing = append(d.Missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			d.Unexpected = append(d.Unexpected, k)
		}
	}
	sortKeys(d.Missing)
	sortKeys(d.Unexpected)
	return d
}

func sortKeys(ks []rules.Key) {
	sort.Slice(ks, func(i, j int) bool {
		a, b := ks[i], ks[j]
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		return a.FieldPath < b.FieldPath
	})
}
