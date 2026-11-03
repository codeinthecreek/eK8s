#!/usr/bin/env bash
# Diagnostic only, workstation only: is the Stage 5 new-connection stall
# (a contiguous over_hang_timeout run when the HPA adds the second pod
# mid-burst) caused by kindnet's NetworkPolicy path? docs/decisions.md,
# 2026-11-02 entries. Never part of a run, a smoke or CI.
#
# kindnet queues the first packet of a new connection to userspace when
# its source or destination is a pod some NetworkPolicy selects, so the
# OFF arm removes both policies on the burst's path: ek8s-system/loadgen
# and the diag variant's ek8s-v<id>/variant. With loadgen's policy gone its
# canary would connect, so the OFF arm also points the canary at
# 192.0.2.1:443 (TEST-NET-1, the renderer's own stand-in), which cannot
# connect. The ON arm is the cluster as deploy/up.sh and loadgen-up.sh
# leave it. Runs alternate ON, OFF, ON, ... with -runs runs per arm
# (at most 15, never extended), each a real `gate -stage5` on
# generation 0 built from this checkout, stress-ng on.
#
# Every run asserts its arm's state:
#   ON   before the gate: ek8s-system/loadgen exists, loadgen's start
#        line has the real canary_addr, POST /canary reads refused twice;
#        once the burst has begun (its first record read): ek8s-v<id>/
#        variant exists and the first hasher pod's IP is in podips-v4;
#        loadgen's IP is in kindnet's podips-v4 set in every snapshot.
#   OFF  before the gate: no NetworkPolicy in ek8s-system, canary_addr is
#        192.0.2.1:443, POST /canary reads refused twice, loadgen's IP is
#        not in podips-v4; loadgen's IP is in no snapshot taken while the
#        gate runs, and no hasher pod's IP is in any snapshot taken once
#        the burst has begun (before it, the variant's policy may still
#        exist for an instant); after the gate: no NetworkPolicy in
#        ek8s-system or ek8s-v<id>.
#   Both before the gate: exactly one stress-ng pod, Running and ready,
#        whose requests and limits, and the stress-dose ConfigMap, equal
#        origin/main's deploy/stress (a run without that stress is a
#        violation, never judged); after the gate: the same pod, with no
#        restart. A run the gate judged (exit 0 or 1) must have records
#        read, so the burst-time checks above cannot pass by never running.
# A failed assertion, a gate fault (exit 2) or a policy that cannot be
# restored ends the experiment: the run is not counted.
#
# Guards, before anything changes:
#   G1 refuses when CI or GITHUB_ACTIONS is set;
#   G2 refuses unless EK8S_DIAG_NOPOLICY=yes-no-isolation;
#   G3 refuses while a variant namespace (label ek8s.io/role=variant)
#      exists, or a process named orchestrator, c0 or gate runs (the names
#      `go run` and `go build` give ./orchestrator/cmd/orchestrator,
#      ./orchestrator/cmd/c0 and ./gate/cmd/gate); the namespace and the
#      first two names are checked again before every run, and the two
#      names on every poll while the gate runs;
#   G4 builds generation 0 from this checkout's HEAD and refuses a tree with
#      changes (untracked and ignored files included) under cmd/service,
#      internal/, go.mod, go.sum or deploy/ek8s-system, and a HEAD
#      that is not an ancestor of origin/main or of PR #70's head (both
#      fetched first; a failed fetch refuses), so no mutant's wrapper ever
#      runs without isolation; the result goes to meta.txt;
#   G5 on exit (INT, TERM and HUP included; bash ignores further signals
#      while it restores, though a second Ctrl-C can still kill a kubectl
#      it is running, which ends in POLICY NOT RESTORED), and after every OFF run, restores the ON state
#      (applies deploy/ek8s-system, puts the canary argument back, then
#      requires the real canary_addr and POST /canary refused twice within
#      60s) and prints "POLICY NOT RESTORED" and exits non-zero if it
#      cannot. On exit it also deletes the diag namespace and turns
#      stress-ng off. SIGKILL or a host crash skips G5, and nothing else
#      would notice: the orchestrator and the gate check neither the policy
#      nor canary_addr, and the stand-in canary always reads refused. After
#      any unclean end, run deploy/up.sh and deploy/loadgen-up.sh <r>, with
#      the r in meta.txt and the exit message, before any orchestrator, c0
#      or gate run.
#
# Output (-out, default ~/workspace/scratch/ek8s-runs/diag-nopolicy-<UTC
# stamp>/): meta.txt, diag.log, summary.tsv (one row per run),
# verdict.txt, and per run run-NN-<arm>/ with gate.out, state.txt,
# records.tsv (index, kind, intended send time, latency, bucket, reason,
# skipped, fault; never a pair or evidence), pods.txt, podips.txt (each
# kindnet set snapshot), netstat.tsv (loadgen's netns TcpExt
# TCPSynRetrans and Tcp RetransSegs, sampled about every second),
# kindnet.log (its raw lines for the run's interval) and syncs.txt (its
# "Syncing nftables rules" lines, with their timestamps).
#
# Verdict (summary.tsv, verdict.txt): a stall is a gate exit 1 whose
# "responses" line names over_hang_timeout, or whose records read hold
# any over_hang_timeout (a pod-health FAIL ends the gate before
# "responses"); a mid-burst add is a hasher pod, other than the first,
# whose creationTimestamp is between the burst's first intended send
# minus 1s (the timestamp is cut to whole seconds) and first +
# burst_sends / burst_rate. OFF is MET with 0 stalls in
# >= 15 runs holding >= 10 mid-burst adds, STALLED with any stall, else
# INCONCLUSIVE; ON is MET with >= 1 stall in its mid-burst-add runs, else
# INCONCLUSIVE. Both arms: CONFIRMED when both are MET, NOT-CONFIRMED
# when OFF STALLED, else INCONCLUSIVE.
#
# Usage, after deploy/up.sh on the workstation (-arm on|off runs one arm
# only, for a dry try; its verdict is per arm):
#   EK8S_DIAG_NOPOLICY=yes-no-isolation deploy/diag/stage5-nopolicy.sh \
#       [-rate R] [-runs 15] [-arm both|on|off] [-out DIR]
# (a relative -out is taken from the directory the script is run from).
# -rate defaults to the -rate argument of the loadgen already deployed
# (whatever deployed it last: a smoke run may have changed it), printed
# with its source 5s before anything is deployed and kept in meta.txt;
# with no loadgen deployed it is required. The burst rate b is 4 x r (gate/cmd/gate).
# Needs go, docker, kind, kubectl, git, pgrep; deploys loadgen at -rate
# (deploy/loadgen-up.sh) and leaves it deployed, in the ON state.
set -euo pipefail

cluster=ek8s
ctx="kind-$cluster"
sys=ek8s-system
stress_ns=ek8s-stress
# An id no other script and no real variant gets (variant-smoke 999999,
# stage5-smoke 999998).
id=999997
ns="ek8s-v$id"
proxy=/api/v1/namespaces/$sys/services/loadgen:control/proxy
standin=192.0.2.1:443
# deploy/loadgen/deployment.yaml.tmpl's argument, verbatim (unexpanded).
# shellcheck disable=SC2016
canary_arg='-canary-addr=$(KUBERNETES_SERVICE_HOST):$(KUBERNETES_SERVICE_PORT)'
# kindnetd's table (kind v0.33.0 images/kindnetd/cmd/kindnetd/main.go)
# and kube-network-policies' set of policy-selected pod IPs.
kn_table=kindnet-network-policies
kn_set=podips-v4
max_runs=15
# G4 also accepts commits of this script's own PR before it merges.
# GitHub keeps refs/pull/<n>/head after the branch is deleted.
diag_pr=70
# loadgen's per-request client timeout, 3 x HangTimeout + 1s (DESIGN.md
# sec 10): the last send of a burst completes within it.
client_timeout_s=7

rate="" rate_src="" runs=$max_runs arm=both out="" gate_pid="" touched=0 g4_result="" want_stress=""

k() { kubectl --context "$ctx" "$@"; }
say() { echo "stage5-nopolicy.sh: $*" | if [ -n "${log_file:-}" ]; then tee -a "$log_file"; else cat; fi; }
die() { say "FAIL: $*" >&2; exit 1; }
now() { date -u +%Y-%m-%dT%H:%M:%S.%NZ; }
epoch() { date -u -d "$1" +%s.%N; }

# --- pure helpers (no cluster calls) ------------------------------------

# records_tsv: GET /targets/{id}/records bodies on stdin, one record per
# output line: index, kind, intended_wall, latency_ns, bucket, reason,
# skipped, fault, tab-separated. Each record's pair and evidence (the
# last two fields) are cut before anything else is read, so no password
# or hash reaches the output. A JSON string never holds an unescaped
# quote, so `{"seq":` and `,"` occur only as structure.
records_tsv() {
	sed 's/{"seq":/\n{"seq":/g' | { grep '^{"seq":' || true; } | sed -E 's/,"(pair|evidence)":.*$//; s/\]?,"gap":.*$//; s/\},?$//' |
		awk -F',"' -v OFS='\t' '{
			delete v
			for (i = 1; i <= NF; i++) {
				f = $i
				sub(/^\{?"/, "", f)
				key = f; sub(/".*$/, "", key)
				val = f; sub(/^[^"]*":/, "", val)
				sub(/\}$/, "", val); gsub(/^"|"$/, "", val)
				v[key] = val
			}
			print v["index"], v["kind"], v["intended_wall"], v["latency_ns"], v["bucket"], v["reason"], v["skipped"], v["fault"]
		}'
}

# json_int NAME BODY: the integer field NAME of a flat JSON object.
json_int() {
	[[ $2 =~ \"$1\":([0-9]+)[,}] ]] || return 1
	echo "${BASH_REMATCH[1]}"
}

# json_num NAME BODY: the number field NAME of a flat JSON object.
json_num() {
	[[ $2 =~ \"$1\":([0-9.eE+-]+)[,}] ]] || return 1
	echo "${BASH_REMATCH[1]}"
}

# netstat_counters: /proc/<pid>/net/netstat then /proc/<pid>/net/snmp on
# stdin; prints "TCPSynRetrans RetransSegs" (each a header line of names
# followed by a line of values with the same prefix).
netstat_counters() {
	awk '
		$1 == "TcpExt:" && !ext_seen { for (i = 2; i <= NF; i++) ext[i] = $i; ext_seen = 1; next }
		$1 == "TcpExt:" && ext_seen == 1 { for (i = 2; i <= NF; i++) if (ext[i] == "TCPSynRetrans") syn = $i; ext_seen = 2; next }
		$1 == "Tcp:" && !tcp_seen { for (i = 2; i <= NF; i++) tcp[i] = $i; tcp_seen = 1; next }
		$1 == "Tcp:" && tcp_seen == 1 { for (i = 2; i <= NF; i++) if (tcp[i] == "RetransSegs") rs = $i; tcp_seen = 2; next }
		END { if (syn == "" || rs == "") exit 1; print syn, rs }'
}

# stall_stats: records.tsv on stdin; prints, tab-separated, the number of
# distinct indices read, the over_hang_timeout count, the longest run of
# consecutive indices that are each failed/over_hang_timeout, and that
# run's first and last intended send times ("-" when there is none). An
# index not read ends a run.
stall_stats() {
	sort -t "$(printf '\t')" -k1,1n -u | awk -F'\t' -v OFS='\t' '
		{
			n++
			oht = ($5 == "failed" && $6 == "over_hang_timeout")
			if (oht) {
				total++
				if (prev_oht && $1 == prev + 1) { len++ } else { len = 1; first = $3 }
				if (len > best) { best = len; bfirst = first; blast = $3 }
			}
			prev = $1; prev_oht = oht
		}
		END {
			if (best == 0) { bfirst = "-"; blast = "-" }
			print n + 0, total + 0, best + 0, bfirst, blast
		}'
}

# arm_verdicts: summary.tsv rows (no header) on stdin; prints the ON and
# OFF tallies and verdicts and the overall verdict, as described above.
# Columns: run arm gate_rc result mid_burst_add ...
arm_verdicts() {
	awk -F'\t' -v need_runs="$max_runs" -v want="$arm" '
		{
			a = $2; runs[a]++
			if ($5 == "yes") adds[a]++
			if ($4 == "STALL") { stalls[a]++; if ($5 == "yes") add_stalls[a]++ }
		}
		END {
			on_v = "INCONCLUSIVE"
			if (add_stalls["on"] + 0 >= 1) on_v = "MET"
			off_v = "INCONCLUSIVE"
			if (stalls["off"] + 0 > 0) off_v = "STALLED"
			else if (runs["off"] + 0 >= need_runs && adds["off"] + 0 >= 10) off_v = "MET"
			printf "ON: runs %d, mid-burst adds %d, stalls %d (%d in add runs): %s\n", runs["on"], adds["on"], stalls["on"], add_stalls["on"], on_v
			printf "OFF: runs %d, mid-burst adds %d, stalls %d (%d in add runs): %s\n", runs["off"], adds["off"], stalls["off"], add_stalls["off"], off_v
			if (want != "both") { print "overall: none (-arm " want ")"; exit }
			v = "INCONCLUSIVE"
			if (off_v == "STALLED") v = "NOT-CONFIRMED"
			else if (off_v == "MET" && on_v == "MET") v = "CONFIRMED"
			print "overall: " v
		}'
}

# --- cluster helpers ----------------------------------------------------

api_get() { k get --raw "$proxy$1"; }
api_post() { k create --raw "$proxy$1" -f "$work/empty"; }
api_delete() { k delete --raw "$proxy$1"; }

# loadgen_pod: the one loadgen pod not being deleted.
loadgen_pod() {
	local pods
	pods=$(k -n "$sys" get pods -l app=loadgen \
		-o jsonpath='{range .items[*]}{.metadata.name} {.metadata.deletionTimestamp}{"\n"}{end}' | awk 'NF == 1 { print $1 }')
	[ "$(grep -c . <<<"$pods")" -eq 1 ] || die "want one loadgen pod, have: ${pods:-none}"
	echo "$pods"
}

# start_line POD: loadgen's start log line.
start_line() {
	local logs
	# Into a variable first: grep -m1 would end the pipe early and SIGPIPE
	# kubectl under pipefail (deploy/stage5-smoke.sh does the same).
	logs=$(k -n "$sys" logs "$1" -c loadgen) || die "cannot read $1's log"
	grep -m1 'msg="loadgen starting"' <<<"$logs" || die "no start line in $1's log"
}

# pod_ip NS POD: the pod's IP, never empty.
pod_ip() {
	local ip
	ip=$(k -n "$1" get pod "$2" -o jsonpath='{.status.podIP}') || die "cannot read $1/$2's IP"
	[[ $ip =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "$1/$2 has no IPv4 address: '$ip'"
	echo "$ip"
}

# canary: POST /canary's result, once GET /config answers (the Service
# follows a new pod's readiness asynchronously).
canary() {
	local body deadline=$((SECONDS + 30))
	until api_get /config >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || die "GET /config did not answer in 30s"
		sleep 1
	done
	body=$(api_post /canary) || die "POST /canary failed"
	[[ $body =~ ^\{\"boot_id\":\"[0-9a-f]{32}\",\"result\":\"(connected|refused|error)\"\}$ ]] ||
		die "POST /canary: ${body:0:300}"
	echo "${BASH_REMATCH[1]}"
}

# canary_refused_twice BOUND: polls POST /canary every 2s until refused,
# at most BOUND seconds, then needs refused once more (loadgen-smoke.sh
# check (3)). Returns non-zero otherwise.
canary_refused_twice() {
	local deadline=$((SECONDS + $1)) r
	while :; do
		r=$(canary)
		[ "$r" = refused ] && break
		[ "$SECONDS" -lt "$deadline" ] || { say "POST /canary still $r after $1s"; return 1; }
		sleep 2
	done
	r=$(canary)
	[ "$r" = refused ] || { say "POST /canary $r after refused"; return 1; }
}

# set_ips: the IPv4 elements of kindnet's podips-v4 set, one per line.
set_ips() {
	local s
	s=$(docker exec "$node" nft list set inet "$kn_table" "$kn_set") || die "cannot list set inet $kn_table $kn_set on $node"
	grep -A1000 'elements' <<<"$s" | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' || true
}

# wait_set IP present|absent BOUND: waits for kindnet's next sync.
wait_set() {
	local deadline=$((SECONDS + $3)) ips
	[ -n "$1" ] || return 1
	while :; do
		ips=$(set_ips) || return 1
		if grep -qxF "$1" <<<"$ips"; then [ "$2" = present ] && return 0; else [ "$2" = absent ] && return 0; fi
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 1
	done
}

# loadgen_pid POD: the loadgen process's pid in the node's pid namespace,
# whose /proc/<pid>/net is loadgen's network namespace.
loadgen_pid() {
	local cid pid comm
	cid=$(k -n "$sys" get pod "$1" -o jsonpath='{.status.containerStatuses[?(@.name=="loadgen")].containerID}')
	cid=${cid#containerd://}
	[[ $cid =~ ^[0-9a-f]{64}$ ]] || die "loadgen's containerID is not a containerd id: '$cid'"
	# The top-level info object's pid, by path: the output also holds nested
	# "pid" keys (the OCI spec's process, the pid namespace) that come
	# first. -o go-template --template is crictl inspect's own (cri-tools
	# v1.36.0, the version kind v0.33.0's base image builds, cmd/crictl/
	# container.go "inspect" flags and util.go outputStatusData: the one
	# status is the top-level object {status, info}; templates.go
	# tmplExecuteRawJSON decodes with UseNumber, so a large pid prints as
	# digits, and sets missingkey=error, so an absent info.pid fails).
	pid=$(docker exec "$node" crictl inspect -o go-template --template '{{.info.pid}}' "$cid") ||
		die "crictl inspect $cid gave no info.pid"
	[[ $pid =~ ^[1-9][0-9]*$ ]] || die "crictl inspect $cid: info.pid is not a pid: '${pid:0:100}'"
	docker exec "$node" test -r "/proc/$pid/net/netstat" || die "cannot read /proc/$pid/net/netstat on $node"
	comm=$(docker exec "$node" cat "/proc/$pid/comm") || die "cannot read /proc/$pid/comm on $node"
	[ "$comm" = loadgen ] || die "pid $pid on $node is '$comm', not loadgen"
	echo "$pid"
}

# counters PID: "TCPSynRetrans RetransSegs" in PID's network namespace.
counters() {
	docker exec "$node" cat "/proc/$1/net/netstat" "/proc/$1/net/snmp" | netstat_counters
}

# canary_index: the index of loadgen's -canary-addr argument.
canary_index() {
	local name idx
	name=$(k -n "$sys" get deploy loadgen -o jsonpath='{.spec.template.spec.containers[0].name}')
	[ "$name" = loadgen ] || die "loadgen's container 0 is '$name'"
	idx=$(k -n "$sys" get deploy loadgen -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' |
		awk '/^-canary-addr=/ { n++; i = NR - 1 } END { if (n == 1) print i }')
	[ -n "$idx" ] || die "loadgen has not exactly one -canary-addr argument"
	echo "$idx"
}

# canary_value: loadgen's -canary-addr argument as deployed.
canary_value() {
	k -n "$sys" get deploy loadgen -o jsonpath="{.spec.template.spec.containers[0].args[$(canary_index)]}"
}

# set_canary VALUE: replaces the -canary-addr argument (a JSON patch whose
# test op makes it fail unless the old value is still there) and waits
# for the Recreate rollout.
set_canary() {
	local idx old
	# Every step is checked: the restore runs with errexit off.
	idx=$(canary_index) || die "cannot find loadgen's -canary-addr argument"
	old=$(canary_value) || die "cannot read loadgen's -canary-addr argument"
	[ "$old" = "$1" ] && return 0
	k -n "$sys" patch deploy loadgen --type=json -p \
		"[{\"op\":\"test\",\"path\":\"/spec/template/spec/containers/0/args/$idx\",\"value\":\"$old\"},{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/$idx\",\"value\":\"$1\"}]" >/dev/null ||
		die "cannot patch loadgen's -canary-addr to $1"
	k -n "$sys" rollout status deploy/loadgen --timeout=180s >/dev/null || die "loadgen not Ready after setting $1"
}

# check_on FILE: asserts the ON state, appending what it saw to FILE.
check_on() {
	local pod start lip
	k -n "$sys" get networkpolicy loadgen >/dev/null 2>&1 || die "ON: NetworkPolicy $sys/loadgen missing"
	[ "$(canary_value)" = "$canary_arg" ] || die "ON: loadgen's argument is '$(canary_value)', not $canary_arg"
	pod=$(loadgen_pod) || die "ON: no loadgen pod"
	start=$(start_line "$pod") || die "ON: no start line"
	[[ " $start " == *" canary_addr=$api "* ]] || die "ON: start line's canary_addr is not $api: $start"
	canary_refused_twice 60 || die "ON: POST /canary not refused twice within 60s"
	lip=$(pod_ip "$sys" "$pod") || die "ON: no IP for $pod"
	wait_set "$lip" present 15 || die "ON: loadgen's IP $lip not in $kn_set within 15s"
	echo "ON $(now): NetworkPolicy $sys/loadgen present; canary_addr=$api; POST /canary refused twice; $lip in $kn_set" >>"$1"
}

# check_off FILE: asserts the OFF state before a run.
check_off() {
	local pod start lip nps
	nps=$(k -n "$sys" get networkpolicy -o name) || die "OFF: cannot list NetworkPolicies in $sys"
	[ -z "$nps" ] || die "OFF: NetworkPolicies in $sys: $nps"
	[ "$(canary_value)" = "-canary-addr=$standin" ] || die "OFF: loadgen's argument is '$(canary_value)'"
	pod=$(loadgen_pod) || die "OFF: no loadgen pod"
	start=$(start_line "$pod") || die "OFF: no start line"
	[[ " $start " == *" canary_addr=$standin "* ]] || die "OFF: start line's canary_addr is not $standin: $start"
	canary_refused_twice 30 || die "OFF: POST /canary to $standin not refused twice within 30s"
	lip=$(pod_ip "$sys" "$pod") || die "OFF: no IP for $pod"
	wait_set "$lip" absent 15 || die "OFF: loadgen's IP $lip still in $kn_set after 15s"
	echo "OFF $(now): no NetworkPolicy in $sys; canary_addr=$standin; POST /canary refused twice; $lip not in $kn_set" >>"$1"
}

# set_off: removes loadgen's policy and points the canary at the stand-in.
set_off() {
	k -n "$sys" delete networkpolicy loadgen --ignore-not-found >/dev/null || die "cannot delete NetworkPolicy $sys/loadgen"
	set_canary "-canary-addr=$standin"
}

# restore_on FILE: the ON state, re-applied and asserted (G5).
restore_on() {
	k apply -k deploy/ek8s-system >/dev/null || die "cannot apply deploy/ek8s-system"
	set_canary "$canary_arg"
	check_on "$1"
}

# deployed_rate: the -rate argument of the loadgen Deployment, if any.
deployed_rate() {
	# Exactly one -rate argument (-burst-rate= does not match), else nothing.
	{ k -n "$sys" get deploy loadgen -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' 2>/dev/null || true; } |
		awk '/^-rate=/ { n++; v = substr($0, 7) } END { if (n == 1) print v }'
}

# g4_ancestry: fetches origin/main and PR #$diag_pr's head, then prints
# which of them HEAD is an ancestor of; fails otherwise.
g4_ancestry() {
	local head
	head=$(git rev-parse HEAD) || return 1
	git fetch --quiet origin "+refs/heads/main:refs/remotes/origin/main" "+refs/pull/$diag_pr/head:refs/ek8s-diag/pr$diag_pr" ||
		{ echo "G4: git fetch of origin/main and PR #$diag_pr's head failed" >&2; return 1; }
	if git merge-base --is-ancestor "$head" refs/remotes/origin/main; then
		echo "G4: HEAD $head is an ancestor of origin/main $(git rev-parse refs/remotes/origin/main)"
	elif git merge-base --is-ancestor "$head" "refs/ek8s-diag/pr$diag_pr"; then
		echo "G4: HEAD $head is an ancestor of PR #$diag_pr's head $(git rev-parse "refs/ek8s-diag/pr$diag_pr")"
	else
		echo "G4: HEAD $head is an ancestor of neither origin/main nor PR #$diag_pr's head" >&2
		return 1
	fi
}

# main_stress: origin/main's stress-ng requests and limits and its dose,
# read from deploy/stress at origin/main; prints one line:
# "<req cpu> <req mem> <lim cpu> <lim mem> <K=V,...>" (dose keys sorted).
main_stress() {
	local dep dose res d
	dep=$(git show refs/remotes/origin/main:deploy/stress/deployment.yaml) || return 1
	dose=$(git show refs/remotes/origin/main:deploy/stress/dose.yaml) || return 1
	res=$(awk '
		/^[[:space:]]*resources:/ { inres = 1; next }
		inres && /^[[:space:]]*requests:/ { sec = "req"; next }
		inres && /^[[:space:]]*limits:/ { sec = "lim"; next }
		inres && sec != "" && /^[[:space:]]*(cpu|memory):/ {
			v = $2; gsub(/"/, "", v); val[sec "." $1] = v; next
		}
		inres && sec != "" { inres = 0 }
		END { print val["req.cpu:"], val["req.memory:"], val["lim.cpu:"], val["lim.memory:"] }' <<<"$dep")
	d=$(sed -nE 's/^[[:space:]]+([A-Z_]+):[[:space:]]*"([^"]*)".*/\1=\2/p' <<<"$dose" | sort | paste -sd, -)
	if ! [[ $res =~ ^[^[:space:]]+\ [^[:space:]]+\ [^[:space:]]+\ [^[:space:]]+$ ]] || [ -z "$d" ]; then
		echo "cannot read stress-ng's resources or dose from origin/main: '$res' '$d'" >&2
		return 1
	fi
	echo "$res $d"
}

# stress_state: the one stress-ng pod, Running and ready, at main's dose
# ($want_stress); prints "<pod> <restarts> <req cpu> <req mem> <lim cpu>
# <lim mem> <dose>", or fails saying why.
stress_state() {
	local pods pod phase ready restarts res d key keys
	pods=$(k -n "$stress_ns" get pods -l app=stress-ng \
		-o jsonpath='{range .items[*]}{.metadata.name} {.status.phase} {.metadata.deletionTimestamp}{"\n"}{end}') ||
		{ echo "cannot list stress-ng pods" >&2; return 1; }
	[ "$(grep -c . <<<"$pods")" -eq 1 ] || { echo "want one stress-ng pod, have: ${pods:-none}" >&2; return 1; }
	read -r pod phase _ <<<"$pods"
	if [ "$phase" != Running ] || [ "$(awk '{ print NF }' <<<"$pods")" -ne 2 ]; then
		echo "stress-ng pod not Running, or being deleted: $pods" >&2
		return 1
	fi
	ready=$(k -n "$stress_ns" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].ready} {.status.containerStatuses[0].restartCount}') ||
		{ echo "cannot read $pod's status" >&2; return 1; }
	read -r ready restarts <<<"$ready"
	[ "$ready" = true ] || { echo "stress-ng container not ready" >&2; return 1; }
	res=$(k -n "$stress_ns" get pod "$pod" -o jsonpath='{.spec.containers[0].resources.requests.cpu} {.spec.containers[0].resources.requests.memory} {.spec.containers[0].resources.limits.cpu} {.spec.containers[0].resources.limits.memory}') ||
		{ echo "cannot read $pod's resources" >&2; return 1; }
	keys=$(sed 's/=[^,]*//g; s/,/ /g' <<<"${want_stress##* }")
	d=""
	for key in $keys; do
		d+="${d:+,}$key=$(k -n "$stress_ns" get configmap stress-dose -o jsonpath="{.data.$key}")" ||
			{ echo "cannot read stress-dose" >&2; return 1; }
	done
	[ "$res $d" = "$want_stress" ] || { echo "stress-ng at '$res $d', origin/main has '$want_stress'" >&2; return 1; }
	echo "$pod $restarts $res $d"
}

# live_procs: prints the G3 process names that are running.
live_procs() {
	local p rc
	for p in orchestrator c0; do
		rc=0
		pgrep -x "$p" >/dev/null || rc=$?
		case "$rc" in
		0) echo "$p" ;;
		1) ;;
		*) echo "$p(pgrep failed: exit $rc)" ;;
		esac
	done
}

# live_guard: G3's namespace and process checks.
live_guard() {
	local live procs
	live=$(k get namespaces -l ek8s.io/role=variant -o name) || die "G3: cannot list namespaces"
	[ -z "$live" ] || die "G3: variant namespaces exist (an orchestrator run, c0, or a failed earlier run?): $live"
	procs=$(live_procs)
	[ -z "$procs" ] || die "G3: running: $procs"
}

# --- one run ------------------------------------------------------------

# run_one N ARM
run_one() {
	local n=$1 a=$2 pods_ok=0 rdir pod lip pid t0 gpid kpid rc cur=0 body next result mid=no
	local stress0 stress1 start_wall="" start_e="" end_e="" stats nread oht longest sfirst slast syn0="" rs0="" syn1="" rs1="" ts c
	rdir="$out/run-$(printf %02d "$n")-$a"
	mkdir -p "$rdir"
	live_guard
	! k get namespace "$ns" >/dev/null 2>&1 || die "namespace $ns exists"

	if [ "$a" = off ]; then
		set_off
		check_off "$rdir/state.txt"
	else
		check_on "$rdir/state.txt"
	fi
	if ! stress0=$(stress_state 2>"$work/stress.err"); then
		echo "VIOLATION: stress-ng before the gate: $(cat "$work/stress.err")" >>"$rdir/state.txt"
		die "run $n ($a): stress-ng is not Running at origin/main's dose ($(cat "$work/stress.err")); the run is not judged"
	fi
	echo "stress before the gate: $stress0" >>"$rdir/state.txt"
	pod=$(loadgen_pod)
	lip=$(pod_ip "$sys" "$pod")
	pid=$(loadgen_pid "$pod")
	t0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	say "run $n ($a): loadgen $pod ($lip, pid $pid), gate starting at $t0"

	"$work/gate" -stage5 -image "$image" -id "$id" -rate "$rate" >"$rdir/gate.out" 2>&1 &
	gate_pid=$!
	gpid=$gate_pid
	kpid=""
	if [ "$a" = off ]; then
		# The gate creates the bundle with its policy; delete it as soon as
		# it exists (the gate waits for Ready before its canary and burst).
		(
			until k -n "$ns" get networkpolicy variant >/dev/null 2>&1; do
				kill -0 "$gpid" 2>/dev/null || exit 0
				sleep 0.1
			done
			k -n "$ns" delete networkpolicy variant >/dev/null
			echo "deleted $ns/variant at $(now)" >>"$rdir/state.txt"
		) &
		kpid=$!
	fi

	: >"$rdir/records.tsv"
	: >"$rdir/pods.txt"
	pods_ok=0
	printf 'time\tTCPSynRetrans\tRetransSegs\n' >"$rdir/netstat.tsv"
	: >"$rdir/podips.txt"
	while kill -0 "$gpid" 2>/dev/null; do
		ts=$(now)
		if c=$(counters "$pid"); then printf '%s\t%s\t%s\n' "$ts" "${c% *}" "${c#* }" >>"$rdir/netstat.tsv"; fi
		if body=$(api_get "/targets/$id/records?since=$cur" 2>/dev/null) && next=$(json_int next "${body##*\"gap\"}"); then
			records_tsv <<<"$body" >>"$rdir/records.tsv"
			cur=$next
		fi
		# A failed listing keeps the last good one (pods_ok counts listings).
		if k -n "$ns" get pods -l app=hasher -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.creationTimestamp} {.status.podIP} {.status.conditions[?(@.type=="Ready")].lastTransitionTime}{"\n"}{end}' \
			>"$rdir/pods.tmp" 2>/dev/null; then
			mv "$rdir/pods.tmp" "$rdir/pods.txt"
			pods_ok=$((pods_ok + 1))
		fi
		local ips hip first_ip procs
		ips=$(set_ips)
		{ echo "== $ts"; echo "$ips"; } >>"$rdir/podips.txt"
		procs=$(live_procs)
		[ -z "$procs" ] || echo "VIOLATION $ts: G3: running: $procs" >>"$rdir/state.txt"
		if [ "$a" = off ]; then
			! grep -qxF "$lip" <<<"$ips" || echo "VIOLATION $ts: loadgen's $lip in $kn_set" >>"$rdir/state.txt"
			# Hasher pods only once the burst has begun: until the watcher
			# deletes it, the variant's policy may put a pod in the set.
			if [ -s "$rdir/records.tsv" ]; then
				while read -r hip; do
					[ -z "$hip" ] || ! grep -qxF "$hip" <<<"$ips" || echo "VIOLATION $ts: $hip in $kn_set" >>"$rdir/state.txt"
				done < <(awk '{ print $3 }' "$rdir/pods.txt")
			fi
		else
			grep -qxF "$lip" <<<"$ips" || echo "VIOLATION $ts: loadgen's $lip not in $kn_set" >>"$rdir/state.txt"
			if [ -s "$rdir/records.tsv" ]; then
				k -n "$ns" get networkpolicy variant >/dev/null 2>&1 || echo "VIOLATION $ts: $ns/variant missing during the burst" >>"$rdir/state.txt"
				# The first pod was Ready before the burst, so kindnet has
				# synced it; a newer pod may lag (the lag the canary exists for).
				first_ip=$(sort -k2 "$rdir/pods.txt" | awk 'NR == 1 { print $3 }')
				[ -z "$first_ip" ] || grep -qxF "$first_ip" <<<"$ips" ||
					echo "VIOLATION $ts: first hasher pod's $first_ip not in $kn_set" >>"$rdir/state.txt"
			fi
		fi
		sleep 0.5
	done
	rc=0
	wait "$gpid" || rc=$?
	gate_pid=""
	[ -z "$kpid" ] || wait "$kpid" || true
	# The tail of the records, unless the gate already DELETEd the target
	# (it does on a fail, after reading them).
	while body=$(api_get "/targets/$id/records?since=$cur" 2>/dev/null) && next=$(json_int next "${body##*\"gap\"}") && [ "$next" -gt "$cur" ]; do
		records_tsv <<<"$body" >>"$rdir/records.tsv"
		cur=$next
	done
	if api_get /targets 2>/dev/null | grep -q "\"id\":\"$id\""; then
		api_delete "/targets/$id?through=$((cur > 0 ? cur - 1 : 0))" >/dev/null 2>&1 || say "run $n: DELETE /targets/$id failed (left in loadgen)"
	fi
	if c=$(counters "$pid"); then printf '%s\t%s\t%s\n' "$(now)" "${c% *}" "${c#* }" >>"$rdir/netstat.tsv"; fi

	k -n kube-system logs ds/kindnet --timestamps --since-time="$t0" >"$rdir/kindnet.log" 2>&1 || say "run $n: cannot read kindnet's log"
	grep -F 'Syncing nftables rules' "$rdir/kindnet.log" >"$rdir/syncs.txt" || true

	# The same pod, no restart, the same resources and dose.
	if ! stress1=$(stress_state 2>"$work/stress.err") || [ "$stress1" != "$stress0" ]; then
		echo "VIOLATION: stress-ng changed during the run: before '$stress0', after '${stress1:-$(cat "$work/stress.err")}'" >>"$rdir/state.txt"
	fi
	if [ "$a" = off ]; then
		local nps_sys nps_ns
		if ! nps_sys=$(k -n "$sys" get networkpolicy -o name) || [ -n "$nps_sys" ]; then
			echo "VIOLATION: a NetworkPolicy in $sys after the run, or the listing failed: ${nps_sys:-}" >>"$rdir/state.txt"
		fi
		if ! nps_ns=$(k -n "$ns" get networkpolicy -o name) || [ -n "$nps_ns" ]; then
			echo "VIOLATION: a NetworkPolicy in $ns after the run, or the listing failed: ${nps_ns:-}" >>"$rdir/state.txt"
		fi
	else
		k -n "$ns" get networkpolicy variant >/dev/null 2>&1 || echo "VIOLATION: $ns/variant missing after the run" >>"$rdir/state.txt"
	fi

	stats=$(stall_stats <"$rdir/records.tsv")
	IFS=$'\t' read -r nread oht longest sfirst slast <<<"$stats"
	case "$rc" in
	0) result=PASS ;;
	1)
		result=FAIL
		if grep -Eq '^stage 5 responses +FAIL:.*over_hang_timeout' "$rdir/gate.out" || [ "$oht" -gt 0 ]; then result=STALL; fi
		;;
	*) result=FAULT ;;
	esac
	if [ "$result" != FAULT ] && [ "$nread" -eq 0 ]; then
		echo "VIOLATION: the gate judged the burst (exit $rc) but no record was read, so the burst-time checks never ran" >>"$rdir/state.txt"
	fi
	start_wall=$(awk -F'\t' '$1 == 0 { print $3; exit }' "$rdir/records.tsv")
	if [ -n "$start_wall" ] && [ "$pods_ok" -gt 0 ]; then
		start_e=$(epoch "$start_wall")
		end_e=$(awk -v s="$start_e" -v n="$burst_sends" -v b="$burst_rate" 'BEGIN { printf "%.3f", s + n / b }')
		# Every hasher pod but the first created, by creation time.
		local created
		while read -r created; do
			[ -n "$created" ] || continue
			if awk -v c="$(epoch "$created")" -v s="$start_e" -v e="$end_e" 'BEGIN { exit !(c >= s - 1 && c <= e) }'; then mid=yes; fi
		done < <(awk '{ print $2 }' "$rdir/pods.txt" | sort | tail -n +2)
		# Counters: the last sample before the first send, the first at or
		# after the last send's client timeout (or the last sample).
		read -r syn0 rs0 syn1 rs1 < <(awk -F'\t' -v s="$start_e" -v e="$end_e" -v t="$client_timeout_s" '
			NR == 1 { next }
			{ cmd = "date -u -d " $1 " +%s.%N"; cmd | getline x; close(cmd)
			  if (x < s) { a1 = $2; a2 = $3 } else if (a1 == "") { a1 = $2; a2 = $3 }
			  if (x >= e + t && b1 == "") { b1 = $2; b2 = $3 }
			  l1 = $2; l2 = $3 }
			END { if (b1 == "") { b1 = l1; b2 = l2 }; print a1, a2, b1, b2 }' "$rdir/netstat.tsv")
	else
		mid=unknown
	fi
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$n" "$a" "$rc" "$result" "$mid" "${start_wall:--}" "$burst_sends" "$nread" "$oht" "$longest" "$sfirst" "$slast" \
		"$(awk -v a="$syn0" -v b="$syn1" 'BEGIN { if (a == "" || b == "") print "-"; else print b - a }')" \
		"$(awk -v a="$rs0" -v b="$rs1" 'BEGIN { if (a == "" || b == "") print "-"; else print b - a }')" \
		"$(grep -c 'Could not receive message' "$rdir/kindnet.log" || true)" \
		"$(grep -c . "$rdir/syncs.txt" || true)" \
		"$(awk '{ print $1 "@" $2 }' "$rdir/pods.txt" | sort -t@ -k2 | tr '\n' ' ' | sed 's/ $//; s/^$/-/')" \
		"$stress0" >>"$out/summary.tsv"
	say "run $n ($a): gate exit $rc, $result; mid-burst add $mid; over_hang_timeout $oht, longest run $longest ($sfirst .. $slast); $nread of $burst_sends records read"

	k delete namespace "$ns" --wait=true --timeout=300s >/dev/null || die "namespace $ns not deleted in 300s"
	if [ "$a" = off ]; then
		restore_on "$rdir/state.txt" || die "ON state not restored after run $n"
	fi
	! grep -q '^VIOLATION' "$rdir/state.txt" || die "run $n ($a): state assertion failed, see $rdir/state.txt; the experiment stops"
	[ "$result" != FAULT ] || die "run $n ($a): gate fault (exit $rc), see $rdir/gate.out; the experiment stops"
}

# --- main ---------------------------------------------------------------

main() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
		-rate) rate=$2; shift 2 ;;
		-runs) runs=$2; shift 2 ;;
		-arm) arm=$2; shift 2 ;;
		-out) out=$2; shift 2 ;;
		*) die "usage: EK8S_DIAG_NOPOLICY=yes-no-isolation $0 [-rate R] [-runs N<=15] [-arm both|on|off] [-out DIR]" ;;
		esac
	done
	if ! [[ $runs =~ ^[0-9]+$ ]] || [ "$runs" -lt 1 ] || [ "$runs" -gt "$max_runs" ]; then
		die "-runs $runs: want 1 to $max_runs (never extended)"
	fi
	case "$arm" in both | on | off) ;; *) die "-arm $arm: want both, on or off" ;; esac
	# A relative -out is the caller's, taken before moving to the repo root.
	case "$out" in /* | "") ;; *) out="$PWD/$out" ;; esac
	cd "$(dirname "$0")/../.."

	# G1, G2: before any cluster call.
	if [ -n "${CI:-}" ] || [ -n "${GITHUB_ACTIONS:-}" ]; then
		die "G1: CI or GITHUB_ACTIONS is set; this diagnostic is workstation only"
	fi
	[ "${EK8S_DIAG_NOPOLICY:-}" = yes-no-isolation ] || die "G2: set EK8S_DIAG_NOPOLICY=yes-no-isolation (this removes NetworkPolicy isolation while it runs)"
	for tool in go docker kind kubectl git pgrep; do
		command -v "$tool" >/dev/null || die "$tool not on PATH"
	done
	# The output directory first, so a refusal from here on is recorded.
	out=${out:-$HOME/workspace/scratch/ek8s-runs/diag-nopolicy-$(date -u +%Y%m%dT%H%M%SZ)}
	[ ! -e "$out" ] || die "$out exists"
	mkdir -p "$out"
	log_file="$out/diag.log"
	echo "started $(now)" >"$out/meta.txt"

	# G4: generation 0 is this checkout's, and the checkout is main's or
	# this PR's. Untracked and ignored files count too: go build compiles
	# them. deploy/ek8s-system is what the restore applies.
	local changed g4_paths="cmd/service internal go.mod go.sum deploy/ek8s-system"
	# shellcheck disable=SC2086
	changed=$(git status --porcelain --untracked-files=all --ignored -- $g4_paths) || die "G4: git status failed"
	[ -z "$changed" ] || die "G4: $g4_paths differ from HEAD (untracked and ignored files included): $changed"
	g4_result=$(g4_ancestry) || { echo "G4: refused" >>"$out/meta.txt"; die "G4: refused (see above)"; }
	say "$g4_result"
	echo "$g4_result" >>"$out/meta.txt"
	want_stress=$(main_stress) || die "cannot read origin/main's deploy/stress"
	echo "stress expected (origin/main deploy/stress): $want_stress" >>"$out/meta.txt"
	k get nodes >/dev/null || die "cluster unreachable (context $ctx); run deploy/up.sh"
	if [ -z "$rate" ]; then
		rate=$(deployed_rate)
		[ -n "$rate" ] || die "no -rate given and no loadgen deployed to take it from; pass -rate R (b = 4 x R)"
		rate_src="the deployed loadgen's -rate argument"
	else
		rate_src="-rate"
	fi
	# Printed before anything is deployed: the default is whatever loadgen
	# was last deployed with, which a smoke run may have changed.
	say "r = $rate (from $rate_src), so b = 4 x r; stop now (Ctrl-C) if that is not the r intended"
	# G3.
	live_guard
	local grc=0
	pgrep -x gate >/dev/null || grc=$?
	[ "$grc" -eq 1 ] || die "G3: a process named gate is running, or pgrep failed (exit $grc)"
	! k get namespace "$ns" >/dev/null 2>&1 || die "namespace $ns exists (a failed earlier run?); delete it first"
	k -n "$sys" get networkpolicy loadgen >/dev/null 2>&1 || die "NetworkPolicy $sys/loadgen missing at start; run deploy/up.sh"
	sleep 5
	work=$(mktemp -d)
	: >"$work/empty"
	trap on_exit EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	trap 'exit 129' HUP
	node=$(k get nodes -o jsonpath='{.items[0].metadata.name}')
	api=$(k -n default get service kubernetes -o jsonpath='{.spec.clusterIP}:{.spec.ports[0].port}')

	# Generation 0's image, as gate Stage 1 builds it (deploy/stage5-smoke.sh).
	CGO_ENABLED=0 go build -trimpath -o "$work/service" ./cmd/service
	cat >"$work/Dockerfile" <<-'EOF'
		FROM scratch
		COPY service /service
		USER 65532:65532
		EXPOSE 8080
		ENTRYPOINT ["/service"]
	EOF
	image="ek8s-wrapper:$(sha256sum "$work/service" | cut -c1-12)"
	docker build -q -t "$image" "$work" >/dev/null
	go build -o "$work/gate" ./gate/cmd/gate

	touched=1
	deploy/loadgen-up.sh "$rate" >/dev/null
	local cfg deadline=$((SECONDS + 30))
	until cfg=$(api_get /config 2>/dev/null); do
		[ "$SECONDS" -lt "$deadline" ] || die "GET /config did not answer in 30s"
		sleep 1
	done
	burst_sends=$(json_int burst_sends "$cfg") || die "no burst_sends in /config: ${cfg:0:300}"
	burst_rate=$(json_num burst_rate "$cfg") || die "no burst_rate in /config: ${cfg:0:300}"
	# A fresh pod, so the dose it runs is the ConfigMap's as checked
	# (stress_state compares the ConfigMap, which a running pod read at its
	# start); the same pod must then last the whole experiment.
	k -n "$stress_ns" scale deploy/stress-ng --replicas=0 >/dev/null
	k -n "$stress_ns" wait --for=delete pod -l app=stress-ng --timeout=120s >/dev/null 2>&1 ||
		[ -z "$(k -n "$stress_ns" get pods -l app=stress-ng -o name)" ] || die "old stress-ng pods still present"
	k -n "$stress_ns" scale deploy/stress-ng --replicas=1 >/dev/null
	k -n "$stress_ns" rollout status deploy/stress-ng --timeout=180s >/dev/null || die "stress-ng not Ready"
	stress_state >/dev/null || die "stress-ng is not at origin/main's dose; run deploy/up.sh"

	{
		echo "git $(git rev-parse HEAD)"
		echo "rate $rate (from $rate_src) burst_rate $burst_rate burst_sends $burst_sends runs/arm $runs arm $arm"
		echo "image $image"
		echo "kind $(kind version)"
		echo "node image $(docker inspect -f '{{.Config.Image}}' "$node" 2>/dev/null || echo unknown)"
		echo "kindnet $(k -n kube-system get ds kindnet -o jsonpath='{..image}')"
		echo "api $api standin $standin"
	} >>"$out/meta.txt"
	printf 'run\tarm\tgate_rc\tresult\tmid_burst_add\tburst_start\tburst_sends\trecords_read\tover_hang_timeout\tlongest_run\tstall_first_send\tstall_last_send\tsyn_retrans\tretrans_segs\tkindnet_enoent\tkindnet_sync_lines\tpods\tstress\n' >"$out/summary.tsv"

	local i order=""
	case "$arm" in
	both) for ((i = 0; i < runs; i++)); do order+=" on off"; done ;;
	*) for ((i = 0; i < runs; i++)); do order+=" $arm"; done ;;
	esac
	i=0
	for a in $order; do
		i=$((i + 1))
		run_one "$i" "$a"
	done
	tail -n +2 "$out/summary.tsv" | arm_verdicts | tee "$out/verdict.txt" | sed 's/^/stage5-nopolicy.sh: /'
}

# on_exit: G5, then stress-ng off and the scratch dir gone.
on_exit() {
	local rc=$?
	# The restore must finish or report: no errexit (each step checks
	# itself), and bash ignores INT, TERM, HUP and PIPE (a closed terminal)
	# until it has. A kubectl it runs still gets the terminal's SIGINT; a
	# killed step ends in POLICY NOT RESTORED.
	set +e
	trap '' INT TERM HUP PIPE
	trap - EXIT
	if [ "${touched:-0}" = 1 ]; then
		# The gate cancels and drains its burst on SIGINT (gate/cmd/gate:
		# signal.NotifyContext(os.Interrupt)); the policy watcher is killed.
		if [ -n "${gate_pid:-}" ] && kill -INT "$gate_pid" 2>/dev/null; then
			say "interrupting the running gate (pid $gate_pid) and waiting for it"
			wait "$gate_pid" 2>/dev/null || true
		fi
		jobs -p | xargs -r kill -TERM 2>/dev/null || true
		wait 2>/dev/null || true
		if ! k delete namespace "$ns" --ignore-not-found --wait=true --timeout=300s >/dev/null 2>&1; then
			say "namespace $ns not deleted: it may still run generation 0 without its NetworkPolicy; delete it by hand" >&2
			rc=1
		fi
		if ! (restore_on "$out/restore.txt"); then
			say "POLICY NOT RESTORED: check NetworkPolicy $sys/loadgen and loadgen's -canary-addr, then run deploy/up.sh and deploy/loadgen-up.sh ${rate:-<r>} (the r this run used, from ${rate_src:-unknown}; meta.txt has it)" >&2
			rc=1
		fi
		k -n "$stress_ns" scale deploy/stress-ng --replicas=0 >/dev/null 2>&1 || say "stress-ng not scaled to 0; scale it by hand"
	fi
	if [ -n "${out:-}" ] && [ -d "$out" ] && [ ! -e "$out/verdict.txt" ]; then
		echo "STOPPED before the last run (exit $rc): no verdict; summary.tsv holds the runs that completed" >"$out/verdict.txt"
	fi
	rm -rf "${work:-}"
	[ -z "${out:-}" ] || say "output in $out"
	exit "$rc"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	main "$@"
fi
