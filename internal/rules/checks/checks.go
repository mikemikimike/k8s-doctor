// Package checks holds the built-in rules.
package checks

import (
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// Rule categories, used to group results in reports and evals.
const (
	CategoryResources    = "resources"
	CategoryProbes       = "probes"
	CategoryImages       = "images"
	CategoryAvailability = "availability"
	CategorySecurity     = "security"
	CategoryHygiene      = "hygiene"
)

// All returns a new instance of every built-in rule.
func All() []rules.Rule {
	return []rules.Rule{
		newResourceRequestsMissing(),
		newMemoryLimitMissing(),
		newReadinessProbeMissing(),
		newLivenessProbeMissing(),
		newImageTagMutable(),
		newImagePullPolicyStale(),
		newSingleReplica(),
		newPDBMissing(),
		newPDBBlocksDrain(),
		newRunAsRoot(),
		newPrivileged(),
		newPrivilegeEscalation(),
		newRootFilesystemWritable(),
		newHostPath(),
		newHostNamespace(),
		newDefaultNamespace(),
	}
}

// Registry returns a registry with every built-in rule.
func Registry() *rules.Registry {
	return rules.NewRegistry().MustRegister(All()...)
}

// meta implements the static parts of rules.Rule; each rule embeds it
// and adds Evaluate.
type meta struct {
	id          string
	category    string
	severity    rules.Severity
	modes       []snapshot.Mode
	description string
}

func (m meta) ID() string               { return m.id }
func (m meta) Category() string         { return m.category }
func (m meta) Severity() rules.Severity { return m.severity }
func (m meta) Modes() []snapshot.Mode   { return m.modes }
func (m meta) Description() string      { return m.description }

// eachContainer calls fn for every regular (non-init) container of every
// workload that keep(w) accepts.
func eachContainer(s *snapshot.Snapshot, keep func(*snapshot.Workload) bool, fn func(*snapshot.Workload, *snapshot.Container)) {
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		if keep != nil && !keep(w) {
			continue
		}
		for j := range w.Template.Spec.Containers {
			fn(w, &w.Template.Spec.Containers[j])
		}
	}
}

// eachPodContainer calls fn for every init and regular container of every
// workload, with the container's manifest field path.
func eachPodContainer(s *snapshot.Snapshot, fn func(w *snapshot.Workload, c *snapshot.Container, path string)) {
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		for j := range w.Template.Spec.InitContainers {
			c := &w.Template.Spec.InitContainers[j]
			fn(w, c, rules.InitContainerPath(w, c))
		}
		for j := range w.Template.Spec.Containers {
			c := &w.Template.Spec.Containers[j]
			fn(w, c, rules.ContainerPath(w, c))
		}
	}
}
