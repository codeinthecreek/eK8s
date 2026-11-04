# Handoff packets: what Code sessions hand back to the planning/review chat

The planning/review chat cannot read the repo; it sees only what the owner
pastes from a Code session. A packet is the smallest paste that lets the
chat make a design decision, review code, or let the owner approve a
commit or push. Code produces a packet only when the owner asks for one
by name ("design packet", "review packet", "approval packet", "sync
packet since <sha>" or "sync packet since #N"; CLAUDE.md, "Working mode:
autonomous").

## Rules for every packet

- CLAUDE.md "Reply rules" apply to every packet: the packet is the final
  text of the reply, pasted in full, and its "main" is origin/main.
- Output exactly one code block fenced with FOUR backticks, and nothing
  outside it, so it is a single copy on an iPad.
- Every diff in a packet is pasted verbatim from the git command's
  output, never retyped or reassembled; if it is too long, split it into
  several verbatim parts, each labelled with its own command.
- ASCII only. Transliterate any non-ASCII in quoted repo text (e.g. a
  middle dot to `-`) and say so in one line of the packet.
- First line, one header line: packet type, branch, HEAD short SHA, main,
  date, e.g.
  `REVIEW PACKET | claude/foo-ab12cd | HEAD a0bceaa | main 37ea0ef (#21) | 2026-10-01`
  (the Sync packet has its own header).
  main is `git rev-parse --short origin/main` after
  `git fetch origin main`; #N is the most recent "Merge pull request #N"
  on origin/main (`git log origin/main --grep='^Merge pull request' -1`).
- The chat cannot open repo files: quote the lines that matter (from
  DESIGN.md, CLAUDE.md, docs/decisions.md, docs/STATUS.md or code)
  instead of citing a file alone. Quote only the relevant lines, each
  quote headed by `path:start-end`.
- Mark everything that is inference or recommendation rather than fact
  read from the repo or from command output: end the line with
  `(inferred)`, or start it with `Recommendation (mine):`.
- Command output only where it is evidence: exit status, and for a
  failure only the failing part (test name, error, a few lines of
  context). Never full logs.
- Last section, `Omitted:` listing anything left out for size, so the
  chat can ask for it (or `Omitted: nothing`).
- When any owner reply since the last packet carries chat feedback,
  conditions, or an approval with changes (e.g. "approved, but ..."), the
  next packet opens (after the header) with a `Feedback:` section: one
  line per item,
  `<item> -- done | declined: <reason> | question: <question>`, so the loop
  closes visibly. A plain approval with no conditions is not feedback.
- Review and approval packets also carry a `Self-review:` section
  (in an approval packet after `Feedback:` when there is one; in a review
  packet after `Added complexity:`, see "Review packet"): the reviewer
  subagent's findings on the final tree (CLAUDE.md), one line per finding,
  `<n> <severity> <file:line> <finding> -- fixed | declined: <reason> | deferred: <where recorded>`,
  or `Self-review: skipped (docs-only: only Markdown files changed)`.
- When the change under feedback was self-reviewed, end each `Feedback:`
  line with one tag: `[missed]` (checkable in-repo, reviewer did not
  report it), `[repo-unseeable]` (needed context outside the repo) or
  `[dup]` (reviewer had already reported it). `missed` items are
  candidates for the reviewer's checklist (.claude/agents/reviewer.md).
- A packet that ends a task also carries the report items (CLAUDE.md
  (a)-(g)) not already covered, after the type-specific sections and
  before `Omitted:`: `Read:` (files the owner must understand vs
  boilerplate), `Verify:` (exact commands, one per line), `Notes:` (Go
  idioms or concurrency patterns used; omit when there are none), a line
  saying docs/STATUS.md was updated in this PR, and uncommitted/unpushed
  state. (a) maps to Task/stat/Feedback and (b) to the `(inferred)`
  lines. Design packets carry only the items that apply.
- A `Verify:` line for a test of a probabilistic check, or a seed or
  negative test for one (randomized inputs, timing, concurrency,
  retries), carries its `-count` (default `-count=20`), and the packet
  states the count the test was validated at.

## Chat side

The planning chat works from copies of repo files that go stale. Before
summarizing project state or writing docs from its copies, it asks for
the current origin/main SHA and compares it with the newest PR in
docs/STATUS.md Recent. When a packet header's main is newer than its
copies, it asks for a sync packet (or for the project-knowledge files to
be re-added from GitHub) before relying on them.

A prompt that asks Code for answers to Feedback quotes each item's text
in full, since compaction drops earlier messages.

## Which packet

When the owner asks for a packet at the end of a task without naming
its type, pick by what the next step needs:

- Go change the chat has not reviewed: review packet.
- Docs-only change, or a change already reviewed: approval packet.
- A choice that blocks work: design packet.
- Read-only investigation: ANSWER PACKET, its answer in one four-backtick
  block, with the same header and `(inferred)` rules.

## Design packet

For a decision the owner needs to make before or during implementation.

- Question: one or two lines.
- Constraints: quoted lines that bear on it (non-negotiables from
  CLAUDE.md/DESIGN.md, relevant docs/decisions.md entries).
- Options: 2-4, each with concrete trade-offs and what it would change in
  the repo (files, packages, DESIGN.md sections).
- `Recommendation (mine):` one option and the main reason.
- Blocked until decided: what cannot proceed.
- Code: short excerpts, only if the decision depends on them.

## Review packet

For code review of a change.

- Order: header, `Feedback:` (when there is one), Task, `Added
  complexity:`, `Self-review:`, then the items below in turn.
- Task: the PR title, or the first line of the prompt the session
  received, verbatim; then:
  - Intent: 1-2 lines, the problem this change solves.
  - Constraints touched: each DESIGN.md or CLAUDE.md non-negotiable, or
    docs/decisions.md entry, the change relies on or alters, quoted as
    above (`path:start-end`), or "none".
  These come first, so the owner can review intent before the diff.
- `Added complexity:`, after Task: one line for each new validation,
  check, defence, retry, limit or guard the change adds,
  `<file:line> <what it adds> -- threat: concrete: <demonstrated case, failing test, or a cheat or integrity reason DESIGN.md sec 8-9 names> | hypothetical: <one line>`,
  ending `(task prompt)` when the task prompt asked for it; or
  `Added complexity: none`. A risk DESIGN.md sec 15 lists as an accepted
  limit is never grounds for a guard beyond the mitigation sec 15 itself
  names (for example, the driver never passing candidate output into a
  prompt). A hypothetical item is not built by
  default; it goes to docs/AFTER-FIRST-RUN.md (CLAUDE.md finding
  routing), unless the owner asks for it (a task prompt that specifies it
  counts as asking). Exception: a
  hypothetical that bears on a non-negotiable (hashcore isolation,
  protected paths, fail-closed classification, containment) is never
  auto-parked; it goes to the owner as a question (in the escalation
  batch, CLAUDE.md "Working mode: autonomous", or in the packet when there
  is one), and the guard stays in the change until the owner answers. A hypothetical
  fitness-integrity guard is parked like any other: per DESIGN.md sec 7,
  that concern rests on the live checks and the sec 15 accepted limits.
  Nothing in this section weakens an existing fail-closed rule.
- Base and HEAD short SHAs. Base is `git merge-base origin/main HEAD`. If
  the owner says "since <sha>", diff from that SHA instead and say so
  (re-review after fixes); `<base>` below then means that SHA.
- Protected paths touched: output of
  `git diff --name-only <base> -- internal/hashcore internal/classifier internal/wrapper/hasher.go`,
  or "none". State whether any Argon2 parameter literal changed (the
  literals live in internal/hashcore/hashcore.go `paramsSegment` and
  `params()`; also check
  `git diff -U0 <base> -- '*.go' | grep -E '^[-+].*(19456|paramsSegment|Memory:|Iterations:|Parallelism:)'`,
  limited to Go files so docs quoting the pattern never match).
- Checks, one line each, pass/fail: `go build ./...`, `go vet ./...`,
  `go test ./...`, `gofmt -l .` (passes on empty output). On failure,
  only the failing part.
- `git diff --stat <base>`
- Full diff, in three parts, all excluding generated files (a
  `// Code generated ... DO NOT EDIT.` header); list any excluded under
  `Omitted:`.
  - Go files except tests:
    `git diff -W <base> -- '*.go' ':(exclude)*_test.go'`. Git's default
    function-boundary rule already gives whole Go functions with their
    doc comments (checked on git 2.43), so no `.gitattributes` entry is
    needed.
  - Go test files: `git diff -U10 <base> -- '*_test.go'`.
  - Everything else:
    `git diff <base> -- . ':(exclude)*.go' ':(exclude)go.sum' ':(exclude)docs/STATUS.md'`
    (no -W: on Markdown it widens to the whole file).
  - docs/STATUS.md: no diff unless the owner asks; it stays in the
    `--stat` and is listed under `Omitted:`.
- If the full diff exceeds about 600 lines: everything except the full
  diff, plus a per-file line count of it, so the owner can choose what to
  include.
- Before a commit exists: mark untracked files with `git add -N <files>`,
  then diff the working tree against the base (the commands above, no
  HEAD on the right); list those files. Undo the marks afterwards with a
  path-limited `git reset -q -- <the files you marked>`, never a bare
  `git reset`, so any deliberately staged changes are left alone.
- Shallow clone: if `git merge-base origin/main HEAD` fails, run
  `git fetch --deepen=50 origin main` (repeat, or `--unshallow`) until it
  resolves. Never fall back to a two-dot diff against origin/main.

## Approval packet

For the owner's go-ahead to commit or push.

- Commit: the proposed commit message verbatim; `git diff --stat` of what
  would be committed; the protected-paths line and the checks lines (as in
  the review packet); anything left uncommitted or untracked after the
  commit.
- Push: branch, target (remote and branch), and each commit to be pushed
  as: short SHA, subject, author email, committer email, signed yes/no.
  Signed comes from the raw header
  (`git cat-file commit <sha> | grep -qE '^gpgsig(-sha256)? '`), not from
  `%G?`, which needs the signer's key locally.

## Sync packet

For bringing the chat's copies up to date. The owner asks for "sync packet
since <sha>" or "sync packet since #N"; if neither is given, ask for one.
Resolve #N to its merge commit with
`git log origin/main --grep='^Merge pull request #N from' -1` (the " from"
keeps #2 from matching #21), and use that commit as <sha>. Header:
`SYNC PACKET | since <sha> | main <sha> (#N) | <date>`.

- `git log --oneline <sha>..origin/main`
- docs/STATUS.md in full.
- docs/decisions.md entries added or changed since <sha>, quoted in full.
- `git diff <sha> origin/main -- DESIGN.md CLAUDE.md README.md docs/handoff.md docs/overview.md docs/workflow/ .claude/agents/reviewer.md`
- If that diff is over about 400 lines: per-file line counts plus the
  changed section headings, with the rest listed under `Omitted:`.
- Never include Go code.
