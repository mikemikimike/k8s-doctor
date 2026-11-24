package checks

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

func runtimeSnapshot(pods ...snapshot.Pod) *snapshot.Snapshot {
	return &snapshot.Snapshot{Runtime: &snapshot.Runtime{Pods: pods}}
}

func barePod(name string, cs snapshot.ContainerStatus) snapshot.Pod {
	return snapshot.Pod{ObjectMeta: snapshot.ObjectMeta{Namespace: "shop", Name: name}, Containers: []snapshot.ContainerStatus{cs}}
}

func TestCrashloopingRestartCount(t *testing.T) {
	cases := []struct {
		name string
		cs   snapshot.ContainerStatus
		want bool
	}{
		{"backoff", snapshot.ContainerStatus{Name: "c", State: "waiting", Reason: "CrashLoopBackOff", RestartCount: 1}, true},
		{"many restarts, not ready", snapshot.ContainerStatus{Name: "c", State: "running", RestartCount: 9}, true},
		{"many restarts, ready again", snapshot.ContainerStatus{Name: "c", State: "running", Ready: true, RestartCount: 9}, false},
		{"few restarts, not ready", snapshot.ContainerStatus{Name: "c", State: "running", RestartCount: 4}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := len(newPodCrashlooping().Evaluate(runtimeSnapshot(barePod("p", c.cs)))) == 1
			if got != c.want {
				t.Errorf("flagged = %v, want %v", got, c.want)
			}
		})
	}
}

// The snapshot has no collection time to judge recency against, so the
// finding carries when the OOM kill happened and the reader decides.
func TestOOMKilledEvidenceKeepsWhen(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	cs := snapshot.ContainerStatus{
		Name: "c", State: "running", Ready: true, RestartCount: 1,
		LastTermination: &snapshot.Termination{Reason: "OOMKilled", ExitCode: 137, FinishedAt: at},
	}
	f := newContainerOOMKilled().Evaluate(runtimeSnapshot(barePod("p", cs)))
	if len(f) != 1 {
		t.Fatalf("got %d findings, want 1", len(f))
	}
	if got := f[0].Evidence["lastOOMKilledAt"]; got != "2026-11-02T09:00:00Z" {
		t.Errorf("lastOOMKilledAt = %q, want the termination time", got)
	}
}

func TestRuntimeEvidenceCapsPodNames(t *testing.T) {
	var pods []snapshot.Pod
	for i := range 12 {
		p := barePod("api-"+strconv.Itoa(i), snapshot.ContainerStatus{Name: "c", State: "waiting", Reason: "CrashLoopBackOff"})
		p.Owner = &snapshot.ResourceRef{Kind: "Deployment", Namespace: "shop", Name: "api"}
		pods = append(pods, p)
	}
	s := runtimeSnapshot(pods...)
	s.Spec.Workloads = []snapshot.Workload{{
		Kind: snapshot.KindDeployment, ObjectMeta: snapshot.ObjectMeta{Namespace: "shop", Name: "api"},
		Template: snapshot.PodTemplate{Spec: snapshot.PodSpec{Containers: []snapshot.Container{{Name: "c"}}}},
	}}
	f := newPodCrashlooping().Evaluate(s)
	if len(f) != 1 {
		t.Fatalf("got %d findings, want 1", len(f))
	}
	ev := f[0].Evidence
	if want := "api-0,api-1,api-10,api-11,api-2,+7 more"; ev["pods"] != want {
		t.Errorf("pods evidence = %q, want %q", ev["pods"], want)
	}
	if ev["podCount"] != "12" || !strings.Contains(f[0].Message, "12 pod(s)") {
		t.Errorf("podCount = %q, message %q, want the full count of 12", ev["podCount"], f[0].Message)
	}
}

// TestRemovedAPIKeys catches typos that would make an entry never match.
func TestRemovedAPIKeys(t *testing.T) {
	for key, r := range removedAPIs {
		i := strings.LastIndex(key, "/")
		apiVersion, kind := key[:i], key[i+1:]
		version := apiVersion[strings.LastIndex(apiVersion, "/")+1:]
		if !strings.Contains(apiVersion, "/") || !strings.HasPrefix(version, "v") || kind == "" ||
			kind[0] < 'A' || kind[0] > 'Z' {
			t.Errorf("malformed key %q", key)
		}
		if _, err := strconv.ParseFloat(r.removedIn, 64); err != nil || r.replacement == "" {
			t.Errorf("%s: bad entry %+v", key, r)
		}
	}
}

func TestHPAMissingRequestsChecksEveryHPA(t *testing.T) {
	w := snapshot.Workload{
		Kind: snapshot.KindDeployment, ObjectMeta: snapshot.ObjectMeta{Namespace: "shop", Name: "api"},
		Template: snapshot.PodTemplate{Spec: snapshot.PodSpec{Containers: []snapshot.Container{{Name: "api"}}}},
	}
	target := snapshot.ScaleTargetRef{Kind: snapshot.KindDeployment, Name: "api"}
	s := &snapshot.Snapshot{Spec: snapshot.Spec{
		Workloads: []snapshot.Workload{w},
		HorizontalPodAutoscalers: []snapshot.HorizontalPodAutoscaler{
			{
				ObjectMeta: snapshot.ObjectMeta{Namespace: "shop", Name: "by-pods"}, TargetRef: target,
				Metrics: []snapshot.HPAMetric{{Type: "Pods", Resource: "cpu", Target: "AverageValue"}},
			},
			{
				ObjectMeta: snapshot.ObjectMeta{Namespace: "shop", Name: "by-cpu"}, TargetRef: target,
				Metrics: []snapshot.HPAMetric{{Type: "Resource", Resource: "cpu", Target: "Utilization"}},
			},
		},
	}}
	if f := newHPAMissingRequests().Evaluate(s); len(f) != 1 {
		t.Errorf("got %d findings, want 1 from the second HPA", len(f))
	}
}
