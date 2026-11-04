package collector_test

import (
	"context"
	"encoding/json"
	"os"
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
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
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

	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pom := func(kind, ns, name string) runtime.Object {
		return &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: kind}, ObjectMeta: meta(ns, name)}
	}
	md := metadatafake.NewSimpleMetadataClient(scheme, pom("Secret", "shop", "api-db"), pom("ConfigMap", "shop", "api-config"))
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
	cs := s.Runtime.Pods[0].Containers[0]
	if cs.RestartCount != 7 || cs.Reason != "CrashLoopBackOff" || cs.LastTermination == nil || cs.LastTermination.Reason != "OOMKilled" {
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
	jsonSnapshot, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := snapshot.Decode(strings.NewReader(string(jsonSnapshot)))
	if err != nil {
		t.Fatalf("decode collected snapshot: %v", err)
	}
	if decoded.SchemaVersion != snapshot.SchemaVersion {
		t.Fatalf("schema version = %q, want %q", decoded.SchemaVersion, snapshot.SchemaVersion)
	}
	for _, event := range decoded.Runtime.Events {
		if event.InvolvedObject.Kind == "ReplicaSet" && (event.Owner == nil || event.Owner.String() != "Deployment/shop/api") {
			t.Fatalf("serialized ReplicaSet event owner = %+v", event.Owner)
		}
	}

	raw, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "literal-env-secret") {
		t.Error("an env literal reached the snapshot")
	}

	// The collector must only ever read.
	for _, a := range typed.Actions() {
		if v := a.GetVerb(); v != "list" && v != "get" {
			t.Errorf("collector issued a %q request on %s", v, a.GetResource().Resource)
		}
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
	if _, err := collector.Cluster(context.Background(), c, collector.ClusterOptions{Namespaces: []string{"nope"}}); err == nil {
		t.Error("expected an error for a missing namespace")
	}
}

// TestReadOnlyClusterRole keeps deploy/rbac in sync with the collector: the
// ClusterRole grants only get/list/watch, and covers every resource read.
func TestReadOnlyClusterRole(t *testing.T) {
	data, err := os.ReadFile("../../deploy/rbac/readonly-clusterrole.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var role *rbacv1.ClusterRole
	for _, doc := range strings.Split(string(data), "\n---\n") {
		var r rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == "ClusterRole" {
			role = &r
		}
	}
	if role == nil {
		t.Fatal("no ClusterRole in deploy/rbac/readonly-clusterrole.yaml")
	}
	granted := map[collector.ReadOnlyResource]bool{}
	for _, rule := range role.Rules {
		for _, v := range rule.Verbs {
			if v != "get" && v != "list" && v != "watch" {
				t.Errorf("ClusterRole grants write verb %q", v)
			}
		}
		for _, g := range rule.APIGroups {
			for _, r := range rule.Resources {
				if g == "*" || r == "*" {
					t.Errorf("ClusterRole uses a wildcard (%q/%q)", g, r)
				}
				granted[collector.ReadOnlyResource{Group: g, Resource: r}] = true
			}
		}
	}
	for _, r := range collector.RequiredResources {
		if !granted[r] {
			t.Errorf("collector reads %s/%s but the ClusterRole does not grant it", r.Group, r.Resource)
		}
		delete(granted, r)
	}
	for r := range granted {
		t.Errorf("ClusterRole grants %s/%s, which the collector never reads", r.Group, r.Resource)
	}
}
