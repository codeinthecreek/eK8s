# Handoff packets: what Code sessions hand back to the planning/review chat

The planning/review chat cannot read the repo; it sees only what the owner
pastes from a Code session. A packet is the smallest paste that lets the
chat make a design decision, review code, or let the owner approve a
commit or push. Code produces a packet only when the owner asks for one
by name: "design packet", "review packet" or "approval packet".

## Rules for every packet

- Output exactly one code block fenced with FOUR backticks, and nothing
  outside it, so it is a single copy on an iPad.
- ASCII only. Transliterate any non-ASCII in quoted repo text (e.g. a
  middle dot to `-`) and say so in one line of the packet.
- First line, one header line: packet type, branch, HEAD short SHA, date,
  e.g. `REVIEW PACKET | claude/foo-ab12cd | HEAD a0bceaa | 2026-10-01`.
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
- When the owner has pasted chat feedback since the last packet, the next
  packet opens (after the header) with a `Feedback:` section: one line per
  item, `<item> -- done | declined: <reason> | question: <question>`, so
  the loop closes visibly.

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

- Task: the PR title, or the first line of the prompt the session
  received, verbatim; then 2-3 lines of intent.
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
