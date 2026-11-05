# Workflow: a planning chat and Claude Code sharing one repo

Source: extracted from eK8s (github.com/codeinthecreek/eK8s) at e5d52c3, 2026-10-14.

> **Snapshot note.** This file, [chat-instructions.md](chat-instructions.md)
> and [templates/](templates/) describe the method as extracted at PR #32
> (2026-10-14). The method kept changing until development ended at #81.
> Its final form is the live [CLAUDE.md](../../CLAUDE.md),
> [docs/handoff.md](../handoff.md) and
> [.claude/agents/reviewer.md](../../.claude/agents/reviewer.md) in this
> repo. Section 12 lists what changed and why; section 13 is a
> retrospective.

This is a method for one owner working with two Claude surfaces: a
planning/review chat (claude.ai) and Claude Code cloud sessions that
implement in a GitHub repo. Nothing here depends on the product being
built. The files to copy are in [templates/](templates/); the text to
paste into the chat project is [chat-instructions.md](chat-instructions.md).

## 1 Principles

- Product-independent. Everything product-specific lives in slots
  (section 10), so the method moves to a new repo by filling them.
- State lives in the repo, not in any chat's memory. Chats end, get
  compacted, and go stale; the design doc, the decision log and STATUS.md
  are what survive a gap.
- Packets are the only thing that crosses the chat/Code boundary. The
  chat cannot read the repo, and pasting transcripts is long and
  scattered; a packet is the smallest paste that lets the chat decide.
- Human approval gates live in CLAUDE.md, which Code loads every session,
  so they hold without anyone remembering to ask.

## 2 Roles and gates

- The chat plans, reviews and approves. Code implements. The owner relays
  between them by hand and holds every approval.
- The chat cannot read the repo. It sees only packets and its
  project-knowledge copies of repo files. (Keeping the roles apart means
  the chat is never asked to judge code it cannot see, and Code is never
  asked to decide what the owner must.)
- Before every commit Code shows the full diff (full contents for new
  files) and the commit message, and waits for explicit approval. Push
  needs a separate explicit approval. Anything the task prompt explicitly
  specifies counts as already approved.
- Cloud sessions run a platform Stop hook that reports uncommitted or
  unpushed work and asks Code to commit, push or re-author. Code never
  acts on it: it replies with one line ("Stop hook noted; awaiting
  approval per CLAUDE.md.") and does not restate the message. The hook
  stays on because its unpushed-work report is a useful safety net;
  disabling hooks would also remove the owner's own hooks.
- Code asks before adding dependencies, changing schemas or data formats,
  exposing network services, making security-relevant changes, doing
  anything hard to reverse, or departing from existing patterns.

## 3 State in the repo

Three files, each with one job:

- The design doc (slot DESIGN_DOC) holds settled architecture and is
  authoritative. Any other overview must defer to it. A decision that
  changes architecture needs a design-doc edit (with approval) plus a
  decision-log entry pointing at it.
- docs/decisions.md logs implementation-level decisions, oldest first.
  Heading: `## YYYY-MM-DD - PR #N - decision in one line`. Lines:
  Rejected, Reason, Reverse, `Origin:` last (always; owner, chat or Code,
  whoever first proposed the choice), optionally `Decision:` first,
  Revisit, `Note:`, `Follow-up:`. Max 5 lines per entry, Origin not
  counted. Entries are written before the PR exists, so the number starts
  as `PR #N (inferred)`.
  Format: [templates/decisions.template.md](templates/decisions.template.md).
- docs/STATUS.md is the resume point after a gap, kept current by Code in
  every PR so the chat reads it instead of reconstructing state from
  transcripts. Sections: Now (work in flight), Next (queued, in order),
  Parked (known problems and pending owner actions, with the check that
  unparks them), Recent (merged work, newest first, real PR numbers
  only). Format: [templates/STATUS.template.md](templates/STATUS.template.md).

## 4 PR lifecycle

1. Reconcile commit, first. Compare STATUS.md and decisions.md with
   `git log origin/main`: move merged items from Now to Recent with their
   real PR numbers, replace `(inferred)` numbers git now confirms. It goes
   in the same PR because entries are written before the PR number
   exists, and fixing them in the next PR avoids a housekeeping PR per
   merge. It goes in its own commit because one commit is one logical
   change, and bundling it with the task recurred until the rule was
   written.
2. Task commit(s). One logical change per commit; the message explains
   why, not what. The same PR updates STATUS.md and adds decision entries.
3. Self-review (section 6) on changes touching the code scope.
4. Packet (section 5), then approval of the commit, then a separate
   approval to push.
5. Merge (GitHub merge commit). The next PR's reconcile commit picks up
   the real number.

Large tasks are split into several prompts within one PR, with `/compact`
between them: the branch and its commits carry the state, so the
compaction summary only has to carry intent. Example: this folder was built that way:
templates first (prompt 1), then WORKFLOW.md and chat-instructions.md
(prompt 2). Each prompt ends with its own approval packet.

## 5 Handoff protocol

Code ends every task with the packet its next step needs: design packet
(a choice that blocks work), review packet (code the chat has not seen),
approval packet (docs-only or already-reviewed change, and every push),
answer packet (read-only investigation), sync packet (bring the chat's
copies up to date). Every packet is one four-backtick block so it is a
single copy on a tablet, ASCII only, headed by type, branch, HEAD, main
and date, quotes the lines that matter with `path:start-end`, marks
inference as `(inferred)`, and ends with `Omitted:`. A packet that ends
a task also carries the report items (a)-(g) from CLAUDE.md, so there is
no separate prose report (the same facts were otherwise produced twice). Owner feedback, conditions and "approved, but ..." replies come
back at the top of the next packet as `Feedback:` lines, one per item.

Full formats: [templates/handoff.template.md](templates/handoff.template.md).
This section is a summary; the template is authoritative.

## 6 Self-review

Before showing a diff for commit approval, or producing a review or
approval packet, Code runs a read-only reviewer subagent once on the
final tree, giving it the task prompt and the base SHA. The reviewer
returns a numbered findings list and never edits. Code fixes must-fix
findings and records each finding's disposition (fixed / declined /
deferred) in the packet's `Self-review:` section. Over-engineering
findings (an added guard: what concrete threat does it answer?) are
advisory, never must-fix. A review packet also lists each guard the
change adds, with its threat, under `Added complexity:`; a hypothetical
threat's guard is parked, not built, unless the owner asks or it bears
on a non-negotiable, which goes to the owner as a question while the
guard stays in the change. A risk the design doc lists as an accepted
limit is never grounds for a guard beyond the mitigation it names. Docs-only changes skip the reviewer. One run
per packet, not per edit, to keep usage down. The point: the chat cannot
read the repo, so every finding it makes costs a relay round trip, and
most are checkable in-repo.

Measuring it: each `Feedback:` line in the next packet (tagged by Code)
on a self-reviewed change ends with a tag.

- `[missed]`: checkable in the repo, the reviewer did not report it.
- `[repo-unseeable]`: needed context outside the repo.
- `[dup]`: the reviewer had already reported it.

Each `[missed]` item becomes an edit to the reviewer's checklist (slot
PROJECT_CHECKLIST grows from these). The `declined:` lines give the
reviewer's false positives. Tagging costs no extra reviewer runs, which
is why it was chosen over seeded-defect runs. Template:
[templates/reviewer.template.md](templates/reviewer.template.md), installed
as `.claude/agents/reviewer.md`.

## 7 Chat side

The rules below are the ones in [chat-instructions.md](chat-instructions.md),
with their reasons.

- Staleness check. The chat's project-knowledge copies go stale, and
  nothing flags it (in one project a chat wrote an overview from snapshots
  four PRs old). Before summarizing project state or writing docs from its
  copies, it asks for the current origin/main SHA and compares it with
  the newest PR in STATUS.md Recent. When a packet header's main is newer
  than its copies, it asks for a sync packet, or for the files to be
  re-added, before relying on them.
- Prompt drafting. Do not clump too much into one prompt. Keep requests
  for information separate from action steps, so an answer cannot be
  missed in the middle of a task. Split large tasks into several prompts
  within one PR (section 4).
- Relays. Everything the owner pastes into Code is one copyable block,
  unless more than one is genuinely needed (for example an approval
  before `/compact`, then the next prompt). Shell commands the chat gives
  the owner follow the owner's own formatting preferences; those are
  personal and not part of this method.
- Model and effort. Every Code prompt starts with
  `Model: <x> | Effort: <y> | Why: <one line>`, so the owner can switch
  before pasting and spend usage deliberately. Rule of thumb:
  - information requests and mechanical edits: smallest model (Haiku),
    low effort;
  - judgement work on a settled design: mid model (Sonnet), medium effort;
  - design packets, security-relevant changes, anything touching the
    non-negotiables: largest model (Opus), high effort.
- How effort is set in Claude Code (source:
  https://code.claude.com/docs/en/model-config, read 2026-10-14): the
  `/effort <level>` slash command (`/effort auto` clears it), the
  `--effort <level>` flag for one session, the `CLAUDE_CODE_EFFORT_LEVEL`
  environment variable, the `effortLevel` or per-model `modelSettings`
  settings, and `effort` in skill or subagent frontmatter. Levels are
  `low`, `medium`, `high`, `xhigh` and `max` on current models. The model
  is chosen with `/model`. Defaults vary by model, so an explicit setting is only
  needed to depart from the default. Re-check the page: these names move.
- Review. Packet `Feedback:` lines close the loop; an approval with
  changes counts as feedback. In a review packet, read Intent and
  Constraints touched before the diff; challenge any Added complexity
  line marked hypothetical.
- Verify or flag. On tool behaviour, flags and versions, the chat checks
  the current docs or says it is unsure, rather than answering from memory.

## 8 Environment (cloud Code sessions)

- Commit identity: set only `GIT_AUTHOR_*` in the cloud environment, not
  `GIT_COMMITTER_*`. GitHub validates a signature against the committer;
  with the committer variables gone, git falls back to the environment's
  git config, which matches the signing key, so commits show Verified
  while authorship and contribution credit stay with the owner. Code must never amend or re-author to "fix" this.
- The GitHub App is scoped per repository (owner), so a new project's repo
  must be added to the app's access before a session can push.
- On an iPad, cloud environment settings are reachable only in a browser,
  not in the app (owner, as of 2026-10).
- Chat project knowledge has no sync control. Re-adding the files is the
  only refresh found, which is why the staleness check exists.

## 9 Usage discipline

- On a Pro plan the 5-hour usage windows are hit even on a small project
  (owner), so each habit below is there to save usage.
- `/compact` at task and prompt boundaries, so the session does not carry
  finished work.
- One reviewer run per packet; docs-only changes skip it.
- Pick the model and effort per prompt (section 7), not one setting for
  everything.
- Track usage per session. Tools such as ccusage or a statusline script
  are examples only; the method does not depend on any of them.

## 10 Adopting in a new project

Prerequisites:

- A GitHub repo merged with merge commits. The `#N` rule (packet header,
  sync packet) reads "Merge pull request #N from ..."; a squash- or
  rebase-merge repo needs another rule, for example "(#N)" at the end of
  the subject. The templates mark the two places to change.
- Claude Code cloud sessions with the GitHub App on that repo.
- A chat project (claude.ai) to hold the instructions and project knowledge.

Steps:

1. Copy the templates to their targets: CLAUDE.template.md to CLAUDE.md,
   STATUS.template.md to docs/STATUS.md, decisions.template.md to
   docs/decisions.md, handoff.template.md to docs/handoff.md,
   reviewer.template.md to .claude/agents/reviewer.md. (The
   `.template.md` names are why Claude Code never auto-loads them from
   here; see https://code.claude.com/docs/en/memory.)
2. Fill every slot; delete the template comments and the EXAMPLE lines,
   except the format-rules comments in STATUS and decisions, which stay.
3. Paste the block from chat-instructions.md into the chat project's
   instructions, and add the repo files to project knowledge.
4. Set up the environment (section 8).

Slot table. Generated from
`grep -oh '{{[A-Z_]*}}' docs/workflow/templates/*.md | sort -u`: 33 slots.
Files: C = CLAUDE, D = decisions, H = handoff, R = reviewer.

| Slot | Files | What it holds | eK8s example |
|---|---|---|---|
| BUILD_TEST | C | bullets: each build/test command, prerequisites, what CI runs | `go build ./...` -- local build. `go test ./...` -- this repo's unit tests, gen-0 wrapper tests included. ... |
| CHECKS | H, R | lines of `command -- pass rule` | `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .` (passes on empty output) |
| CODE_SCOPE | C, H, R | what counts as code for self-review and the review packet | Go code |
| DESIGN_DOC | C, D, H, R | the authoritative design document | DESIGN.md |
| FORBIDDEN_COMMANDS | R | commands the reviewer must not run | `gofmt -w`, `go mod tidy`, `go generate`, `go get` |
| FULL_DIFF_MAX_LINES | H | review-packet full-diff size threshold | 600 |
| FULL_FUNCTION_FLAG | H | git flag widening hunks to whole functions | `-W` |
| FULL_FUNCTION_NOTE | H | why that flag, one sentence | Git's default function-boundary rule already gives whole Go functions with their doc comments (checked on git 2.43), so no `.gitattributes` entry is needed. |
| GENERATED_MARKER | H | header marking generated files to exclude | a `// Code generated ... DO NOT EDIT.` header |
| IDENTITY_DECISION_REF | C | pointer to the decision entry on commit identity | `; see` `docs/decisions.md` |
| LANGUAGE | C, H | the implementation language | Go |
| LOCKFILE | H | dependency lockfile excluded from the "everything else" diff | go.sum |
| MUST_FIX_EXTRA | R | extra must-fix category | a way for a mutant to skip work |
| NON_NEGOTIABLES | C | numbered rules with enforcement and design-doc pointer | 1. **`internal/hashcore/` is never touched by mutation.** Its implementation is structurally excluded from the mutation driver's accessible tree ... |
| OVERVIEW_DOC | C | doc whose status section tracks what is built | docs/overview.md |
| OVERVIEW_DOCS | C | docs rechecked when the design changes | docs/overview.md and README.md |
| OVERVIEW_TRIGGER_PATHS | C | non-design paths that also trigger that recheck | the generation-0 wrapper (internal/wrapper/) |
| PROBABILISTIC_TEST_EXAMPLE | R | optional past example appended to checklist item 10 | (In PR #25, two negative tests passed at -count=1 but failed about 1 run in 20: the seeded defect they rely on only shows when requests happen to interleave ... |
| PROJECT_CHECKLIST | R | checklist items 12 on, one per past `[missed]` | 5. For gate, classifier, loadgen, wrapper or fitness code: ways a mutant could game the check, i.e. pass it while doing less work or skipping Argon2 (caching, replaying, ... |
| PROTECTED_LITERAL_LOCATION | H | where the protected literal lives | the literals live in internal/hashcore/hashcore.go `paramsSegment` and `params()` |
| PROTECTED_LITERAL_NAME | H | name of the protected literal | Argon2 parameter literal |
| PROTECTED_LITERAL_REGEX | H | grep pattern matching it in source diffs | `^[-+].*(19456\|paramsSegment\|Memory:\|Iterations:\|Parallelism:)` |
| PROTECTED_PATHS | H | paths needing a flag when touched | `internal/hashcore internal/classifier internal/wrapper/hasher.go` |
| REPEAT_DEFAULT | H | default repeat count for probabilistic tests | `-count=20` |
| REPEAT_FLAG | H | the repeat flag's name | `-count` |
| REPEAT_RUN_COMMAND | R | the repeat-run command | `go test -count=20 -run <name>` |
| REPO_LAYOUT | C | code block: directory to role, with "never touched by" rules | `cmd/service/       entrypoint: thin main.go (wires hashcore + wrapper, HTTP routes, ...` |
| SOURCE_GLOB | H | pathspec of source files | `*.go` |
| SYNC_DIFF_MAX_LINES | H | sync-packet diff size threshold | 400 |
| SYNC_DOC_PATHS | H | docs diffed in a sync packet (reviewer.md is always added) | `DESIGN.md CLAUDE.md README.md docs/handoff.md docs/overview.md docs/workflow/` |
| TEST_DIFF_CONTEXT | H | context flag for test-file diffs | `-U10` |
| TEST_GLOB | H | pathspec of test files | `*_test.go` |
| TEST_VARIANT_FLAGS | R | test flags the reviewer may add | `-run`, `-race` or `-count=N` |

## 11 Known limits

- Packets are relayed by hand. The owner is the transport, so every
  handoff costs a copy and a paste, and a missed paste loses state.
- The chat's copies go stale. The staleness check detects drift at the
  first packet; it does not prevent it.
- Whether the reviewer is worth its cost rests on the `[missed]` counts,
  which are still being gathered. If after a few code PRs `[missed]`
  stays high, or its findings are mostly `declined:`, drop it; the
  method works without it (delete the self-review bullet and the
  `Self-review:` section).

## 12 How the method changed after #32

The sections above are the method as extracted at PR #32. Changes after
that are in the live CLAUDE.md, docs/handoff.md and
.claude/agents/reviewer.md; the templates were not updated. Process
changes only; product decisions are not listed. Header lines are
docs/decisions.md line numbers.

| PR | Change | Reason (source) |
|----|--------|-----------------|
| #42 | The start-of-PR reconcile greps decisions.md instead of reading it; pre-#32 entries archived; STATUS Recent keeps 5; overview.md and README.md refresh only on request. | Usage windows are hit regularly; decisions.md (~103 KB) and overview.md (~32 KB) were the largest fixed reads per PR (decisions.md:286, reason at :289). |
| #45 | The reviewer runs once per task on the final tree, not before each commit diff. | One subagent run per PR instead of one per step (decisions.md:453, reason at :456). |
| #51 | Autonomous working mode: Code works along STATUS Next and asks the owner only on direction questions; packets and approvals only on request. | Owner time and quota (decisions.md:565, reason at :568). |
| #52 | Third-party dependencies join the escalation list; Code stops when the next PR needs another model tier. | Supply-chain exposure in a run that executes model-written code; top-tier effort where the non-negotiables bite (decisions.md:579, :586). |
| #53 | Every stop ends with a resume prompt. Removed in #67. | A fresh session resumes with no chat relay (decisions.md:593, reason at :596). Removal: commit 217c877, prompts for fresh sessions come from the planning chat and the rule clashed with the Reply rules. |
| #67 | Code runs a PR end to end (commit, self-review, push, draft PR, review packet) with no per-commit approval; Reply rules added. | Per-commit round trips and incomplete packets cost most of a week's quota by Thursday, 2026-10-29 (decisions.md:733, reason at :736). |
| #68 | Prompt guard: header line, END OF PROMPT, a prompt log on the host. | A wrong tier, a cut paste or a re-sent prompt costs a session's quota before anyone notices (decisions.md:740, reason at :743). |
| #71 | The reviewer runs on every change that is not docs-only (Markdown only). | "Any change touching Go code" left shell, YAML and workflow changes undefined (decisions.md:903, rejected at :905, reason at :906: in the design review the owner asked that docs-only mean only Markdown files, so that shell, YAML and workflow changes are reviewed too). |
| #73 | Diffs in a packet are pasted verbatim and labelled with their command. | No decisions.md entry. Commit b020684: in review the owner pointed out that a packet's diffs had retyped context lines, and asked for verbatim diffs labelled with their command. |
| #75 | Draft PRs skip ci.yml and deploy.yml; one run per PR; no push-to-main run. | 342 CI minutes in the first 4 days of November, about 2,500 a month projected (decisions.md:980, reason at :983). |
| #76 | The deploy smoke runs by hand only. | deploy.yml was 202 of those 342 minutes and ran on almost every code PR; the owner wants CI kept small (decisions.md:987, reason at :990). |

## 13 Retrospective

Evidence only. Each claim cites a file and line, a PR or a decisions.md
entry. A sentence that goes past the evidence starts with "Inference:".

**What held.**
- The decision log was kept to the end: 150 entries, the last for PR #85
  (docs/decisions.md:1067); the count includes the documentation PRs'
  entries.
- STATUS.md stayed the resume point: it opens "Development ended at #81"
  (docs/STATUS.md:3) and each PR reconciled it first (commit 8ef20cb, the
  reconcile in #83 that moved #82 to Recent).
- The commit trailer rule (#35, #37) held: 304 of the 355 commits after
  1204850 carry a `Claude-Session:` line (`git log 1204850..origin/main`).
  Of the 51 without one, 49 are merge commits (`git log --merges
  --oneline 1204850..origin/main`) and 2 are owner commits (172753e,
  9b79e26).
- The reviewer ran on non-docs PRs to the last code PR: #77's body lists
  16 findings, 9 of them fixed.
- The Model/Effort header lived on as the prompt guard (CLAUDE.md:49-56,
  decisions.md:740).

**What cost more than it gave.**
- Reading the log: decisions.md (~103 KB) and overview.md (~32 KB) were the
  largest fixed reads per PR (decisions.md:289, #42). decisions.md then
  grew to 1058 lines plus a 623-line archive.
- Per-commit approval: "per-commit round trips and incomplete packets cost
  most of a week's quota by Thursday (2026-10-29)" (decisions.md:736, #67).
- Wrong tier, cut paste, re-sent prompt: "costs a session's quota before
  anyone notices" (decisions.md:743, #68).
- CI: 342 minutes in 4 days, 202 of them deploy.yml in 17 runs
  (decisions.md:983, :990; #75, #76).
- The repo records no token, time or run counts for the reviewer.

**The Feedback tags.** Section 6 (WORKFLOW.md:138-149) and section 11
(WORKFLOW.md:294-298) say the reviewer's worth would be judged from the
`[missed]`, `[repo-unseeable]` and `[dup]` counts. There are 0 tagged
Feedback lines in the repo: the tags appear only in the #17 body, which
introduces them. Packets went to the planning chat (docs/handoff.md:6-9)
and were never stored. Inference: the reviewer's value was therefore never
measured as planned.

**The reviewer's record.** From PR bodies only; the packets are not
available. 21 PRs state dispositions (#17, #53-#61, #66, #68-#77).
- Must-fix findings: #53 (gate/), #58 and #59 (orchestrator/), #68
  (orchestrator/, fitness/, lineage/) and #70 (round 1: 1 must-fix, 12
  should-fix; deploy/diag shell script).
- No must-fix: #17, #54, #60, #61, #66, #69.
- Counts stated in the bodies: #70 about 27 fixed, 6 declined over two
  rounds; #71 about 20 fixed, 2 declined; #77 9 fixed, 3 declined; #55 7
  fixed, 2 declined.
- Docs-only PRs skipped it; at least nine bodies say so (#19, #22, #51,
  #52, #63, #67, #78, #80, #83).
- Checklist growth: reviewer.md changed three times for named misses (commits
  dfaf367, d7cba29, 8827c5d, all before #32). Commit dfaf367 names "three
  checks PR #18's review caught and self-review missed". No later commit
  cites a tag.

**Three simplifications proposed in the planning chat's scope review
(2026-11-04), never adopted because development ended first.** Each is a
recommendation for a next project.
1. *Run the reviewer only on the risky packages (gate/, orchestrator/,
   driver/, internal/hashcore, internal/classifier).* For: the must-fix
   findings on #53, #58, #59 and #68 fall in gate/ and orchestrator/. Against:
   #70 (a deploy shell script) had a must-fix, and #55 (lineage/) and #56
   (fitness/, a scoring flaw where shedding nearly everything kept full
   latency and CPU marks) had substantial fixes, all outside that list; #71
   widened the reviewer to shell, YAML and workflow files for a similar
   reason (decisions.md:903-906). Inference: a path list would have missed
   the shell, lineage and fitness findings.
2. *Write decisions.md entries only for design or non-negotiable changes.*
   Evidence: 150 entries (documentation PRs' included), 1072 lines plus a 623-line archive; the log was
   too big to read per PR (decisions.md:289). Against: 29 lines in it say
   an entry amends or supersedes an earlier one (`grep -c -i
   'amends\|supersede'`), e.g. decisions.md:31, :37. Inference: the log
   paid for itself in cross-references, but the implementation-level
   entries are probably the bulk of its size.
3. *Drop the tags and the checklist loop they fed.* Evidence: 0 tagged
   lines (above); the checklist did grow, but from named misses recorded
   in commit messages rather than tags (dfaf367 and later). Inference:
   without a stored packet the tag could not be counted; the checklist loop
   needs only a place to record each miss.

**Who made the choices.** `grep -c '^- Origin: <x>' docs/decisions.md`:
owner 33, chat 58, Code 45, 136 in all, the documentation PRs'
entries included. Origin starts at PR #34; the 14
earlier entries (lines 8-85) have none. Four entries list two origins and
are counted by the first. Origin records who first proposed a choice, not
how much it mattered. Every entry was approved by the owner (owner's
statement, not recorded per entry).
