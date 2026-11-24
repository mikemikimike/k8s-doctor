package checks

import (
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// hostPath flags hostPath volumes, which mount a directory of the node into
// the pod.
type hostPath struct{ meta }

func newHostPath() rules.Rule {
	return hostPath{meta{
		id:       "security-host-path",
		category: CategorySecurity,
		severity: rules.SeverityHigh,
		modes:    rules.AllModes,
		description: "The pod mounts a directory from the node. Depending on the path, that exposes host " +
			"credentials, the container runtime socket or other pods' data, and it ties the pod to whatever " +
			"happens to be on that node. Use a ConfigMap, Secret, emptyDir or PersistentVolumeClaim instead.",
	}}
}

func (hostPath) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		for _, v := range w.Template.Spec.Volumes {
			if v.Type != "hostPath" {
				continue
			}
			out = append(out, rules.Finding{
				Resource:  w.Ref(),
				FieldPath: rules.PodSpecField(w, "volumes[name="+v.Name+"].hostPath"),
				Message:   "volume " + v.Name + " mounts host path " + v.HostPath,
				Evidence:  map[string]string{"volume": v.Name, "hostPath": v.HostPath},
			})
		}
	}
	return out
}

// hostNamespace flags pods that share the node's network, PID or IPC
// namespace.
type hostNamespace struct{ meta }

func newHostNamespace() rules.Rule {
	return hostNamespace{meta{
		id:       "security-host-namespace",
		category: CategorySecurity,
		severity: rules.SeverityHigh,
		modes:    rules.AllModes,
		description: "The pod shares a namespace with the node. hostNetwork exposes the node's interfaces and " +
			"bypasses NetworkPolicies; hostPID lets it see and signal every process on the node; hostIPC lets " +
			"it read other processes' shared memory.",
	}}
}

func (hostNamespace) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		spec := &w.Template.Spec
		for _, ns := range []struct {
			field string
			on    bool
		}{{"hostNetwork", spec.HostNetwork}, {"hostPID", spec.HostPID}, {"hostIPC", spec.HostIPC}} {
			if !ns.on {
				continue
			}
			out = append(out, rules.Finding{
				Resource:  w.Ref(),
				FieldPath: rules.PodSpecField(w, ns.field),
				Message:   "pod shares the node's namespace (" + ns.field + ": true)",
				Evidence:  map[string]string{ns.field: "true"},
			})
		}
	}
	return out
}

// defaultNamespace flags workloads in the default namespace. In manifest
// mode that includes manifests with no namespace that no overlay sets,
// which is where they would land.
type defaultNamespace struct{ meta }

func newDefaultNamespace() rules.Rule {
	return defaultNamespace{meta{
		id:       "namespace-default",
		category: CategoryHygiene,
		severity: rules.SeverityLow,
		modes:    rules.AllModes,
		description: "The workload runs in the default namespace, where it shares quotas, RBAC and network " +
			"policies with anything else deployed without a namespace. Give each application its own namespace.",
	}}
}

func (defaultNamespace) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		if w.Namespace != "default" {
			continue
		}
		out = append(out, rules.Finding{
			Resource:  w.Ref(),
			FieldPath: "metadata.namespace",
			Message:   w.Kind + " " + w.Name + " is in the default namespace",
			Evidence:  map[string]string{"namespace": "default"},
		})
	}
	return out
}
