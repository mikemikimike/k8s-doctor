package checks

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// crashRestartThreshold is the restart count from which a container that
// is not ready counts as crashlooping even when it is between back-offs at
// collection time. Restart counts never reset, so a ready container is not
// judged by its count: it may have restarted those times over months.
const crashRestartThreshold = 5

// maxEvidencePods caps the pod names a runtime finding lists, so a large
// incident does not produce one enormous evidence string.
const maxEvidencePods = 5

// podEvidence lists the first maxEvidencePods pod names, then "+N more".
func podEvidence(pods []string) string {
	if len(pods) <= maxEvidencePods {
		return strings.Join(pods, ",")
	}
	return strings.Join(pods[:maxEvidencePods], ",") + ",+" + strconv.Itoa(len(pods)-maxEvidencePods) + " more"
}

// containerTarget is where a runtime finding points: the workload that owns
// the pod and the container's field path in its manifest, or the bare pod
// when no owner is known.
type containerTarget struct {
	ref  snapshot.ResourceRef
	path string
}

// runtimeTarget resolves a pod's container to its workload in the spec. A
// pod whose owner is not in the snapshot (a bare pod, or an owner kind the
// snapshot does not model) is reported on the pod itself.
func runtimeTarget(s *snapshot.Snapshot, p *snapshot.Pod, container string) containerTarget {
	if p.Owner != nil {
		for i := range s.Spec.Workloads {
			w := &s.Spec.Workloads[i]
			if w.Ref() != *p.Owner {
				continue
			}
			for j := range w.Template.Spec.InitContainers {
				if c := &w.Template.Spec.InitContainers[j]; c.Name == container {
					return containerTarget{w.Ref(), rules.InitContainerPath(w, c)}
				}
			}
			for j := range w.Template.Spec.Containers {
				if c := &w.Template.Spec.Containers[j]; c.Name == container {
					return containerTarget{w.Ref(), rules.ContainerPath(w, c)}
				}
			}
		}
	}
	return containerTarget{p.Ref(), "spec.containers[name=" + container + "]"}
}

// podGroup collects the pods of one target that show a problem.
type podGroup struct {
	target    containerTarget
	container string
	pods      []string
	maxCount  int32
	reasons   []string
}

// groupPods runs match on every container status and groups the matches by
// target, so a Deployment with five crashing replicas gives one finding.
func groupPods(s *snapshot.Snapshot, match func(*snapshot.ContainerStatus) (reason string, count int32, ok bool)) []*podGroup {
	var order []*podGroup
	groups := map[containerTarget]*podGroup{}
	if s.Runtime == nil {
		return nil
	}
	for i := range s.Runtime.Pods {
		p := &s.Runtime.Pods[i]
		for j := range p.Containers {
			cs := &p.Containers[j]
			reason, count, ok := match(cs)
			if !ok {
				continue
			}
			t := runtimeTarget(s, p, cs.Name)
			g := groups[t]
			if g == nil {
				g = &podGroup{target: t, container: cs.Name}
				groups[t] = g
				order = append(order, g)
			}
			g.pods = append(g.pods, p.Name)
			g.maxCount = max(g.maxCount, count)
			if reason != "" && !slices.Contains(g.reasons, reason) {
				g.reasons = append(g.reasons, reason)
			}
		}
	}
	for _, g := range order {
		slices.Sort(g.pods)
		slices.Sort(g.reasons)
	}
	return order
}

// podCrashlooping flags containers in CrashLoopBackOff, or that restarted
// at least crashRestartThreshold times. It looks at restarts only; pods
// that never become ready after a rollout are a separate check (#54).
type podCrashlooping struct{ meta }

func newPodCrashlooping() rules.Rule {
	return podCrashlooping{meta{
		id:       "pod-crashlooping",
		category: CategoryAvailability,
		severity: rules.SeverityHigh,
		modes:    rules.ClusterOnly,
		description: "The container keeps exiting and being restarted. Each restart backs off longer, up to " +
			"five minutes, so the pod spends most of its time not serving. Check the previous container's " +
			"logs (kubectl logs --previous) and its last exit reason.",
	}}
}

func (podCrashlooping) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	for _, g := range groupPods(s, func(cs *snapshot.ContainerStatus) (string, int32, bool) {
		backoff := cs.State == "waiting" && cs.Reason == "CrashLoopBackOff"
		if !backoff && (cs.Ready || cs.RestartCount < crashRestartThreshold) {
			return "", 0, false
		}
		reason := ""
		if cs.LastTermination != nil {
			reason = cs.LastTermination.Reason
		}
		return reason, cs.RestartCount, true
	}) {
		evidence := map[string]string{
			"container":   g.container,
			"pods":        podEvidence(g.pods),
			"podCount":    strconv.Itoa(len(g.pods)),
			"maxRestarts": strconv.Itoa(int(g.maxCount)),
		}
		if len(g.reasons) > 0 {
			evidence["lastExitReason"] = strings.Join(g.reasons, ",")
		}
		out = append(out, rules.Finding{
			Resource:  g.target.ref,
			FieldPath: g.target.path,
			Message: "container " + g.container + " is crashlooping in " + strconv.Itoa(len(g.pods)) + " pod(s), up to " +
				strconv.Itoa(int(g.maxCount)) + " restarts",
			Evidence: evidence,
		})
	}
	return out
}

// containerOOMKilled flags containers whose current or previous run ended
// with OOMKilled: the memory limit is below what the process uses.
type containerOOMKilled struct{ meta }

func newContainerOOMKilled() rules.Rule {
	return containerOOMKilled{meta{
		id:       "container-oomkilled",
		category: CategoryResources,
		severity: rules.SeverityHigh,
		modes:    rules.ClusterOnly,
		description: "The container was killed for exceeding its memory limit. Either the limit is too low for " +
			"the workload's real peak, or the process leaks memory. Compare the limit with observed usage " +
			"before raising it. Kubernetes keeps the last termination until the container exits again, so " +
			"check lastOOMKilledAt: an old kill on a container that has run fine since may already be fixed.",
	}}
}

func (containerOOMKilled) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	for _, g := range groupPods(s, func(cs *snapshot.ContainerStatus) (string, int32, bool) {
		if cs.State == "terminated" && cs.Reason == "OOMKilled" {
			return "now", cs.RestartCount, true
		}
		if t := cs.LastTermination; t != nil && t.Reason == "OOMKilled" {
			if t.FinishedAt.IsZero() {
				return "", cs.RestartCount, true
			}
			return t.FinishedAt.UTC().Format(time.RFC3339), cs.RestartCount, true
		}
		return "", 0, false
	}) {
		evidence := map[string]string{
			"container": g.container,
			"pods":      podEvidence(g.pods),
			"podCount":  strconv.Itoa(len(g.pods)),
		}
		// reasons holds "now" or RFC 3339 times, sorted, so the last one
		// is the most recent kill.
		if n := len(g.reasons); n > 0 {
			evidence["lastOOMKilledAt"] = g.reasons[n-1]
		}
		out = append(out, rules.Finding{
			Resource:  g.target.ref,
			FieldPath: g.target.path + ".resources.limits.memory",
			Message:   "container " + g.container + " was OOMKilled in " + strconv.Itoa(len(g.pods)) + " pod(s)",
			Evidence:  evidence,
		})
	}
	return out
}
