# Status: resume point for work after a gap

Maintained by Code sessions; every PR updates it (CLAUDE.md). Lines marked
*(inferred)* are guesses, not read from the repo; correct them.

## Now
- #81 (inferred): results write-up, overview and README refresh, STATUS cleanup

## Next
- Open follow-ups: docs/AFTER-FIRST-RUN.md (stress-based fitness calibration, if the owner wants it; a significance margin for culls; checking that the cull tie rule matches l.live order; the deploy/README.md host notes for Arch and Ubuntu). Nothing there blocks another run except as the owner decides.

## Parked
- driver TestSandboxTree fails on the workstation's Arch go1.27.3 (also on origin/main 91fdfe6): the toolchain's vendored x/crypto under /usr/lib/go/src/vendor/golang.org/x/crypto (TLS ciphers, no argon2) is visible in the sandbox; to be fixed in a separate driver PR (owner decision R1, 2026-11-05).
- Gate Details carry candidate-controlled output (sb.withOutput: Stage 1 boot, Stage 2, Stage 4 concurrency; the Stage 4 race-report excerpt is the output tail) and a mutant can log request bodies, so pool and fresh passwords can appear in Report Details. The driver (DESIGN.md sec 12 per-call context) must never pass gate Detail or candidate output into a prompt verbatim, or later mutants could learn pool pairs (table lookup, verify memoization). The integrity evidence kept in lineage (the offending response body, DESIGN.md sec 9) is candidate-controlled too and falls under the same rule. Driver side structural since #57: BuildPrompt reads lineage records and fitness.Params only, and lineage records hold no Detail or evidence; the orchestrator must keep it so (never a Detail in a record or in a prompt): orchestrator.WindowResult holds counts, reason names and scores only (#58). orchestrator.Loop records check names and reason tokens only, and logs a failing gate Detail to its stderr only, quoted and cut to 1 KiB (#59).
- Pending workstation (loadgen numbers; DESIGN.md sec 10, 15): (Q4) check the lag thresholds (p99 10 ms, any 100 ms) against a real run; (Q7) acceptance check: zero readiness failures for gen 0 at 4 x 1 pod (HPA pinned, PR B), stress on, rate r, over >= 3 windows.

## Recent
- #80 c0 on run 4 generation 7 recorded: C = 32 (gen 0: 31); docs/decisions.md, 2026-11-05
- #79 Run 4 recorded (run4-20261105T033013Z; exit 0, 8 generations, 36 turns, 9 windows, 0 excluded; best-ever 0.9066 gen 1 window 2; docs/decisions.md, 2026-11-05)
- #78 Run 3 recorded: GO (run3-20261105T021240Z; exit 0, 2 generations, 13 turns, 3 windows, 0 excluded; gen 0 stress success_rate 0.9954/0.9837/0.9552, spread 0.0331/0.0511 > drift 0.0198/0.0431; docs/decisions.md, 2026-11-05)
- #77 PR B: loadgen 768Mi, maxRate 50, maxBurstSends 6000; variant HPA pinned to 1/1; C0 = 31 re-measured, so run 3 uses r = 31, b = 124, n = 3,720 (docs/decisions.md, 2026-11-05)
- #76 deploy smoke (deploy.yml) runs by hand only, no PR or push trigger; ci.yml unchanged (docs/decisions.md, 2026-11-04)
