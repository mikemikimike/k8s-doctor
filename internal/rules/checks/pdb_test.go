package checks

import (
	"testing"

	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// A workload scaled to zero has no pods to protect, so no minAvailable can
// block a drain of it.
func TestPDBBlocksDrainScaledToZero(t *testing.T) {
	zero := int32(0)
	for _, minAvailable := range []string{"0", "1"} {
		s := &snapshot.Snapshot{Spec: snapshot.Spec{
			Workloads: []snapshot.Workload{{
				Kind: snapshot.KindDeployment, ObjectMeta: snapshot.ObjectMeta{Namespace: "staging", Name: "api"},
				Replicas: &zero, Template: snapshot.PodTemplate{Labels: map[string]string{"app": "api"}},
			}},
			PodDisruptionBudgets: []snapshot.PodDisruptionBudget{{
				ObjectMeta: snapshot.ObjectMeta{Namespace: "staging", Name: "api"},
				Selector:   map[string]string{"app": "api"}, MinAvailable: minAvailable,
			}},
		}}
		if f := newPDBBlocksDrain().Evaluate(s); len(f) != 0 {
			t.Errorf("minAvailable %s on a workload scaled to zero: got %v, want no finding", minAvailable, f)
		}
	}
}
