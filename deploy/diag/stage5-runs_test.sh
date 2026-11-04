#!/usr/bin/env bash
# Tests stage5-runs.sh's pure helpers without a cluster: the stop rule,
# the verdict, the record reduction and the drop-counter accounting. Run
# from anywhere: deploy/diag/stage5-runs_test.sh (exits non-zero on the
# first failure). Needs bash and awk (mawk or gawk).
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=deploy/diag/stage5-runs.sh
source ./stage5-runs.sh

fails=0
check() { # check NAME WANT GOT
	if [ "$2" = "$3" ]; then
		echo "ok   $1"
	else
		echo "FAIL $1: want '$2', got '$3'"
		fails=$((fails + 1))
	fi
}
yes_no() { if "$@"; then echo yes; else echo no; fi; }

# --- stop rule: at least 15, past 15 only while adds < 10, at most 25,
# the first run that is not PASS ends it, -try runs one.
try=0
check "another_run after 1" yes "$(yes_no another_run 1 1 PASS)"
check "another_run after 14, 14 adds" yes "$(yes_no another_run 14 14 PASS)"
check "another_run after 15, 10 adds" no "$(yes_no another_run 15 10 PASS)"
check "another_run after 15, 9 adds" yes "$(yes_no another_run 15 9 PASS)"
check "another_run after 24, 0 adds" yes "$(yes_no another_run 24 0 PASS)"
check "another_run after 25, 0 adds" no "$(yes_no another_run 25 0 PASS)"
check "another_run after a stall" no "$(yes_no another_run 1 0 STALL)"
check "another_run after a FAIL" no "$(yes_no another_run 1 0 FAIL)"
check "another_run after DROPS" no "$(yes_no another_run 1 0 DROPS)"
check "another_run after a FAULT" no "$(yes_no another_run 1 0 FAULT)"
try=1
check "another_run with -try" no "$(yes_no another_run 1 0 PASS)"
try=0

# --- verdict: summary.tsv rows (run gate_rc result mid_burst_add ...).
rows() { # rows N ADDS [RESULT_OF_LAST]
	local i
	for ((i = 1; i <= $1; i++)); do
		local m=no r=PASS
		[ "$i" -gt "$2" ] || m=yes
		[ "$i" -lt "$1" ] || r=${3:-PASS}
		printf '%s\t0\t%s\t%s\n' "$i" "$r" "$m"
	done
}
check "verdict MET at 10 adds" "runs 15, mid-burst adds 10, stalls 0 (0 in add runs), other FAILs 0, DROPS 0: MET" "$(rows 15 10 | verdict)"
check "verdict INCONCLUSIVE at 9 adds" "runs 25, mid-burst adds 9, stalls 0 (0 in add runs), other FAILs 0, DROPS 0: INCONCLUSIVE" "$(rows 25 9 | verdict)"
check "verdict STALLED" "runs 3, mid-burst adds 3, stalls 1 (1 in add runs), other FAILs 0, DROPS 0: STALLED" "$(rows 3 3 STALL | verdict)"
check "verdict: a FAIL run ends INCONCLUSIVE" "runs 15, mid-burst adds 15, stalls 0 (0 in add runs), other FAILs 1, DROPS 0: INCONCLUSIVE" "$(rows 15 15 FAIL | verdict)"
check "verdict: a DROPS run ends INCONCLUSIVE" "runs 15, mid-burst adds 15, stalls 0 (0 in add runs), other FAILs 0, DROPS 1: INCONCLUSIVE" "$(rows 15 15 DROPS | verdict)"
check "verdict: a short set with a DROPS run" "runs 3, mid-burst adds 3, stalls 0 (0 in add runs), other FAILs 0, DROPS 1: INCONCLUSIVE" "$(rows 3 3 DROPS | verdict)"
check "verdict: fewer than min_runs all-PASS rows" "runs 14, mid-burst adds 14, stalls 0 (0 in add runs), other FAILs 0, DROPS 0: INCONCLUSIVE" "$(rows 14 14 | verdict)"
check "verdict: a stall with a DROPS run is STALLED" "runs 3, mid-burst adds 3, stalls 1 (1 in add runs), other FAILs 0, DROPS 1: STALLED" "$( { rows 2 2 DROPS; printf '3\t1\tSTALL\tyes\n'; } | verdict)"
check "verdict unknown add is no add" "runs 1, mid-burst adds 0, stalls 0 (0 in add runs), other FAILs 0, DROPS 0: INCONCLUSIVE" "$(printf '1\t0\tPASS\tunknown\n' | verdict)"
try=1
check "verdict with -try" "runs 1, mid-burst adds 1, stalls 0 (0 in add runs), other FAILs 0, DROPS 0: none (-try)" "$(rows 1 1 | verdict)"
try=0

# --- records_tsv: pair and evidence never reach the output.
# shellcheck disable=SC2016 # a literal hash, not an expansion
body='{"records":[{"seq":1,"index":0,"kind":"hash","intended_wall":"2026-11-03T10:00:00Z","latency_ns":5,"bucket":"success","reason":"","skipped":false,"fault":false,"pair":{"password":"SECRETPW","hash":"$argon2id$x"},"evidence":"SECRETEV"}],"gap":false,"next":2}'
got=$(records_tsv <<<"$body")
check "records_tsv fields" "$(printf '0\thash\t2026-11-03T10:00:00Z\t5\tsuccess\t\tfalse\tfalse')" "$got"
check "records_tsv drops secrets" no "$(grep -qE 'SECRET|argon2' <<<"$got" && echo yes || echo no)"

# --- drop_rules: Felix's "Unknown interface" and end-of-tier Drop rules,
# hash comment cut, other rules ignored.
save='*filter
:cali-from-wl-dispatch - [0:0]
[0:0] -A cali-from-wl-dispatch -i calia1 -m comment --comment "cali:aaa" -g cali-fw-calia1
[5:300] -A cali-from-wl-dispatch -m comment --comment "cali:bbb" -m comment --comment "Unknown interface" -j DROP
[7:420] -A cali-fw-calia1 -m comment --comment "cali:ccc" -m comment --comment "End of tier default. Drop if no policies passed packet" -m mark --mark 0x0/0x10000 -j DROP
[9:9] -A cali-fw-calia1 -m comment --comment "cali:ddd" -m comment --comment "End of tier default. Pass if no policies passed packet" -j RETURN
COMMIT'
got=$(drop_rules <<<"$save")
want=$(printf '%s\n' \
	"$(printf '5\tunknown_if\t-A cali-from-wl-dispatch -m comment --comment "Unknown interface" -j DROP')" \
	"$(printf '7\tend_of_tier\t-A cali-fw-calia1 -m comment --comment "End of tier default. Drop if no policies passed packet" -m mark --mark 0x0/0x10000 -j DROP')")
check "drop_rules" "$want" "$got"

# --- drop_tally: rises summed, a fall is a rewrite counted from 0, a new
# rule counts from 0 after read 0, burst increases from the burst read on.
u='unknown_if	-A cali-from-wl-dispatch -m comment --comment "Unknown interface" -j DROP'
e='end_of_tier	-A cali-tw-calib2 -m comment --comment "End of tier default. Drop if no policies passed packet" -j DROP'
reads=$(printf '%s\n' \
	"0	5	$u" \
	"1	6	$u" \
	"2	9	$u" "2	3	$e" \
	"3	2	$u" "3	4	$e" \
	"4	4	$u" "4	4	$e")
tally=$(drop_tally 2 <<<"$reads")
# unknown_if: 5 ->6 (+1) ->9 (+3) ->2 (rewrite, +2) ->4 (+2) = 8; burst from
# read 2 on: +2 +2 = 4; one reset. end_of_tier: new at read 2 (+3), +1, +0
# = 4; burst: +1 +0 = 1.
check "drop_tally unknown_if row" "$(printf 'unknown_if\tcali-from-wl-dispatch\t5\t4\t8\t4\t1')" "$(awk -F'\t' '$1 == "unknown_if"' <<<"$tally" | cut -f1-7)"
check "drop_tally end_of_tier row" "$(printf 'end_of_tier\tcali-tw-calib2\t3\t4\t4\t1\t0')" "$(awk -F'\t' '$1 == "end_of_tier"' <<<"$tally" | cut -f1-7)"
check "drop_sum run" 8 "$(drop_sum unknown_if run_increase <<<"$tally")"
check "drop_sum burst" 1 "$(drop_sum end_of_tier burst_increase <<<"$tally")"
check "drop_sum no rule" - "$(drop_sum nothing run_increase <<<"$tally")"
tally=$(drop_tally "" <<<"$reads")
check "drop_tally without a burst read" - "$(drop_sum unknown_if burst_increase <<<"$tally")"
# A rule missing from a read counts from 0 when it is back (a reset).
tally=$(drop_tally "" <<<"$(printf '%s\n' "0	5	$u" "1	1	$e" "2	7	$u")")
check "drop_tally rule back after a gap" "$(printf 'unknown_if\tcali-from-wl-dispatch\t5\t7\t7\t-\t1')" "$(awk -F'\t' '$1 == "unknown_if"' <<<"$tally" | cut -f1-7)"

[ "$fails" -eq 0 ] || { echo "stage5-runs_test.sh: $fails failed"; exit 1; }
echo "stage5-runs_test.sh: PASS"
