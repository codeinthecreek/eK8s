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
# off and deletes the loadgen Deployment (its targets with it) and the
# candidate's namespace, and waits until the namespace is gone (it
# terminates for a while, its pod has a grace period): stage5-runs.sh's G3
# refuses while ek8s-v999998 exists, even terminating. A namespace that is
# not gone in 180 s fails the smoke. A smoke run after this one finds
# neither. The final line "stage5-smoke.sh: all checks passed" comes only
# after that.
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
	k -n "$sys" delete deploy/loadgen --ignore-not-found --wait=true >/dev/null 2>&1 || true
	# --wait=true returns once the namespace object is gone, not when the
	# delete is accepted (the earlier --wait=false let the script report PASS
	# with ek8s-v999998 still Terminating).
	if ! k delete namespace "$ns" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 ||
		k get namespace "$ns" >/dev/null 2>&1; then
		echo "stage5-smoke.sh: FAIL: namespace $ns is not gone after 180s" >&2
		[ "$status" -ne 0 ] || status=1
	fi
	rm -rf "$work"
	[ "$status" -ne 0 ] || echo "stage5-smoke.sh: all checks passed"
	exit "$status"
}
trap cleanup EXIT

deploy/loadgen-up.sh "$rate"

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

# At least one, not exactly one: the burst can push the HPA (70% of the
# CPU request) to scale up, and a second Ready replica is no failure.
ready=$(k -n "$ns" get deploy hasher -o jsonpath='{.status.readyReplicas}')
[ "${ready:-0}" -ge 1 ] 2>/dev/null ||
	die "Deployment hasher in $ns is not Ready after the pass (readyReplicas ${ready:-0})"
ok "the candidate is still Ready in $ns"

k get --raw "$proxy/targets" | grep -q "\"id\":\"$id\"" ||
	die "loadgen does not hold target $id after the pass"
ok "loadgen holds target $id"

node=$(k get nodes -o jsonpath='{.items[0].metadata.name}')
# Into a variable first: grep -q would end the pipe early and SIGPIPE
# kubectl under pipefail.
metrics=$(k get --raw "/api/v1/nodes/$node/proxy/metrics/resource")
grep -Eq "^pod_cpu_usage_seconds_total\{[^}]*namespace=\"$ns\"[^}]*\} [0-9.e+-]+" <<<"$metrics" ||
	die "the kubelet reports no pod_cpu_usage_seconds_total for namespace $ns"
ok "the kubelet reports pod_cpu_usage_seconds_total for $ns"
