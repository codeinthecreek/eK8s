# Results: runs 1-4 and the c0 capacity measurements

This document reports what the first four evolutionary runs and two capacity
measurements (c0) showed. It is written for an engineer who knows Linux and
Kubernetes but not this project. Each claim is followed by the evidence for
it. A sentence that goes beyond the evidence starts with "Inference:".

The run records (the attempts log, summary and c0 output of each run) are not
in the repo; they stayed on the workstation that made the runs. The numbers
here are as recorded in the `docs/decisions.md` entries for PRs #77-#80
(runs 3 and 4, and the two c0 measurements), and for runs 1 and 2 in the
entries dated 2026-10-28 and 2026-10-31. "#N" is a pull request number in
this repo (`git log --grep '#N'` finds its merge). Process terms such as
"owner" are defined in the README. Runs are named by run id. Scores are
the project's fitness (0 to 1, higher is
better), described in `DESIGN.md` sec 9.

## Summary

One pod running the fixed Argon2id core can serve about 32 successes/s,
whatever the wrapper around it does, which matches what one CPU buys at
0.031 CPU-seconds per success (section 4 gives the measurements and labels
the inference). Evolution therefore found no
extra throughput. It found a shorter wait for a hashing slot, 500 ms down to
67 ms, which cut the stress-half p95 latency about 3 times (about 590 to about
184 ms) at the same throughput (sections 3.2 and 5). Most of the score gain
came in the first step, 500 to 250 ms; later steps are small and within noise
(section 3.4).

## 1. Setup in brief

- One workstation (8 cores, clocks pinned to `performance`, turbo off; how to
  pin them: `deploy/README.md`, "Host clocks") runs a kind cluster. Detail:
  `docs/overview.md`, `DESIGN.md` sec 2 and sec 11.
- Each candidate program ("variant") runs as one pod with a 250m CPU request
  and a 1000m CPU limit. Its wrapper has 2 concurrency slots for hashing.
  The 2 is derived (Go rounds a 1000m cgroup limit up to GOMAXPROCS = 2); it
  was not read at run time.
- The hashing core is fixed: Argon2id with t = 2, m = 19456 KiB, p = 1. It is
  not part of what evolves. A variant is the wrapper around it: how many
  requests wait, for how long, and when they are shed with an HTTP 503.
- The load is open-loop: requests are sent at a fixed rate r whether or not
  earlier ones have finished. Runs 3 and 4 use r = 31 requests/s per variant.
- A window is a baseline half (300 s of load, no neighbour) followed by a
  stress half (300 s of load while a stress-ng neighbour takes about 3 CPUs:
  3 CPU workers at 100% load, `deploy/stress/dose.yaml:11-12`, request 2750m
  and limit 3, `deploy/stress/deployment.yaml:70,73`). A variant's window score is the lower
  of its two halves' scores (`DESIGN.md` sec 9, "fitness = min").
  A half scores 0.4 x success rate + share served x (0.3 x latency score +
  0.3 x CPU score) (`fitness/params.go:34-45`, `fitness/score.go:134`).
  Success rate is (successes + 0.5 x graceful 503 sheds) / requests
  (`fitness/score.go:117`, `fitness/params.go:40`). The latency score falls
  linearly from 1 at p95 = 25 ms to 0 at 2 s, and the CPU score from 1 at
  0.025 to 0 at 0.5 CPU-seconds per success. The share served (successes /
  requests) scales the latency and CPU terms, so a variant that sheds more
  keeps less of them (`DESIGN.md` sec 9).
- Several variants are live at once and are scored in the same window.
  After each full window the lowest-ranked is culled (once four are live),
  and the AI proposes a new variant from a parent. The AI edits only the
  wrapper source (`internal/wrapper/`), in a sandbox.
- c0 is a separate tool (`orchestrator/cmd/c0`; `deploy/README.md`, "C0")
  that finds C0: the highest rate one generation-0 pod
  sustains with at most 1% of requests shed or failed and p95 of successes at
  most 500 ms (`HangTimeout`/4, `HangTimeout` = 2 s), at no stress.

## 2. Runs 1-4

| Run id | Start (UTC) | Wall clock | r (req/s) | burst b (req/s) | Generations | AI turns | AI cost (USD, as reported by claude -p) | Windows | Excluded | Outcome |
|---|---|---|---|---|---|---|---|---|---|---|
| run1 | 2026-10-28 04:53 | 1 h 22 min | 11 (provisional) | 44 (inferred: b = 4r) | 10 | 43 | 0.198 | 11 (38 variant scores) | 0 | exit 0; baseline-only windows |
| run2b | 2026-10-31 11:17 | 39 min | 15.5 | 62 | 2 | 13 | 0.071 | 3 | 0 | exit 0; owner verdict STOP |
| run3-20261105T021240Z | 2026-11-05 02:15 | 45 min | 31 | 124 | 2 | 13 | 0.055 | 3 | 0 | exit 0; owner verdict GO |
| run4-20261105T033013Z | 2026-11-05 03:35 | 2 h 36 min | 31 | 124 | 8 | 36 | 0.171 | 9 | 0 | exit 0; 8 of 8 mutations passed the gate |

Wall clock runs from the first gate line of the run's log to the time in its
`summary.json`. Cost is `usage.cost_usd` from `summary.json`, summed over the
run's AI turns, as reported by claude -p. run1 and run2b are the run ids as
recorded; those two runs predate the timestamped ids.

run2b is the second attempt at run 2. The first attempt failed its own
Stage 5 gate on generation 0 before any AI turn; that was traced to a network
stall in the kindnet CNI, which Calico replaced in PR #71
(`docs/decisions.md`, 2026-11-02 and 2026-11-03). Burst b is the rate of the
gate's Stage 5 burst. Run 1's b is not in its record; 44 follows from the
b = 4r rule and is an inference.

What each run established:

- **run1**: the pipeline works end to end (10 of 10 mutations passed the
  gate, no exclusions). It did not discriminate between variants. r = 11 was
  far below capacity, windows had no stress half, every variant had
  success rate 1, and the CPU floor (0.05 s) sat above the host's measured
  0.0424 s per success, so that term clamped to 1. Inference: culls ranked
  p95 jitter (`docs/decisions.md`, 2026-10-28).
- **run2b**: a stress half was added and floors were recalibrated. Load was
  still far below capacity: generation 0's stress-half success rate was 1
  in all three windows, and window-to-window drift of the same variant
  (about 0.004) exceeded the spread between variants in one window (about
  0.0002). Verdict STOP (`docs/decisions.md`, 2026-10-31).
- **run3-20261105T021240Z**: r was raised to C0 = 31 (c0, PR #77). Generation 0
  now degraded under stress: stress-half success rate 0.9954, 0.9837, 0.9552 and
  p95 568-598 ms across the three windows. Spread between variants in a
  window (0.0331, 0.0511) exceeded generation 0's window-to-window drift
  (0.0198, 0.0431) both times. Verdict GO. Caveat recorded with it: live
  count grew 1, 2, 3 across the windows and generation 0's score fell with it,
  so drift and spread are confounded.
- **run4-20261105T033013Z**: the first run long enough to show a trend.
  Section 3.

## 3. Run 4

### 3.1 How the lineage changed

Every one of the 8 mutations changed one line of one file, the constant
`acquireWait` in `internal/wrapper/tunables.go:21`: how long a validated
request waits for a hashing slot before it is shed with a 503. Evidence: a
diff of each genome against its parent in the run's genome archive; nothing
else under `internal/wrapper/` differs. The AI's explanation is the
`rationale` it recorded with each attempt. All 8 say the change was not built
or run, so its effect was untested when proposed. That is by design: the AI's
sandbox allows only the Read and Edit tools (`docs/overview.md` sec 11), so it cannot build or run anything, and it sees only a scratch copy of
the wrapper.

| Gen | Parent | acquireWait (parent -> gen) | AI's stated reason, paraphrased |
|---|---|---|---|
| 0 | none | `HangTimeout`/4 = 500 ms | the seed wrapper |
| 1 | 0 | 500 -> 250 ms (/4 -> /8) | Stress p95 (571 ms) sits near the 500 ms queue cap; a shorter wait sheds sooner and should lower p95 at the cost of more 503s. |
| 2 | 1 | 250 -> 154 ms (/8 -> /13) | Lower p95, the weakest component, by shedding slot-less requests sooner. |
| 3 | 2 | 154 -> 250 ms (/13 -> /8) | Gen 2 scored 0.881 against gen 1's 0.889, so the shorter wait did not help; go back. |
| 4 | 2 | 154 -> 200 ms (/13 -> /10) | 250 and 154 ms looked about equally good; try between them. |
| 5 | 3 | 250 -> 333 ms (/8 -> /6) | Nothing at 154-250 ms helped; test the other direction, a longer wait to turn sheds into successes. |
| 6 | 3 | 250 -> 100 ms (/8 -> /20) | 154-333 ms gave no clear gain; try a clearly shorter wait. |
| 7 | 6 | 100 -> 67 ms (/20 -> /30) | The step to 100 ms raised fitness, so continue in that direction. |
| 8 | 5 | 333 -> 67 ms (/6 -> /30) | Gen 6's step to 100 ms raised fitness, so test a still shorter wait. |

Gen 7's ancestry is 0 -> 1 -> 2 -> 3 -> 6 -> 7. Two genomes recur: gen 3 has
the same genome hash as gen 1 (250 ms), and gen 8 the same as gen 7 (67 ms).

**Net diff, gen 0 to gen 7 (confirmed against the genome archive; the only
differing file is `tunables.go`):**

```
-	acquireWait = classifier.HangTimeout / 4
+	acquireWait = classifier.HangTimeout / 30
```

PR #80's entry records the same one-line diff.

### 3.2 Stress p95 down the lineage

Stress-half values, mean over the windows in which each generation was scored
(range in brackets). Generations 4, 5 and 8 are off gen 7's lineage.

| Gen | Windows scored | acquireWait | Stress p95 (ms) | Stress success rate | Stress cpu/success (s) |
|---|---|---|---|---|---|
| 0 | 4 | 500 ms | 592 (571-614) | 0.964 (0.929-0.994) | 0.0345 |
| 1 | 8 | 250 ms | 362 (358-366) | 0.945 (0.931-0.975) | 0.0362 |
| 2 | 6 | 154 ms | 270 (260-273) | 0.934 (0.928-0.950) | 0.0365 |
| 3 | 4 | 250 ms | 364 (362-365) | 0.940 (0.936-0.942) | 0.0371 |
| 4 | 1 | 200 ms | 314 | 0.928 | 0.0363 |
| 5 | 1 | 333 ms | 450 | 0.937 | 0.0375 |
| 6 | 3 | 100 ms | 213 (211-215) | 0.932 (0.930-0.934) | 0.0383 |
| 7 | 2 | 67 ms | 184 (182-186) | 0.936 (0.935-0.936) | 0.0355 |
| 8 | 1 | 67 ms | 182 | 0.933 | 0.0373 |

p95 falls monotonically with the wait: about 600, 450, 360, 314, 270, 213,
184 ms for 500, 333, 250, 200, 154, 100, 67 ms. Within a window with four live
variants, the stress-half success rate stays in a narrow band whatever the wait:
in window 9, gens 1, 6, 7, 8 have 0.938, 0.930, 0.935, 0.933. Generation 0's
mean success rate looks higher (0.964) because it was scored in windows 1-4
with 1 to 4 live variants (section 3.4); in window 4, with four live, it is
0.929. Inference: p95 tracks roughly the wait plus about 115 ms (the gap is
92-117 ms across the seven wait settings), and the shorter wait removes slow
successes by turning them into fast sheds without changing how many requests
are lost.

Comparing gen 0 and gen 7 stress p95 crosses windows, which section 3.4 rules
out for scores. For p95 alone it is justified because stress p95 barely moves
with live count: gen 0 stayed at 571-614 ms over live counts 1-4, and gen 1 at
358-366 ms over live counts 2-4.

### 3.3 Per-window rankings

Window score (rank) for each live variant; blank = not live. A variant is
culled after the window in which it ranks last with four live.

| Win | Live | Gen 0 | Gen 1 | Gen 2 | Gen 3 | Gen 4 | Gen 5 | Gen 6 | Gen 7 | Gen 8 | Culled after |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 1 | 0.9033 (1) | | | | | | | | | none |
| 2 | 2 | 0.8789 (2) | 0.9066 (1) | | | | | | | | none |
| 3 | 3 | 0.8430 (3) | 0.8887 (1) | 0.8808 (2) | | | | | | | none |
| 4 | 4 | 0.8046 (4) | 0.8491 (2) | 0.8505 (1) | 0.8459 (3) | | | | | | gen 0 |
| 5 | 4 | | 0.8380 (3) | 0.8481 (2) | 0.8491 (1) | 0.8370 (4) | | | | | gen 4 |
| 6 | 4 | | 0.8450 (3) | 0.8542 (2) | 0.8546 (1) | | 0.8360 (4) | | | | gen 5 |
| 7 | 4 | | 0.8517 (2) | 0.8512 (3) | 0.8408 (4) | | | 0.8577 (1) | | | gen 3 |
| 8 | 4 | | 0.8533 (3) | 0.8474 (4) | | | | 0.8626 (2) | 0.8717 (1) | | gen 2 |
| 9 | 4 | | 0.8491 (4) | | | | | 0.8561 (3) | 0.8684 (1) | 0.8647 (2) | gen 1 |

Gen 7 ranks first in both windows it was scored (8 and 9). Gen 6 ranks first
in window 7 and second in window 8.

### 3.4 What can and cannot be compared

**Compare variants only within a window.** All variants in a window face the
same stress, the same host state and the same live count.

Three measurements of noise are in the records:

1. Live count moves every score. Generation 0 alone scored 0.9033, then 0.8789,
   0.8430, 0.8046 in windows 1-4 as the live count went 1, 2, 3, 4 (its p95
   barely moved, 571-614 ms). Run 3 showed the same fall with live count.
   Cause not isolated: live count and elapsed time are confounded.
2. Same variant, windows 5-9 (live count constant at 4): the range between a
   variant's best and worst window is 0.0153 (gen 1), 0.0068 (gen 2),
   0.0138 (gen 3), 0.0065 (gen 6) and 0.0033 (gen 7, two windows). Largest
   change between consecutive windows: 0.0138 (gen 3, window 6 to 7).
3. Identical genomes in the same window: gen 1 and gen 3 (same hash) differ
   by 0.0111, 0.0096 and 0.0109 in windows 5-7 and by 0.0032 in window 4, and
   the sign flips (gen 3 higher in 5 and 6, gen 1 higher in 7). Gen 7 and
   gen 8 (same hash) differ by 0.0037 in window 9.

The gen 7 margin over gen 1 is 0.0184 (window 8) and 0.0193 (window 9), about
1.2 to 3 times the drift in measurement 2 and about 1.7 times the largest
identical-genome gap in measurement 3. Gen 7 wins both windows against gen 1,
and the p95 gap (184 vs 362 ms) is far outside any p95 noise in the table
above. Inference: gen 7 is better than gen 1, but the score margin alone is
not strong evidence, and the ordering of gens 6, 7 and 8 (67 to 100 ms) is not
resolved.

Gen 1 beat gen 0 by 0.0277, 0.0457 and 0.0445 in windows 2, 3 and 4, and gens
2 and 3 beat it by 0.041 to 0.046 in window 4, several times the drift. The
500 ms to 250 ms step is the clear gain. Steps after it are small.

**Why 0.9066 is not a fair comparison.** The best-ever score of the run,
0.9066, is gen 1 in window 2, with two live variants. Gen 1 scored 0.838 to
0.853 in windows 5-9 with four live. No later variant beat 0.9066 (gen 7's
best is 0.8717), but later variants faced a window with four live variants
and gen 1 itself scored 0.05 to 0.07 lower in windows 5-9 than in window 2
(live count and elapsed time both changed; the cause is not isolated).
Scores from different windows reflect live count as well as quality, so "never beaten"
says nothing about gen 7 versus gen 1. The within-window comparison above is
the fair one.

## 4. Capacity (c0)

c0 steps the rate up a ladder against one pod of one wrapper, at no stress,
and reports per step the requests, sheds, failures, successes, p95 and CPU
per success. Runs: c0-20261105T000346Z (gen 0, PR #77) and c0-run4g7-20261105
(gen 7, PR #80). Settings were the same: pinned clocks, ladder
8, 16, 24, 28, 32, 36, 40, 44, 48, confirm 2, one pod, 0 restarts. Pass
means at most 1% shed or failed and p95 at most 500 ms.

Generation 0 (image ek8s-wrapper:ecb783ac3370), in the order run:

| r | requests | shed | failed | successes | p95 ms | cpu/success s | verdict |
|---|---|---|---|---|---|---|---|
| 8 | 2362 | 0 | 0 | 2362 | 37.5 | 0.0324 | pass |
| 16 | 4710 | 0 | 0 | 4710 | 35.6 | 0.0317 | pass |
| 24 | 7077 | 0 | 0 | 7077 | 34.8 | 0.0299 | pass |
| 28 | 8217 | 0 | 0 | 8217 | 33.5 | 0.0305 | pass |
| 32 | 9390 | 27 | 0 | 9363 | 552.2 | 0.0311 | fail |
| 30 | 8823 | 0 | 0 | 8823 | 34.3 | 0.0315 | pass |
| 31 | 9104 | 4 | 0 | 9100 | 402.9 | 0.0307 | pass |
| 31 | 9112 | 0 | 0 | 9112 | 324.3 | 0.0305 | pass |
| 31 | 9112 | 0 | 0 | 9112 | 398.5 | 0.0299 | pass |

Generation 7 (image ek8s-wrapper:d38833d16929), in the order run:

| r | requests | shed | failed | successes | p95 ms | cpu/success s | verdict |
|---|---|---|---|---|---|---|---|
| 8 | 2342 | 0 | 0 | 2342 | 33.7 | 0.0306 | pass |
| 16 | 4720 | 0 | 0 | 4720 | 33.1 | 0.0313 | pass |
| 24 | 7042 | 0 | 0 | 7042 | 33.3 | 0.0311 | pass |
| 28 | 8221 | 0 | 0 | 8221 | 33.0 | 0.0306 | pass |
| 32 | 9388 | 22 | 0 | 9366 | 124.2 | 0.0316 | pass |
| 36 | 10565 | 1209 | 0 | 9356 | 161.2 | 0.0318 | fail |
| 34 | 9993 | 623 | 0 | 9370 | 157.4 | 0.0315 | fail |
| 33 | 9713 | 341 | 0 | 9372 | 153.4 | 0.0317 | fail |
| 32 | 9392 | 7 | 0 | 9385 | 109.5 | 0.0313 | pass |
| 32 | 9392 | 23 | 0 | 9369 | 127.4 | 0.0312 | pass |

Results: **C = 31 for gen 0 and C = 32 for gen 7.**

- Below r = 28 the pod keeps up: no sheds, p95 33-38 ms, for both.
- Successes per step plateau at about 9370 from r = 32 up. Gen 7: 9366,
  9385, 9369 at r = 32; 9372 at 33; 9370 at 34; 9356 at 36, while requests rise
  from 9388 to 10565. Gen 0 was run only to r = 32 (9363, the same plateau),
  so its plateau above 32 is not measured.
- cpu/success is flat at about 0.031 s (0.0299-0.0324) across all steps of
  both runs.
- The plateau matches one CPU, the pod's limit. A step sends for about 293 s (requests / r,
  e.g. 9388 / 32; inferred from the data, not read from a setting), so 9370
  successes is about 32 successes/s. 32/s x 0.031 s = about 1.0 CPU, the
  pod's limit. Inference: the pod is CPU-bound at its limit and no wrapper
  policy can raise throughput above that, because the hashing cost per
  success is fixed by the core.
- Gen 7's higher C comes from passing the p95 gate at r = 32, not from more
  throughput. At r = 32 gen 0 fails on p95 (552 ms; its shed rate, 0.29%, is
  within the 1% limit). Gen 7 passes with p95 110-127 ms and 7 to 23 sheds in
  three repeats. At r = 33 gen 7 fails on shed rate (341 of 9713, 3.5%).
  Gen 0 passed at r = 31 with p95 of 324-403 ms, near the 500 ms limit, so the
  gate tolerates a long queue wait; gen 7's short wait is what keeps p95 low.

## 5. Conclusion

The fixed Argon2id core caps one pod at about 32 successes/s whatever the
wrapper does (section 4). So evolution had no throughput to find. What it
found was a load-shedding policy: wait for a hashing slot for 67 ms instead
of 500 ms, and shed the rest with a 503. Under stress that cut the p95 tail
from about 590 ms (gen 0) to about 184 ms (gen 7), roughly 3 times (a
cross-window comparison, justified for p95 in section 3.2), with the stress
success rate no lower: at four live variants gen 0 had 0.929 (window 4) and
gen 7 had 0.935 and 0.936 (windows 8 and 9). These are different windows, so
the success-rate comparison is indicative only. Most of the score gain came in one step (500 ms to 250 ms,
section 3.4); the later steps down to 67 ms are smaller and their ordering is
within noise.

## 6. Limitations

- One workstation, one kind cluster, one pinned-clock configuration. Nothing
  here says how the numbers move on other hardware.
- One run per configuration. Run 4 is a single trajectory; repeating it might
  not stop at the same wait.
- Live-count changes confound window scores (section 3.4). Only same-window
  comparisons are used for ranking claims.
- Score margins between gens 1, 6, 7, 8 are about 1.2 to 3 times the measured
  drift, so they are indicative, not significant. No significance test was
  run.
- The AI changed only one constant. Run 4 therefore says little about whether
  evolution can find structural changes to the wrapper.
- Run 4 did not record the image tags it deployed. The gen 7 c0 image
  (d38833d16929) was rebuilt from the recorded gen 7 genome (its digest
  matches the record), so it cannot be tied to the image that run 4
  deployed. Both c0 runs were made on a tree with uncommitted changes
  (`repo_dirty` is true in both records). For gen 0 the only uncommitted change
  was `docs/STATUS.md` (`docs/decisions.md`, PR #77 entry). For gen 7 it was
  the gen 7 `tunables.go`, the change under test (the worktree's git status
  before the run showed only `internal/wrapper/tunables.go` modified; the c0
  record itself holds only `repo_dirty: true`). The slot count of 2 is derived,
  not read at run time.
- c0 measures at no stress. Stress-half cpu/success in run 4 (0.034-0.038 s) is
  higher than c0's 0.031 s; the cause was not investigated.

## 7. Reproducing these results

Bring up the cluster and run the orchestrator as in the README ("Bring up
the cluster"). Repeat the capacity measurement with `orchestrator/cmd/c0`,
following `deploy/README.md` ("C0"), with pinned host clocks. Runs 3 and 4
used r = 31, the C0 of generation 0.
A repeat will not give the same lineage: the AI's proposals and the parent
draws differ, and section 6 lists the other limits.
