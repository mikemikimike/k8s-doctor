package checks

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// minHAReplicas is the replica count from which a workload is treated as
// highly available and so needs a PodDisruptionBudget.
const minHAReplicas = 2

// lowestReplicas returns the fewest replicas a Deployment or StatefulSet
// runs: its HPA's minReplicas when one targets it, else its replica count.
func lowestReplicas(s *snapshot.Spec, w *snapshot.Workload) (int32, bool) {
	replicas, ok := w.EffectiveReplicas()
	if !ok {
		return 0, false
	}
	if hpa := s.FindHPAFor(w); hpa != nil {
		if hpa.MinReplicas == nil {
			return 1, true
		}
		return *hpa.MinReplicas, true
	}
	return replicas, true
}

// selects reports whether a PDB's selector matches a pod template's labels.
// policy/v1 treats an empty selector as selecting every pod in the
// namespace. The snapshot keeps matchLabels only, so a PDB that uses only
// matchExpressions also looks empty and counts as a match: that can hide a
// missing PDB, never invent one.
func selects(pdb *snapshot.PodDisruptionBudget, w *snapshot.Workload) bool {
	if pdb.Namespace != w.Namespace {
		return false
	}
	for k, v := range pdb.Selector {
		if w.Template.Labels[k] != v {
			return false
		}
	}
	return true
}

// pdbMissing flags Deployments and StatefulSets that run at least two
// replicas but have no PodDisruptionBudget, so a node drain can evict all of
// them at once.
type pdbMissing struct{ meta }

func newPDBMissing() rules.Rule {
	return pdbMissing{meta{
		id:       "pdb-missing",
		category: CategoryAvailability,
		severity: rules.SeverityMedium,
		modes:    rules.AllModes,
		description: "The workload runs several replicas for availability, but no PodDisruptionBudget selects " +
			"its pods. A node drain, cluster upgrade or autoscaler scale-down may then evict every replica at " +
			"the same time.",
	}}
}

func (pdbMissing) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		if w.Kind != snapshot.KindDeployment && w.Kind != snapshot.KindStatefulSet {
			continue
		}
		replicas, _ := lowestReplicas(&s.Spec, w)
		if replicas < minHAReplicas {
			continue
		}
		covered := false
		for j := range s.Spec.PodDisruptionBudgets {
			if selects(&s.Spec.PodDisruptionBudgets[j], w) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		out = append(out, rules.Finding{
			Resource: w.Ref(),
			// The fix is a PDB matching these pod labels.
			FieldPath: "spec.selector",
			Message:   w.Kind + " " + w.Name + " runs at least " + strconv.Itoa(int(replicas)) + " replicas with no PodDisruptionBudget",
			Evidence:  map[string]string{"replicas": strconv.Itoa(int(replicas)), "podLabels": labelString(w.Template.Labels)},
		})
	}
	return out
}

// pdbBlocksDrain flags PodDisruptionBudgets that never allow an eviction:
// maxUnavailable of 0, or minAvailable of 100% or at least the replica count
// of every workload they select. Node drains then hang until someone
// deletes the PDB by hand.
type pdbBlocksDrain struct{ meta }

func newPDBBlocksDrain() rules.Rule {
	return pdbBlocksDrain{meta{
		id:       "pdb-blocks-drain",
		category: CategoryAvailability,
		severity: rules.SeverityHigh,
		modes:    rules.AllModes,
		description: "The PodDisruptionBudget allows no voluntary disruption, so draining a node that runs " +
			"one of its pods blocks indefinitely. Cluster upgrades and node replacements stall until the PDB " +
			"is edited or deleted by hand.",
	}}
}

func (pdbBlocksDrain) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	for i := range s.Spec.PodDisruptionBudgets {
		p := &s.Spec.PodDisruptionBudgets[i]
		ref := snapshot.ResourceRef{Kind: "PodDisruptionBudget", Namespace: p.Namespace, Name: p.Name}
		switch {
		case p.MaxUnavailable == "0" || p.MaxUnavailable == "0%":
			out = append(out, rules.Finding{
				Resource:  ref,
				FieldPath: "spec.maxUnavailable",
				Message:   "PodDisruptionBudget " + p.Name + " sets maxUnavailable to " + p.MaxUnavailable,
				Evidence:  map[string]string{"maxUnavailable": p.MaxUnavailable},
			})
		case p.MinAvailable == "100%":
			out = append(out, rules.Finding{
				Resource:  ref,
				FieldPath: "spec.minAvailable",
				Message:   "PodDisruptionBudget " + p.Name + " requires 100% of pods to stay available",
				Evidence:  map[string]string{"minAvailable": p.MinAvailable},
			})
		case p.MinAvailable != "" && !strings.HasSuffix(p.MinAvailable, "%"):
			if f, ok := minAvailableBlocks(s, p, ref); ok {
				out = append(out, f)
			}
		}
	}
	return out
}

// minAvailableBlocks checks an integer minAvailable against the fewest
// replicas of the workloads the PDB selects. With no selected workload in
// the snapshot, or only ones scaled to zero, there is nothing to compare,
// so it stays quiet.
func minAvailableBlocks(s *snapshot.Snapshot, p *snapshot.PodDisruptionBudget, ref snapshot.ResourceRef) (rules.Finding, bool) {
	minAvailable, err := strconv.Atoi(p.MinAvailable)
	if err != nil {
		return rules.Finding{}, false
	}
	var targets []string
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		replicas, ok := lowestReplicas(&s.Spec, w)
		if !ok || replicas == 0 || !selects(p, w) {
			continue // a workload scaled to zero has no pods to evict
		}
		if int32(minAvailable) < replicas {
			return rules.Finding{}, false
		}
		targets = append(targets, w.Ref().String()+"="+strconv.Itoa(int(replicas)))
	}
	if len(targets) == 0 {
		return rules.Finding{}, false
	}
	return rules.Finding{
		Resource:  ref,
		FieldPath: "spec.minAvailable",
		Message:   "PodDisruptionBudget " + p.Name + " requires " + p.MinAvailable + " pods but its workload runs no more than that",
		Evidence:  map[string]string{"minAvailable": p.MinAvailable, "replicas": strings.Join(targets, ",")},
	}, true
}

func labelString(labels map[string]string) string {
	keys := slices.Sorted(maps.Keys(labels))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}
