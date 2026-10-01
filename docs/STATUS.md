# Status: resume point for work after a gap

Maintained by Code sessions; every PR updates it (CLAUDE.md). Lines marked
*(inferred)* are guesses, not read from the repo; correct them.

## Now
- `claude/ci-workflow-path-filtering-irtpz5` (PR #12 *(inferred)*): CI runs only when Go, go.mod/go.sum, testdata or workflow files change, plus a weekly schedule and manual dispatch so govulncheck still runs.

## Next
- Build internal/classifier (DESIGN.md sec 5, sec 8): the three-bucket classifier function (success/graceful_shed/failed, with verify ground-truth checking) and the shared test-vector pool, generated once as testdata by a small tool using internal/hashcore, including the gate-only negative set (non-exact params; very-large-m entries hand-constructed).

## Parked
- (none)

## Recent
- #11 Add docs/handoff.md packet formats for the planning/review chat (2026-10-01)
- #10 Add docs/STATUS.md as the resume point after long gaps (2026-10-01)
- #9 decisions: record dropping GIT_COMMITTER_* from the cloud environment (2026-10-01)
- #8 CLAUDE.md: don't act on the cloud Stop hook's commit/push prompts (2026-10-01)
- #7 CI: vet, test, race, govulncheck (2026-10-01)
- #6 Generation 0: hashcore, wrapper seed, cmd/service (2026-09-27)
- #5 Add working agreement and decision log (2026-09-27)
