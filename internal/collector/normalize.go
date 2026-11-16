package collector

import (
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// The functions in this file turn typed Kubernetes API objects into snapshot
// spec entries. Both collectors use them, which is what keeps the spec
// section identical across modes (ADR-0002).

// DefaultNamespace is the namespace of a namespaced object that names none
// and is not placed in one by an overlay, matching what the API server does.
const DefaultNamespace = "default"

func objectMeta(m metav1.ObjectMeta) snapshot.ObjectMeta {
	ns := m.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	return snapshot.ObjectMeta{Name: m.Name, Namespace: ns, Labels: nonEmpty(m.Labels)}
}

func nonEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func selectorLabels(s *metav1.LabelSelector) map[string]string {
	if s == nil {
		return nil
	}
	return nonEmpty(s.MatchLabels)
}

// Deployment normalizes an apps/v1 Deployment.
func Deployment(apiVersion string, d *appsv1.Deployment) snapshot.Workload {
	return workload(snapshot.KindDeployment, apiVersion, d.ObjectMeta, d.Spec.Replicas, d.Spec.Selector, d.Spec.Template)
}

// StatefulSet normalizes an apps/v1 StatefulSet.
func StatefulSet(apiVersion string, s *appsv1.StatefulSet) snapshot.Workload {
	return workload(snapshot.KindStatefulSet, apiVersion, s.ObjectMeta, s.Spec.Replicas, s.Spec.Selector, s.Spec.Template)
}

// DaemonSet normalizes an apps/v1 DaemonSet.
func DaemonSet(apiVersion string, d *appsv1.DaemonSet) snapshot.Workload {
	return workload(snapshot.KindDaemonSet, apiVersion, d.ObjectMeta, nil, d.Spec.Selector, d.Spec.Template)
}

// Job normalizes a batch/v1 Job.
func Job(apiVersion string, j *batchv1.Job) snapshot.Workload {
	return workload(snapshot.KindJob, apiVersion, j.ObjectMeta, nil, j.Spec.Selector, j.Spec.Template)
}

// CronJob normalizes a batch/v1 CronJob.
func CronJob(apiVersion string, c *batchv1.CronJob) snapshot.Workload {
	return workload(snapshot.KindCronJob, apiVersion, c.ObjectMeta, nil, nil, c.Spec.JobTemplate.Spec.Template)
}

func workload(kind, apiVersion string, m metav1.ObjectMeta, replicas *int32, sel *metav1.LabelSelector, t corev1.PodTemplateSpec) snapshot.Workload {
	return snapshot.Workload{
		Kind:       kind,
		APIVersion: apiVersion,
		ObjectMeta: objectMeta(m),
		Replicas:   replicas,
		Selector:   selectorLabels(sel),
		Template: snapshot.PodTemplate{
			Labels: nonEmpty(t.Labels),
			Spec:   podSpec(&t.Spec),
		},
	}
}

func podSpec(p *corev1.PodSpec) snapshot.PodSpec {
	out := snapshot.PodSpec{
		ServiceAccountName: p.ServiceAccountName,
		HostNetwork:        p.HostNetwork,
		HostPID:            p.HostPID,
		HostIPC:            p.HostIPC,
		Containers:         make([]snapshot.Container, 0, len(p.Containers)),
	}
	if sc := p.SecurityContext; sc != nil {
		out.SecurityContext = &snapshot.PodSecurityContext{RunAsNonRoot: sc.RunAsNonRoot, RunAsUser: sc.RunAsUser}
		if sc.SeccompProfile != nil {
			out.SecurityContext.SeccompProfile = &snapshot.SeccompProfile{Type: string(sc.SeccompProfile.Type)}
		}
	}
	if a := p.Affinity; a != nil && a.PodAntiAffinity != nil {
		paa := &snapshot.PodAntiAffinity{}
		for _, term := range a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			paa.RequiredTopologyKeys = append(paa.RequiredTopologyKeys, term.TopologyKey)
		}
		for _, term := range a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			paa.PreferredTopologyKeys = append(paa.PreferredTopologyKeys, term.PodAffinityTerm.TopologyKey)
		}
		out.Affinity = &snapshot.Affinity{PodAntiAffinity: paa}
	}
	for _, c := range p.TopologySpreadConstraints {
		out.TopologySpreadConstraints = append(out.TopologySpreadConstraints, snapshot.TopologySpreadConstraint{
			TopologyKey:       c.TopologyKey,
			MaxSkew:           c.MaxSkew,
			WhenUnsatisfiable: string(c.WhenUnsatisfiable),
		})
	}
	for i := range p.Volumes {
		out.Volumes = append(out.Volumes, volume(&p.Volumes[i]))
	}
	for i := range p.InitContainers {
		out.InitContainers = append(out.InitContainers, container(&p.InitContainers[i]))
	}
	for i := range p.Containers {
		out.Containers = append(out.Containers, container(&p.Containers[i]))
	}
	return out
}

func volume(v *corev1.Volume) snapshot.Volume {
	out := snapshot.Volume{Name: v.Name}
	switch src := v.VolumeSource; {
	case src.HostPath != nil:
		out.Type, out.HostPath = "hostPath", src.HostPath.Path
	case src.ConfigMap != nil:
		out.Type, out.Source = "configMap", src.ConfigMap.Name
	case src.Secret != nil:
		out.Type, out.Source = "secret", src.Secret.SecretName
	case src.PersistentVolumeClaim != nil:
		out.Type, out.Source = "persistentVolumeClaim", src.PersistentVolumeClaim.ClaimName
	case src.EmptyDir != nil:
		out.Type = "emptyDir"
	case src.Projected != nil:
		out.Type = "projected"
	case src.DownwardAPI != nil:
		out.Type = "downwardAPI"
	case src.Ephemeral != nil:
		out.Type = "ephemeral"
	case src.CSI != nil:
		out.Type = "csi"
	default:
		out.Type = "other"
	}
	return out
}

func container(c *corev1.Container) snapshot.Container {
	out := snapshot.Container{
		Name:            c.Name,
		Image:           c.Image,
		ImagePullPolicy: string(c.ImagePullPolicy),
		Resources: snapshot.Resources{
			Requests: quantities(c.Resources.Requests),
			Limits:   quantities(c.Resources.Limits),
		},
		ReadinessProbe: probe(c.ReadinessProbe),
		LivenessProbe:  probe(c.LivenessProbe),
		StartupProbe:   probe(c.StartupProbe),
	}
	if sc := c.SecurityContext; sc != nil {
		out.SecurityContext = &snapshot.ContainerSecurityContext{
			RunAsNonRoot:             sc.RunAsNonRoot,
			RunAsUser:                sc.RunAsUser,
			Privileged:               sc.Privileged,
			AllowPrivilegeEscalation: sc.AllowPrivilegeEscalation,
			ReadOnlyRootFilesystem:   sc.ReadOnlyRootFilesystem,
		}
		if caps := sc.Capabilities; caps != nil {
			out.SecurityContext.Capabilities = &snapshot.Capabilities{
				Add:  capabilityNames(caps.Add),
				Drop: capabilityNames(caps.Drop),
			}
		}
	}
	for _, p := range c.Ports {
		out.Ports = append(out.Ports, snapshot.ContainerPort{
			Name: p.Name, ContainerPort: p.ContainerPort, Protocol: string(p.Protocol),
		})
	}
	// Literal env values are dropped here: only the name and the kind of
	// source survive, so values can never reach a report or a prompt.
	for _, e := range c.Env {
		out.Env = append(out.Env, snapshot.EnvVar{Name: e.Name, SourceType: envSource(&e)})
	}
	return out
}

func envSource(e *corev1.EnvVar) string {
	switch vf := e.ValueFrom; {
	case vf == nil:
		return "value"
	case vf.ConfigMapKeyRef != nil:
		return "configMapKeyRef"
	case vf.SecretKeyRef != nil:
		return "secretKeyRef"
	case vf.FieldRef != nil:
		return "fieldRef"
	case vf.ResourceFieldRef != nil:
		return "resourceFieldRef"
	default:
		return "other"
	}
}

func capabilityNames(cs []corev1.Capability) []string {
	if len(cs) == 0 {
		return nil
	}
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

func quantities(rl corev1.ResourceList) map[string]string {
	if len(rl) == 0 {
		return nil
	}
	out := make(map[string]string, len(rl))
	for name, q := range rl {
		out[string(name)] = q.String()
	}
	return out
}

func probe(p *corev1.Probe) *snapshot.Probe {
	if p == nil {
		return nil
	}
	out := &snapshot.Probe{
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       p.PeriodSeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
		FailureThreshold:    p.FailureThreshold,
	}
	switch h := p.ProbeHandler; {
	case h.HTTPGet != nil:
		out.Handler, out.Path, out.Port = "httpGet", h.HTTPGet.Path, h.HTTPGet.Port.String()
	case h.TCPSocket != nil:
		out.Handler, out.Port = "tcpSocket", h.TCPSocket.Port.String()
	case h.GRPC != nil:
		out.Handler, out.Port = "grpc", strconv.Itoa(int(h.GRPC.Port))
	case h.Exec != nil:
		out.Handler = "exec"
	default:
		out.Handler = "unknown"
	}
	return out
}

// Service normalizes a core/v1 Service.
func Service(s *corev1.Service) snapshot.Service {
	out := snapshot.Service{ObjectMeta: objectMeta(s.ObjectMeta), Type: string(s.Spec.Type), Selector: nonEmpty(s.Spec.Selector)}
	for _, p := range s.Spec.Ports {
		sp := snapshot.ServicePort{Name: p.Name, Port: p.Port, Protocol: string(p.Protocol)}
		if p.TargetPort.String() != "0" {
			sp.TargetPort = p.TargetPort.String()
		}
		out.Ports = append(out.Ports, sp)
	}
	return out
}

// PodDisruptionBudget normalizes a policy PodDisruptionBudget (v1 and
// v1beta1 share this shape).
func PodDisruptionBudget(apiVersion string, p *policyv1.PodDisruptionBudget) snapshot.PodDisruptionBudget {
	out := snapshot.PodDisruptionBudget{
		APIVersion: apiVersion,
		ObjectMeta: objectMeta(p.ObjectMeta),
		Selector:   selectorLabels(p.Spec.Selector),
	}
	if p.Spec.MinAvailable != nil {
		out.MinAvailable = p.Spec.MinAvailable.String()
	}
	if p.Spec.MaxUnavailable != nil {
		out.MaxUnavailable = p.Spec.MaxUnavailable.String()
	}
	return out
}

// HorizontalPodAutoscaler normalizes an autoscaling/v2 HPA. Older versions
// are converted to v2 by the caller.
func HorizontalPodAutoscaler(apiVersion string, h *autoscalingv2.HorizontalPodAutoscaler) snapshot.HorizontalPodAutoscaler {
	out := snapshot.HorizontalPodAutoscaler{
		APIVersion:  apiVersion,
		ObjectMeta:  objectMeta(h.ObjectMeta),
		TargetRef:   snapshot.ScaleTargetRef{Kind: h.Spec.ScaleTargetRef.Kind, Name: h.Spec.ScaleTargetRef.Name},
		MinReplicas: h.Spec.MinReplicas,
		MaxReplicas: h.Spec.MaxReplicas,
	}
	for _, m := range h.Spec.Metrics {
		hm := snapshot.HPAMetric{Type: string(m.Type)}
		if m.Resource != nil {
			hm.Resource = string(m.Resource.Name)
			hm.Target = string(m.Resource.Target.Type)
		}
		if m.ContainerResource != nil {
			hm.Resource = string(m.ContainerResource.Name)
			hm.Target = string(m.ContainerResource.Target.Type)
		}
		out.Metrics = append(out.Metrics, hm)
	}
	return out
}

// Namespace normalizes a core/v1 Namespace.
func Namespace(n *corev1.Namespace) snapshot.Namespace {
	return snapshot.Namespace{Name: n.Name, Labels: nonEmpty(n.Labels)}
}

// configRef records a ConfigMap or Secret by name and sorted key names.
func configRef(m metav1.ObjectMeta, typ string, keys []string) snapshot.ConfigRef {
	sort.Strings(keys)
	return snapshot.ConfigRef{ObjectMeta: objectMeta(m), Type: typ, Keys: keys}
}
