package checks

import (
	"strconv"

	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

func isTrue(b *bool) bool  { return b != nil && *b }
func isFalse(b *bool) bool { return b != nil && !*b }

// runAsRoot flags containers that may run as root: neither the container
// nor the pod sets runAsNonRoot: true, and no non-zero runAsUser is set.
// The container's securityContext overrides the pod's, field by field.
type runAsRoot struct{ meta }

func newRunAsRoot() rules.Rule {
	return runAsRoot{meta{
		id:       "security-run-as-root",
		category: CategorySecurity,
		severity: rules.SeverityMedium,
		modes:    rules.AllModes,
		description: "Nothing stops the container from running as root (UID 0). If the image's default user " +
			"is root, a compromise of the process starts with root inside the container, one kernel or " +
			"runtime bug away from the node. Set runAsNonRoot: true, or a non-zero runAsUser.",
	}}
}

func (runAsRoot) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	eachPodContainer(s, func(w *snapshot.Workload, c *snapshot.Container, path string) {
		var nonRoot *bool
		var user *int64
		if psc := w.Template.Spec.SecurityContext; psc != nil {
			nonRoot, user = psc.RunAsNonRoot, psc.RunAsUser
		}
		if csc := c.SecurityContext; csc != nil {
			if csc.RunAsNonRoot != nil {
				nonRoot = csc.RunAsNonRoot
			}
			if csc.RunAsUser != nil {
				user = csc.RunAsUser
			}
		}
		if user != nil && *user > 0 {
			return
		}
		if isTrue(nonRoot) && user == nil {
			return
		}
		evidence := map[string]string{"container": c.Name, "runAsNonRoot": "unset"}
		if nonRoot != nil {
			evidence["runAsNonRoot"] = strconv.FormatBool(*nonRoot)
		}
		msg := "container " + c.Name + " may run as root"
		if user != nil {
			evidence["runAsUser"] = "0"
			msg = "container " + c.Name + " runs as root (runAsUser: 0)"
		}
		out = append(out, rules.Finding{
			Resource:  w.Ref(),
			FieldPath: path + ".securityContext.runAsNonRoot",
			Message:   msg,
			Evidence:  evidence,
		})
	})
	return out
}

// privileged flags privileged containers, which have every capability and
// full access to the host's devices.
type privileged struct{ meta }

func newPrivileged() rules.Rule {
	return privileged{meta{
		id:       "security-privileged",
		category: CategorySecurity,
		severity: rules.SeverityCritical,
		modes:    rules.AllModes,
		description: "The container is privileged: it has every Linux capability and access to all host " +
			"devices, so anyone who controls it effectively controls the node. Grant the specific " +
			"capabilities it needs instead.",
	}}
}

func (privileged) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	eachPodContainer(s, func(w *snapshot.Workload, c *snapshot.Container, path string) {
		if c.SecurityContext == nil || !isTrue(c.SecurityContext.Privileged) {
			return
		}
		out = append(out, rules.Finding{
			Resource:  w.Ref(),
			FieldPath: path + ".securityContext.privileged",
			Message:   "container " + c.Name + " is privileged",
			Evidence:  map[string]string{"container": c.Name, "privileged": "true"},
		})
	})
	return out
}

// privilegeEscalation flags containers that do not set
// allowPrivilegeEscalation: false. Unset means allowed. Privileged
// containers are left to security-privileged, which already covers them.
type privilegeEscalation struct{ meta }

func newPrivilegeEscalation() rules.Rule {
	return privilegeEscalation{meta{
		id:       "security-privilege-escalation",
		category: CategorySecurity,
		severity: rules.SeverityMedium,
		modes:    rules.AllModes,
		description: "allowPrivilegeEscalation is not set to false, so a process in the container can gain " +
			"more privileges than its parent, for example through a setuid binary. The restricted Pod " +
			"Security Standard requires it to be false.",
	}}
}

func (privilegeEscalation) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	eachPodContainer(s, func(w *snapshot.Workload, c *snapshot.Container, path string) {
		sc := c.SecurityContext
		if sc != nil && (isFalse(sc.AllowPrivilegeEscalation) || isTrue(sc.Privileged)) {
			return
		}
		value := "unset"
		if sc != nil && sc.AllowPrivilegeEscalation != nil {
			value = "true"
		}
		out = append(out, rules.Finding{
			Resource:  w.Ref(),
			FieldPath: path + ".securityContext.allowPrivilegeEscalation",
			Message:   "container " + c.Name + " allows privilege escalation",
			Evidence:  map[string]string{"container": c.Name, "allowPrivilegeEscalation": value},
		})
	})
	return out
}

// rootFilesystemWritable flags containers without readOnlyRootFilesystem:
// true. It is low severity: many images need a writable path, which an
// emptyDir mount provides without opening up the whole filesystem.
type rootFilesystemWritable struct{ meta }

func newRootFilesystemWritable() rules.Rule {
	return rootFilesystemWritable{meta{
		id:       "security-root-filesystem-writable",
		category: CategorySecurity,
		severity: rules.SeverityLow,
		modes:    rules.AllModes,
		description: "The container's root filesystem is writable, so an attacker can modify binaries or drop " +
			"tools into it. Set readOnlyRootFilesystem: true and mount an emptyDir where the process needs to " +
			"write.",
	}}
}

func (rootFilesystemWritable) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	eachPodContainer(s, func(w *snapshot.Workload, c *snapshot.Container, path string) {
		if c.SecurityContext != nil && isTrue(c.SecurityContext.ReadOnlyRootFilesystem) {
			return
		}
		out = append(out, rules.Finding{
			Resource:  w.Ref(),
			FieldPath: path + ".securityContext.readOnlyRootFilesystem",
			Message:   "container " + c.Name + " has a writable root filesystem",
			Evidence:  map[string]string{"container": c.Name},
		})
	})
	return out
}
