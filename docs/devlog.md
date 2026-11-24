# Devlog

Short notes on what was built and what was learned, newest first.

## 2026-11-20: Rules batch 3: crashloops, OOMKills, spread, HPA requests, removed APIs

- `pod-crashlooping` and `container-oomkilled` read the runtime section, so they run in cluster mode only. They report against the owning workload's container path, not the Pod, and group replicas: ten crashing replicas of one Deployment are one finding, with the pod names in the evidence. A bare Pod is reported as itself.
- A crashloop is either `CrashLoopBackOff` now, or 5 or more restarts on a container that is not ready. The count catches a container caught between back-offs; it is ignored on a ready container because restart counts never reset, and a pod with five restarts over two months is not crashlooping. The snapshot has no collection time, so OOM kills cannot be filtered by age; the finding carries `lastOOMKilledAt` instead. `rollout-pods-not-ready` (#54) will cover pods that are merely not ready, so the two stay apart.
- `spread-missing` is low severity: since 1.24 the scheduler spreads pods across nodes and zones by default, so a missing constraint is a soft risk, not a broken workload.
- `hpa-missing-requests` checks every container of the target, because utilization is computed over all of them and one missing request disables the metric.
- `api-version-removed` uses a static table rather than discovery, so it works offline and on manifests no current schema would accept. It only sees kinds the snapshot models today; generic objects (#43) will extend it to Ingress, RBAC and CRDs, which the table already lists.

## 2026-11-20: Rules batch 2: PDBs, securityContext, risky defaults

- Nine rules: `pdb-missing`, `pdb-blocks-drain`, four securityContext rules (root, privileged, privilege escalation, writable root filesystem), `security-host-path`, `security-host-namespace` and `namespace-default`.
- PDB rules use the fewest replicas a workload can run: its HPA's `minReplicas` when one targets it. A `minAvailable: 2` PDB on a Deployment the HPA can scale down to 2 blocks drains at exactly that moment.
- The snapshot keeps only a PDB's `matchLabels`, so a PDB that selects with `matchExpressions` alone looks like it selects everything. That can hide a missing PDB but never invent one, which is the right way to be wrong for a default-on rule.
- Security rules cover init containers too, and treat container fields as overriding pod fields one by one, as the kubelet does. A privileged container is reported once, by `security-privileged`, rather than also as privilege escalation.
- The clean fixtures had to be hardened to stay clean, which is itself a useful check that the rules are satisfiable with ordinary manifests.
- Thresholds (2 replicas for HA) are constants for now; rule parameters arrive with #45.

## 2026-11-16: Cross-mode collector tests

- One set of manifests (`internal/collector/testdata/crossmode`, all five workload kinds plus Service, PDB, HPA, Secret and ConfigMap) is collected in manifest mode and, after `kubectl apply` in kind, in cluster mode. The `spec` sections must match once server defaults are accounted for.
- The test found a real mismatch: the API server turns an absent pod `securityContext` into `{}`. The normalizer now records an empty one as absent in both modes.
- The comparison fills unset fields with their server default rather than dropping the field, so it cannot hide a manifest that sets a different value.

## 2026-10-22: Read-only cluster collector

- client-go typed clients for workloads and runtime, plus the metadata client for Secrets and ConfigMaps so values never cross the wire. The cost is that cluster snapshots carry no key names.
- Pods are attributed to their top-level workload through owner references (ReplicaSet to Deployment, Job to CronJob), using one ReplicaSet and Job list per namespace instead of a lookup per Pod.
- `deploy/rbac` ships the minimal ClusterRole. A unit test keeps it in sync with the resources the collector reads, and `make test-kind` runs the collector as a ServiceAccount bound only to that role, then checks no planted Secret, ConfigMap or env value appears in the output.
- The fake clientset test also asserts that every request the collector makes is a `get` or `list`.

## 2026-10-22: Manifest and kustomize collector

- Overlays are built in-process with kustomize's `krusty` API, so generators, patches, image overrides and name prefixes behave exactly like `kustomize build`.
- Normalization from typed Kubernetes API objects to the snapshot lives in one place (`internal/collector/normalize.go`) so the cluster collector (#7) reuses it, which is what keeps the spec section identical across modes.
- Secrets and ConfigMaps are decoded into a struct whose value type discards its input, so values are never held as Go strings, not just left out of the output. A test greps the snapshot for every planted value.
- Dependencies are pinned to the newest releases that still build on Go 1.24 (k8s.io/api v0.34.10, kustomize/api v0.21.1). Moving the module to a newer Go would unlock current releases.

## 2026-10-22: CLI and JSON report

- Versioned JSON report (`v1`) with a JSON Schema that every fixture's report is validated against in tests. Terminal output and, later, HTML and eval all render from it.
- Exit codes 10–13 encode the worst severity at or above `--fail-on`, kept apart from 1 (error) and 2 (usage) so CI can tell a broken run from a failing one.
- `--enable-rule`/`--disable-rule` layer over the config file; naming one rule in both is an error rather than a silent precedence rule.

## 2026-10-21: Foundations

- Scaffolded the Go module, Makefile, lint (golangci-lint v2 with gofumpt/goimports), pre-commit hooks and CI.
- Wrote ADR-0001 (Go, read-only, rules find and the LLM explains, LLM data policy) and ADR-0002 (snapshot schema split into spec and runtime).
- Built the rule engine: severity model, stable finding IDs, mode-aware rules with an explicit "not run" list, config file with rule toggles and severity overrides.
- Defined the fixture format and a runner that diffs actual against expected findings, plus a test that fails if any rule lacks positive, clean and near-miss fixtures.
- First rule batch: resource requests, memory limits, readiness and liveness probes, mutable image tags, stale pull policy, single-replica Deployments.

Decisions worth remembering:

- A limit without a request is not flagged as a missing request, because Kubernetes copies the limit into the request.
- CPU limits are deliberately not required; only memory limits are, since exceeding a CPU limit throttles while unbounded memory can take down the node.
- `imagePullPolicy` is only flagged when it is explicitly `IfNotPresent`/`Never` on a mutable tag. Left unset, Kubernetes defaults it to `Always` for `:latest`.
