package checks

import (
	"testing"

	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// TestRunAsRootMerge pins how pod and container securityContext fields
// combine: each container field overrides its pod field on its own.
func TestRunAsRootMerge(t *testing.T) {
	yes, no := true, false
	root, app := int64(0), int64(1000)
	cases := []struct {
		name     string
		pod      *snapshot.PodSecurityContext
		ctr      *snapshot.ContainerSecurityContext
		init     bool
		wantRoot bool
	}{
		{name: "nothing set", wantRoot: true},
		{name: "pod runAsNonRoot", pod: &snapshot.PodSecurityContext{RunAsNonRoot: &yes}},
		{name: "pod non-zero user", pod: &snapshot.PodSecurityContext{RunAsUser: &app}},
		{name: "runAsNonRoot with pod user 0", pod: &snapshot.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &root}, wantRoot: true},
		{name: "container false over pod true", pod: &snapshot.PodSecurityContext{RunAsNonRoot: &yes}, ctr: &snapshot.ContainerSecurityContext{RunAsNonRoot: &no}, wantRoot: true},
		{name: "container user over pod user 0", pod: &snapshot.PodSecurityContext{RunAsUser: &root}, ctr: &snapshot.ContainerSecurityContext{RunAsUser: &app}},
		{name: "container user 0 over pod non-root", pod: &snapshot.PodSecurityContext{RunAsNonRoot: &yes}, ctr: &snapshot.ContainerSecurityContext{RunAsUser: &root}, wantRoot: true},
		{name: "init container inherits pod", pod: &snapshot.PodSecurityContext{RunAsNonRoot: &yes}, init: true},
		{name: "init container unset", init: true, wantRoot: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := snapshot.Workload{Kind: snapshot.KindDeployment, ObjectMeta: snapshot.ObjectMeta{Namespace: "shop", Name: "api"}}
			w.Template.Spec.SecurityContext = c.pod
			ctr := []snapshot.Container{{Name: "api", Image: "app:1", SecurityContext: c.ctr}}
			if c.init {
				w.Template.Spec.InitContainers = ctr
			} else {
				w.Template.Spec.Containers = ctr
			}
			s := &snapshot.Snapshot{Spec: snapshot.Spec{Workloads: []snapshot.Workload{w}}}
			got := len(newRunAsRoot().Evaluate(s)) == 1
			if got != c.wantRoot {
				t.Errorf("flagged = %v, want %v", got, c.wantRoot)
			}
		})
	}
}
