# Status: resume point for work after a gap

Maintained by Code sessions; every PR updates it (CLAUDE.md). Lines marked
*(inferred)* are guesses, not read from the repo; correct them.

## Now
- `claude/classifier-testvectors-pool-ymr1of` (PR #16 *(inferred)*): internal/classifier PR B of 2: test-vector pool in internal/classifier/testvectors (embedded vectors.json: 256 passwords x match/mismatch pairs, 7 gate-only negatives), its generator (./gen, via hashcore), pool-validity and hashcore-vs-classifier agreement tests; DESIGN.md sec 5/sec 7 make the Stage 1 allowlist match internal/classifier exactly.

## Next
- gate/ (DESIGN.md sec 7), consuming testvectors for Stage 2 interop and contract conformance (*(inferred)* next step; not yet planned).

## Parked
- Fitness (sec 9): treat integrity failures (verify_mismatch, verify_error, wrong_match, non_canonical_phc, salt_reused) as disqualifying, not one failed request: a mutant can relabel undelivered Argon2 results (timed-out clients) as hashes for later requests, caught only by sampled FullVerify.
- Verify-result caching: a finite pool lets a mutant memoize (password, hash) -> match and skip Argon2; loadgen should draw some verify pairs from the variant's own fresh /hash output.

## Recent
- #15 internal/classifier PR A: three-bucket response classifier with injected Verifier (2026-10-02)
- #14 handoff: shorten review-packet full diffs (2026-10-02)
- #13 CLAUDE.md: reconcile STATUS.md and decisions.md with main at PR start (2026-10-01)
- #12 CI: run only on Go-relevant changes, plus weekly and on demand (2026-10-01)
- #11 Add docs/handoff.md packet formats for the planning/review chat (2026-10-01)
- #10 Add docs/STATUS.md as the resume point after long gaps (2026-10-01)
- #9 decisions: record dropping GIT_COMMITTER_* from the cloud environment (2026-10-01)
- #8 CLAUDE.md: don't act on the cloud Stop hook's commit/push prompts (2026-10-01)
- #7 CI: vet, test, race, govulncheck (2026-10-01)
- #6 Generation 0: hashcore, wrapper seed, cmd/service (2026-09-27)
- #5 Add working agreement and decision log (2026-09-27)
