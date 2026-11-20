#!/usr/bin/env bash
# Integration test for the cluster collector (#7).
#
# Creates a kind cluster (unless one named $KIND_CLUSTER exists), applies the
# read-only ClusterRole from deploy/rbac and some workloads, then runs
# k8s-doctor as the k8s-doctor ServiceAccount, which has nothing but that
# role. Passing proves the collector needs no write verbs and never exposes
# Secret, ConfigMap or env values.
#
# Usage: test/kind/run.sh   (or: make test-kind)
# It also applies internal/collector/testdata/crossmode and runs the
# cross-mode test (#9): the spec collected from the cluster must match the
# spec built from those manifests.
#
# Env:   KIND, KUBECTL, K8S_DOCTOR, GO (binaries), KIND_CLUSTER, KEEP_CLUSTER=1
set -euo pipefail

cd "$(dirname "$0")/../.."
KIND=${KIND:-kind}
KUBECTL=${KUBECTL:-kubectl}
K8S_DOCTOR=${K8S_DOCTOR:-bin/k8s-doctor}
GO=${GO:-go}
CLUSTER=${KIND_CLUSTER:-k8s-doctor-test}
CTX="kind-${CLUSTER}"
WORK=$(mktemp -d)

created=0
cleanup() {
  rm -rf "$WORK"
  if [[ $created == 1 && ${KEEP_CLUSTER:-0} != 1 ]]; then
    "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok: $*"; }

if ! "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  "$KIND" create cluster --name "$CLUSTER" --wait 120s
  created=1
fi

"$KUBECTL" --context "$CTX" apply -f deploy/rbac/readonly-clusterrole.yaml >/dev/null
"$KUBECTL" --context "$CTX" apply -f test/kind/workloads.yaml >/dev/null
"$KUBECTL" --context "$CTX" apply -f internal/collector/testdata/crossmode >/dev/null
# Let the Deployment create its ReplicaSet and pod so runtime data exists.
for _ in $(seq 1 30); do
  [[ -n $("$KUBECTL" --context "$CTX" -n shop get pods -l app=api -o name 2>/dev/null) ]] && break
  sleep 2
done

# A kubeconfig that authenticates as a ServiceAccount: sa_kubeconfig NS NAME OUT
server=$("$KUBECTL" config view --raw --minify --context "$CTX" -o jsonpath='{.clusters[0].cluster.server}')
ca=$("$KUBECTL" config view --raw --minify --context "$CTX" -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
sa_kubeconfig() {
  local token
  token=$("$KUBECTL" --context "$CTX" -n "$1" create token "$2" --duration=10m)
  cat >"$3" <<KCFG
apiVersion: v1
kind: Config
clusters: [{name: kind, cluster: {server: "$server", certificate-authority-data: "$ca"}}]
users: [{name: $2, user: {token: "$token"}}]
contexts: [{name: k8s-doctor-readonly, context: {cluster: kind, user: $2}}]
current-context: k8s-doctor-readonly
KCFG
}
sa_kubeconfig k8s-doctor k8s-doctor "$WORK/kubeconfig"
SA=(--kubeconfig "$WORK/kubeconfig")

# The identity really is read-only.
for verb in create update patch delete; do
  [[ $("$KUBECTL" "${SA[@]}" auth can-i "$verb" deployments -A) == no ]] || fail "ServiceAccount can $verb deployments"
done
[[ $("$KUBECTL" "${SA[@]}" auth can-i list pods -A) == yes ]] || fail "ServiceAccount cannot list pods"
for check in "get secrets" "watch pods"; do
  # shellcheck disable=SC2086
  [[ $("$KUBECTL" "${SA[@]}" auth can-i $check -A) == no ]] || fail "ServiceAccount can $check"
done
pass "ServiceAccount has read verbs only"

# Collect under that identity. Any RBAC denial fails the run.
"$K8S_DOCTOR" snapshot --cluster "${SA[@]}" >"$WORK/snapshot.json" 2>"$WORK/stderr" \
  || { cat "$WORK/stderr" >&2; fail "snapshot --cluster failed"; }
pass "collected a snapshot with the read-only ClusterRole"

jq -e '.mode == "cluster" and .source.cluster == "k8s-doctor-readonly" and (.runtime | type == "object")' "$WORK/snapshot.json" >/dev/null \
  || fail "snapshot header is wrong"
for ref in Deployment/shop/api CronJob/shop/nightly; do
  jq -e --arg r "$ref" '[.spec.workloads[] | "\(.kind)/\(.namespace)/\(.name)"] | index($r) != null' "$WORK/snapshot.json" >/dev/null \
    || fail "$ref missing from snapshot"
done
jq -e '[.spec.secrets[].name] | index("api-db") != null' "$WORK/snapshot.json" >/dev/null || fail "Secret api-db not recorded by name"
jq -e '[.spec.workloads[] | .namespace] | all(. != "kube-system")' "$WORK/snapshot.json" >/dev/null || fail "system namespace collected"
jq -e '[.runtime.pods[] | select(.owner.kind == "Deployment" and .owner.name == "api")] | length > 0' "$WORK/snapshot.json" >/dev/null \
  || fail "api pod not attributed to its Deployment"
pass "snapshot contains the expected workloads, secrets by name, and pod ownership"

for leak in kind-test-secret-value-do-not-leak kind-test-configmap-value kind-test-env-value \
  crossmode-secret-value crossmode-configmap-value crossmode-env-value; do
  if grep -q "$leak" "$WORK/snapshot.json"; then fail "snapshot contains $leak"; fi
done
pass "no Secret, ConfigMap or env values in the snapshot"

# The namespaced Role: one namespace, no Secret access at all.
sed 's/my-app/shop/g' deploy/rbac/readonly-role-namespaced.yaml | "$KUBECTL" --context "$CTX" apply -f - >/dev/null
sa_kubeconfig shop k8s-doctor "$WORK/kubeconfig-ns"
NS=(--kubeconfig "$WORK/kubeconfig-ns")
for check in "list secrets" "get secrets"; do
  # shellcheck disable=SC2086
  [[ $("$KUBECTL" "${NS[@]}" -n shop auth can-i $check) == no ]] || fail "namespaced ServiceAccount can $check"
done
[[ $("$KUBECTL" "${NS[@]}" auth can-i list pods -A) == no ]] || fail "namespaced ServiceAccount can list pods cluster-wide"
"$K8S_DOCTOR" snapshot --cluster "${NS[@]}" --namespace shop --skip-secrets >"$WORK/snapshot-ns.json" 2>"$WORK/stderr" \
  || { cat "$WORK/stderr" >&2; fail "snapshot with the namespaced Role failed"; }
jq -e '([.spec.workloads[] | "\(.kind)/\(.namespace)/\(.name)"] | index("Deployment/shop/api") != null) and (.spec.secrets == null)' "$WORK/snapshot-ns.json" >/dev/null \
  || fail "namespaced snapshot is missing workloads or has secrets"
pass "collected one namespace with the namespaced Role and --skip-secrets"

# Cross-mode: cluster spec equals manifest spec, as the read-only identity.
K8S_DOCTOR_KIND_KUBECONFIG="$WORK/kubeconfig" "$GO" test -count=1 -run 'TestCrossModeKind$' -v ./internal/collector \
  || fail "cluster and manifest modes disagree on spec"
pass "cluster and manifest modes agree on spec"

# The full check runs too; exit codes 10-13 just mean findings. It covers
# every non-system namespace, shop and crossmode alike, so these assertions
# only require some findings and a findings exit code, never exact counts.
set +e
"$K8S_DOCTOR" check --cluster "${SA[@]}" --format json --report "$WORK/report.json" >/dev/null 2>"$WORK/stderr"
code=$?
set -e
[[ $code == 0 || ($code -ge 10 && $code -le 13) ]] || { cat "$WORK/stderr" >&2; fail "check --cluster exited $code"; }
jq -e '.input.mode == "cluster" and .summary.total > 0' "$WORK/report.json" >/dev/null || fail "report is empty or not cluster mode"
pass "check --cluster produced a report (exit $code)"

echo "kind integration test passed"
