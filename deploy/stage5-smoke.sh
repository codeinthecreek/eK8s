#!/usr/bin/env bash
# Gate Stage 5 smoke test, after deploy/up.sh: deploys loadgen at r = 2
# (deploy/loadgen-up.sh), builds generation 0's image, switches stress-ng
# on and runs `gate -stage5` on it (one burst of 240 sends at b = 8 over
# 30 s, every hash verified on the host) and checks
#   (a) the gate exits 0 and prints PASS for pod-health, responses and
#       hash-verify;
#   (b) the candidate's Deployment is still Ready in ek8s-v<id> (a pass
#       leaves the variant where it is, DESIGN.md sec 11);
#   (c) loadgen still holds the candidate as a target;
#   (d) the kubelet's /metrics/resource, through the API server, reports
#       pod_cpu_usage_seconds_total for the candidate's pod (the counter
#       fitness/ reads, DESIGN.md sec 9).
# Exits non-zero on the first failed check. On exit it switches stress-ng
# off and deletes the candidate's namespace and the loadgen Deployment
# (its targets with it), so that a smoke run after this one finds neither.
# Refuses to start if a pod labelled app=loadgen or the namespace exists.
# Run from anywhere; needs go, docker, kind and kubectl.
set -euo pipefail
cd "$(dirname "$0")/.."

cluster=ek8s
ctx="kind-$cluster"
sys=ek8s-system
rate=2
# An id no other smoke and no real variant gets.
id=999998
ns="ek8s-v$id"
proxy=/api/v1/namespaces/$sys/services/loadgen:control/proxy

k() { kubectl --context "$ctx" "$@"; }
die() { echo "stage5-smoke.sh: FAIL: $*" >&2; exit 1; }
ok() { echo "stage5-smoke.sh: ok: $*"; }

for tool in go docker kind kubectl; do
	command -v "$tool" >/dev/null || die "$tool not on PATH"
done
k get nodes >/dev/null || die "cluster unreachable (context $ctx); run deploy/up.sh"
k get namespace "$sys" >/dev/null 2>&1 || die "namespace $sys missing; run deploy/up.sh"
# Before the trap below: what is found here is not this run's to delete.
[ -z "$(k -n "$sys" get pods -l app=loadgen -o name)" ] ||
	die "pods labelled app=loadgen exist in $sys (a running loadgen, or a failed earlier smoke?); delete them first"
! k get namespace "$ns" >/dev/null 2>&1 ||
	die "namespace $ns exists (a failed earlier run?); delete it first"

work=$(mktemp -d)
cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		k -n "$ns" describe pods 2>&1 | tail -n 60 || true
		k -n "$sys" logs deploy/loadgen --tail=40 2>&1 || true
	fi
	k -n ek8s-stress scale deploy/stress-ng --replicas=0 >/dev/null 2>&1 || true
	k delete namespace "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	k -n "$sys" delete deploy/loadgen --ignore-not-found --wait=true >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT

deploy/loadgen-up.sh "$rate"
# At CI size the node cannot hold loadgen's 750m request, the system pods,
# stress-ng and the candidate's 250m together (the hasher pod stayed
# Unschedulable), so loadgen's request shrinks here; its limit stays. The
# patch replaces the pod (Recreate), so a new boot_id, before any run.
k -n "$sys" patch deploy/loadgen --type=json -p \
	'[{"op":"replace","path":"/spec/template/spec/containers/0/resources/requests/cpu","value":"100m"}]'
k -n "$sys" rollout status deploy/loadgen --timeout=180s || die "loadgen not Ready after the patch"

# Generation 0's image, as gate Stage 1 builds it: a static binary in a
# FROM scratch image tagged by the binary's sha256 (gate/docker.go).
CGO_ENABLED=0 go build -trimpath -o "$work/service" ./cmd/service
cat >"$work/Dockerfile" <<'EOF'
FROM scratch
COPY service /service
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/service"]
EOF
image="ek8s-wrapper:$(sha256sum "$work/service" | cut -c1-12)"
docker build -q -t "$image" "$work" >/dev/null

# The orchestrator's job in a gap: stress-ng on, Running, before Stage 5.
# At CI size the node has no CPU left for stress-ng's 500m request, so
# its requests shrink here;
# its limits stay, so it still contends. deploy/up.sh restores the
# requests on a re-run (apply -k).
k -n ek8s-stress patch deploy/stress-ng --type=json -p \
	'[{"op":"replace","path":"/spec/template/spec/containers/0/resources/requests","value":{"cpu":"10m","memory":"16Mi"}}]'
k -n ek8s-stress scale deploy/stress-ng --replicas=1
k -n ek8s-stress rollout status deploy/stress-ng --timeout=180s || die "stress-ng not Ready"

go run ./gate/cmd/gate -stage5 -image "$image" -id "$id" -rate "$rate" | tee "$work/gate.out"
for check in pod-health responses hash-verify; do
	grep -Eq "^stage 5 $check +PASS$" "$work/gate.out" || die "no PASS line for $check"
done
ok "gate Stage 5 passed: pod-health, responses, hash-verify"

[ "$(k -n "$ns" get deploy hasher -o jsonpath='{.status.readyReplicas}')" = 1 ] ||
	die "Deployment hasher in $ns is not Ready after the pass"
ok "the candidate is still Ready in $ns"

k get --raw "$proxy/targets" | grep -q "\"id\":\"$id\"" ||
	die "loadgen does not hold target $id after the pass"
ok "loadgen holds target $id"

node=$(k get nodes -o jsonpath='{.items[0].metadata.name}')
k get --raw "/api/v1/nodes/$node/proxy/metrics/resource" |
	grep -Eq "^pod_cpu_usage_seconds_total\{[^}]*namespace=\"$ns\"[^}]*\} [0-9.e+-]+" ||
	die "the kubelet reports no pod_cpu_usage_seconds_total for namespace $ns"
ok "the kubelet reports pod_cpu_usage_seconds_total for $ns"
