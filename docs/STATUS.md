# Status: resume point for work after a gap

Maintained by Code sessions; every PR updates it (CLAUDE.md). Lines marked
*(inferred)* are guesses, not read from the repo; correct them.

## Now
- `claude/status-file-housekeeping-00qv57` (PR #10): add this file, the STATUS.md working-agreement bullet, and its decisions entry.

## Next
- "Define how this repo's Code sessions hand material back to the planning/review chat."

## Parked
- Build internal/classifier (DESIGN.md sec 5, sec 8): the three-bucket classifier function (success/graceful_shed/failed, with verify ground-truth checking) and the shared test-vector pool, generated once as testdata by a small tool using internal/hashcore, including the gate-only negative set (non-exact params; very-large-m entries hand-constructed). Unblocks when: the hand-back PR is merged.

## Recent
- #9 decisions: record dropping GIT_COMMITTER_* from the cloud environment (2026-10-01)
- #8 CLAUDE.md: don't act on the cloud Stop hook's commit/push prompts (2026-10-01)
- #7 CI: vet, test, race, govulncheck (2026-10-01)
- #6 Generation 0: hashcore, wrapper seed, cmd/service (2026-09-27)
- #5 Add working agreement and decision log (2026-09-27)
