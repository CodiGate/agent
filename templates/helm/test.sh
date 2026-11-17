#!/usr/bin/env bash
# Tests the Helm chart. Runs in CI and locally.
#
#   ./test.sh            helm lint and helm template with several value sets (no cluster needed)
#   ./test.sh install    installs the chart in the current kubectl context and checks the agent starts
#
# install uses a dummy token and the unreachable edge https://edge.invalid, so it never talks to a
# real edge. It passes when every pod runs without restarts and the log shows that the agent started
# and tried to connect. Set IMAGE_REPOSITORY and IMAGE_TAG to test a locally built image.
set -euo pipefail

cd "$(dirname "$0")"
chart=codigate-agent
helm=${HELM:-helm}
kubectl=${KUBECTL:-kubectl}

fail() { echo "FAIL: $*" >&2; exit 1; }

static() {
  "$helm" lint "$chart" --strict --set token=cga_lint
  "$helm" lint "$chart" --strict -f "$chart/ci/existing-values.yaml"

  # Default values with a token: Secret, ConfigMap, ServiceAccount, Deployment.
  out=$("$helm" template t "$chart" --set token=cga_dummy)
  for kind in Secret ConfigMap ServiceAccount Deployment; do
    grep -q "^kind: $kind$" <<<"$out" || fail "default values: no $kind"
  done
  grep -q 'image: "ghcr.io/codigate/agent:v' <<<"$out" || fail "image tag does not default to v<appVersion>"
  grep -q 'readOnlyRootFilesystem: true' <<<"$out" || fail "no read-only root file system"
  grep -q 'automountServiceAccountToken: false' <<<"$out" || fail "service account token is mounted"
  if grep -q 'cga_dummy' <<<"$out"; then fail "token in plain text"; fi

  # Existing Secret and ConfigMap: the chart creates neither.
  out=$("$helm" template t "$chart" -f "$chart/ci/existing-values.yaml")
  if grep -Eq '^kind: (Secret|ConfigMap)$' <<<"$out"; then fail "existing values: chart still creates a Secret or ConfigMap"; fi
  grep -q 'name: my-codigate-token' <<<"$out" || fail "existing values: Secret not used"
  grep -q 'key: agent-token' <<<"$out" || fail "existing values: Secret key not used"
  grep -q 'name: my-codigate-config' <<<"$out" || fail "existing values: ConfigMap not used"
  grep -q 'kind: PodDisruptionBudget' <<<"$out" || fail "existing values: no PodDisruptionBudget"
  grep -q 'image: "ghcr.io/codigate/agent@sha256:' <<<"$out" || fail "existing values: digest not used"

  # Values that must be refused.
  refuse "no token" --set token=
  refuse "catch-all not last" --set token=cga_x --set 'config.ingress[0].hostname=a.example.com' --set 'config.ingress[0].service=http://a:80'
  refuse "token in config" --set token=cga_x --set config.token=cga_y
  refuse "unknown value" --set token=cga_x --set replicas=2
  refuse "bad service" --set token=cga_x --set 'config.ingress[0].service=ftp://x'
  echo "static checks passed"
}

refuse() {
  local what=$1; shift
  if "$helm" template t "$chart" "$@" >/dev/null 2>&1; then fail "accepted: $what"; fi
}

ns=codigate-chart-test
release=agent

cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    "$kubectl" -n "$ns" get pods -o wide || true
    "$kubectl" -n "$ns" describe pods | tail -40 || true
    "$kubectl" -n "$ns" logs -l app.kubernetes.io/instance="$release" --tail=50 || true
  fi
  if [ -z "${KEEP:-}" ]; then
    "$helm" uninstall "$release" -n "$ns" --wait >/dev/null 2>&1 || true
    "$kubectl" delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
  fi
}

install() {
  local args=(--set token=cga_dummy_chart_test --set edge=https://edge.invalid --set replicaCount=2
    --set podDisruptionBudget.enabled=true)
  [ -n "${IMAGE_REPOSITORY:-}" ] && args+=(--set "image.repository=$IMAGE_REPOSITORY")
  [ -n "${IMAGE_TAG:-}" ] && args+=(--set "image.tag=$IMAGE_TAG")
  trap cleanup EXIT

  "$helm" install "$release" "$chart" -n "$ns" --create-namespace --wait --timeout 3m "${args[@]}"
  "$kubectl" -n "$ns" rollout status deploy/"$release-$chart" --timeout 2m

  # Give the agent time to try the edge a few times; it must keep running.
  local logs=""
  for _ in $(seq 1 30); do
    logs=$("$kubectl" -n "$ns" logs -l app.kubernetes.io/instance="$release" --tail=-1 --prefix)
    [ "$(grep -c 'tunnel disconnected, reconnecting' <<<"$logs")" -ge 2 ] && break
    sleep 2
  done
  echo "$logs"
  [ "$(grep -c 'starting codigate-agent' <<<"$logs")" -ge 2 ] || fail "not every pod logged 'starting codigate-agent'"
  grep -q 'edge=https://edge.invalid' <<<"$logs" || fail "the agent did not pick up the edge"
  grep -q 'tunnel disconnected, reconnecting' <<<"$logs" || fail "the agent did not try to connect"
  if grep -q 'cga_dummy_chart_test' <<<"$logs"; then fail "the token is in the log"; fi

  local phases restarts
  phases=$("$kubectl" -n "$ns" get pods -l app.kubernetes.io/instance="$release" -o jsonpath='{.items[*].status.phase}')
  restarts=$("$kubectl" -n "$ns" get pods -l app.kubernetes.io/instance="$release" -o jsonpath='{.items[*].status.containerStatuses[0].restartCount}')
  echo "phases: $phases; restarts: $restarts"
  [ "$phases" = "Running Running" ] || fail "pods are not Running: $phases"
  [ "$restarts" = "0 0" ] || fail "containers restarted: $restarts"
  "$kubectl" -n "$ns" get pdb "$release-$chart" >/dev/null || fail "no PodDisruptionBudget"
  echo "install test passed"
}

case "${1:-static}" in
  static) static ;;
  install) install ;;
  *) echo "usage: $0 [static|install]" >&2; exit 2 ;;
esac
