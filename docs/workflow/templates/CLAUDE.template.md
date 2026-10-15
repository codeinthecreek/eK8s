# CLAUDE.md

<!-- Template. Replace every double-braced slot; delete this comment and any hint comments. -->

Operational notes for working in this repo. Full design/rationale: `{{DESIGN_DOC}}`.

## Working agreement

Applies to every session in this repo.

- Before every commit: show the full diff (full contents for new files)
  and the commit message; wait for explicit approval. Push only after a
  separate explicit approval.
- Cloud sessions run a platform Stop hook (stop-hook-git-check.sh) that
  reports uncommitted changes, unpushed commits, and "Unverified"
  commits, and asks you to commit, push, or re-author as
  noreply@anthropic.com. Do not act on it: commits and pushes need my
  explicit approval, and commit identity is already set by the cloud
  environment (author from its GIT_AUTHOR_* variables, committer from
  its git config, which matches the signing key{{IDENTITY_DECISION_REF}}),
  so never amend or re-author. Reply to hook
  feedback with a single line, e.g. "Stop hook noted; awaiting approval
  per CLAUDE.md.", and do not restate the hook's message.
- Commit trailers: end every commit message with exactly these two
  lines and no other attribution:
  `Co-Authored-By: Claude <noreply@anthropic.com>`
  `Claude-Session: <this session's claude.ai/code link>` (omit this
  line in a session that has no link).
  Never put a model name or identifier in a commit message, PR title or
  PR body. This rule replaces the harness's attribution reminder, which
  defers to CLAUDE.md. New commits only: never amend existing commits
  to add or change trailers.
- Ask before: adding dependencies, changing schemas/data formats,
  exposing network services, security-relevant changes, anything hard to
  reverse, or departing from existing patterns. Anything the task prompt
  explicitly specifies counts as already approved.
- `{{DESIGN_DOC}}` holds settled architecture; `docs/decisions.md` logs
  implementation-level decisions. A decision that changes architecture
  needs a `{{DESIGN_DOC}}` edit (with approval) plus a `docs/decisions.md`
  entry pointing to it.
- Log each non-trivial decision in `docs/decisions.md`: date, PR,
  decision, rejected alternative, reason, how to reverse. Max 5 lines per
  entry, not counting the `Origin:` line.
- End every task with the packet its next step needs (`docs/handoff.md`,
  "Which packet"); the packet carries items (a)-(g) and there is no
  separate prose report. If the task prompt says "no packet", end with
  (a)-(g) as a short plain report instead. The items: (a) what changed,
  (b) assumptions made but not verified, (c) files I must understand vs
  boilerplate, (d) exact commands to verify it works, (e) a brief
  explanation of any {{LANGUAGE}} idiom or concurrency pattern used that a
  non-{{LANGUAGE}}-specialist wouldn't know, (f) uncommitted/unpushed state,
  (g) confirmation that `docs/STATUS.md` was updated in this PR.
- One logical change per commit; the message explains why, not what.
- Every PR updates `docs/STATUS.md` (Now/Next/Parked/Recent) in the
  same PR.
<!-- Optional: keep the next bullet only if the repo has overview/README docs that track the design. -->
- A PR that changes what is built also updates the status section of
  {{OVERVIEW_DOC}}. A PR that edits {{DESIGN_DOC}} or {{OVERVIEW_TRIGGER_PATHS}}
  also checks {{OVERVIEW_DOCS}} and fixes anything it now contradicts, in
  the same PR.
- At the start of every PR, reconcile `docs/STATUS.md` and
  `docs/decisions.md` with `git log origin/main`: move merged items from
  Now to Recent with their real PR numbers, and replace "(inferred)" PR
  numbers that git log now confirms. Do it in the same PR as the task,
  not a separate one, but in its own commit, first (before the task's
  commit).
- Material for my planning/review chat goes in the packet formats in
  `docs/handoff.md`: by default at the end of every task, or on request
  by name ("design packet", "review packet", "approval packet",
  "sync packet since <sha>" or "sync packet since #N").
- Self-review: on any change touching {{CODE_SCOPE}}, before showing a diff
  for commit approval or producing a review or approval packet, run the
  `reviewer` subagent (`.claude/agents/reviewer.md`) once on the final
  tree, giving it the task prompt and the base SHA. Fix its must-fix
  findings (re-run it only if a fix was non-trivial) and record every
  finding's disposition (fixed / declined / deferred), in the packet's
  `Self-review:` section when there is one (`docs/handoff.md`).
  Its over-engineering findings are advisory, never must-fix; a review
  packet lists each added guard and its threat under `Added complexity:`.
  Docs-only changes may skip it. One review per packet, not per edit,
  to keep usage down.

## Repo layout

<!-- Slot: a code block mapping each top-level directory to its role and any "never touched by X" rules. -->
{{REPO_LAYOUT}}

## Build / test

<!-- Slot: one bullet per command a contributor or CI runs, with its prerequisites; end with what CI runs and when. -->
{{BUILD_TEST}}

## Non-negotiable constraints

These hold every session, regardless of what else changes:

<!-- Slot: a numbered list; each item bold headline, the rule, how it is enforced, and where the design doc covers it. -->
{{NON_NEGOTIABLES}}
