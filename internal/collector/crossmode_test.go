package collector_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/rbalukja15/k8s-doctor/internal/collector"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

const (
	crossModeDir       = "testdata/crossmode"
	crossModeNamespace = "crossmode"
	// kindKubeconfigEnv points TestCrossModeKind at a cluster where
	// testdata/crossmode has been applied. test/kind/run.sh sets it.
	kindKubeconfigEnv = "K8S_DOCTOR_KIND_KUBECONFIG"
)

// TestCrossModeFake loads the cross-mode manifests into fake clients and
// checks that the cluster collector and the manifest collector agree on spec.
// The fake API server applies no defaults, so this pins the shared
// normalization; TestCrossModeKind covers a real API server.
func TestCrossModeFake(t *testing.T) {
	typed, meta := decodeManifests(t, crossModeDir)
	md := metadatafake.NewSimpleMetadataClient(metadataScheme(t), meta...)
	c := &collector.Clients{Typed: fake.NewClientset(typed...), Metadata: md, ContextName: "fake"}
	assertCrossMode(t, c)
}

// TestCrossModeKind runs the same comparison against a real cluster, so
// server-populated defaults are present on the cluster side.
func TestCrossModeKind(t *testing.T) {
	kubeconfig := os.Getenv(kindKubeconfigEnv)
	if kubeconfig == "" {
		t.Skipf("%s not set; run test/kind/run.sh (make test-kind)", kindKubeconfigEnv)
	}
	c, err := collector.NewClients(collector.ClusterOptions{Kubeconfig: kubeconfig})
	if err != nil {
		t.Fatal(err)
	}
	assertCrossMode(t, c)
}

func assertCrossMode(t *testing.T, c *collector.Clients) {
	t.Helper()
	fromManifests := collect(t, crossModeDir)
	if n := len(fromManifests.Spec.Workloads); n != 5 {
		t.Fatalf("got %d workloads from %s, want all five kinds", n, crossModeDir)
	}
	if fromManifests.Runtime != nil {
		t.Error("manifest mode snapshot has a runtime section")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fromCluster, err := collector.Cluster(ctx, c, collector.ClusterOptions{Namespaces: []string{crossModeNamespace}})
	if err != nil {
		t.Fatal(err)
	}
	if fromCluster.Runtime == nil {
		t.Error("cluster mode snapshot has no runtime section")
	}

	want := specJSON(t, comparableSpec(fromManifests.Spec, false))
	got := specJSON(t, comparableSpec(fromCluster.Spec, true))
	if !bytes.Equal(got, want) {
		t.Errorf("spec differs between modes (after dropping server defaults)\n--- manifests\n%s\n--- cluster\n%s", want, got)
	}
}

func specJSON(t *testing.T, s snapshot.Spec) []byte {
	t.Helper()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// comparableSpec rewrites a spec so that fields the API server fills in, and
// data only one mode can see, compare equal. Unset fields are set to their
// server default rather than defaults dropped, so a manifest that sets a
// value different from the default still shows up as a difference. The list
// mirrors ADR-0002 "How the two modes converge".
func comparableSpec(in snapshot.Spec, cluster bool) snapshot.Spec {
	var s snapshot.Spec
	raw, _ := json.Marshal(in)
	_ = json.Unmarshal(raw, &s) // deep copy

	for i := range s.Namespaces {
		// Set by the API server on every Namespace.
		delete(s.Namespaces[i].Labels, "kubernetes.io/metadata.name")
		s.Namespaces[i].Labels = nilIfEmpty(s.Namespaces[i].Labels)
	}
	for i := range s.Workloads {
		w := &s.Workloads[i]
		if _, ok := w.EffectiveReplicas(); ok && w.Replicas == nil {
			one := int32(1)
			w.Replicas = &one
		}
		if w.Kind == snapshot.KindJob {
			// The Job controller labels its template and generates the
			// selector, and a Job without labels gets its template's labels.
			for _, k := range []string{"controller-uid", "job-name", "batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name"} {
				delete(w.Labels, k)
				delete(w.Selector, k)
				delete(w.Template.Labels, k)
			}
			if len(w.Labels) == 0 {
				w.Labels = maps.Clone(w.Template.Labels)
			}
			w.Labels = nilIfEmpty(w.Labels)
			w.Selector = nilIfEmpty(w.Selector)
			w.Template.Labels = nilIfEmpty(w.Template.Labels)
		}
		for _, cs := range [][]snapshot.Container{w.Template.Spec.InitContainers, w.Template.Spec.Containers} {
			for j := range cs {
				defaultContainer(&cs[j])
			}
		}
	}
	for i := range s.Services {
		svc := &s.Services[i]
		if svc.Type == "" {
			svc.Type = "ClusterIP"
		}
		for j := range svc.Ports {
			p := &svc.Ports[j]
			if p.Protocol == "" {
				p.Protocol = "TCP"
			}
			if p.TargetPort == "" {
				p.TargetPort = strconv.Itoa(int(p.Port))
			}
		}
	}
	for i := range s.HorizontalPodAutoscalers {
		h := &s.HorizontalPodAutoscalers[i]
		if h.MinReplicas == nil {
			one := int32(1)
			h.MinReplicas = &one
		}
	}
	// Cluster mode reads Secrets and ConfigMaps through the metadata API, so
	// it has neither key names nor the Secret type. Compare names only.
	var configMaps []snapshot.ConfigRef
	for _, r := range s.ConfigMaps {
		if cluster && r.Name == "kube-root-ca.crt" {
			continue // published into every namespace by the API server
		}
		configMaps = append(configMaps, snapshot.ConfigRef{ObjectMeta: r.ObjectMeta})
	}
	s.ConfigMaps = configMaps
	for i := range s.Secrets {
		s.Secrets[i] = snapshot.ConfigRef{ObjectMeta: s.Secrets[i].ObjectMeta}
	}
	return s
}

// defaultContainer fills the server defaults of the container fields the
// snapshot records. Fields it does not record (successThreshold,
// terminationMessagePath, ...) need none, and initialDelaySeconds defaults
// to 0, which is already how the snapshot leaves it unset.
func defaultContainer(c *snapshot.Container) {
	if c.ImagePullPolicy == "" {
		c.ImagePullPolicy = "IfNotPresent"
		if pullsAlways(c.Image) {
			c.ImagePullPolicy = "Always"
		}
	}
	for i := range c.Ports {
		if c.Ports[i].Protocol == "" {
			c.Ports[i].Protocol = "TCP"
		}
	}
	for _, p := range []*snapshot.Probe{c.ReadinessProbe, c.LivenessProbe, c.StartupProbe} {
		if p == nil {
			continue
		}
		if p.PeriodSeconds == 0 {
			p.PeriodSeconds = 10
		}
		if p.TimeoutSeconds == 0 {
			p.TimeoutSeconds = 1
		}
		if p.FailureThreshold == 0 {
			p.FailureThreshold = 3
		}
	}
}

// pullsAlways reports whether Kubernetes defaults imagePullPolicy to
// Always for an image: no tag, or :latest. A digest counts as pinned.
func pullsAlways(image string) bool {
	if strings.Contains(image, "@") {
		return false
	}
	name := image[strings.LastIndex(image, "/")+1:]
	i := strings.LastIndex(name, ":")
	return i < 0 || name[i+1:] == "latest"
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// decodeManifests turns the YAML files in dir into typed objects for the
// fake clientset, and Secrets and ConfigMaps into PartialObjectMetadata for
// the fake metadata client, which is all the cluster collector reads of them.
func decodeManifests(t *testing.T, dir string) (typed, meta []runtime.Object) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decode := scheme.Codecs.UniversalDeserializer().Decode
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		docs := k8syaml.NewYAMLReader(bufio.NewReader(fh))
		for {
			doc, err := docs.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}
			obj, gvk, err := decode(doc, nil, nil)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if gvk.Kind == "Secret" || gvk.Kind == "ConfigMap" {
				m := obj.(metav1.Object)
				meta = append(meta, &metav1.PartialObjectMetadata{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: gvk.Kind},
					ObjectMeta: metav1.ObjectMeta{Namespace: m.GetNamespace(), Name: m.GetName(), Labels: m.GetLabels()},
				})
				continue
			}
			typed = append(typed, obj)
		}
		_ = fh.Close()
	}
	return typed, meta
}

func metadataScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}
