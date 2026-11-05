# Status: where the work stopped

Development ended at #81. Documentation PRs follow until the repo goes
public; they are listed under Now. Lines marked *(inferred)* were not
checked against the tree.

## Now
- #84 (inferred): docs/workflow/ stays the #32 snapshot; WORKFLOW.md gains the evolution table and a retrospective (in progress)

## Next
- No development scheduled. The open backlog is docs/AFTER-FIRST-RUN.md.

## Parked
- driver `TestSandboxTree` fails on go1.27.x (seen on the Arch workstation's go1.27.3, also on origin/main 91fdfe6) and passes on CI's Go 1.26.10: the toolchain's vendored x/crypto under /usr/lib/go/src/vendor/golang.org/x/crypto (TLS ciphers, no argon2) is visible in the sandbox. Not fixed.
- Standing rule for any driver change: gate Details and candidate output can hold pool or fresh passwords, so the driver must never put them into a prompt verbatim, or later mutants could learn pool pairs. The driver's prompt reads lineage records and fitness parameters only, and the orchestrator keeps Details and evidence out of records. See DESIGN.md sec 12 (per-call context) and sec 9 (integrity evidence).

## Recent
- #83 run records published in docs/runs/ (run.json, summary.json, attempts.jsonl, stalls.jsonl, run.log of runs 1-4 and run 2's failed first attempt, c0.json of both c0 runs); docs/RESULTS.md and README.md corrected against them
- #82 documentation fixes before the repo goes public (README, RESULTS, STATUS, AFTER-FIRST-RUN, overview, deploy/README)
- #81 results write-up (docs/RESULTS.md), overview and README refresh for runs 1-4 and the c0 results, STATUS cleanup
- #80 c0 on run 4 generation 7 recorded: C = 32 (gen 0: 31); docs/decisions.md, 2026-11-05
- #79 Run 4 recorded (run4-20261105T033013Z; exit 0, 8 generations, 36 turns, 9 windows, 0 excluded; best-ever 0.9066 gen 1 window 2; docs/decisions.md, 2026-11-05)
