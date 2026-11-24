package checks

import (
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// removedAPI is a served version of a kind that Kubernetes has removed.
type removedAPI struct {
	removedIn   string
	replacement string
}

// removedAPIs maps "apiVersion/Kind" to its removal, after the upstream
// deprecation guide. It is a static table, like pluto's, so a manifest that
// no current schema would validate is still recognised. Kinds the snapshot
// does not record yet (Ingress, PodSecurityPolicy, ...) are listed so they
// match once generic objects land (#43).
var removedAPIs = map[string]removedAPI{
	"extensions/v1beta1/Deployment":                         {"1.16", "apps/v1"},
	"extensions/v1beta1/DaemonSet":                          {"1.16", "apps/v1"},
	"extensions/v1beta1/ReplicaSet":                         {"1.16", "apps/v1"},
	"extensions/v1beta1/NetworkPolicy":                      {"1.16", "networking.k8s.io/v1"},
	"extensions/v1beta1/PodSecurityPolicy":                  {"1.16", "policy/v1beta1, itself removed in 1.25"},
	"apps/v1beta1/Deployment":                               {"1.16", "apps/v1"},
	"apps/v1beta1/StatefulSet":                              {"1.16", "apps/v1"},
	"apps/v1beta2/Deployment":                               {"1.16", "apps/v1"},
	"apps/v1beta2/StatefulSet":                              {"1.16", "apps/v1"},
	"apps/v1beta2/DaemonSet":                                {"1.16", "apps/v1"},
	"batch/v2alpha1/CronJob":                                {"1.21", "batch/v1"},
	"extensions/v1beta1/Ingress":                            {"1.22", "networking.k8s.io/v1"},
	"networking.k8s.io/v1beta1/Ingress":                     {"1.22", "networking.k8s.io/v1"},
	"networking.k8s.io/v1beta1/IngressClass":                {"1.22", "networking.k8s.io/v1"},
	"rbac.authorization.k8s.io/v1beta1/Role":                {"1.22", "rbac.authorization.k8s.io/v1"},
	"rbac.authorization.k8s.io/v1beta1/ClusterRole":         {"1.22", "rbac.authorization.k8s.io/v1"},
	"rbac.authorization.k8s.io/v1beta1/RoleBinding":         {"1.22", "rbac.authorization.k8s.io/v1"},
	"apiextensions.k8s.io/v1beta1/CustomResourceDefinition": {"1.22", "apiextensions.k8s.io/v1"},
	"batch/v1beta1/CronJob":                                 {"1.25", "batch/v1"},
	"policy/v1beta1/PodDisruptionBudget":                    {"1.25", "policy/v1"},
	"policy/v1beta1/PodSecurityPolicy":                      {"1.25", "Pod Security Admission"},
	"autoscaling/v2beta1/HorizontalPodAutoscaler":           {"1.25", "autoscaling/v2"},
	"autoscaling/v2beta2/HorizontalPodAutoscaler":           {"1.26", "autoscaling/v2"},
	"flowcontrol.apiserver.k8s.io/v1beta2/FlowSchema":       {"1.29", "flowcontrol.apiserver.k8s.io/v1"},
	"flowcontrol.apiserver.k8s.io/v1beta3/FlowSchema":       {"1.32", "flowcontrol.apiserver.k8s.io/v1"},
}

// apiRemoved flags objects written against an API version Kubernetes no
// longer serves. It runs on manifests only: a cluster returns objects at
// whatever version the client asks for, so live objects never show one.
type apiRemoved struct{ meta }

func newAPIRemoved() rules.Rule {
	return apiRemoved{meta{
		id:       "api-version-removed",
		category: CategoryHygiene,
		severity: rules.SeverityHigh,
		modes:    []snapshot.Mode{snapshot.ModeManifest},
		description: "The manifest uses an API version that current Kubernetes releases no longer serve, so " +
			"applying it fails with \"no matches for kind\". Move it to the replacement version; some fields " +
			"changed on the way, so check the upstream migration notes.",
	}}
}

func (apiRemoved) Evaluate(s *snapshot.Snapshot) []rules.Finding {
	var out []rules.Finding
	check := func(ref snapshot.ResourceRef, apiVersion string) {
		r, ok := removedAPIs[apiVersion+"/"+ref.Kind]
		if !ok {
			return
		}
		out = append(out, rules.Finding{
			Resource:  ref,
			FieldPath: "apiVersion",
			Message:   ref.Kind + " " + ref.Name + " uses " + apiVersion + ", removed in Kubernetes " + r.removedIn,
			Evidence:  map[string]string{"apiVersion": apiVersion, "removedIn": r.removedIn, "replacement": r.replacement},
		})
	}
	for i := range s.Spec.Workloads {
		w := &s.Spec.Workloads[i]
		check(w.Ref(), w.APIVersion)
	}
	for i := range s.Spec.PodDisruptionBudgets {
		p := &s.Spec.PodDisruptionBudgets[i]
		check(snapshot.ResourceRef{Kind: "PodDisruptionBudget", Namespace: p.Namespace, Name: p.Name}, p.APIVersion)
	}
	for i := range s.Spec.HorizontalPodAutoscalers {
		h := &s.Spec.HorizontalPodAutoscalers[i]
		check(snapshot.ResourceRef{Kind: "HorizontalPodAutoscaler", Namespace: h.Namespace, Name: h.Name}, h.APIVersion)
	}
	return out
}
