#!/usr/bin/env bash
# End-to-end dry run, after deploy/up.sh (skeleton PR 8): runs
# orchestrator/cmd/orchestrator for ORCH_GENERATIONS mutation attempts
# (default 2) on the kind cluster at CI size, with driver/internal/fakeclaude
# as -claude under real bwrap. The fake's dry-run mode (it answers a real
# BuildPrompt prompt) appends a comment to tunables.go in the first attempt,
# which passes the gate, and breaks the build in an attempt whose prompt
# lists exactly one earlier attempt, which fails gate Stage 1. Loadgen runs
# at r = 2 with 30 s windows (the manifest's 5 min W patched down, so the
# run fits a CI job). Checks, after the run:
#   (a) the orchestrator exits 0 and summary.json says reason "generations";
#   (b) attempts.jsonl holds generation 0 (gate passed, scored) and one
#       record per attempt; at least one attempt passed the gate and was
#       scored, and at least one failed at stage 1;
#   (c) every gate-passing genome is in the archive and every mutation has
#       its transcript;
#   (d) the cluster agrees with the records: a variant namespace exists for
#       exactly the variants recorded as currently live, and none for a
#       culled or gate-failed one;
#   (e) run.json records allow_unpinned true and a host_clock object.
# The orchestrator gets -allow-unpinned: the CI runner cannot pin its clocks.
# Not covered: a cull (the population cap is 3, and 4 live variants do not
# fit the CI node), so (d) shows only that live and recorded agree. If
# attempt 1 fails for a real reason, attempt 2 sees one earlier attempt
# and breaks the build too, and the job fails on "no mutation passed".
# The first CI run is also the capacity test (a Pending pod in ek8s-v*).
# Exits non-zero on the first failed check. On exit it switches stress-ng
# off and deletes every variant namespace and the loadgen Deployment, so
# that a smoke run after this one finds none. Refuses to start if a
# variant namespace or a pod labelled app=loadgen exists. Run from
# anywhere; needs go, docker, kind, kubectl, jq, bwrap, nsenter and the
# gate's tools (gcc and a static libc for Stage 4's -race build).
set -euo pipefail
cd "$(dirname "$0")/.."

cluster=ek8s
ctx="kind-$cluster"
sys=ek8s-system
rate=2
window=30
gens=${ORCH_GENERATIONS:-2}

k() { kubectl --context "$ctx" "$@"; }
die() { echo "orchestrator-smoke.sh: FAIL: $*" >&2; exit 1; }
ok() { echo "orchestrator-smoke.sh: ok: $*"; }
# The node's request budget, to see the margin the variants have.
allocated() {
	echo "orchestrator-smoke.sh: node Allocated resources ($1):"
	k describe node 2>&1 | sed -n '/^Allocated resources:/,/^Events:/p' || true
}

for tool in go docker kind kubectl jq bwrap nsenter gcc; do
	command -v "$tool" >/dev/null || die "$tool not on PATH"
done
k get nodes >/dev/null || die "cluster unreachable (context $ctx); run deploy/up.sh"
k get namespace "$sys" >/dev/null 2>&1 || die "namespace $sys missing; run deploy/up.sh"
[ -z "$(k -n "$sys" get pods -l app=loadgen -o name)" ] ||
	die "pods labelled app=loadgen exist in $sys (a running loadgen, or a failed earlier smoke?); delete them first"
# The earlier smokes delete their namespaces with --wait=false: let those
# finish. One that stays is a failed earlier run's, and the orchestrator
# would refuse to start with it.
k wait --for=delete namespace -l ek8s.io/role=variant --timeout=120s >/dev/null 2>&1 || true
[ -z "$(k get namespaces -l ek8s.io/role=variant -o name)" ] ||
	die "variant namespaces exist (a failed earlier run?); delete them first"

work=$(mktemp -d)
cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		# The run directory goes with $work: show it first.
		cat "$work/run/attempts.jsonl" "$work/run/summary.json" 2>&1 | cut -c1-600 || true
		allocated "on failure"
		k get pods -A -o wide 2>&1 | tail -n 40 || true
		k -n "$sys" logs deploy/loadgen --tail=40 2>&1 || true
	fi
	k -n ek8s-stress scale deploy/stress-ng --replicas=0 >/dev/null 2>&1 || true
	k delete namespace -l ek8s.io/role=variant --ignore-not-found --wait=false >/dev/null 2>&1 || true
	k -n "$sys" delete deploy/loadgen --ignore-not-found --wait=true >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT

allocated "at start"
deploy/loadgen-up.sh "$rate"
# CI size, as stage5-smoke.sh: loadgen's and stress-ng's CPU requests
# shrink (limits stay), else the node cannot hold them with the variants.
# The window shrinks too: the manifest's 5 min W would not fit the job.
# The patch replaces the pod (Recreate), so a new boot_id, before any run.
k -n "$sys" patch deploy/loadgen --type=json -p '[
	{"op":"replace","path":"/spec/template/spec/containers/0/resources/requests/cpu","value":"100m"},
	{"op":"replace","path":"/spec/template/spec/containers/0/args/1","value":"-window='"$window"'s"}]'
k -n "$sys" rollout status deploy/loadgen --timeout=180s || die "loadgen not Ready after the patch"
k -n ek8s-stress patch deploy/stress-ng --type=json -p \
	'[{"op":"replace","path":"/spec/template/spec/containers/0/resources/requests","value":{"cpu":"10m","memory":"16Mi"}}]'

# The gate's go commands run with GOPROXY=off.
go mod download
CGO_ENABLED=0 go build -trimpath -o "$work/claude" ./driver/internal/fakeclaude
fakever=$("$work/claude" --version | cut -d' ' -f1)
mkdir "$work/scratch"

# The fake takes any token; the orchestrator reads it and removes it from
# its own environment, as in a real run.
set +e
CLAUDE_CODE_OAUTH_TOKEN=dry-run-token go run ./orchestrator/cmd/orchestrator \
	-run "$work/run" -rate "$rate" -window "$window" -generations "$gens" \
	-claude "$work/claude" -claude-version "$fakever" -scratch "$work/scratch" \
	-allow-unpinned \
	2>&1 | tee "$work/orchestrator.out"
status=${PIPESTATUS[0]}
set -e
[ "$status" -eq 0 ] || die "the orchestrator exited $status"
ok "the orchestrator exited 0"
allocated "after the run"

run="$work/run"
[ "$(jq -r .reason "$run/summary.json")" = generations ] ||
	die "summary.json reason is $(jq -r .reason "$run/summary.json"), want generations"
ok "summary.json: reason generations"

# The CI runner cannot pin its clocks; host_clock is {} where it has no cpufreq.
jq -e '.allow_unpinned == true and (.host_clock | type) == "object"' "$run/run.json" >/dev/null ||
	die "run.json does not record allow_unpinned true and a host_clock object: $(cut -c1-300 "$run/run.json")"
ok "run.json: allow_unpinned, host_clock recorded"

# One record per id as last written (an id can be written more than once).
last=$(jq -s 'group_by(.generation_id) | map(last)' "$run/attempts.jsonl")
n=$(jq length <<<"$last")
[ "$n" -eq $((gens + 1)) ] || die "attempts.jsonl holds $n generations, want $((gens + 1))"
jq -e '.[] | select(.generation_id == "0" and .gate.passed and .fitness != null)' <<<"$last" >/dev/null ||
	die "generation 0 is not recorded as gate-passed and scored"
jq -e '[.[] | select(.parent_id != "" and .gate.passed and .fitness != null)] | length >= 1' <<<"$last" >/dev/null ||
	die "no mutation passed the gate and was scored"
jq -e '[.[] | select(.parent_id != "" and (.gate.passed | not) and .gate.fail_stage == 1)] | length >= 1' <<<"$last" >/dev/null ||
	die "no mutation failed gate stage 1"
ok "attempts.jsonl: $n generations, one scored mutation, one stage 1 failure"

for id in $(jq -r '.[] | select(.gate.passed) | .generation_id' <<<"$last"); do
	[ -d "$run/genomes/$id" ] || die "no archived genome for passed generation $id"
done
for id in $(jq -r '.[] | select(.parent_id != "") | .generation_id' <<<"$last"); do
	[ -s "$run/transcripts/$id.json" ] || die "no transcript for mutation $id"
done
ok "genomes archived, transcripts written"

# The cluster matches the records: only the currently live have a namespace.
for id in $(jq -r '.[] | select(.currently_live) | .generation_id' <<<"$last"); do
	k get namespace "ek8s-v$id" >/dev/null 2>&1 || die "generation $id is recorded live but ek8s-v$id is missing"
done
for id in $(jq -r '.[] | select(.currently_live | not) | .generation_id' <<<"$last"); do
	! k get namespace "ek8s-v$id" >/dev/null 2>&1 || die "generation $id is not live but ek8s-v$id exists"
done
live=$(jq '[.[] | select(.currently_live)] | length' <<<"$last")
have=$(k get namespaces -l ek8s.io/role=variant -o name | wc -l)
[ "$have" -eq "$live" ] || die "the cluster holds $have variant namespaces, the records $live live"
ok "the cluster holds exactly the $live live variants"
