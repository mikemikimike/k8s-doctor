# ADR-0002: Collector design, snapshot schema and read-only guarantees

- **Status:** Accepted
- **Date:** 2026-10-21
- **Issue:** #6. Referenced by #7, #8, #9 and #10.

## Context

Two collectors feed the same rule engine: a cluster collector (#7) and a manifest/kustomize collector (#8). They see different things. A cluster has status, restart counts and Events; a manifest folder has none of that, but it has kustomize overlay metadata that a cluster does not. If the snapshot pretended both modes were equivalent, a rule that needs runtime data would silently find nothing on manifests and look like a pass.

## Decision

### One snapshot, three sections

The snapshot is a JSON document. The Go types in [`internal/snapshot`](../../internal/snapshot) are the normative definition; decoding rejects unknown fields so a typo in a fixture fails loudly.

```text
Snapshot
├── schemaVersion   "v1alpha1"
├── mode            "manifest" | "cluster"
├── source          cluster context, or path + overlay {name, namePrefix, nameSuffix, namespace}
├── spec            desired state, BOTH modes
│   ├── namespaces
│   ├── workloads           Deployment, StatefulSet, DaemonSet, Job, CronJob
│   │     └── template.spec: containers (image, pull policy, resources, probes,
│   │                        securityContext, ports, env names), volumes, affinity,
│   │                        topology spread, host namespaces, pod securityContext
│   ├── services
│   ├── podDisruptionBudgets
│   ├── horizontalPodAutoscalers
│   ├── configMaps          name + key names only
│   └── secrets             name + type + key names only
└── runtime         observed state, CLUSTER mode only (absent in manifest mode)
    ├── pods                phase, node, owner workload, per-container ready /
    │                       restartCount / state / reason / lastTermination (e.g. OOMKilled)
    └── events              involved object, owner workload when applicable,
                            type, reason, message, count
```

Invariants checked on every load:

- `runtime` is **absent** in manifest mode and **present** in cluster mode.
- Workload kinds are limited to the five above. Bare Pods are represented only in `runtime`, owned by their top-level workload (ReplicaSet resolves to Deployment, Job to CronJob).

### What never enters a snapshot

- Secret values. The cluster collector reads Secrets and ConfigMaps through a metadata-only client (`PartialObjectMetadata`), because a normal `list secrets` returns the data. The manifest collector drops `data` and `stringData` at parse time.
- Environment variable literal values. Only the variable name and its source type (`value`, `configMapKeyRef`, `secretKeyRef`, `fieldRef`, ...) are kept.
- ConfigMap data (keys only).

In cluster mode Secrets and ConfigMaps carry names only: key names live in `data`, which the metadata-only read never returns. Manifest mode can list key names because it parses the file locally. Rules must not depend on key names in cluster mode.

### How a rule declares the mode it needs

Every rule implements `Modes()`. A rule that reads `runtime` (crashloops, OOMKilled) returns only `cluster`. When the snapshot's mode is not in a rule's list, the engine does not call the rule and records it under "not run" with the reason, so reports never imply a check passed when it was skipped.

### Field paths point at the manifest

Findings carry a field path into the resource **as written in its manifest**, not into the snapshot, e.g. `spec.template.spec.containers[name=api].readinessProbe` (or `spec.jobTemplate.spec.template.spec...` for CronJobs). List items are addressed by name, never by index, so paths survive reordering and are usable for patches (#18) and drift diffs (#29). The stable finding ID is a hash of rule ID, resource reference and field path.

### Read-only access

The cluster collector needs exactly this, shipped as `deploy/rbac/readonly-clusterrole.yaml` in #7:

| API group | Resources | Verbs |
|---|---|---|
| `""` | namespaces, pods, services, events, configmaps, secrets | get, list, watch |
| `apps` | deployments, statefulsets, daemonsets, replicasets | get, list, watch |
| `batch` | jobs, cronjobs | get, list, watch |
| `policy` | poddisruptionbudgets | get, list, watch |
| `autoscaling` | horizontalpodautoscalers | get, list, watch |

`secrets` and `configmaps` are listed because metadata-only reads still need `list`; the collector never requests their bodies. System namespaces (`kube-system`, `kube-public`, `kube-node-lease`, `local-path-storage`) are skipped unless named with `--namespace`. The #7 test (`make test-kind`, a CI job) runs the collector in kind under a ServiceAccount bound only to this role, which proves no write verb is needed.

### How the two modes converge

Both collectors fill `spec` with the same normalization, so a workload applied to a cluster and the same manifest built locally produce the same `spec` entry. The cross-mode test (#9) compares `spec` only, after dropping fields the API server fills in when the manifest leaves them empty:

- `imagePullPolicy` (defaulted to `Always` for `:latest` or untagged images, `IfNotPresent` otherwise)
- `replicas` on Deployments and StatefulSets (defaulted to 1)
- probe `periodSeconds` (10), `timeoutSeconds` (1), `failureThreshold` (3)
- `protocol` on container and Service ports (`TCP`)
- Service `type` (`ClusterIP`) and `targetPort` (defaults to `port`)
- namespace (`default`) when the manifest has none and no overlay sets one

Rules must treat an unset field and its server default the same way. For example, the single-replica rule treats an unset `replicas` as 1.

## Consequences

- Rules see one model regardless of source, and runtime-dependent rules are skipped visibly in manifest mode.
- The snapshot is a safe artifact to save, attach to a bug report, or use as a fixture, because it never contains secret values.
- Adding a resource kind means extending `internal/snapshot`, both collectors, and the ClusterRole together; the strict decoder and the #9 cross-mode test catch a collector that falls behind.
- `v1alpha1` signals the schema may still change; any incompatible change bumps `schemaVersion`.
