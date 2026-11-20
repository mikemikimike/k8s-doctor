# Devlog

Short notes on what was built and what was learned, newest first.

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
