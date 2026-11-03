package snapshot

import "time"

// Runtime is the observed-state section. Only the cluster collector fills it;
// rules that need it declare cluster mode and are reported as "not run" on
// manifest snapshots.
type Runtime struct {
	Pods   []Pod   `json:"pods,omitempty"`
	Events []Event `json:"events,omitempty"`
}

// Pod is a running or terminated pod and the workload that owns it.
type Pod struct {
	ObjectMeta
	// Owner is the top-level workload (a ReplicaSet is resolved to its
	// Deployment, a Job to its CronJob when applicable).
	Owner      *ResourceRef      `json:"owner,omitempty"`
	Phase      string            `json:"phase"`
	NodeName   string            `json:"nodeName,omitempty"`
	Containers []ContainerStatus `json:"containers,omitempty"`
}

// Ref returns the pod's resource reference.
func (p *Pod) Ref() ResourceRef {
	return ResourceRef{Kind: "Pod", Namespace: p.Namespace, Name: p.Name}
}

// ContainerStatus is the status of one container in a pod.
type ContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
	// State is "running", "waiting" or "terminated".
	State string `json:"state"`
	// Reason is the waiting or terminated reason, e.g. "CrashLoopBackOff".
	Reason string `json:"reason,omitempty"`
	// LastTermination is the previous termination, e.g. reason "OOMKilled".
	LastTermination *Termination `json:"lastTermination,omitempty"`
}

// Termination describes how a container last exited.
type Termination struct {
	Reason     string    `json:"reason"`
	ExitCode   int32     `json:"exitCode"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// Event is a Kubernetes Event about an object. Messages are kept because
// they explain failures, but they pass through redaction before any LLM call.
type Event struct {
	InvolvedObject ResourceRef `json:"involvedObject"`
	// Owner is the top-level workload when the involved object is an
	// intermediate controller such as a ReplicaSet or Job.
	Owner    *ResourceRef `json:"owner,omitempty"`
	Type     string       `json:"type"`
	Reason   string       `json:"reason"`
	Message  string       `json:"message,omitempty"`
	Count    int32        `json:"count,omitempty"`
	LastSeen time.Time    `json:"lastSeen,omitzero"`
}
