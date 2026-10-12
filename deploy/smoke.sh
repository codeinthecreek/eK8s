#!/usr/bin/env bash
# Infra smoke test for the ek8s cluster, after deploy/up.sh: the node's
# reserved resources, metrics-server serving node and pod metrics, and
# stress-ng turning on (loading the CPU) and off again. Exits non-zero on
# the first failed check. Leaves stress-ng off.
#
# Parameters (environment; the defaults match the full-size setup in
# deploy/kind/cluster.yaml and deploy/stress, CI sets the smaller ones):
#   EK8S_RESERVED_CPU_MILLIS    CPU the kubelet reserves (1000)
#   EK8S_STRESS_MIN_CPU_MILLIS  CPU use that shows stress-ng is loading (1000)
set -euo pipefail

ctx=kind-ek8s
reserved=${EK8S_RESERVED_CPU_MILLIS:-1000}
stress_min=${EK8S_STRESS_MIN_CPU_MILLIS:-1000}
k() { kubectl --context "$ctx" "$@"; }
die() { echo "smoke.sh: FAIL: $*" >&2; exit 1; }
ok() { echo "smoke.sh: ok: $*"; }

# millicores converts a Kubernetes CPU quantity ("8", "7500m") to millicores.
millicores() {
	case "$1" in
	*m) echo "${1%m}" ;;
	*) echo $(($1 * 1000)) ;;
	esac
}

# retry runs its arguments every 5s until they succeed or $1 seconds pass.
retry() {
	local deadline=$((SECONDS + $1))
	shift
	until "$@" >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 5
	done
}

k get nodes >/dev/null || die "cluster unreachable (context $ctx); run deploy/up.sh"

node=$(k get nodes -o jsonpath='{.items[0].metadata.name}')
cap=$(millicores "$(k get node "$node" -o jsonpath='{.status.capacity.cpu}')")
alloc=$(millicores "$(k get node "$node" -o jsonpath='{.status.allocatable.cpu}')")
[ $((cap - alloc)) -eq "$reserved" ] ||
	die "node reserves $((cap - alloc))m CPU, want ${reserved}m (the kind config's kubeReserved + systemReserved)"
ok "node $node: capacity ${cap}m, allocatable ${alloc}m"

retry 180 k top node || die "metrics-server: no node metrics after 180s"
ok "metrics-server serves node metrics"

k -n ek8s-stress scale deploy/stress-ng --replicas=1 >/dev/null
trap 'k -n ek8s-stress scale deploy/stress-ng --replicas=0 >/dev/null' EXIT
k -n ek8s-stress rollout status deploy/stress-ng --timeout=120s >/dev/null ||
	die "stress-ng did not become available"
ok "stress-ng running"

# CPU use above stress_min shows the dose is loading the CPU.
stress_busy() {
	local cpu
	cpu=$(k -n ek8s-stress top pod -l app=stress-ng --no-headers | awk '{print $2}')
	[ -n "$cpu" ] && [ "$(millicores "$cpu")" -gt "$stress_min" ]
}
retry 120 stress_busy || die "stress-ng pod metrics not above ${stress_min}m CPU after 120s"
ok "stress-ng loads the CPU: $(k -n ek8s-stress top pod -l app=stress-ng --no-headers)"

k -n ek8s-stress scale deploy/stress-ng --replicas=0 >/dev/null
trap - EXIT
no_stress_pods() { [ -z "$(k -n ek8s-stress get pods -l app=stress-ng -o name)" ]; }
retry 60 no_stress_pods || die "stress-ng pod still present 60s after scale to 0"
ok "stress-ng off"

echo "smoke.sh: PASS"
