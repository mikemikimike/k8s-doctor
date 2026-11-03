package collector_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	"sigs.k8s.io/yaml"

	"github.com/rbalukja15/k8s-doctor/internal/collector"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

func ptr[T any](v T) *T { return &v }

func ownedBy(kind, name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: ptr(true)}}
}

func meta(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name}
}

func podTemplate(app string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  app,
			Image: "ghcr.io/acme/" + app + ":1.0.0",
			Env:   []corev1.EnvVar{{Name: "TOKEN", Value: "literal-env-secret"}},
		}}},
	}
}

func fakeClients(t *testing.T) (*collector.Clients, *fake.Clientset) {
	t.Helper()
	finished := metav1.NewTime(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC))
	typed := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&appsv1.Deployment{ObjectMeta: meta("shop", "api"), Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(2)), Template: podTemplate("api")}},
		&appsv1.Deployment{ObjectMeta: meta("kube-system", "coredns"), Spec: appsv1.DeploymentSpec{Template: podTemplate("coredns")}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-7d9f", OwnerReferences: ownedBy("Deployment", "api")}},
		&batchv1.CronJob{ObjectMeta: meta("shop", "nightly"), Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: podTemplate("nightly")}}}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-2900", OwnerReferences: ownedBy("CronJob", "nightly")}, Spec: batchv1.JobSpec{Template: podTemplate("nightly")}},
		&batchv1.Job{ObjectMeta: meta("shop", "migrate"), Spec: batchv1.JobSpec{Template: podTemplate("migrate")}},
		&corev1.Service{ObjectMeta: meta("shop", "api"), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"}}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-7d9f-x2k9p", OwnerReferences: ownedBy("ReplicaSet", "api-7d9f")},
			Spec:       corev1.PodSpec{NodeName: "worker-1"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "migrate", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
			}}, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "api", RestartCount: 7,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: finished}},
			}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-2900-abcde", OwnerReferences: ownedBy("Job", "nightly-2900")},
			Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
		&corev1.Event{
			ObjectMeta:     meta("shop", "api-7d9f-x2k9p.1"),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "api-7d9f-x2k9p"},
			Type:           "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: 12,
			LastTimestamp: finished,
		},
		&corev1.Event{
			ObjectMeta:     meta("shop", "api-7d9f.1"),
			InvolvedObject: corev1.ObjectReference{Kind: "ReplicaSet", Namespace: "shop", Name: "api-7d9f"},
			Type:           "Warning", Reason: "FailedCreate", Message: "ReplicaSet could not create a pod",
			LastTimestamp: finished,
		},
	)

	pom := func(kind, ns, name string) runtime.Object {
		return &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: kind}, ObjectMeta: meta(ns, name)}
	}
	md := metadatafake.NewSimpleMetadataClient(metadataScheme(t), pom("Secret", "shop", "api-db"), pom("ConfigMap", "shop", "api-config"))
	return &collector.Clients{Typed: typed, Metadata: md, ContextName: "kind-test"}, typed
}

func TestCluster(t *testing.T) {
	c, typed := fakeClients(t)
	s, err := collector.Cluster(context.Background(), c, collector.ClusterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != snapshot.ModeCluster || s.Runtime == nil || s.Source.Cluster != "kind-test" {
		t.Fatalf("unexpected header: mode %s source %+v", s.Mode, s.Source)
	}

	var refs []string
	for i := range s.Spec.Workloads {
		refs = append(refs, s.Spec.Workloads[i].Ref().String())
	}
	want := []string{"CronJob/shop/nightly", "Deployment/shop/api", "Job/shop/migrate"}
	if !slices.Equal(refs, want) {
		t.Errorf("workloads = %v, want %v (kube-system skipped, CronJob runs folded into the CronJob)", refs, want)
	}
	if len(s.Spec.Secrets) != 1 || s.Spec.Secrets[0].Name != "api-db" || len(s.Spec.ConfigMaps) != 1 {
		t.Errorf("secrets/configmaps = %+v / %+v", s.Spec.Secrets, s.Spec.ConfigMaps)
	}

	owners := map[string]string{}
	for _, p := range s.Runtime.Pods {
		owners[p.Name] = p.Owner.String()
	}
	if owners["api-7d9f-x2k9p"] != "Deployment/shop/api" || owners["nightly-2900-abcde"] != "CronJob/shop/nightly" {
		t.Errorf("pod owners not resolved to top-level workloads: %v", owners)
	}
	if init := s.Runtime.Pods[0].Containers[0]; !init.Init || init.Name != "migrate" || init.Reason != "Completed" {
		t.Errorf("init container status = %+v", init)
	}
	cs := s.Runtime.Pods[0].Containers[1]
	if cs.Init || cs.RestartCount != 7 || cs.Reason != "CrashLoopBackOff" || cs.LastTermination == nil || cs.LastTermination.Reason != "OOMKilled" {
		t.Errorf("container status = %+v", cs)
	}
	if len(s.Runtime.Events) != 2 {
		t.Errorf("events = %+v", s.Runtime.Events)
	} else {
		var replicaSetEvent *snapshot.Event
		for i := range s.Runtime.Events {
			if s.Runtime.Events[i].InvolvedObject.Kind == "ReplicaSet" {
				replicaSetEvent = &s.Runtime.Events[i]
			}
		}
		wantOwner := snapshot.ResourceRef{Kind: snapshot.KindDeployment, Namespace: "shop", Name: "api"}
		if replicaSetEvent == nil || replicaSetEvent.Owner == nil || *replicaSetEvent.Owner != wantOwner {
			t.Errorf("ReplicaSet event owner = %+v, want %s", replicaSetEvent, wantOwner)
		}
		if s.Runtime.Events[0].Count != 12 || s.Runtime.Events[0].LastSeen.IsZero() {
			t.Errorf("Pod event = %+v", s.Runtime.Events[0])
		}
	}

	raw, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "literal-env-secret") {
		t.Error("an env literal reached the snapshot")
	}

	// The collector must only use the verbs RequiredResources declares.
	allowed := map[string][]string{}
	for _, r := range collector.RequiredResources {
		allowed[r.Resource] = r.Verbs
	}
	for _, a := range typed.Actions() {
		if res := a.GetResource().Resource; !slices.Contains(allowed[res], a.GetVerb()) {
			t.Errorf("collector issued a %q request on %s", a.GetVerb(), res)
		}
	}
}

func TestClusterSkipSecrets(t *testing.T) {
	c, _ := fakeClients(t)
	s, err := collector.Cluster(context.Background(), c, collector.ClusterOptions{SkipSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Spec.Secrets) != 0 || len(s.Spec.ConfigMaps) != 1 {
		t.Errorf("secrets/configmaps = %+v / %+v", s.Spec.Secrets, s.Spec.ConfigMaps)
	}
}

func TestClusterNamespaceFilter(t *testing.T) {
	c, _ := fakeClients(t)
	s, err := collector.Cluster(context.Background(), c, collector.ClusterOptions{Namespaces: []string{"kube-system"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Spec.Workloads) != 1 || s.Spec.Workloads[0].Name != "coredns" {
		t.Errorf("namespace filter ignored: %+v", s.Spec.Workloads)
	}
	if s, err := collector.Cluster(context.Background(), c, collector.ClusterOptions{Namespaces: []string{"shop", "shop"}}); err != nil || len(s.Spec.Namespaces) != 1 {
		t.Errorf("a namespace named twice: err %v, namespaces %+v", err, s)
	}
	if _, err := collector.Cluster(context.Background(), c, collector.ClusterOptions{Namespaces: []string{"nope"}}); err == nil {
		t.Error("expected an error for a missing namespace")
	}
}

// TestReadOnlyRBAC keeps deploy/rbac in sync with the collector: each role
// grants exactly the verbs the collector uses on exactly the resources it
// reads, with no wildcards.
func TestReadOnlyRBAC(t *testing.T) {
	want := map[string][]string{}
	for _, r := range collector.RequiredResources {
		want[r.Group+"/"+r.Resource] = r.Verbs
	}
	t.Run("ClusterRole", func(t *testing.T) {
		assertRules(t, loadRules(t, "readonly-clusterrole.yaml", "ClusterRole"), want)
	})
	t.Run("namespaced Role", func(t *testing.T) {
		// --namespace --skip-secrets: no Secrets, and the namespace is read
		// with get only.
		ns := maps.Clone(want)
		delete(ns, "/secrets")
		ns["/namespaces"] = []string{"get"}
		assertRules(t, loadRules(t, "readonly-role-namespaced.yaml", "Role"), ns)
	})
}

func loadRules(t *testing.T, file, kind string) []rbacv1.PolicyRule {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../deploy/rbac", file))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(data), "\n---\n") {
		var r struct {
			Kind  string              `json:"kind"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == kind {
			return r.Rules
		}
	}
	t.Fatalf("no %s in %s", kind, file)
	return nil
}

func assertRules(t *testing.T, rules []rbacv1.PolicyRule, want map[string][]string) {
	t.Helper()
	got := map[string][]string{}
	for _, rule := range rules {
		for _, g := range rule.APIGroups {
			for _, r := range rule.Resources {
				if g == "*" || r == "*" || slices.Contains(rule.Verbs, "*") {
					t.Errorf("wildcard in rule %+v", rule)
				}
				got[g+"/"+r] = append(got[g+"/"+r], rule.Verbs...)
			}
		}
	}
	for k, verbs := range got {
		slices.Sort(verbs)
		w := slices.Clone(want[k])
		slices.Sort(w)
		if !slices.Equal(verbs, w) {
			t.Errorf("%s: role grants %v, collector uses %v", k, verbs, w)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: collector reads it but the role does not grant it", k)
		}
	}
}
