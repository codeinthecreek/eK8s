# Status: resume point for work after a gap

<!-- Template. Keep the header paragraph, the four sections and the format comment below;
     replace the example lines (marked EXAMPLE) with real ones. -->

Maintained by Code sessions; every PR updates it (CLAUDE.md). Lines marked
*(inferred)* are guesses, not read from the repo; correct them.

<!-- Format rules:
     Now    = work in flight: `branch` (PR #N), then what it changes; PR number marked
              *(inferred)* until git log confirms it. Empty when nothing is in flight.
     Next   = queued work, in order, with the constraints the next session needs.
     Parked = known problems and pending owner actions, each with the cause or the check
              that unparks it; guesses carry *(inferred)*.
     Recent = merged work, newest first: `- #N summary (YYYY-MM-DD)`, real PR numbers only.
     The start-of-PR reconcile moves Now items to Recent (CLAUDE.md). -->

## Now
- EXAMPLE: `claude/add-retry-budget` (PR #12 *(inferred)*): the client gains a per-request retry budget (default 3, config key `retries`); tests cover budget exhaustion; the design doc's Errors section is updated.

## Next
- EXAMPLE: metrics PR: export the retry count per request; keep label cardinality fixed (decisions entry for PR #9).

## Parked
- EXAMPLE: the integration test `TestRetryBackoff` fails about 1 run in 50 on a 2-CPU runner. Cause not diagnosed *(inferred: timer granularity)*.
- EXAMPLE: pending owner action: rotate the staging credentials before the first live run.

## Recent
- EXAMPLE: #11 Config loader reads `retries` from the environment; invalid values are rejected at start-up (2026-01-15)
- EXAMPLE: #10 Add docs/STATUS.md as the resume point after long gaps (2026-01-14)
