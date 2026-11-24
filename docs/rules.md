# Rules

Run `k8s-doctor rules` for the list built into your binary.

| ID | Category | Default severity | Modes | Flags |
|---|---|---|---|---|
| `resources-requests-missing` | resources | medium | manifest, cluster | A container with no CPU or memory request. A limit counts as a request, since Kubernetes copies it. |
| `resources-memory-limit-missing` | resources | medium | manifest, cluster | A container with no memory limit. CPU limits are not required. |
| `probe-readiness-missing` | probes | medium | manifest, cluster | A Deployment, StatefulSet or DaemonSet container with no readiness probe. Jobs and CronJobs are skipped. |
| `probe-liveness-missing` | probes | low | manifest, cluster | Same scope, no liveness probe. |
| `image-tag-mutable` | images | medium | manifest, cluster | An image with `:latest` or no tag, unless pinned by digest. A registry port is not mistaken for a tag. |
| `image-pull-policy-stale` | images | high | manifest, cluster | A mutable image with an explicit `IfNotPresent` or `Never` pull policy. |
| `deployment-single-replica` | availability | medium | manifest, cluster | A Deployment with one replica (explicit or defaulted), unless an HPA keeps `minReplicas >= 2`. Scaled-to-zero Deployments and StatefulSets are skipped. |
| `pdb-missing` | availability | medium | manifest, cluster | A Deployment or StatefulSet running at least 2 replicas (or held there by an HPA's `minReplicas`) that no PodDisruptionBudget in its namespace selects. A PDB with an empty selector covers every pod in the namespace. |
| `pdb-blocks-drain` | availability | high | manifest, cluster | A PDB with `maxUnavailable: 0`, `minAvailable: 100%`, or an integer `minAvailable` at or above the fewest replicas of every workload it selects. Workloads scaled to zero are ignored. |
| `security-run-as-root` | security | medium | manifest, cluster | A container (init containers included) that may run as UID 0: no `runAsNonRoot: true` and no non-zero `runAsUser` on the container or the pod. Container fields override pod fields. |
| `security-privileged` | security | critical | manifest, cluster | A container with `privileged: true`. |
| `security-privilege-escalation` | security | medium | manifest, cluster | A container that does not set `allowPrivilegeEscalation: false`. Privileged containers are left to `security-privileged`. |
| `security-root-filesystem-writable` | security | low | manifest, cluster | A container without `readOnlyRootFilesystem: true`. |
| `security-host-path` | security | high | manifest, cluster | A `hostPath` volume. |
| `security-host-namespace` | security | high | manifest, cluster | A pod with `hostNetwork`, `hostPID` or `hostIPC` set to true, one finding per field. |
| `namespace-default` | hygiene | low | manifest, cluster | A workload in the `default` namespace, including manifests with no namespace that no overlay sets. |

## Configuring rules

Put a `.k8s-doctor.yaml` in the working directory, or pass `--config`:

```yaml
rules:
  probe-liveness-missing:
    enabled: false
  image-tag-mutable:
    severity: high
```

Severity overrides do not change finding IDs.
