package collector

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// kustomizationFiles are the file names kustomize recognizes.
var kustomizationFiles = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// Manifests collects a snapshot from a directory without a cluster.
//
// If dir holds a kustomization it is built in-process with the kustomize
// krusty API, exactly as `kustomize build dir` would, and the overlay's
// namePrefix, nameSuffix and namespace are recorded for drift matching.
// Otherwise every .yaml, .yml and .json file under dir is parsed as plain
// manifests. Helm charts are out of scope; render them first.
//
// Objects of kinds the snapshot does not model are ignored. Secret values
// are discarded while parsing and never reach the snapshot.
func Manifests(dir string) (*snapshot.Snapshot, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	snap := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Mode:          snapshot.ModeManifest,
		Source:        snapshot.Source{Path: filepath.ToSlash(filepath.Clean(dir))},
	}

	var docs [][]byte
	if kfile := findKustomization(dir); kfile != "" {
		overlay, err := readOverlay(dir, kfile)
		if err != nil {
			return nil, err
		}
		snap.Source.Overlay = overlay
		if docs, err = buildKustomization(dir); err != nil {
			return nil, err
		}
	} else if docs, err = readPlainDir(dir); err != nil {
		return nil, err
	}

	b := newBuilder()
	for _, doc := range docs {
		if err := b.addDocument(doc); err != nil {
			return nil, err
		}
	}
	snap.Spec = b.spec()
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	return snap, nil
}

func findKustomization(dir string) string {
	for _, name := range kustomizationFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name
		}
	}
	return ""
}

// readOverlay reads the identity-changing fields of a kustomization.
func readOverlay(dir, kfile string) (*snapshot.Overlay, error) {
	data, err := os.ReadFile(filepath.Join(dir, kfile))
	if err != nil {
		return nil, err
	}
	var k struct {
		NamePrefix string `json:"namePrefix"`
		NameSuffix string `json:"nameSuffix"`
		Namespace  string `json:"namespace"`
	}
	if err := yaml.Unmarshal(data, &k); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, kfile), err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &snapshot.Overlay{
		Name:       filepath.Base(abs),
		NamePrefix: k.NamePrefix,
		NameSuffix: k.NameSuffix,
		Namespace:  k.Namespace,
	}, nil
}

func buildKustomization(dir string) ([][]byte, error) {
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	resMap, err := k.Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	out, err := resMap.AsYaml()
	if err != nil {
		return nil, err
	}
	return splitDocuments(out)
}

// readPlainDir parses every manifest file under dir in lexical order. A
// nested kustomization is an error: its resources would be read raw,
// without the overlay applied, which is never what the user meant.
func readPlainDir(dir string) ([][]byte, error) {
	var docs [][]byte
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if path != dir && findKustomization(path) != "" {
				return fmt.Errorf("%s contains a kustomization; pass that directory (or an overlay) to analyze it built", path)
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileDocs, err := splitDocuments(data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		docs = append(docs, fileDocs...)
		return nil
	})
	return docs, err
}

// splitDocuments splits a multi-document YAML (or a JSON) stream and
// converts each non-empty document to JSON.
func splitDocuments(data []byte) ([][]byte, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs [][]byte
	for {
		raw, err := r.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		js, err := yaml.YAMLToJSON(raw)
		if err != nil {
			return nil, err
		}
		if t := bytes.TrimSpace(js); len(t) == 0 || bytes.Equal(t, []byte("null")) {
			continue
		}
		docs = append(docs, js)
	}
}

// builder accumulates normalized objects and rejects duplicates.
type builder struct {
	s    snapshot.Spec
	seen map[string]bool
}

func newBuilder() *builder { return &builder{seen: map[string]bool{}} }

func (b *builder) addDocument(doc []byte) error {
	var tm metav1.TypeMeta
	if err := json.Unmarshal(doc, &tm); err != nil {
		return err
	}
	if tm.Kind == "List" {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(doc, &list); err != nil {
			return err
		}
		for _, item := range list.Items {
			if err := b.addDocument(item); err != nil {
				return err
			}
		}
		return nil
	}
	err := b.add(tm, doc)
	if err != nil {
		return fmt.Errorf("%s %s: %w", tm.APIVersion, tm.Kind, err)
	}
	return nil
}

func (b *builder) add(tm metav1.TypeMeta, doc []byte) error {
	group := tm.GroupVersionKind().Group
	switch {
	case group == "apps" && tm.Kind == snapshot.KindDeployment,
		group == "extensions" && tm.Kind == snapshot.KindDeployment:
		var o appsv1.Deployment
		return decodeInto(doc, &o, func() error { return b.addWorkload(Deployment(tm.APIVersion, &o)) })
	case group == "apps" && tm.Kind == snapshot.KindStatefulSet:
		var o appsv1.StatefulSet
		return decodeInto(doc, &o, func() error { return b.addWorkload(StatefulSet(tm.APIVersion, &o)) })
	case (group == "apps" || group == "extensions") && tm.Kind == snapshot.KindDaemonSet:
		var o appsv1.DaemonSet
		return decodeInto(doc, &o, func() error { return b.addWorkload(DaemonSet(tm.APIVersion, &o)) })
	case group == "batch" && tm.Kind == snapshot.KindJob:
		var o batchv1.Job
		return decodeInto(doc, &o, func() error { return b.addWorkload(Job(tm.APIVersion, &o)) })
	case group == "batch" && tm.Kind == snapshot.KindCronJob:
		var o batchv1.CronJob
		return decodeInto(doc, &o, func() error { return b.addWorkload(CronJob(tm.APIVersion, &o)) })
	case group == "" && tm.Kind == "Service":
		var o corev1.Service
		return decodeInto(doc, &o, func() error {
			s := Service(&o)
			return b.addObject("Service", s.Namespace, s.Name, func() { b.s.Services = append(b.s.Services, s) })
		})
	case group == "" && tm.Kind == "Namespace":
		var o corev1.Namespace
		return decodeInto(doc, &o, func() error {
			n := Namespace(&o)
			return b.addObject("Namespace", "", n.Name, func() { b.s.Namespaces = append(b.s.Namespaces, n) })
		})
	case group == "" && tm.Kind == "ConfigMap":
		var o configMapKeys
		return decodeInto(doc, &o, func() error {
			c := configRef(o.ObjectMeta, "", o.keys())
			return b.addObject("ConfigMap", c.Namespace, c.Name, func() { b.s.ConfigMaps = append(b.s.ConfigMaps, c) })
		})
	case group == "" && tm.Kind == "Secret":
		var o secretKeys
		return decodeInto(doc, &o, func() error {
			c := configRef(o.ObjectMeta, o.Type, o.keys())
			return b.addObject("Secret", c.Namespace, c.Name, func() { b.s.Secrets = append(b.s.Secrets, c) })
		})
	case group == "policy" && tm.Kind == "PodDisruptionBudget":
		var o policyv1.PodDisruptionBudget
		return decodeInto(doc, &o, func() error {
			p := PodDisruptionBudget(tm.APIVersion, &o)
			return b.addObject("PodDisruptionBudget", p.Namespace, p.Name, func() {
				b.s.PodDisruptionBudgets = append(b.s.PodDisruptionBudgets, p)
			})
		})
	case group == "autoscaling" && tm.Kind == "HorizontalPodAutoscaler":
		h, err := decodeHPA(tm.APIVersion, doc)
		if err != nil {
			return err
		}
		n := HorizontalPodAutoscaler(tm.APIVersion, h)
		return b.addObject("HorizontalPodAutoscaler", n.Namespace, n.Name, func() {
			b.s.HorizontalPodAutoscalers = append(b.s.HorizontalPodAutoscalers, n)
		})
	default:
		// Kinds the snapshot does not model (Ingress, RBAC, CRDs, ...).
		return nil
	}
}

func decodeInto(doc []byte, into any, then func() error) error {
	if err := json.Unmarshal(doc, into); err != nil {
		return err
	}
	return then()
}

// decodeHPA reads autoscaling/v1 by converting its single CPU target to a
// v2 resource metric; v2 and v2beta2 share the v2 shape.
func decodeHPA(apiVersion string, doc []byte) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	if apiVersion != "autoscaling/v1" {
		var h autoscalingv2.HorizontalPodAutoscaler
		return &h, json.Unmarshal(doc, &h)
	}
	var v1 autoscalingv1.HorizontalPodAutoscaler
	if err := json.Unmarshal(doc, &v1); err != nil {
		return nil, err
	}
	h := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: v1.ObjectMeta}
	h.Spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{
		Kind: v1.Spec.ScaleTargetRef.Kind, Name: v1.Spec.ScaleTargetRef.Name, APIVersion: v1.Spec.ScaleTargetRef.APIVersion,
	}
	h.Spec.MinReplicas, h.Spec.MaxReplicas = v1.Spec.MinReplicas, v1.Spec.MaxReplicas
	if v1.Spec.TargetCPUUtilizationPercentage != nil {
		h.Spec.Metrics = []autoscalingv2.MetricSpec{{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name:   corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: v1.Spec.TargetCPUUtilizationPercentage},
			},
		}}
	}
	return h, nil
}

func (b *builder) addWorkload(w snapshot.Workload) error {
	return b.addObject(w.Kind, w.Namespace, w.Name, func() { b.s.Workloads = append(b.s.Workloads, w) })
}

func (b *builder) addObject(kind, namespace, name string, add func()) error {
	if name == "" {
		return errors.New("metadata.name is required")
	}
	key := snapshot.ResourceRef{Kind: kind, Namespace: namespace, Name: name}.String()
	if b.seen[key] {
		return fmt.Errorf("%s is defined more than once", key)
	}
	b.seen[key] = true
	add()
	return nil
}

// spec returns the accumulated spec with every list sorted by identity, so
// the snapshot does not depend on file or document order.
func (b *builder) spec() snapshot.Spec {
	s := b.s
	sort.Slice(s.Namespaces, func(i, j int) bool { return s.Namespaces[i].Name < s.Namespaces[j].Name })
	sort.Slice(s.Workloads, func(i, j int) bool { return s.Workloads[i].Ref().String() < s.Workloads[j].Ref().String() })
	sortMeta(s.Services, func(x snapshot.Service) snapshot.ObjectMeta { return x.ObjectMeta })
	sortMeta(s.PodDisruptionBudgets, func(x snapshot.PodDisruptionBudget) snapshot.ObjectMeta { return x.ObjectMeta })
	sortMeta(s.HorizontalPodAutoscalers, func(x snapshot.HorizontalPodAutoscaler) snapshot.ObjectMeta { return x.ObjectMeta })
	sortMeta(s.ConfigMaps, func(x snapshot.ConfigRef) snapshot.ObjectMeta { return x.ObjectMeta })
	sortMeta(s.Secrets, func(x snapshot.ConfigRef) snapshot.ObjectMeta { return x.ObjectMeta })
	return s
}

func sortMeta[T any](xs []T, meta func(T) snapshot.ObjectMeta) {
	sort.Slice(xs, func(i, j int) bool {
		a, b := meta(xs[i]), meta(xs[j])
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}

// discard decodes any JSON value and keeps nothing, so Secret and
// ConfigMap values are never held as Go strings.
type discard struct{}

func (*discard) UnmarshalJSON([]byte) error { return nil }

// secretKeys decodes a Secret keeping only metadata, type and key names.
type secretKeys struct {
	metav1.ObjectMeta `json:"metadata"`
	Type              string              `json:"type"`
	Data              map[string]*discard `json:"data"`
	StringData        map[string]*discard `json:"stringData"`
}

func (s *secretKeys) keys() []string { return mergeKeys(s.Data, s.StringData) }

// configMapKeys decodes a ConfigMap keeping only metadata and key names.
type configMapKeys struct {
	metav1.ObjectMeta `json:"metadata"`
	Data              map[string]*discard `json:"data"`
	BinaryData        map[string]*discard `json:"binaryData"`
}

func (c *configMapKeys) keys() []string { return mergeKeys(c.Data, c.BinaryData) }

func mergeKeys(ms ...map[string]*discard) []string {
	set := map[string]bool{}
	for _, m := range ms {
		for k := range m {
			set[k] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
