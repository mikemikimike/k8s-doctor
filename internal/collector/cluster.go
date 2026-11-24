package collector

import (
	"context"
	"fmt"
	"slices"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// ReadOnlyResource is one resource the cluster collector reads and the
// verbs it uses on it. The shipped ClusterRole must grant exactly these; a
// test enforces both directions.
type ReadOnlyResource struct {
	Group    string
	Resource string
	Verbs    []string
}

var listOnly = []string{"list"}

// RequiredResources lists every resource the cluster collector reads.
// Namespaces need get for --namespace; everything else is only listed.
var RequiredResources = []ReadOnlyResource{
	{"", "namespaces", []string{"get", "list"}},
	{"", "pods", listOnly},
	{"", "services", listOnly},
	{"", "events", listOnly},
	{"", "configmaps", listOnly},
	{"", "secrets", listOnly},
	{"apps", "deployments", listOnly},
	{"apps", "statefulsets", listOnly},
	{"apps", "daemonsets", listOnly},
	{"apps", "replicasets", listOnly},
	{"batch", "jobs", listOnly},
	{"batch", "cronjobs", listOnly},
	{"policy", "poddisruptionbudgets", listOnly},
	{"autoscaling", "horizontalpodautoscalers", listOnly},
}

// pageSize bounds each list response; larger collections are paged.
const pageSize = 500

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
	// SkipSecrets leaves Secrets out entirely, so the identity needs no
	// access to them at all.
	SkipSecrets bool
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
	// client-go defaults to 5 QPS, which a large cluster turns into minutes
	// of client-side throttling. A run is a short burst of list calls.
	cfg.QPS, cfg.Burst = 50, 100
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

// Cluster collects a snapshot from a live cluster. It only issues get and
// list requests; it never creates, updates, patches or deletes anything.
// Secrets and ConfigMaps are listed through the metadata API, so their
// contents are never transferred, and in cluster mode only their names are
// recorded.
//
// Without opts.Namespaces each kind is listed once across the cluster and
// system namespaces are filtered out; with it, each namespace is listed in
// turn. Any failed request fails the run: a partial snapshot would make
// rules pass on data they never saw.
func Cluster(ctx context.Context, c *Clients, opts ClusterOptions) (*snapshot.Snapshot, error) {
	// A namespace named twice would be collected twice and fail the
	// duplicate check with a misleading error.
	opts.Namespaces = slices.Compact(slices.Sorted(slices.Values(opts.Namespaces)))
	namespaces, err := selectNamespaces(ctx, c, opts.Namespaces)
	if err != nil {
		return nil, err
	}
	b := newBuilder()
	rt := &snapshot.Runtime{}
	scopes := opts.Namespaces
	if len(scopes) == 0 {
		scopes = []string{metav1.NamespaceAll}
	}
	keep := map[string]bool{}
	for _, ns := range namespaces {
		b.s.Namespaces = append(b.s.Namespaces, ns)
		keep[ns.Name] = true
	}
	col := &clusterCollector{c: c, b: b, rt: rt, keep: func(ns string) bool { return keep[ns] }, skipSecrets: opts.SkipSecrets}
	for _, scope := range scopes {
		if err := col.collect(ctx, scope); err != nil {
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

func selectNamespaces(ctx context.Context, c *Clients, only []string) ([]snapshot.Namespace, error) {
	if len(only) > 0 {
		var out []snapshot.Namespace
		for _, name := range only {
			ns, err := c.Typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, requestError("get namespace "+name, "namespaces", err)
			}
			out = append(out, Namespace(ns))
		}
		return out, nil
	}
	items, err := listAll(ctx, func(ctx context.Context, o metav1.ListOptions) ([]corev1.Namespace, string, error) {
		l, err := c.Typed.CoreV1().Namespaces().List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	if err != nil {
		return nil, requestError("list namespaces", "namespaces", err)
	}
	var out []snapshot.Namespace
	for i := range items {
		if !slices.Contains(SystemNamespaces, items[i].Name) {
			out = append(out, Namespace(&items[i]))
		}
	}
	return out, nil
}

// listAll pages through a list call with continue tokens.
func listAll[T any](ctx context.Context, list func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, error) {
	var out []T
	opts := metav1.ListOptions{Limit: pageSize}
	for {
		items, cont, err := list(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if cont == "" {
			return out, nil
		}
		opts.Continue = cont
	}
}

// requestError names the failed request and, when the API server refused
// it, the grant that is missing.
func requestError(what, resource string, err error) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("%s: %w (the identity needs list on %s; see deploy/rbac)", what, err, resource)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// typedList adapts a typed List call to listAll.
func typedList[L any, T any](list func(context.Context, metav1.ListOptions) (L, error), items func(L) ([]T, string)) func(context.Context, metav1.ListOptions) ([]T, string, error) {
	return func(ctx context.Context, o metav1.ListOptions) ([]T, string, error) {
		l, err := list(ctx, o)
		if err != nil {
			return nil, "", err
		}
		xs, cont := items(l)
		return xs, cont, nil
	}
}

type clusterCollector struct {
	c           *Clients
	b           *builder
	rt          *snapshot.Runtime
	keep        func(namespace string) bool
	skipSecrets bool
}

// collect lists every modelled kind in one namespace, or across all
// namespaces when ns is empty, keeping only objects in selected namespaces.
func (col *clusterCollector) collect(ctx context.Context, ns string) error {
	c, b, keep := col.c, col.b, col.keep
	where := "in all namespaces"
	if ns != "" {
		where = "in " + ns
	}
	wrap := func(resource string, err error) error {
		return requestError("list "+resource+" "+where, resource, err)
	}

	deps, err := listAll(ctx, typedList(c.Typed.AppsV1().Deployments(ns).List, func(l *appsv1.DeploymentList) ([]appsv1.Deployment, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("deployments", err)
	}
	for i := range deps {
		if keep(deps[i].Namespace) {
			if err := b.addWorkload(Deployment("apps/v1", &deps[i])); err != nil {
				return err
			}
		}
	}
	sts, err := listAll(ctx, typedList(c.Typed.AppsV1().StatefulSets(ns).List, func(l *appsv1.StatefulSetList) ([]appsv1.StatefulSet, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("statefulsets", err)
	}
	for i := range sts {
		if keep(sts[i].Namespace) {
			if err := b.addWorkload(StatefulSet("apps/v1", &sts[i])); err != nil {
				return err
			}
		}
	}
	dss, err := listAll(ctx, typedList(c.Typed.AppsV1().DaemonSets(ns).List, func(l *appsv1.DaemonSetList) ([]appsv1.DaemonSet, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("daemonsets", err)
	}
	for i := range dss {
		if keep(dss[i].Namespace) {
			if err := b.addWorkload(DaemonSet("apps/v1", &dss[i])); err != nil {
				return err
			}
		}
	}
	cjs, err := listAll(ctx, typedList(c.Typed.BatchV1().CronJobs(ns).List, func(l *batchv1.CronJobList) ([]batchv1.CronJob, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("cronjobs", err)
	}
	for i := range cjs {
		if keep(cjs[i].Namespace) {
			if err := b.addWorkload(CronJob("batch/v1", &cjs[i])); err != nil {
				return err
			}
		}
	}
	jobs, err := listAll(ctx, typedList(c.Typed.BatchV1().Jobs(ns).List, func(l *batchv1.JobList) ([]batchv1.Job, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("jobs", err)
	}
	jobs = slices.DeleteFunc(jobs, func(j batchv1.Job) bool { return !keep(j.Namespace) })
	for i := range jobs {
		// Jobs created by a CronJob are runs of that CronJob, not workloads
		// of their own; their pods are attributed to the CronJob below.
		if controller(jobs[i].OwnerReferences, "CronJob") != "" {
			continue
		}
		if err := b.addWorkload(Job("batch/v1", &jobs[i])); err != nil {
			return err
		}
	}

	svcs, err := listAll(ctx, typedList(c.Typed.CoreV1().Services(ns).List, func(l *corev1.ServiceList) ([]corev1.Service, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("services", err)
	}
	for i := range svcs {
		if !keep(svcs[i].Namespace) {
			continue
		}
		s := Service(&svcs[i])
		if err := b.addObject("Service", s.Namespace, s.Name, func() { b.s.Services = append(b.s.Services, s) }); err != nil {
			return err
		}
	}
	pdbs, err := listAll(ctx, typedList(c.Typed.PolicyV1().PodDisruptionBudgets(ns).List, func(l *policyv1.PodDisruptionBudgetList) ([]policyv1.PodDisruptionBudget, string) {
		return l.Items, l.Continue
	}))
	if err != nil {
		return wrap("poddisruptionbudgets", err)
	}
	for i := range pdbs {
		if !keep(pdbs[i].Namespace) {
			continue
		}
		p := PodDisruptionBudget("policy/v1", &pdbs[i])
		if err := b.addObject("PodDisruptionBudget", p.Namespace, p.Name, func() {
			b.s.PodDisruptionBudgets = append(b.s.PodDisruptionBudgets, p)
		}); err != nil {
			return err
		}
	}
	hpas, err := listAll(ctx, typedList(c.Typed.AutoscalingV2().HorizontalPodAutoscalers(ns).List, func(l *autoscalingv2.HorizontalPodAutoscalerList) ([]autoscalingv2.HorizontalPodAutoscaler, string) {
		return l.Items, l.Continue
	}))
	if err != nil {
		return wrap("horizontalpodautoscalers", err)
	}
	for i := range hpas {
		if !keep(hpas[i].Namespace) {
			continue
		}
		h := HorizontalPodAutoscaler("autoscaling/v2", &hpas[i])
		if err := b.addObject("HorizontalPodAutoscaler", h.Namespace, h.Name, func() {
			b.s.HorizontalPodAutoscalers = append(b.s.HorizontalPodAutoscalers, h)
		}); err != nil {
			return err
		}
	}

	if err := col.collectConfigRefs(ctx, ns, wrap); err != nil {
		return err
	}
	return col.collectRuntime(ctx, ns, jobs, wrap)
}

// collectConfigRefs lists ConfigMaps and Secrets as PartialObjectMetadata:
// the API server sends metadata only, so values never cross the wire.
func (col *clusterCollector) collectConfigRefs(ctx context.Context, ns string, wrap func(string, error) error) error {
	b := col.b
	type configKind struct {
		name string
		gvr  schema.GroupVersionResource
		add  func(snapshot.ConfigRef)
	}
	kinds := []configKind{
		{"ConfigMap", corev1.SchemeGroupVersion.WithResource("configmaps"), func(r snapshot.ConfigRef) { b.s.ConfigMaps = append(b.s.ConfigMaps, r) }},
	}
	if !col.skipSecrets {
		kinds = append(kinds, configKind{"Secret", corev1.SchemeGroupVersion.WithResource("secrets"), func(r snapshot.ConfigRef) { b.s.Secrets = append(b.s.Secrets, r) }})
	}
	for _, kind := range kinds {
		res := col.c.Metadata.Resource(kind.gvr).Namespace(ns)
		items, err := listAll(ctx, typedList(res.List, func(l *metav1.PartialObjectMetadataList) ([]metav1.PartialObjectMetadata, string) {
			return l.Items, l.Continue
		}))
		if err != nil {
			return wrap(kind.gvr.Resource, err)
		}
		for i := range items {
			if !col.keep(items[i].Namespace) {
				continue
			}
			ref := snapshot.ConfigRef{ObjectMeta: objectMeta(items[i].ObjectMeta)}
			if err := b.addObject(kind.name, ref.Namespace, ref.Name, func() { kind.add(ref) }); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectRuntime records pod status and events, attributing each pod to
// its top-level workload.
func (col *clusterCollector) collectRuntime(ctx context.Context, ns string, jobs []batchv1.Job, wrap func(string, error) error) error {
	c, rt := col.c, col.rt
	rss, err := listAll(ctx, typedList(c.Typed.AppsV1().ReplicaSets(ns).List, func(l *appsv1.ReplicaSetList) ([]appsv1.ReplicaSet, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("replicasets", err)
	}
	owners := ownerIndex(rss, jobs)

	pods, err := listAll(ctx, typedList(c.Typed.CoreV1().Pods(ns).List, func(l *corev1.PodList) ([]corev1.Pod, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("pods", err)
	}
	for i := range pods {
		if col.keep(pods[i].Namespace) {
			rt.Pods = append(rt.Pods, Pod(&pods[i], owners))
		}
	}

	events, err := listAll(ctx, typedList(c.Typed.CoreV1().Events(ns).List, func(l *corev1.EventList) ([]corev1.Event, string) { return l.Items, l.Continue }))
	if err != nil {
		return wrap("events", err)
	}
	for i := range events {
		if !col.keep(events[i].Namespace) {
			continue
		}
		event := Event(&events[i])
		ownerKey := event.InvolvedObject.Namespace + "/" + event.InvolvedObject.Kind + "/" + event.InvolvedObject.Name
		if owner, ok := owners[ownerKey]; ok {
			event.Owner = &owner
		}
		rt.Events = append(rt.Events, event)
	}
	return nil
}

// ownerIndex maps intermediate controllers to the workload above them:
// ReplicaSet to Deployment, Job to CronJob. Keys are "namespace/Kind/name".
func ownerIndex(rss []appsv1.ReplicaSet, jobs []batchv1.Job) map[string]snapshot.ResourceRef {
	idx := map[string]snapshot.ResourceRef{}
	for i := range rss {
		if dep := controller(rss[i].OwnerReferences, "Deployment"); dep != "" {
			idx[rss[i].Namespace+"/ReplicaSet/"+rss[i].Name] = snapshot.ResourceRef{Kind: snapshot.KindDeployment, Namespace: rss[i].Namespace, Name: dep}
		}
	}
	for i := range jobs {
		if cj := controller(jobs[i].OwnerReferences, "CronJob"); cj != "" {
			idx[jobs[i].Namespace+"/Job/"+jobs[i].Name] = snapshot.ResourceRef{Kind: snapshot.KindCronJob, Namespace: jobs[i].Namespace, Name: cj}
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
		if top, ok := owners[out.Namespace+"/"+r.Kind+"/"+r.Name]; ok {
			ref = top
		}
		out.Owner = &ref
	}
	for i := range p.Status.InitContainerStatuses {
		cs := containerStatus(&p.Status.InitContainerStatuses[i])
		cs.Init = true
		out.Containers = append(out.Containers, cs)
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
