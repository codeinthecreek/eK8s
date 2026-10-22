#!/usr/bin/env bash
# loadgen smoke test, after deploy/up.sh: deploys loadgen in ek8s-system
# with deploy/loadgen-up.sh and checks, at r = 2,
#   (5) the pod becomes Ready;
#   (6) its start line carries the memory limit and GOMEMLIMIT, 90% of it;
#   (3) its canary address is the API server's ClusterIP and port, and the
#       policy canary reads refused within 60s, then once more
#       (DESIGN.md sec 11);
#   (4) its network namespace has one listening socket, TCP 9090, and no
#       bound UDP socket, checked before any run (a run's DNS lookups
#       open UDP sockets);
#   (1) every control endpoint answers through the API server's service
#       proxy: config, a target, an upload, a window run started, refused
#       deletion while busy, cancelled, its records read, the target
#       deleted, the canary; every body's boot_id is /config's;
# then, redeployed at r = 19 (a new pod, so a new boot_id),
#   (2) the largest upload loadgen takes, max(window_pairs, burst_pairs)
#       pairs of the largest size, is accepted.
# Exits non-zero on the first failed check, after printing the loadgen
# pod's description and its log. Always deletes the loadgen Deployment
# on exit. Refuses to start if a pod labelled app=loadgen exists. Run from
# anywhere; needs go, docker, kind and kubectl.
set -euo pipefail
cd "$(dirname "$0")/.."

cluster=ek8s
ctx="kind-$cluster"
sys=ek8s-system
# The same curl image as deploy/variant-smoke.sh (see the comment there);
# the two pins must match.
curl_src=curlimages/curl@sha256:43366cd60f226c7655181a0f7e85c468a41d182fdd2dc2c1c3b872a2b9d05d7a
curl_tag=ek8s-curl:43366cd60f22
# The control API through the API server's service proxy
# (deploy/ek8s-system/service.yaml).
proxy=/api/v1/namespaces/$sys/services/loadgen:control/proxy
# The smoke's target: an id no real variant gets, as in variant-smoke.sh,
# whose namespace must not exist (checked below), so that no variant is
# sent load: the window run's sends fail at DNS, and it is cancelled at
# once.
id=999999

k() { kubectl --context "$ctx" "$@"; }
die() { echo "loadgen-smoke.sh: FAIL: $*" >&2; exit 1; }
ok() { echo "loadgen-smoke.sh: ok: $*"; }

for tool in go docker kind kubectl; do
	command -v "$tool" >/dev/null || die "$tool not on PATH"
done
k get nodes >/dev/null || die "cluster unreachable (context $ctx); run deploy/up.sh"
k get namespace "$sys" >/dev/null 2>&1 || die "namespace $sys missing; run deploy/up.sh"
# Before the trap below: a pod found here is not this run's to delete.
[ -z "$(k -n "$sys" get pods -l app=loadgen -o name)" ] ||
	die "pods labelled app=loadgen exist in $sys (a running loadgen, or a failed earlier smoke?); delete them first"
! k get namespace "ek8s-v$id" >/dev/null 2>&1 ||
	die "namespace ek8s-v$id exists (a failed variant-smoke run?): the smoke would send it load; delete it first"

work=$(mktemp -d)
# On failure, the pod's description and loadgen's log; always, the
# Deployment, its pods with it (foreground), so that a smoke run after
# this one finds no app=loadgen pod.
on_exit() {
	local rc=$?
	if [ "$rc" -ne 0 ]; then
		echo "=== loadgen pods in $sys" >&2
		k -n "$sys" describe pods -l app=loadgen >&2 || true
		echo "=== loadgen's log" >&2
		k -n "$sys" logs -l app=loadgen -c loadgen --tail=-1 >&2 || true
	fi
	k -n "$sys" delete deployment loadgen --ignore-not-found --cascade=foreground --wait=true --timeout=120s >/dev/null || true
	rm -rf "$work"
	exit "$rc"
}
trap on_exit EXIT

docker pull -q "$curl_src" >/dev/null
docker tag "$curl_src" "$curl_tag"
kind load docker-image "$curl_tag" --name "$cluster"
echo "loadgen-smoke.sh: the node before loadgen:"
k describe node | sed -n '/^Allocated resources:/,/^Events:/p'

# loadgen_pod: the one app=loadgen pod not being deleted.
loadgen_pod() {
	local pods
	pods=$(k -n "$sys" get pods -l app=loadgen \
		-o jsonpath='{range .items[*]}{.metadata.name} {.metadata.deletionTimestamp}{"\n"}{end}' | awk 'NF == 1 { print $1 }')
	[ "$(grep -c . <<<"$pods")" -eq 1 ] || die "want one loadgen pod, have: ${pods:-none}"
	echo "$pods"
}

# api_get, api_post, api_put, api_delete PATH [FILE]: one control call
# through the service proxy, with FILE's bytes as the body (none if
# omitted); prints the response body. kubectl exits 0 on a 2xx status
# and does not say which; on any other it exits 1 and prints only
# "Error from server (<reason>): <message>" for the status to stderr, not
# the body. So a failed call's message is kubectl's, above the FAIL line.
: >"$work/empty"
api_get() { k get --raw "$proxy$1"; }
api_post() { k create --raw "$proxy$1" -f "${2:-$work/empty}"; }
api_put() { k replace --raw "$proxy$1" -f "${2:-$work/empty}"; }
api_delete() { k delete --raw "$proxy$1"; }

# config: GET /config, once the Service has an endpoint for the new pod:
# the EndpointSlice follows readiness asynchronously, so allow it 30s.
config() {
	local deadline=$((SECONDS + 30)) body
	until body=$(api_get /config 2>"$work/config.err"); do
		[ "$SECONDS" -lt "$deadline" ] || die "GET /config did not answer in 30s: $(cat "$work/config.err")"
		sleep 2
	done
	echo "$body"
}

# short BODY: BODY cut to 500 bytes, for a message.
short() { echo "${1:0:500}"; }

# boot_of BODY: BODY's boot_id, which is its first field.
boot_of() {
	[[ $1 =~ ^\{\"boot_id\":\"([0-9a-f]{32})\"[,}] ]] || die "no boot_id first in: $(short "$1")"
	echo "${BASH_REMATCH[1]}"
}

# same_boot WHAT BODY: BODY's boot_id is $boot, /config's.
same_boot() {
	[ "$(boot_of "$2")" = "$boot" ] || die "$1: boot_id is not /config's $boot: $(short "$2")"
}

# want WHAT BODY EXPECTED: BODY is exactly EXPECTED.
want() { [ "$2" = "$3" ] || die "$1: got $(short "$2"), want $3"; }

# int NAME BODY: the integer field NAME of a flat JSON object.
int() {
	[[ $2 =~ \"$1\":([0-9]+)[,}] ]] || die "no integer $1 in: $(short "$2")"
	echo "${BASH_REMATCH[1]}"
}

# pairs N FILE: an upload body of N pairs in compact JSON, each the
# largest the upload cap allows for (maxPairJSON, loadgen/control_run.go):
# a 1,024-byte password, a 97-byte exact-params hash and match false. The
# upload checks only that each pair has the three fields.
pairs() {
	awk -v n="$1" 'BEGIN {
		pw = sprintf("%1024s", ""); gsub(/ /, "A", pw)
		salt = sprintf("%22s", ""); gsub(/ /, "A", salt)
		sum = sprintf("%43s", ""); gsub(/ /, "A", sum)
		hash = "$argon2id$v=19$m=19456,t=2,p=1$" salt "$" sum
		printf "{\"pairs\":["
		for (i = 0; i < n; i++)
			printf "%s{\"password\":\"%s\",\"hash\":\"%s\",\"match\":false}", (i ? "," : ""), pw, hash
		printf "]}"
	}' >"$2"
}

# --- (5) r = 2: the pod becomes Ready (loadgen-up.sh waits for the rollout).
deploy/loadgen-up.sh 2 || die "(5) loadgen at r=2 not Ready"
pod=$(loadgen_pod)
ok "(5) $sys/$pod Ready at r=2"

# --- (6) the start line's memory limit (256Mi) and GOMEMLIMIT (90%).
logs=$(k -n "$sys" logs "$pod" -c loadgen) || die "(6) cannot read $pod's log"
start=$(grep -m1 'msg="loadgen starting"' <<<"$logs") || die "(6) no start line in $pod's log"
for w in mem_limit_bytes=268435456 go_mem_limit=241591911; do
	[[ " $start " == *" $w "* ]] || die "(6) start line lacks $w: $start"
done
ok "(6) start line: mem_limit_bytes=268435456 go_mem_limit=241591911"

# --- (3) the canary address is the API server's ClusterIP and port, as
# the kubelet expanded them into the args; the canary reads refused.
# kindnet enforces a new pod's policy only after a short lag (seen in CI:
# connected at about 0.7s), so the smoke waits for enforcement rather than
# asserting it at Ready: the canary is exactly what detects that gap.
# connected and error both mean poll again, every 2s for up to 60s; the
# first refused must be followed by another, so a flapping policy fails.
api=$(k -n default get service kubernetes -o jsonpath='{.spec.clusterIP}:{.spec.ports[0].port}')
[[ " $start " == *" canary_addr=$api "* ]] || die "(3) start line's canary_addr is not $api: $start"
cfg=$(config)
boot=$(boot_of "$cfg")
created=$(k -n "$sys" get pod "$pod" -o jsonpath='{.metadata.creationTimestamp}') || die "(3) cannot read $pod's creationTimestamp"
[ -n "$created" ] && created=$(date -d "$created" +%s) || die "(3) $pod's creationTimestamp is not a time: '$created'"
# canary: POST /canary, its boot_id /config's; prints its result.
canary() {
	local body
	body=$(api_post /canary) || die "(3) POST /canary failed"
	same_boot "(3) POST /canary" "$body"
	[[ $body =~ ^\{\"boot_id\":\"[0-9a-f]{32}\",\"result\":\"(connected|refused|error)\"\}$ ]] ||
		die "(3) POST /canary: $(short "$body")"
	echo "${BASH_REMATCH[1]}"
}
deadline=$((SECONDS + 60)) misses=0
while :; do
	result=$(canary)
	age=$(($(date +%s) - created))
	echo "loadgen-smoke.sh: (3) POST /canary at pod age ${age}s: $result"
	[ "$result" = refused ] && break
	misses=$((misses + 1))
	[ "$SECONDS" -lt "$deadline" ] || die "(3) POST /canary not refused in 60s: $misses results, each above"
	sleep 2
done
result=$(canary)
[ "$result" = refused ] ||
	die "(3) POST /canary at pod age $(($(date +%s) - created))s: $result, after refused at ${age}s"
ok "(3) canary_addr=$api; POST /canary refused at pod age ${age}s after $misses not refused, and again"

# --- (4) loadgen's network namespace, which an ephemeral container
# shares: one listening TCP socket, on 9090 (2382 hex), and no UDP socket
# bound to a port. The restricted profile meets the namespace's Pod
# Security level; the curl image has busybox sh and sed.
k -n "$sys" debug "$pod" --profile=restricted --image="$curl_tag" --image-pull-policy=Never \
	-c netcheck -q --attach=false -- \
	sh -c 'for f in tcp tcp6 udp udp6; do if [ -r /proc/net/$f ]; then sed "s|^|$f |" /proc/net/$f; fi; done' >/dev/null ||
	die "(4) kubectl debug failed"
deadline=$((SECONDS + 60))
until code=$(k -n "$sys" get pod "$pod" \
	-o jsonpath='{.status.ephemeralContainerStatuses[?(@.name=="netcheck")].state.terminated.exitCode}') &&
	[ -n "$code" ]; do
	[ "$SECONDS" -lt "$deadline" ] || die "(4) netcheck did not finish in 60s"
	sleep 2
done
[ "$code" = 0 ] || die "(4) netcheck exited $code: $(k -n "$sys" logs "$pod" -c netcheck 2>&1)"
net=$(k -n "$sys" logs "$pod" -c netcheck) || die "(4) cannot read netcheck's log"
echo "$net"
# Each row is "<file> <sl>: <local> <remote> <st> ...", after a header row
# whose second field is "sl".
listen=$(awk '($1 == "tcp" || $1 == "tcp6") && $5 == "0A" { print $1, $3 }' <<<"$net")
[ "$(grep -c . <<<"$listen")" -eq 1 ] && [[ $listen == *:2382 ]] ||
	die "(4) want one listening TCP socket, on port 9090 (2382 hex); have: ${listen:-none}"
awk '$1 == "udp" && $2 == "sl" { found = 1 } END { exit !found }' <<<"$net" ||
	die "(4) netcheck printed no /proc/net/udp"
udp=$(awk '($1 == "udp" || $1 == "udp6") && $2 != "sl" && $3 !~ /:0000$/ { print $1, $3 }' <<<"$net")
[ -z "$udp" ] || die "(4) UDP sockets bound to a port: $udp"
ok "(4) one listening socket ($listen), no bound UDP socket"

# --- (1) every control endpoint, through the service proxy.
want "(1) GET /config" "$(grep -o '"r":[^,]*,"window_s":[^,]*,"burst_rate":[^,]*,' <<<"$cfg")" '"r":2,"window_s":300,"burst_rate":8,'
window_pairs=$(int window_pairs "$cfg")
ok "(1) GET /config: r 2, window_s 300, burst_rate 8, window_pairs $window_pairs, boot_id $boot"

body=$(api_put "/targets/$id") || die "(1) PUT /targets/$id failed"
want "(1) PUT /targets/$id" "$body" "{\"boot_id\":\"$boot\",\"id\":\"$id\",\"next\":0}"
body=$(api_get /targets) || die "(1) GET /targets failed"
want "(1) GET /targets" "$body" "{\"boot_id\":\"$boot\",\"targets\":[{\"id\":\"$id\",\"next\":0}]}"
ok "(1) PUT /targets/$id, GET /targets"

run=smoke-window
pairs "$window_pairs" "$work/window.json"
body=$(api_put "/targets/$id/pairs/$run" "$work/window.json") || die "(1) upload failed"
want "(1) PUT /targets/$id/pairs/$run" "$body" "{\"boot_id\":\"$boot\",\"count\":$window_pairs}"
printf '{"kind":"window","targets":["%s"]}' "$id" >"$work/start.json"
body=$(api_post "/runs/$run/start" "$work/start.json") || die "(1) start failed"
same_boot "(1) POST /runs/$run/start" "$body"
[[ $body == *"\"run_id\":\"$run\",\"start_wall\":"* ]] || die "(1) POST /runs/$run/start: $(short "$body")"
ok "(1) uploaded $window_pairs pairs, started window run $run on target $id"

# through = MaxUint64: no record can be unread, so a 409 here can only be
# the busy target's.
if body=$(api_delete "/targets/$id?through=18446744073709551615" 2>&1); then
	die "(1) DELETE /targets/$id during the run succeeded: $(short "$body")"
fi
[[ $body == *"Error from server (Conflict)"* ]] ||
	die "(1) DELETE /targets/$id during the run: $body, want a 409"
body=$(api_get "/runs/$run") || die "(1) GET /runs/$run failed"
same_boot "(1) GET /runs/$run" "$body"
[[ $body == *"\"targets\":{\"$id\":{\"state\":\"running\"}}}" ]] || die "(1) GET /runs/$run: $(short "$body")"
# Cancelled once the target holds a record, so that the records read
# below end at a real Seq: GET /targets shows the Ring's Next.
deadline=$((SECONDS + 10))
until body=$(api_get /targets) && [ "$(int next "$body")" -gt 0 ]; do
	[ "$SECONDS" -lt "$deadline" ] || die "(1) target $id has no record 10s into the run: $(short "$body")"
	sleep 1
done
body=$(api_post "/runs/$run/cancel") || die "(1) cancel failed"
same_boot "(1) POST /runs/$run/cancel" "$body"
ok "(1) DELETE /targets/$id while busy: 409; GET /runs/$run: running; cancelled"

# A cancelled run's targets stay running until their sends in flight are
# recorded: within the client timeout, 7s.
deadline=$((SECONDS + 30))
while :; do
	body=$(api_get "/runs/$run") || die "(1) GET /runs/$run failed"
	same_boot "(1) GET /runs/$run" "$body"
	[[ $body == *'"state":"running"'* ]] || break
	[ "$SECONDS" -lt "$deadline" ] || die "(1) run $run still running 30s after the cancel: $(short "$body")"
	sleep 1
done
[[ $body == *"\"targets\":{\"$id\":{\"state\":\"cancelled\"}}}" ]] || die "(1) run $run ended: $(short "$body")"

# Every record, page by page, to the end ("next" is a page's only
# top-level field of that name); through is the last Seq read,
# or 0 when there are none: Ring.Next is then 0, and deleteTarget takes
# any through.
since=0 n=0
while :; do
	body=$(api_get "/targets/$id/records?since=$since") || die "(1) GET records failed"
	same_boot "(1) GET /targets/$id/records" "$body"
	[[ $body == *"\"gap\":false,\"next\":"* ]] || die "(1) records page: $(short "$body")"
	[[ $body == *'"records":[]'* ]] && break
	n=$((n + $(grep -o '"seq":' <<<"$body" | wc -l)))
	since=$(int next "$body")
done
through=0
[ "$since" -eq 0 ] || through=$((since - 1))
body=$(api_delete "/targets/$id?through=$through") || die "(1) DELETE /targets/$id?through=$through failed"
want "(1) DELETE /targets/$id?through=$through" "$body" "{\"boot_id\":\"$boot\"}"
body=$(api_post /canary) || die "(1) POST /canary failed"
want "(1) POST /canary" "$body" "{\"boot_id\":\"$boot\",\"result\":\"refused\"}"
ok "(1) run $run cancelled; read $n records; DELETE /targets/$id?through=$through: 200; POST /canary: refused"

# --- (2) r = 19: the largest upload, max(window_pairs, burst_pairs)
# pairs of the largest size, under the cap loadgen derives from them.
deploy/loadgen-up.sh 19 || die "(2) loadgen at r=19 not Ready"
pod=$(loadgen_pod)
cfg=$(config)
old_boot=$boot
boot=$(boot_of "$cfg")
[ "$boot" != "$old_boot" ] || die "(2) boot_id unchanged after the redeploy: $boot"
want "(2) GET /config" "$(grep -o '"r":[^,]*,"window_s":[^,]*,"burst_rate":[^,]*,' <<<"$cfg")" '"r":19,"window_s":300,"burst_rate":76,'
window_pairs=$(int window_pairs "$cfg")
burst_pairs=$(int burst_pairs "$cfg")
most=$((window_pairs > burst_pairs ? window_pairs : burst_pairs))
body=$(api_put "/targets/$id") || die "(2) PUT /targets/$id failed"
want "(2) PUT /targets/$id" "$body" "{\"boot_id\":\"$boot\",\"id\":\"$id\",\"next\":0}"
pairs "$most" "$work/largest.json"
size=$(wc -c <"$work/largest.json")
echo "loadgen-smoke.sh: the largest upload: $most pairs, $size bytes (3 MiB is 3145728)"
if [ "$size" -gt 3145728 ]; then
	echo "loadgen-smoke.sh: over 3 MiB: this upload exercises the API server's decode limit on the proxy path"
else
	echo "loadgen-smoke.sh: under 3 MiB: the API server's decode limit does not apply to this upload"
fi
body=$(api_put "/targets/$id/pairs/smoke-largest" "$work/largest.json") || die "(2) upload of $size bytes failed"
want "(2) PUT /targets/$id/pairs/smoke-largest" "$body" "{\"boot_id\":\"$boot\",\"count\":$most}"
ok "(2) $sys/$pod at r=19: uploaded $most pairs, $size bytes"

echo "loadgen-smoke.sh: all checks passed"
