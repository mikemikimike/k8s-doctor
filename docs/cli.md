# CLI

```console
$ k8s-doctor --cluster                                   # the current kubeconfig context, read-only
$ k8s-doctor --cluster --context prod --namespace shop   # one context, selected namespaces
$ k8s-doctor snapshot --cluster > snapshot.json          # collect only, analyze later or elsewhere
$ k8s-doctor --manifests deploy/overlays/prod           # a kustomize overlay, built in-process
$ k8s-doctor --manifests deploy/                         # a folder of plain manifests
$ k8s-doctor check --snapshot snapshot.json            # or simply: k8s-doctor --snapshot snapshot.json
$ k8s-doctor check --snapshot snapshot.json --format json > report.json
$ k8s-doctor check --snapshot snapshot.json --report report.json --fail-on medium
$ k8s-doctor rules                                      # list built-in rules
$ k8s-doctor schema                                     # JSON Schema of the report
```

Exactly one of `--cluster`, `--manifests` and `--snapshot` is required. `k8s-doctor snapshot` takes `--cluster` or `--manifests` and prints the snapshot JSON instead of a report, which is useful for sharing what was analyzed or building fixtures.

### Cluster mode

- Read-only: the collector only issues `get` and `list` requests. [`deploy/rbac/readonly-clusterrole.yaml`](../deploy/rbac/readonly-clusterrole.yaml) is the minimal ClusterRole, plus a ServiceAccount and binding to run it under.
- Every namespace is read except `kube-system`, `kube-public`, `kube-node-lease` and `local-path-storage`. Name namespaces with `--namespace` to narrow it, or to include a system namespace explicitly.
- Secrets and ConfigMaps are listed through the metadata API, so their values are never sent to k8s-doctor. In cluster mode the snapshot records their names only, not their key names.
- Pods, container statuses and Events go into `runtime`. Each Pod is attributed to its top-level workload (ReplicaSet to Deployment, Job to CronJob). Events retain their involved object and include an optional top-level owner when it is a ReplicaSet or Job. Jobs created by a CronJob are folded into the CronJob rather than listed as workloads.

### Manifests mode

- If the directory holds a `kustomization.yaml` (or `.yml`, or `Kustomization`), it is built in-process with kustomize's `krusty` API, exactly as `kustomize build` would. The overlay's `namePrefix`, `nameSuffix` and `namespace` are recorded for drift matching (#28).
- Otherwise every `.yaml`, `.yml` and `.json` file under it is parsed, including multi-document files and `kind: List`. Hidden directories are skipped. A nested directory with its own kustomization is an error, because reading its files raw would skip the overlay: point `--manifests` at that overlay instead.
- Objects without a namespace get `default`, as the API server would assign.
- Kinds the snapshot does not model (Ingress, RBAC, CRDs, ...) are ignored.
- Secret and ConfigMap values and literal env values are dropped while parsing. Only names and key names reach the snapshot.
- Helm charts are out of scope. Render them first (`helm template ... > rendered/`) and point `--manifests` at the output.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--snapshot <file>` | | Analyze a snapshot JSON file ([ADR-0002](adr/0002-collector-and-snapshot.md)). |
| `--cluster` | | Analyze a live cluster, read-only. |
| `--kubeconfig <file>` | `$KUBECONFIG` or `~/.kube/config` | Kubeconfig for `--cluster`. |
| `--context <name>` | current context | Kubeconfig context for `--cluster`. |
| `--namespace <ns>` | all but system namespaces | Namespaces for `--cluster`, repeatable or comma-separated. |
| `--timeout` | `2m` | Time limit for collecting from the cluster. |
| `--manifests <dir>` | | Analyze a folder of plain manifests, or a kustomize overlay built in-process. |
| `--config <file>` | `./.k8s-doctor.yaml` if present | Rule toggles and severity overrides ([rules](rules.md#configuring-rules)). |
| `--enable-rule`, `--disable-rule` | | Rule IDs, repeatable or comma-separated. Applied on top of the config file. Naming a rule in both is an error. |
| `--format` | `text` | `text` or `json`. |
| `--report <file>` | | Also write the JSON report to a file, whatever `--format` is. |
| `--fail-on` | `high` | Lowest severity that makes the exit code non-zero: `critical`, `high`, `medium`, `low` or `none`. |
| `--color` | `auto` | `auto` (only on a terminal, and never when `NO_COLOR` is set), `always` or `never`. |
| `--explain` | off | Print each finding's explanation under it in text output. |
| `--llm` | off | Send finding context with best-effort, pattern-based redaction to the configured LLM provider. Requires an `llm.provider` in the config; otherwise the command exits with a usage error. |
| `--no-llm` | off | Never call an LLM; explanations use deterministic rule descriptions and the report's `input.llm` is `false`. |

When `.k8s-doctor.yaml` contains an `llm` block, the `openai-compatible`
provider (also accepted as `openai`) uses `model`, optional `timeout` and
`retries`. Set `K8S_DOCTOR_LLM_ENDPOINT` and, when required, the
`K8S_DOCTOR_LLM_API_KEY` environment variable outside the repository config.
The endpoint must use HTTPS; HTTP is allowed only for loopback development.
Provider calls are disabled unless `--llm` is passed, and `--no-llm` always
disables them. The default request timeout is 30 seconds, with three retries
using exponential backoff; 429 responses honor `Retry-After` values up to two
minutes and skip retrying when the requested delay is longer. The explanation
phase has a two-minute overall deadline. Prompts use best-effort, pattern-based
credential redaction, include explicit instructions to treat cluster data as
untrusted, and cap context at 32 KiB. When a workload exceeds the context
budget, labels and selectors are trimmed first to preserve container, probe
and resource context where possible.
Provider errors fall back to the rule description and produce a warning on
stderr. `input.llm` is true only when at least one generated explanation is
included in the report. The default configuration is offline.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | No findings at or above `--fail-on`. |
| 1 | Error: unreadable input or config, unknown rule ID, failed write. |
| 2 | Usage error: bad flag value, no input or more than one. |
| 10, 11, 12, 13 | Findings at or above `--fail-on`; the worst is low, medium, high or critical. |

Codes 10–13 are distinct from 1 and 2 so a CI job can tell "the tool failed" from "the tool found problems". A typical gate:

```yaml
- run: k8s-doctor check --snapshot snapshot.json --report k8s-doctor.json --fail-on high
```

## JSON report

The JSON report is the single source of truth: the terminal output, the HTML report (#19) and the eval harness (#21) are all rendered from it. It is validated by [`internal/report/schema/report.v1.json`](../internal/report/schema/report.v1.json) (JSON Schema draft 2020-12), which `k8s-doctor schema` prints.

| Field | Contents |
|---|---|
| `schemaVersion` | `"v1"`. Additive changes keep it; removals or renames bump it. |
| `tool` | Name and version. |
| `generatedAt` | UTC timestamp. Everything else is deterministic for the same input. |
| `input` | Mode, cluster context or path, overlay name, and whether the LLM was used. |
| `summary` | Total, counts for all four severities, worst severity, rules run and not run. |
| `findings` | Most severe first. Each has its stable `id`, `ruleId`, `severity`, `resource`, `fieldPath`, `message`, `evidence`, `category` and an `explanation` with `source` `rule` or `llm`. |
| `groups` | Root-cause groups, each listing its `findingIds` (#17). Empty for now. |
| `rules` | Every registered rule with its effective severity and `status` `run` or `not-run`, with a `reason` when not run. |
