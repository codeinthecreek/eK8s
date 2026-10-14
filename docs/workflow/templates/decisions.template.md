# Decision log: implementation-level decisions, oldest first ({{DESIGN_DOC}} holds architecture)

<!-- Template. Format rules:
     - One entry per non-trivial decision, appended at the end (oldest first).
     - Heading: `## YYYY-MM-DD - PR #N - decision in one line`, optionally followed by
       `(task prompt; {{DESIGN_DOC}} sec N)` when the task prompt or the design doc
       already specifies it. ASCII only (the original uses a middle dot as the separator).
     - Entries are written before the PR is opened, so the number starts as `PR #N (inferred)`;
       the start-of-PR reconcile (CLAUDE.md) replaces it with the real number.
     - Lines: Rejected, Reason, Reverse (always). Optional: `Decision:` as the first line
       (when the heading cannot carry the whole decision), Revisit (the condition that
       should reopen the decision), `Note:` and `Follow-up:`. Max 5 lines per entry.
     - An architectural decision also needs a {{DESIGN_DOC}} edit, and says so under Reverse.
     Replace the EXAMPLE entry with real ones. -->

## 2026-01-12 - PR #9 - EXAMPLE: retries use a fixed 100 ms backoff, not exponential
- Rejected: exponential backoff with jitter.
- Reason: the upstream limit is per second, not per burst, so spacing retries evenly is enough and keeps tests deterministic.
- Revisit: if the upstream moves to a token-bucket limit.
- Reverse: replace `backoff()` in the client with the exponential version; no schema or config change.
