package collector

import (
	"context"
	"fmt"
	"slices"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// SystemNamespaces are skipped by default: their workloads belong to the
// cluster, not to the user, and would drown the report.
var SystemNamespaces = []string{"kube-system", "kube-public", "kube-node-lease", "local-path-storage"}

// ReadOnlyResource is one resource the cluster collector reads. The shipped
// ClusterRole must grant get, list and watch on every entry, and nothing
// else; a test enforces both directions.
type ReadOnlyResource struct {
	Group    string
	Resource string
}

// RequiredResources lists every resource the cluster collector reads.
var RequiredResources = []ReadOnlyResource{
	{"", "namespaces"},
	{"", "pods"},
	{"", "services"},
	{"", "events"},
	{"", "configmaps"},
	{"", "secrets"},
	{"apps", "deployments"},
	{"apps", "statefulsets"},
	{"apps", "daemonsets"},
	{"apps", "replicasets"},
	{"batch", "jobs"},
	{"batch", "cronjobs"},
	{"policy", "poddisruptionbudgets"},
	{"autoscaling", "horizontalpodautoscalers"},
}

// ClusterOptions selects what to collect.
type ClusterOptions struct {
	// Kubeconfig is the kubeconfig path; empty uses the default loading
	// rules ($KUBECONFIG, then ~/.kube/config, then in-cluster config).
	Kubeconfig string
	// Context overrides the kubeconfig's current context.
	Context string
	// Namespaces limits collection to these namespaces. Empty means every
	// namespace except SystemNamespaces.
	Namespaces []string
}

// Clients are the API clients the collector uses. Only the typed clientset
// and the metadata client are needed, which makes it easy to substitute
// fakes in tests.
type Clients struct {
	Typed    kubernetes.Interface
	Metadata metadata.Interface
	// ContextName is recorded as the snapshot source.
	ContextName string
}

// NewClients builds clients from a kubeconfig.
func NewClients(opts ClusterOptions) (*Clients, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		rules.ExplicitPath = opts.Kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: opts.Context})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	name := opts.Context
	if name == "" {
		if raw, err := cc.RawConfig(); err == nil {
			name = raw.CurrentContext
		}
	}
	if name == "" {
		name = "in-cluster"
	}
	return newClients(cfg, name)
}

func newClients(cfg *rest.Config, name string) (*Clients, error) {
	cfg = rest.CopyConfig(cfg)
	cfg.UserAgent = "k8s-doctor (read-only)"
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	md, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{Typed: typed, Metadata: md, ContextName: name}, nil
}

// Cluster collects a snapshot from a live cluster. It only issues list
// requests; it never creates, updates, patches or deletes anything. Secrets
// and ConfigMaps are listed through the metadata API, so their contents are
// never transferred, and in cluster mode only their names are recorded.
func Cluster(ctx context.Context, c *Clients, opts ClusterOptions) (*snapshot.Snapshot, error) {
	namespaces, err := selectNamespaces(ctx, c, opts.Namespaces)
	if err != nil {
		return nil, err
	}
	b := newBuilder()
	rt := &snapshot.Runtime{}
	for _, ns := range namespaces {
		b.s.Namespaces = append(b.s.Namespaces, ns.snap)
		if err := collectNamespace(ctx, c, ns.snap.Name, b, rt); err != nil {
			return nil, err
		}
	}
	snap := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Mode:          snapshot.ModeCluster,
		Source:        snapshot.Source{Cluster: c.ContextName},
		Spec:          b.spec(),
		Runtime:       rt,
	}
	sortRuntime(rt)
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	return snap, nil
}

type namespaceInfo struct{ snap snapshot.Namespace }

func selectNamespaces(ctx context.Context, c *Clients, only []string) ([]namespaceInfo, error) {
	if len(only) > 0 {
		var out []namespaceInfo
		for _, name := range only {
			ns, err := c.Typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("namespace %s: %w", name, err)
			}
			out = append(out, namespaceInfo{Namespace(ns)})
		}
		return out, nil
	}
	list, err := c.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	var out []namespaceInfo
	for i := range list.Items {
		if slices.Contains(SystemNamespaces, list.Items[i].Name) {
			continue
		}
		out = append(out, namespaceInfo{Namespace(&list.Items[i])})
	}
	return out, nil
}

// collectNamespace lists every modelled kind in one namespace.
func collectNamespace(ctx context.Context, c *Clients, ns string, b *builder, rt *snapshot.Runtime) error {
	lo := metav1.ListOptions{}
	wrap := func(what string, err error) error { return fmt.Errorf("list %s in %s: %w", what, ns, err) }

	deps, err := c.Typed.AppsV1().Deployments(ns).List(ctx, lo)
	if err != nil {
		return wrap("deployments", err)
	}
	for i := range deps.Items {
		if err := b.addWorkload(Deployment("apps/v1", &deps.Items[i])); err != nil {
			return err
		}
	}
	sts, err := c.Typed.AppsV1().StatefulSets(ns).List(ctx, lo)
	if err != nil {
		return wrap("statefulsets", err)
	}
	for i := range sts.Items {
		if err := b.addWorkload(StatefulSet("apps/v1", &sts.Items[i])); err != nil {
			return err
		}
	}
	dss, err := c.Typed.AppsV1().DaemonSets(ns).List(ctx, lo)
	if err != nil {
		return wrap("daemonsets", err)
	}
	for i := range dss.Items {
		if err := b.addWorkload(DaemonSet("apps/v1", &dss.Items[i])); err != nil {
			return err
		}
	}
	cjs, err := c.Typed.BatchV1().CronJobs(ns).List(ctx, lo)
	if err != nil {
		return wrap("cronjobs", err)
	}
	for i := range cjs.Items {
		if err := b.addWorkload(CronJob("batch/v1", &cjs.Items[i])); err != nil {
			return err
		}
	}
	jobs, err := c.Typed.BatchV1().Jobs(ns).List(ctx, lo)
	if err != nil {
		return wrap("jobs", err)
	}
	for i := range jobs.Items {
		// Jobs created by a CronJob are runs of that CronJob, not workloads
		// of their own; their pods are attributed to the CronJob below.
		if controller(jobs.Items[i].OwnerReferences, "CronJob") != "" {
			continue
		}
		if err := b.addWorkload(Job("batch/v1", &jobs.Items[i])); err != nil {
			return err
		}
	}

	svcs, err := c.Typed.CoreV1().Services(ns).List(ctx, lo)
	if err != nil {
		return wrap("services", err)
	}
	for i := range svcs.Items {
		s := Service(&svcs.Items[i])
		if err := b.addObject("Service", s.Namespace, s.Name, func() { b.s.Services = append(b.s.Services, s) }); err != nil {
			return err
		}
	}
	pdbs, err := c.Typed.PolicyV1().PodDisruptionBudgets(ns).List(ctx, lo)
	if err != nil {
		return wrap("poddisruptionbudgets", err)
	}
	for i := range pdbs.Items {
		p := PodDisruptionBudget("policy/v1", &pdbs.Items[i])
		if err := b.addObject("PodDisruptionBudget", p.Namespace, p.Name, func() {
			b.s.PodDisruptionBudgets = append(b.s.PodDisruptionBudgets, p)
		}); err != nil {
			return err
		}
	}
	hpas, err := c.Typed.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, lo)
	if err != nil {
		return wrap("horizontalpodautoscalers", err)
	}
	for i := range hpas.Items {
		h := HorizontalPodAutoscaler("autoscaling/v2", &hpas.Items[i])
		if err := b.addObject("HorizontalPodAutoscaler", h.Namespace, h.Name, func() {
			b.s.HorizontalPodAutoscalers = append(b.s.HorizontalPodAutoscalers, h)
		}); err != nil {
			return err
		}
	}

	if err := collectConfigRefs(ctx, c, ns, b); err != nil {
		return err
	}
	return collectRuntime(ctx, c, ns, jobs.Items, rt)
}

// collectConfigRefs lists ConfigMaps and Secrets as PartialObjectMetadata:
// the API server sends metadata only, so values never cross the wire.
func collectConfigRefs(ctx context.Context, c *Clients, ns string, b *builder) error {
	for _, kind := range []struct {
		name string
		gvr  schema.GroupVersionResource
		add  func(snapshot.ConfigRef)
	}{
		{"ConfigMap", corev1.SchemeGroupVersion.WithResource("configmaps"), func(r snapshot.ConfigRef) { b.s.ConfigMaps = append(b.s.ConfigMaps, r) }},
		{"Secret", corev1.SchemeGroupVersion.WithResource("secrets"), func(r snapshot.ConfigRef) { b.s.Secrets = append(b.s.Secrets, r) }},
	} {
		list, err := c.Metadata.Resource(kind.gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list %s metadata in %s: %w", kind.gvr.Resource, ns, err)
		}
		for i := range list.Items {
			ref := snapshot.ConfigRef{ObjectMeta: objectMeta(list.Items[i].ObjectMeta)}
			if err := b.addObject(kind.name, ref.Namespace, ref.Name, func() { kind.add(ref) }); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectRuntime records pod status and events, attributing each pod to
// its top-level workload.
func collectRuntime(ctx context.Context, c *Clients, ns string, jobs []batchv1.Job, rt *snapshot.Runtime) error {
	rss, err := c.Typed.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list replicasets in %s: %w", ns, err)
	}
	owners := ownerIndex(ns, rss.Items, jobs)

	pods, err := c.Typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods in %s: %w", ns, err)
	}
	for i := range pods.Items {
		rt.Pods = append(rt.Pods, Pod(&pods.Items[i], owners))
	}

	events, err := c.Typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list events in %s: %w", ns, err)
	}
	for i := range events.Items {
		event := Event(&events.Items[i])
		if owner, ok := owners[event.InvolvedObject.Kind+"/"+event.InvolvedObject.Name]; ok {
			event.Owner = &owner
		}
		rt.Events = append(rt.Events, event)
	}
	return nil
}

// ownerIndex maps intermediate controllers to the workload above them:
// ReplicaSet to Deployment, Job to CronJob. Keys are "Kind/name".
func ownerIndex(ns string, rss []appsv1.ReplicaSet, jobs []batchv1.Job) map[string]snapshot.ResourceRef {
	idx := map[string]snapshot.ResourceRef{}
	for i := range rss {
		if dep := controller(rss[i].OwnerReferences, "Deployment"); dep != "" {
			idx["ReplicaSet/"+rss[i].Name] = snapshot.ResourceRef{Kind: snapshot.KindDeployment, Namespace: ns, Name: dep}
		}
	}
	for i := range jobs {
		if cj := controller(jobs[i].OwnerReferences, "CronJob"); cj != "" {
			idx["Job/"+jobs[i].Name] = snapshot.ResourceRef{Kind: snapshot.KindCronJob, Namespace: ns, Name: cj}
		}
	}
	return idx
}

func controller(refs []metav1.OwnerReference, kind string) string {
	for _, r := range refs {
		if r.Kind == kind && r.Controller != nil && *r.Controller {
			return r.Name
		}
	}
	return ""
}

// Pod normalizes a pod's status. owners resolves ReplicaSets and Jobs to
// the workload that created them.
func Pod(p *corev1.Pod, owners map[string]snapshot.ResourceRef) snapshot.Pod {
	out := snapshot.Pod{
		ObjectMeta: objectMeta(p.ObjectMeta),
		Phase:      string(p.Status.Phase),
		NodeName:   p.Spec.NodeName,
	}
	for _, r := range p.OwnerReferences {
		if r.Controller == nil || !*r.Controller {
			continue
		}
		ref := snapshot.ResourceRef{Kind: r.Kind, Namespace: out.Namespace, Name: r.Name}
		if top, ok := owners[r.Kind+"/"+r.Name]; ok {
			ref = top
		}
		out.Owner = &ref
	}
	for i := range p.Status.ContainerStatuses {
		out.Containers = append(out.Containers, containerStatus(&p.Status.ContainerStatuses[i]))
	}
	return out
}

func containerStatus(cs *corev1.ContainerStatus) snapshot.ContainerStatus {
	out := snapshot.ContainerStatus{Name: cs.Name, Ready: cs.Ready, RestartCount: cs.RestartCount}
	switch s := cs.State; {
	case s.Running != nil:
		out.State = "running"
	case s.Waiting != nil:
		out.State, out.Reason = "waiting", s.Waiting.Reason
	case s.Terminated != nil:
		out.State, out.Reason = "terminated", s.Terminated.Reason
	default:
		out.State = "unknown"
	}
	if t := cs.LastTerminationState.Terminated; t != nil {
		out.LastTermination = &snapshot.Termination{Reason: t.Reason, ExitCode: t.ExitCode, FinishedAt: t.FinishedAt.UTC()}
	}
	return out
}

// Event normalizes a core/v1 Event. The message is kept because it explains
// failures; it is redacted before any LLM call (#26).
func Event(e *corev1.Event) snapshot.Event {
	out := snapshot.Event{
		InvolvedObject: snapshot.ResourceRef{
			Kind: e.InvolvedObject.Kind, Namespace: e.InvolvedObject.Namespace, Name: e.InvolvedObject.Name,
		},
		Type:    e.Type,
		Reason:  e.Reason,
		Message: e.Message,
		Count:   e.Count,
	}
	switch {
	case e.Series != nil:
		out.Count = e.Series.Count
		out.LastSeen = e.Series.LastObservedTime.UTC()
	case !e.LastTimestamp.IsZero():
		out.LastSeen = e.LastTimestamp.UTC()
	case !e.EventTime.IsZero():
		out.LastSeen = e.EventTime.UTC()
	}
	return out
}

func sortRuntime(rt *snapshot.Runtime) {
	sort.Slice(rt.Pods, func(i, j int) bool { return rt.Pods[i].Ref().String() < rt.Pods[j].Ref().String() })
	sort.SliceStable(rt.Events, func(i, j int) bool {
		a, b := rt.Events[i], rt.Events[j]
		if a.InvolvedObject.String() != b.InvolvedObject.String() {
			return a.InvolvedObject.String() < b.InvolvedObject.String()
		}
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.Before(b.LastSeen)
		}
		return a.Reason < b.Reason
	})
}
