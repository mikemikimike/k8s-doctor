package collector_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbalukja15/k8s-doctor/internal/collector"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

var update = flag.Bool("update", false, "rewrite golden snapshot files")

func collect(t *testing.T, dir string) *snapshot.Snapshot {
	t.Helper()
	s, err := collector.Manifests(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func marshal(t *testing.T, s *snapshot.Snapshot) []byte {
	t.Helper()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestGolden compares each collected snapshot with its golden file. Run
// `go test ./internal/collector -update` after an intended change.
func TestGolden(t *testing.T) {
	for name, dir := range map[string]string{
		"plain":          "testdata/plain",
		"kustomize-base": "testdata/kustomize/base",
		"kustomize-prod": "testdata/kustomize/overlays/prod",
	} {
		t.Run(name, func(t *testing.T) {
			got := marshal(t, collect(t, dir))
			golden := filepath.Join("testdata", name+".golden.json")
			if *update {
				if err := os.WriteFile(golden, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("snapshot differs from %s; rerun with -update if intended\n%s", golden, got)
			}
			// The golden file must itself be a valid snapshot.
			if _, err := snapshot.Load(golden); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestNoSecretValues proves that values from Secrets, ConfigMaps, env
// literals and kustomize generators never reach the snapshot.
func TestNoSecretValues(t *testing.T) {
	leaks := []string{
		"supersecret-password", "c3VwZXJzZWNyZXQtcGFzc3dvcmQ=", "plaintext-username-value",
		"debug-config-value", "s3cr3t-literal-token",
		"generated-secret-token", "hello-from-generator",
	}
	for _, dir := range []string{"testdata/plain", "testdata/kustomize/overlays/prod"} {
		out := string(marshal(t, collect(t, dir)))
		for _, leak := range leaks {
			if strings.Contains(out, leak) {
				t.Errorf("%s: snapshot contains %q", dir, leak)
			}
		}
	}
}

func TestPlainDirectory(t *testing.T) {
	s := collect(t, "testdata/plain")
	if s.Mode != snapshot.ModeManifest || s.Runtime != nil || s.Source.Overlay != nil {
		t.Fatalf("unexpected mode/runtime/overlay: %+v", s.Source)
	}
	if len(s.Spec.Workloads) != 2 {
		t.Fatalf("got %d workloads, want Deployment and CronJob from a List", len(s.Spec.Workloads))
	}
	api := s.Spec.Workloads[1]
	if api.Ref().String() != "Deployment/default/api" {
		t.Errorf("namespace should default to %q: %s", collector.DefaultNamespace, api.Ref())
	}
	env := api.Template.Spec.Containers[0].Env
	if len(env) != 2 || env[0].SourceType != "value" || env[1].SourceType != "secretKeyRef" {
		t.Errorf("env = %+v", env)
	}
	if len(s.Spec.Secrets) != 1 || strings.Join(s.Spec.Secrets[0].Keys, ",") != "password,username" {
		t.Errorf("secret keys = %+v", s.Spec.Secrets)
	}
	hpa := s.Spec.HorizontalPodAutoscalers
	if len(hpa) != 1 || len(hpa[0].Metrics) != 1 || hpa[0].Metrics[0].Resource != "cpu" || hpa[0].Metrics[0].Target != "Utilization" {
		t.Errorf("autoscaling/v1 HPA not converted to a cpu utilization metric: %+v", hpa)
	}
	if len(s.Spec.Services) != 1 || len(s.Spec.PodDisruptionBudgets) != 1 || len(s.Spec.Namespaces) != 1 {
		t.Errorf("missing Service, PDB or Namespace: %+v", s.Spec)
	}
}

func TestKustomizeOverlay(t *testing.T) {
	s := collect(t, "testdata/kustomize/overlays/prod")
	o := s.Source.Overlay
	if o == nil || o.Name != "prod" || o.NamePrefix != "prod-" || o.NameSuffix != "-v2" || o.Namespace != "shop-prod" {
		t.Fatalf("overlay metadata = %+v", o)
	}
	if len(s.Spec.Workloads) != 1 {
		t.Fatalf("workloads = %+v", s.Spec.Workloads)
	}
	w := s.Spec.Workloads[0]
	if w.Ref().String() != "Deployment/shop-prod/prod-web-v2" || w.Replicas == nil || *w.Replicas != 3 {
		t.Errorf("overlay not applied: %s replicas %v", w.Ref(), w.Replicas)
	}
	// Generated names carry the prefix, suffix and content hash, and the
	// volume reference is rewritten to match.
	vol := w.Template.Spec.Volumes[0]
	if !strings.HasPrefix(vol.Source, "prod-web-secret-v2-") || len(s.Spec.Secrets) != 1 || s.Spec.Secrets[0].Name != vol.Source {
		t.Errorf("secretGenerator name not propagated: volume %q, secrets %+v", vol.Source, s.Spec.Secrets)
	}
}

func TestDeterministic(t *testing.T) {
	a := marshal(t, collect(t, "testdata/plain"))
	b := marshal(t, collect(t, "testdata/plain"))
	if !bytes.Equal(a, b) {
		t.Fatal("collecting the same directory twice gave different snapshots")
	}
}

func TestErrors(t *testing.T) {
	dup := t.TempDir()
	doc := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n"
	for _, f := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(dup, f), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	notDir := filepath.Join(dup, "a.yaml")
	cases := map[string]struct{ dir, want string }{
		"nested kustomization": {"testdata/nested", "contains a kustomization"},
		"duplicate object":     {dup, "defined more than once"},
		"missing directory":    {"testdata/nope", "no such file"},
		"file, not directory":  {notDir, "not a directory"},
	}
	for name, c := range cases {
		if _, err := collector.Manifests(c.dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, c.want)
		}
	}
}
