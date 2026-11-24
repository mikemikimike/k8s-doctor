package checks

import (
	"strconv"

	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// spreadMissing flags Deployments and StatefulSets running at least two
// replicas with neither pod anti-affinity nor topology spread constraints.
// It is low severity because kube-scheduler's default constraints already
// prefer spreading across nodes and zones; this only makes it explicit
// and lets it be required.
type spreadMissing struct{ meta }

func newSpreadMissing() rules.Rule {
	return spreadMissing{meta{
		id:       "spread-missing",
		category: CategoryAvailability,
		severity: rules.SeverityLow,
		modes:    rules.AllModes,
		description: "The workload runs several replicas but declares no pod anti-affinity or topology spread " +
			"constraint, so nothing guarantees the replicas land on different nodes or zones. One node or zone " +
			"failure can then take all of them down.",
	}}
}

func (spreadMissing) Evaluate(s *snapshot.Snapshot) []rules.Finding {
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
		spec := &w.Template.Spec
		if len(spec.TopologySpreadConstraints) > 0 {
			continue
		}
		if a := spec.Affinity; a != nil && a.PodAntiAffinity != nil &&
			len(a.PodAntiAffinity.RequiredTopologyKeys)+len(a.PodAntiAffinity.PreferredTopologyKeys) > 0 {
			continue
		}
		out = append(out, rules.Finding{
			Resource:  w.Ref(),
			FieldPath: rules.PodSpecField(w, "topologySpreadConstraints"),
			Message:   w.Kind + " " + w.Name + " runs at least " + strconv.Itoa(int(replicas)) + " replicas with no anti-affinity or topology spread",
			Evidence:  map[string]string{"replicas": strconv.Itoa(int(replicas))},
		})
	}
	return out
}

// hpaMissingRequests flags HPAs that scale on CPU or memory utilization
// while a container of the target workload has no request for that
// resource. Utilization is usage divided by request, so the HPA cannot
// compute it and never scales.
type hpaMissingRequests struct{ meta }

func newHPAMissingRequests() rules.Rule {
	return hpaMissingRequests{meta{
		id:       "hpa-missing-requests",
		category: CategoryResources,
		severity: rules.SeverityHigh,
		modes:    rules.AllModes,
		description: "The HorizontalPodAutoscaler scales on resource utilization, which is usage as a share of " +
			"the request, but a container of its target has no request for that resource. The HPA reports " +
			"the metric as unknown and never scales the workload. A default request from a namespace " +
			"LimitRange is not visible to this check; if one applies, the finding is a false positive.",
	}}
}

func (hpaMissingRequests) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		// Every HPA that targets the workload, not just the first: two HPAs
		// on one target is a misconfiguration, but each can still be broken.
		for k := range s.Spec.HorizontalPodAutoscalers {
			if h := &s.Spec.HorizontalPodAutoscalers[k]; targets(h, w) {
				out = append(out, missingRequests(h, w)...)
			}
		}
	}
	return out
}

func targets(h *snapshot.HorizontalPodAutoscaler, w *snapshot.Workload) bool {
	return h.Namespace == w.Namespace && h.TargetRef.Kind == w.Kind && h.TargetRef.Name == w.Name
}

func missingRequests(hpa *snapshot.HorizontalPodAutoscaler, w *snapshot.Workload) []rules.Finding {
	var out []rules.Finding
	for _, m := range hpa.Metrics {
		if m.Type != "Resource" || m.Target != "Utilization" || (m.Resource != "cpu" && m.Resource != "memory") {
			continue
		}
		for j := range w.Template.Spec.Containers {
			c := &w.Template.Spec.Containers[j]
			if hasQuantity(c.Resources.Requests, m.Resource) || hasQuantity(c.Resources.Limits, m.Resource) {
				continue
			}
			out = append(out, rules.Finding{
				Resource:  w.Ref(),
				FieldPath: rules.ContainerPath(w, c) + ".resources.requests." + m.Resource,
				Message: "HPA " + hpa.Name + " scales on " + m.Resource + " utilization but container " + c.Name +
					" has no " + m.Resource + " request",
				Evidence: map[string]string{"hpa": hpa.Name, "metric": m.Resource, "container": c.Name},
			})
		}
	}
	return out
}
