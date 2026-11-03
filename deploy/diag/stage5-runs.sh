#!/usr/bin/env bash
# Diagnostic only, workstation only: does Stage 5 still stall (a
# contiguous over_hang_timeout run when the HPA adds the second pod
# mid-burst) now that Calico enforces the NetworkPolicies? The owner's
# validation of D2 (docs/decisions.md, 2026-11-03). Never part of a run, a
# smoke or CI.
#
# Adapted from stage5-nopolicy.sh (PR #70), which found the stall on
# kindnet's policy path; git keeps it and decisions.md its result. One
# arm, the cluster as deploy/up.sh and loadgen-up.sh leave it: no OFF arm,
# no canary patch, no policy removed. So stage5-nopolicy.sh's G1 (refuse in
# CI), G2 (EK8S_DIAG_NOPOLICY, the opt-in variable) and G5 (restore the
# policies and the canary on exit) are gone: they guarded isolation
# removal, which this script never does. Each run is a real `gate -stage5`
# on generation 0 built from this checkout, stress-ng on.
#
# Every run asserts the deployed state:
#   before the gate: DaemonSet kube-system/calico-node Ready;
#        NetworkPolicy ek8s-system/loadgen exists; loadgen's -canary-addr
#        argument is the template's and its start line has the real
#        canary_addr; POST /canary reads refused twice; exactly one
#        stress-ng pod, Running and ready, whose requests and limits, and
#        the stress-dose ConfigMap, equal origin/main's deploy/stress (a run
#        without that stress is a violation, never judged);
#   once the burst has begun (its first record read), and after the gate:
#        NetworkPolicy ek8s-v<id>/variant exists;
#   after the gate: the same stress-ng pod, with no restart. A run the gate
#        judged (exit 0 or 1) must have records read, so the burst-time
#        checks cannot pass by never running.
# A failed assertion or a gate fault (exit 2) ends the experiment: the run
# is not counted.
#
# Guards, before anything changes (numbered as in stage5-nopolicy.sh):
#   G3 refuses while a variant namespace (label ek8s.io/role=variant)
#      exists, or a process named orchestrator, c0 or gate runs (the names
#      `go run` and `go build` give ./orchestrator/cmd/orchestrator,
#      ./orchestrator/cmd/c0 and ./gate/cmd/gate): the script redeploys
#      loadgen and runs its own gate, which would corrupt a live run. The
#      namespace and the first two names are checked again before every
#      run, and the two names on every poll while the gate runs;
#   G4 builds generation 0, the gate and loadgen from this checkout's HEAD
#      and refuses a tree with changes (untracked and ignored files
#      included) under cmd/service, internal/, gate/, loadgen/, deploy/,
#      go.mod or go.sum, and a HEAD that is not an ancestor of origin/main
#      or of PR #71's head (both fetched first; a failed fetch refuses), so
#      the result is about main's code; the result goes to meta.txt.
# On exit (INT, TERM and HUP included) the script stops a running gate,
# deletes the diag namespace and turns stress-ng off. SIGKILL or a host
# crash skips that: then a gate may still run, stress-ng stays on and
# ek8s-v999997 stays (G3 refuses the next start while it exists), so
# kill any leftover gate, delete ek8s-v999997 and scale stress-ng to 0
# before any orchestrator, c0 or gate run.
#
# Stop rule, fixed in advance: at least 15 runs; past 15, another run only
# while the mid-burst adds so far are under 10; at most 25. The first
# stall ends the experiment.
#
# Output (-out, default ~/workspace/scratch/ek8s-runs/diag-runs-<UTC
# stamp>/): meta.txt, diag.log, summary.tsv (one row per run),
# verdict.txt, and per run run-NN/ with gate.out, state.txt, records.tsv
# (index, kind, intended send time, latency, bucket, reason, skipped,
# fault; never a pair or evidence), pods.txt, netstat.tsv (loadgen's netns
# TcpExt TCPSynRetrans and Tcp RetransSegs, sampled about every second),
# iptables.tsv (Felix's drop rules, below), calico-node.log (its raw lines
# for the run's span) and cni-unblocking.txt (the CNI's "Unblocking pod
# creation" lines in the run's span).
#
# iptables.tsv: the packet counters (`iptables-save -c` on the node) of
# every rule commented "Unknown interface" (felix/rules/dispatch.go: a
# packet from a pod Felix has not programmed, or forwarded to one) or
# "End of tier <tier>. Drop if no policies passed packet"
# (felix/rules/endpoints.go: a packet a programmed pod's policy denies),
# read before the gate, on every poll while it runs (about every second,
# like netstat.tsv) and after it. A rule is keyed by its chain and text without Felix's
# "cali:<hash>" comment, since the hash changes whenever the rules before
# it do (a new pod added to a dispatch chain), and Felix's rewrite then
# starts the rule's counters at 0: a counter lower than the last read is
# taken as such a reset and counted from 0. Each rule's increases are
# summed over the run and over the burst (from the first read once a
# record has been read). The gate's canary runs before the burst and is
# denied by loadgen's egress policy, so it adds end-of-tier drops to the
# run but not to the burst. A drop in the burst is the burst's own
# traffic denied: Felix not yet programmed for a new pod, or a new pod's
# IP not yet in a policy's IP set. Drops between a rule's last read and
# its rewrite or deletion are lost, so the sums are lower bounds.
#
# Verdict (summary.tsv, verdict.txt): a stall is a gate exit 1 whose
# "responses" line names over_hang_timeout, or whose records read hold
# any over_hang_timeout (a pod-health FAIL ends the gate before
# "responses"); a mid-burst add is a hasher pod, other than the first,
# whose creationTimestamp is between the burst's first intended send
# minus 1s (the timestamp is cut to whole seconds) and first +
# burst_sends / burst_rate. STALLED with any stall; MET with 0 stalls and
# >= 10 mid-burst-add runs; else INCONCLUSIVE. A gate FAIL that is not a
# stall is counted (other FAILs) but does not change the verdict.
#
# deploy/diag/stage5-runs_test.sh tests the pure helpers (stop rule,
# verdict, drop accounting) without a cluster.
#
# Usage, after deploy/up.sh on the workstation:
#   deploy/diag/stage5-runs.sh [-rate R] [-try] [-out DIR]
# -try runs one run and gives no verdict, for a dry try. A relative -out is
# taken from the directory the script is run from. -rate defaults to the
# -rate argument of the loadgen already deployed (whatever deployed it
# last: a smoke run may have changed it), printed with its source 5s
# before anything is deployed and kept in meta.txt; with no loadgen
# deployed it is required. The burst rate b is 4 x r (gate/cmd/gate).
# Needs go, docker, kind, kubectl, git, pgrep; deploys loadgen at -rate
# (deploy/loadgen-up.sh) and leaves it deployed.
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
# deploy/loadgen/deployment.yaml.tmpl's argument, verbatim (unexpanded).
# shellcheck disable=SC2016
canary_arg='-canary-addr=$(KUBERNETES_SERVICE_HOST):$(KUBERNETES_SERVICE_PORT)'
# The stop rule (header).
min_runs=15
max_runs=25
need_adds=10
cni_log=/var/log/calico/cni/cni.log
# G4 also accepts commits of this script's own PR before it merges.
# GitHub keeps refs/pull/<n>/head after the branch is deleted.
diag_pr=71
# loadgen's per-request client timeout, 3 x HangTimeout + 1s (DESIGN.md
# sec 10): the last send of a burst completes within it.
client_timeout_s=7

rate="" rate_src="" try=0 out="" gate_pid="" touched=0 g4_result="" want_stress=""

k() { kubectl --context "$ctx" "$@"; }
say() { echo "stage5-runs.sh: $*" | if [ -n "${log_file:-}" ]; then tee -a "$log_file"; else cat; fi; }
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

# drop_rules: `iptables-save -c -t filter` on stdin; one line per rule
# iptables.tsv counts (header): packets, kind (unknown_if or end_of_tier)
# and the rule without its counters and its "cali:<hash>" comment,
# tab-separated. Felix renders a rule's comments as -m comment --comment
# "<text>", the hash first (felix/iptables/renderer.go); the end-of-tier
# text names the deny action, Drop (felix/iptables/actions.go).
drop_rules() {
	awk -v OFS='\t' '
		/^\[[0-9]+:[0-9]+\] -A / {
			kind = ""
			if (index($0, "--comment \"Unknown interface\"")) kind = "unknown_if"
			else if ($0 ~ /--comment "End of tier [^"]*\. Drop if no policies passed packet"/) kind = "end_of_tier"
			if (kind == "") next
			pk = $1; sub(/^\[/, "", pk); sub(/:.*$/, "", pk)
			rule = $0; sub(/^\[[0-9]+:[0-9]+\] /, "", rule)
			gsub(/ -m comment --comment "cali:[^"]*"/, "", rule)
			print pk, kind, rule
		}'
}

# drop_tally BURST: drop_rules lines on stdin, each prefixed with its read's
# index (0 before the gate, then 1, 2, ... in order) and a tab; prints
# iptables.tsv (header): kind, chain, first and last counter read, the
# increase over the run and over the burst, the resets seen, and the rule.
# An increase between two consecutive reads is the counter's rise, or the
# new counter when it fell (a rewrite, counted from 0); a rule absent from
# the previous read counts from 0 (new, or recreated: a reset), except in
# read 0. BURST is the first read's index once a record was read ("" when
# none was): an increase belongs to the burst when its earlier read is at
# or after BURST, and every burst increase is "-" with BURST "".
drop_tally() {
	awk -F'\t' -v OFS='\t' -v burst="$1" '
		function flush(   k, v, inc) {
			for (k in cnt) {
				v = cnt[k]
				if (!(k in seen)) {
					seen[k] = 1; order[++n] = k; first[k] = v; run[k] = 0; bur[k] = 0; resets[k] = 0
					inc = (ri == 0 ? 0 : v)
				} else if (prev_i[k] == ri - 1 && v >= prev_v[k]) {
					inc = v - prev_v[k]
				} else {
					inc = v; resets[k]++
				}
				run[k] += inc
				if (burst != "" && ri - 1 >= burst + 0) bur[k] += inc
				prev_i[k] = ri; prev_v[k] = v
			}
			delete cnt
		}
		NR == 1 { ri = $1 + 0 }
		$1 + 0 != ri { flush(); ri = $1 + 0 }
		{ cnt[$3 FS $4] += $2 }
		END {
			if (NR > 0) flush()
			print "kind", "chain", "first", "last", "run_increase", "burst_increase", "resets", "rule"
			for (j = 1; j <= n; j++) {
				k = order[j]
				split(k, kr, FS); split(kr[2], w, " ")
				print kr[1], w[2], first[k], prev_v[k], run[k], (burst == "" ? "-" : bur[k]), resets[k], kr[2]
			}
		}'
}

# drop_sum KIND COLUMN: iptables.tsv on stdin; the sum of COLUMN
# (run_increase or burst_increase) over KIND's rules, or "-" when no rule of
# KIND has a number there.
drop_sum() {
	awk -F'\t' -v kind="$1" -v col="$2" '
		NR == 1 { for (i = 1; i <= NF; i++) if ($i == col) c = i; next }
		$1 == kind && $c != "-" { s += $c; any = 1 }
		END { print (any ? s : "-") }'
}

# another_run I ADDS RESULT: succeeds when the stop rule (header) allows
# another run after run I, with ADDS mid-burst-add runs so far and RESULT
# run I's result.
another_run() {
	[ "$3" != STALL ] || return 1
	[ "$try" = 0 ] || return 1
	[ "$1" -lt "$max_runs" ] || return 1
	[ "$1" -lt "$min_runs" ] || [ "$2" -lt "$need_adds" ]
}

# verdict: summary.tsv rows (no header) on stdin; prints the tally and the
# verdict, as in the header. Columns: run gate_rc result mid_burst_add ...
verdict() {
	awk -F'\t' -v need_adds="$need_adds" -v try="$try" '
		{
			runs++
			if ($4 == "yes") adds++
			if ($3 == "STALL") { stalls++; if ($4 == "yes") add_stalls++ }
			if ($3 == "FAIL") fails++
		}
		END {
			v = "INCONCLUSIVE"
			if (stalls + 0 > 0) v = "STALLED"
			else if (adds + 0 >= need_adds) v = "MET"
			if (try) v = "none (-try)"
			printf "runs %d, mid-burst adds %d, stalls %d (%d in add runs), other FAILs %d: %s\n", runs, adds, stalls, add_stalls, fails, v
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
		r=$(canary) || return 1
		[ "$r" = refused ] && break
		[ "$SECONDS" -lt "$deadline" ] || { say "POST /canary still $r after $1s"; return 1; }
		sleep 2
	done
	r=$(canary) || return 1
	[ "$r" = refused ] || { say "POST /canary $r after refused"; return 1; }
}

# drop_snapshot: drop_rules of the node's filter table. A read with no
# such rule fails: the dispatch chains always carry one, and an empty read
# would make drop_tally count every rule's whole counter again.
drop_snapshot() {
	local s r
	s=$(docker exec "$node" iptables-save -c -t filter) || return 1
	r=$(drop_rules <<<"$s")
	[ -n "$r" ] || return 1
	echo "$r"
}

# cni_size: the byte size of the node's CNI log.
cni_size() {
	local n
	n=$(docker exec "$node" stat -c %s "$cni_log") || return 1
	[[ $n =~ ^[0-9]+$ ]] || return 1
	echo "$n"
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

# check_deployed FILE: asserts the deployed state before a run (header),
# appending what it saw to FILE.
check_deployed() {
	local pod start
	k -n kube-system rollout status ds/calico-node --timeout=30s >/dev/null || die "DaemonSet kube-system/calico-node not Ready"
	k -n "$sys" get networkpolicy loadgen >/dev/null 2>&1 || die "NetworkPolicy $sys/loadgen missing"
	[ "$(canary_value)" = "$canary_arg" ] || die "loadgen's argument is '$(canary_value)', not $canary_arg"
	pod=$(loadgen_pod) || die "no loadgen pod"
	start=$(start_line "$pod") || die "no start line"
	[[ " $start " == *" canary_addr=$api "* ]] || die "start line's canary_addr is not $api: $start"
	canary_refused_twice 60 || die "POST /canary not refused twice within 60s"
	echo "$(now): calico-node Ready; NetworkPolicy $sys/loadgen present; canary_addr=$api; POST /canary refused twice" >>"$1"
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

# run_one N
run_one() {
	local n=$1 pods_ok=0 rdir pod lip pid t0 gpid rc cur=0 body next result mid=no
	local stress0 stress1 start_wall="" start_e="" end_e="" stats nread oht longest sfirst slast syn0="" rs0="" syn1="" rs1="" ts c
	local burst_read="" reads=0 cni0 cni1 cni_span unblock warn_err drops_u drops_e drops_eb targets
	rdir="$out/run-$(printf %02d "$n")"
	mkdir -p "$rdir"
	live_guard
	! k get namespace "$ns" >/dev/null 2>&1 || die "namespace $ns exists"

	check_deployed "$rdir/state.txt"
	if ! stress0=$(stress_state 2>"$work/stress.err"); then
		echo "VIOLATION: stress-ng before the gate: $(cat "$work/stress.err")" >>"$rdir/state.txt"
		die "run $n: stress-ng is not Running at origin/main's dose ($(cat "$work/stress.err")); the run is not judged"
	fi
	echo "stress before the gate: $stress0" >>"$rdir/state.txt"
	pod=$(loadgen_pod)
	lip=$(pod_ip "$sys" "$pod")
	pid=$(loadgen_pid "$pod")
	drop_snapshot | sed 's/^/0\t/' >"$work/drops" || die "run $n: cannot read the node's iptables-save"
	cni0=$(cni_size) || die "run $n: cannot stat $cni_log on $node"
	t0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	say "run $n: loadgen $pod ($lip, pid $pid), gate starting at $t0"

	"$work/gate" -stage5 -image "$image" -id "$id" -rate "$rate" >"$rdir/gate.out" 2>&1 &
	gate_pid=$!
	gpid=$gate_pid

	: >"$rdir/records.tsv"
	: >"$rdir/pods.txt"
	pods_ok=0
	printf 'time\tTCPSynRetrans\tRetransSegs\n' >"$rdir/netstat.tsv"
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
		local procs
		procs=$(live_procs)
		[ -z "$procs" ] || echo "VIOLATION $ts: G3: running: $procs" >>"$rdir/state.txt"
		# Felix's drop counters, every poll (header, iptables.tsv); a failed
		# read is skipped and noted.
		if drop_snapshot | sed "s/^/$((reads + 1))\t/" >>"$work/drops"; then
			reads=$((reads + 1))
			# The first read once the burst has begun: the canary is done, so
			# drops from here on are the burst's own traffic.
			if [ -z "$burst_read" ] && [ -s "$rdir/records.tsv" ]; then
				burst_read=$reads
				echo "burst's first drop-counter read ($burst_read) at $ts" >>"$rdir/state.txt"
			fi
		else
			echo "NOTE $ts: iptables-save on $node failed; read skipped" >>"$rdir/state.txt"
		fi
		if [ -s "$rdir/records.tsv" ]; then
			k -n "$ns" get networkpolicy variant >/dev/null 2>&1 || echo "VIOLATION $ts: $ns/variant missing during the burst" >>"$rdir/state.txt"
		fi
		sleep 0.5
	done
	rc=0
	wait "$gpid" || rc=$?
	gate_pid=""
	if ! drop_snapshot | sed "s/^/$((reads + 1))\t/" >>"$work/drops"; then
		echo "VIOLATION: cannot read the node's iptables-save after the gate" >>"$rdir/state.txt"
	fi
	# The tail of the records, unless the gate already DELETEd the target
	# (it does on a fail, after reading them).
	while body=$(api_get "/targets/$id/records?since=$cur" 2>/dev/null) && next=$(json_int next "${body##*\"gap\"}") && [ "$next" -gt "$cur" ]; do
		records_tsv <<<"$body" >>"$rdir/records.tsv"
		cur=$next
	done
	# Into a variable first: grep -q would SIGPIPE kubectl under pipefail.
	targets=$(api_get /targets 2>/dev/null || true)
	if grep -q "\"id\":\"$id\"" <<<"$targets"; then
		api_delete "/targets/$id?through=$((cur > 0 ? cur - 1 : 0))" >/dev/null 2>&1 || say "run $n: DELETE /targets/$id failed (left in loadgen)"
	fi
	if c=$(counters "$pid"); then printf '%s\t%s\t%s\n' "$(now)" "${c% *}" "${c#* }" >>"$rdir/netstat.tsv"; fi

	# Felix's drop counters (header, iptables.tsv).
	drop_tally "$burst_read" <"$work/drops" >"$rdir/iptables.tsv"
	drops_u=$(drop_sum unknown_if run_increase <"$rdir/iptables.tsv")
	drops_e=$(drop_sum end_of_tier run_increase <"$rdir/iptables.tsv")
	drops_eb=$(drop_sum end_of_tier burst_increase <"$rdir/iptables.tsv")
	# calico-node's log for the run's span; warnings and worse counted.
	if k -n kube-system logs ds/calico-node -c calico-node --timestamps --since-time="$t0" >"$rdir/calico-node.log" 2>&1; then
		warn_err=$(grep -cE '\[(WARNING|ERROR|FATAL|PANIC)\]' "$rdir/calico-node.log" || true)
	else
		say "run $n: cannot read calico-node's log"
		warn_err=-
	fi
	# The CNI log's lines written since the gate started. A log rotated in
	# between (timberjack, at 100 MB) is read whole and noted.
	cni_span=""
	if cni1=$(cni_size); then
		if [ "$cni1" -lt "$cni0" ]; then
			echo "NOTE: $cni_log rotated during the run; its whole new file counted" >>"$rdir/state.txt"
			cni0=0
		fi
		cni_span=$(docker exec "$node" tail -c "+$((cni0 + 1))" "$cni_log") || cni_span="" cni1=""
	fi
	if [ -n "${cni1:-}" ]; then
		{ grep -F 'Unblocking pod creation' <<<"$cni_span" || true; } >"$rdir/cni-unblocking.txt"
		unblock=$(grep -c . "$rdir/cni-unblocking.txt" || true)
	else
		say "run $n: cannot read $cni_log on $node"
		unblock=-
	fi

	# The same pod, no restart, the same resources and dose.
	if ! stress1=$(stress_state 2>"$work/stress.err") || [ "$stress1" != "$stress0" ]; then
		echo "VIOLATION: stress-ng changed during the run: before '$stress0', after '${stress1:-$(cat "$work/stress.err")}'" >>"$rdir/state.txt"
	fi
	k -n "$ns" get networkpolicy variant >/dev/null 2>&1 || echo "VIOLATION: $ns/variant missing after the run" >>"$rdir/state.txt"

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
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$n" "$rc" "$result" "$mid" "${start_wall:--}" "$burst_sends" "$nread" "$oht" "$longest" "$sfirst" "$slast" \
		"$(awk -v a="$syn0" -v b="$syn1" 'BEGIN { if (a == "" || b == "") print "-"; else print b - a }')" \
		"$(awk -v a="$rs0" -v b="$rs1" 'BEGIN { if (a == "" || b == "") print "-"; else print b - a }')" \
		"$drops_u" "$drops_e" "$drops_eb" "$warn_err" "$unblock" \
		"$(awk '{ print $1 "@" $2 }' "$rdir/pods.txt" | sort -t@ -k2 | tr '\n' ' ' | sed 's/ $//; s/^$/-/')" \
		"$stress0" >>"$out/summary.tsv"
	say "run $n: gate exit $rc, $result; mid-burst add $mid; over_hang_timeout $oht, longest run $longest ($sfirst .. $slast); $nread of $burst_sends records read; drops: unknown interface $drops_u, end of tier $drops_e ($drops_eb in the burst); calico-node warnings/errors $warn_err; CNI unblocking $unblock"

	k delete namespace "$ns" --wait=true --timeout=300s >/dev/null || die "namespace $ns not deleted in 300s"
	! grep -q '^VIOLATION' "$rdir/state.txt" || die "run $n: state assertion failed, see $rdir/state.txt; the experiment stops"
	[ "$result" != FAULT ] || die "run $n: gate fault (exit $rc), see $rdir/gate.out; the experiment stops"
}

# --- main ---------------------------------------------------------------

main() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
		-rate) rate=$2; shift 2 ;;
		-try) try=1; shift ;;
		-out) out=$2; shift 2 ;;
		*) die "usage: $0 [-rate R] [-try] [-out DIR]" ;;
		esac
	done
	# A relative -out is the caller's, taken before moving to the repo root.
	case "$out" in /* | "") ;; *) out="$PWD/$out" ;; esac
	cd "$(dirname "$0")/../.."

	for tool in go docker kind kubectl git pgrep; do
		command -v "$tool" >/dev/null || die "$tool not on PATH"
	done
	# The output directory first, so a refusal from here on is recorded.
	out=${out:-$HOME/workspace/scratch/ek8s-runs/diag-runs-$(date -u +%Y%m%dT%H%M%SZ)}
	[ ! -e "$out" ] || die "$out exists"
	mkdir -p "$out"
	log_file="$out/diag.log"
	echo "started $(now)" >"$out/meta.txt"

	# G4: generation 0, the gate and loadgen are this checkout's, and the
	# checkout is main's or this PR's. Untracked and ignored files count
	# too: go build compiles them.
	local changed g4_paths="cmd/service internal gate loadgen deploy go.mod go.sum"
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
		echo "rate $rate (from $rate_src) burst_rate $burst_rate burst_sends $burst_sends try $try"
		echo "stop rule: at least $min_runs runs, past $min_runs only while mid-burst adds < $need_adds, at most $max_runs; the first stall ends it"
		echo "image $image"
		echo "kind $(kind version)"
		echo "node image $(docker inspect -f '{{.Config.Image}}' "$node" 2>/dev/null || echo unknown)"
		echo "calico-node $(k -n kube-system get ds calico-node -o jsonpath='{.spec.template.spec.containers[?(@.name=="calico-node")].image}')"
		echo "api $api"
	} >>"$out/meta.txt"
	printf 'run\tgate_rc\tresult\tmid_burst_add\tburst_start\tburst_sends\trecords_read\tover_hang_timeout\tlongest_run\tstall_first_send\tstall_last_send\tsyn_retrans\tretrans_segs\tunknown_if_drops\tend_of_tier_drops\tend_of_tier_drops_burst\tcalico_warn_err\tcni_unblocking\tpods\tstress\n' >"$out/summary.tsv"

	local i=0 adds=0 last
	while :; do
		i=$((i + 1))
		run_one "$i"
		last=$(tail -n 1 "$out/summary.tsv")
		[ "$(cut -f4 <<<"$last")" != yes ] || adds=$((adds + 1))
		[ "$(cut -f3 <<<"$last")" != STALL ] || say "run $i stalled: the experiment ends (stop rule)"
		another_run "$i" "$adds" "$(cut -f3 <<<"$last")" || break
	done
	tail -n +2 "$out/summary.tsv" | verdict | tee "$out/verdict.txt" | sed 's/^/stage5-runs.sh: /'
}

# on_exit: the gate stopped, the diag namespace deleted, stress-ng off
# and the scratch dir gone.
on_exit() {
	local rc=$?
	# The cleanup must finish or report: no errexit (each step checks
	# itself), and bash ignores INT, TERM, HUP and PIPE (a closed terminal)
	# until it has.
	set +e
	trap '' INT TERM HUP PIPE
	trap - EXIT
	if [ "${touched:-0}" = 1 ]; then
		# The gate cancels and drains its burst on SIGINT (gate/cmd/gate:
		# signal.NotifyContext(os.Interrupt)).
		if [ -n "${gate_pid:-}" ] && kill -INT "$gate_pid" 2>/dev/null; then
			say "interrupting the running gate (pid $gate_pid) and waiting for it"
			wait "$gate_pid" 2>/dev/null || true
		fi
		jobs -p | xargs -r kill -TERM 2>/dev/null || true
		wait 2>/dev/null || true
		if ! k delete namespace "$ns" --ignore-not-found --wait=true --timeout=300s >/dev/null 2>&1; then
			say "namespace $ns not deleted; delete it by hand before any orchestrator, c0 or gate run" >&2
			rc=1
		fi
		k -n "$stress_ns" scale deploy/stress-ng --replicas=0 >/dev/null 2>&1 || say "stress-ng not scaled to 0; scale it by hand"
	fi
	if [ -n "${out:-}" ] && [ -d "$out" ] && [ ! -e "$out/verdict.txt" ]; then
		echo "STOPPED before the stop rule ended it (exit $rc): no verdict; summary.tsv holds the runs that completed" >"$out/verdict.txt"
	fi
	rm -rf "${work:-}"
	[ -z "${out:-}" ] || say "output in $out"
	exit "$rc"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	main "$@"
fi
