#!/usr/bin/env bash
# Infra smoke test for the ek8s cluster, after deploy/up.sh: Calico as
# the only CNI (calico-node Ready and no kindnet, its IP pool, its
# iptables chains on the node, its CNI config the only one), the node's
# reserved resources, metrics-server serving node and pod metrics, and
# stress-ng turning on (loading the CPU) and off again. Exits non-zero on
# the first failed check. Leaves stress-ng off. Needs kubectl and docker.
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

# --- Calico (deploy/calico) is the CNI and NetworkPolicy engine, and
# kind's kindnet is gone (the kind configs disable it).
k -n kube-system rollout status ds/calico-node --timeout=60s >/dev/null ||
	die "DaemonSet kube-system/calico-node not Ready"
kindnet=$(k -n kube-system get daemonset kindnet --ignore-not-found -o name) ||
	die "cannot check kube-system for the kindnet DaemonSet"
[ -z "$kindnet" ] || die "kube-system runs kindnet beside Calico (a cluster built before Calico?)"
ok "calico-node Ready; no kindnet DaemonSet"
# The default pool calico-node created from deploy/calico's patch: kind's
# podSubnet, no IPIP tunnel.
pool=$(k get ippools.crd.projectcalico.org default-ipv4-ippool -o jsonpath='{.spec.cidr} {.spec.ipipMode}') ||
	die "cannot read IPPool default-ipv4-ippool"
[ "$pool" = "10.244.0.0/16 Never" ] || die "IPPool default-ipv4-ippool is '$pool', want '10.244.0.0/16 Never'"
ok "IPPool default-ipv4-ippool: cidr 10.244.0.0/16, ipipMode Never"
# Felix's chains in the node's filter table (the kind node is a docker
# container named like the node). cali-from-wl-dispatch ends in the
# "Unknown interface" drop that keeps an unprogrammed pod closed.
chains=$(docker exec "$node" iptables-save -t filter) || die "cannot read iptables-save on node $node"
for c in cali-FORWARD cali-from-wl-dispatch cali-to-wl-dispatch; do
	grep -q "^:$c " <<<"$chains" || die "node $node's filter table has no chain $c (Felix has not programmed it)"
done
ok "node $node: $(grep -c '^:cali-' <<<"$chains") cali- chains in the filter table"
# The node's CNI configs: only Calico's conflist (beside it install-cni
# writes calico-kubeconfig, which is no config), carrying the policy wait.
# The file names containerd's CNI loader reads (.conf, .conflist, .json).
confs=$(docker exec "$node" sh -c 'cd /etc/cni/net.d && ls -- *.conf *.conflist *.json 2>/dev/null' || true)
[ "$confs" = 10-calico.conflist ] || die "node $node's /etc/cni/net.d CNI configs are '$confs', want only 10-calico.conflist"
conflist=$(docker exec "$node" cat /etc/cni/net.d/10-calico.conflist) || die "cannot read node $node's 10-calico.conflist"
grep -q '"policy_setup_timeout_seconds": *10[,}[:space:]]' <<<"$conflist" ||
	die "node $node's 10-calico.conflist lacks \"policy_setup_timeout_seconds\": 10"
ok "node $node: /etc/cni/net.d holds one CNI config, 10-calico.conflist, with policy_setup_timeout_seconds 10"

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
