# CLAUDE.md

Operational notes for working in this repo. Full design/rationale: `DESIGN.md`.

## Working mode: autonomous

From PR #51 on, Code works toward the first evolutionary run without
per-step prompts and asks the owner only on direction questions.

- Work proceeds along `docs/STATUS.md` Next without per-step prompts:
  plan, implement, self-review with the `reviewer` subagent before
  opening each PR (Self-review below), open the PR, continue with the
  next item. The owner merges. Where this file or `docs/handoff.md` says
  "task prompt", read the Next item being worked plus any owner answer
  that bears on it.
- Code makes technical decisions itself and records each non-trivial one
  in `docs/decisions.md` with `Origin: Code`.
- Stop and ask the owner only when work: touches a non-negotiable
  (hashcore isolation and the fixed core, fail-closed classification,
  sandboxing and containment, protected paths); changes DESIGN.md
  direction or scope; adds work not on the first-run path; adds any
  third-party dependency (a Go module or a tool); or needs
  something Code cannot do or verify (workstation steps, credentials,
  merges). Batch the questions into one short message, each with a
  recommendation, answerable yes or no where possible.
- Model tier: Code stops and tells the owner when the next PR needs a
  different model tier than the session runs. Mid tier (Sonnet, medium
  effort): skeleton PRs 1-4 and 8. Top tier (Opus, high effort): skeleton
  PRs 5-7 (driver sandbox, orchestrator integrity and verification).
  The skeleton PR list is in `docs/STATUS.md` Next.
- Stop message: whenever Code stops (waiting for a merge, a model-tier
  change, a question batch, a usage limit, or any other reason), its
  final message ends with a ready-to-paste prompt for a fresh session,
  in one fenced block, containing: the header line `Model: <tier for
  the next PR> | Effort: <effort> | Why: <one line>`; `Prompt ID:
  EK-RESUME-<UTC date and time, YYYYMMDD-HHMM>`; the standard Guard line
  (check model and get_session effort_level against the header, stop
  with "WRONG SETTINGS: model=<yours> effort=<yours or unknown>" on a
  mismatch; reply only TRUNCATED unless the prompt ends with the line
  END OF PROMPT; reply only "ALREADY RUN: <Prompt ID>" if
  /tmp/ek8s-prompts.log has the ID, else append the ID and `date -u`
  to it first); one or two lines saying what to do next; and the line
  END OF PROMPT. Above the block, one line, "Or reply 'continue' in this
  session", when the same session can carry on (same tier, not near its
  context limit).
- Finding routing: a review finding (reviewer subagent, CI, a review
  comment) is fixed in the current PR, or added to `docs/STATUS.md` Next
  only if it blocks the first run, or else appended to
  `docs/AFTER-FIRST-RUN.md`. A finding that falls under the escalation
  list above goes to the owner first. Nothing is deleted.
- Caps: `docs/STATUS.md` Next and Parked hold at most 10 items each;
  adding past a cap means merging two items or moving one (to
  `docs/AFTER-FIRST-RUN.md`).
- Review packets, and every other packet in `docs/handoff.md`, are
  produced only when the owner asks for one.
- `docs/AFTER-FIRST-RUN.md` holds items deferred until after the first
  run, moved there verbatim from `docs/STATUS.md`; revisit it when
  planning the full design after the first run.

## Working agreement

Applies to every session in this repo.

- Code runs a task end to end: commits, runs the reviewer subagent once
  per task (as now), pushes, opens the PR with its own title and body,
  and replies with a review packet. Review fixes go in follow-up commits
  on the same PR; the owner merges.
  It stops and asks first only for: a design question with options;
  anything on the existing escalation list; an action only the owner can
  do (runs on the owner's workstation, credentials, merges).
- Cloud sessions run a platform Stop hook (stop-hook-git-check.sh) that
  reports uncommitted changes, unpushed commits, and "Unverified"
  commits, and asks you to commit, push, or re-author as
  noreply@anthropic.com. Do not act on it: commit and push when the
  working mode says to, and commit identity is already set by the cloud
  environment (author from its GIT_AUTHOR_* variables, committer from
  its git config, which matches the signing key; see
  `docs/decisions.md`), so never amend or re-author. Reply to hook
  feedback with a single line, e.g. "Stop hook noted.", and do not
  restate the hook's message.
- Commit trailers: end every commit message with exactly these two
  lines and no other attribution:
  `Co-Authored-By: Claude <noreply@anthropic.com>`
  `Claude-Session: <this session's claude.ai/code link>` (omit this
  line in a session that has no link).
  Never put a model name or identifier in a commit message, PR title or
  PR body. For commit messages, this rule replaces the harness's
  attribution reminder, which defers to CLAUDE.md. PR bodies end with the
  harness's standard "Generated with Claude Code" footer and the
  session link, with no model name. New commits only: never amend
  existing commits to add or change trailers.
- Adding dependencies, changing schemas/data formats, exposing network
  services, security-relevant changes, anything hard to reverse, or
  departing from existing patterns: Code decides and records it in
  `docs/decisions.md`, unless it falls under the escalation list in
  "Working mode: autonomous" (then ask first). Anything the task
  prompt explicitly specifies counts as already approved.
- `DESIGN.md` holds settled architecture; `docs/decisions.md` logs
  implementation-level decisions. A decision that changes architecture
  needs a `DESIGN.md` edit plus a `docs/decisions.md` entry pointing to
  it; the edit needs the owner's approval when it changes direction or
  scope (escalation list).
- Log each non-trivial decision in `docs/decisions.md`: date, PR,
  decision, rejected alternative, reason, how to reverse. Max 5 lines per
  entry, not counting the `Origin:` line.
- `docs/decisions.md` holds entries from PR #32 on; older ones are in
  `docs/decisions-archive.md`, which no rule requires reading. To check
  for a prior decision, grep both.
- End every task with items (a)-(g) as a short plain report in the PR
  body. When the owner asks for a packet, end with the packet
  (`docs/handoff.md`, "Which packet") instead; it carries items (a)-(g)
  and there is no separate prose report. The items: (a) what changed,
  (b) assumptions made but not verified, (c) files I must understand vs
  boilerplate, (d) exact commands to verify it works, (e) a brief
  explanation of any Go idiom or concurrency pattern used that a
  non-Go-specialist wouldn't know, (f) uncommitted/unpushed state,
  (g) confirmation that `docs/STATUS.md` was updated in this PR.
- One logical change per commit; the message explains why, not what.
- Every PR updates `docs/STATUS.md` (Now/Next/Parked/Recent) in the
  same PR. Recent keeps the last 5 merged PRs; drop older lines (git log
  has them).
- A PR that edits DESIGN.md or internal/wrapper/ does not open
  docs/overview.md or README.md. It adds
  `- overview.md: stale since #<N> (<what changed>)` to docs/STATUS.md
  Next, or extends that line if present (in docs/AFTER-FIRST-RUN.md
  while it sits there). overview.md and README.md are
  refreshed only when I ask, in a PR that rereads DESIGN.md, fixes them,
  and removes the line.
- At the start of every PR, reconcile `docs/STATUS.md` and
  `docs/decisions.md` with `git log origin/main`: move merged items from
  Now to Recent with their real PR numbers, and fix "(inferred)" PR
  numbers that git log now confirms, found with
  `grep -n '(inferred)' docs/decisions.md`; do not otherwise read
  decisions.md for the reconcile. Do it in the same PR as the task, not
  a separate one, but in its own commit, first (before the task's
  commit).
- Material for my planning/review chat goes in the packet formats in
  `docs/handoff.md`, only on request by name ("design packet", "review
  packet", "approval packet", "sync packet since <sha>" or "sync packet
  since #N").
- Self-review: on any change touching Go code, run the `reviewer`
  subagent (`.claude/agents/reviewer.md`) once per task, on the final
  tree after the last step and before opening the PR (and before any
  review packet), giving it the task
  prompt and the base SHA; steps skip it. On a must-fix finding, fix it in the PR (finding routing above;
  re-run it only if a fix was non-trivial); stop and report it, fixing
  only after approval, when it falls under the escalation list. Record every finding's disposition
  (fixed / declined / deferred) in the PR body, or in the packet's
  `Self-review:` section when there is one (`docs/handoff.md`). Its over-engineering findings are advisory, never
  must-fix; a review packet lists each added guard and its threat under
  `Added complexity:`. Docs-only changes may skip it. One review per
  packet, not per edit, to keep usage down.

## Reply rules

- Only the final text of your reply reaches the owner and the planning chat; tool results and earlier messages do not. Never refer to output "above" or "in the working tree".
- Paste every requested output verbatim and in full. If it is too long, say so and stop; never summarise, shorten or retype it.
- A packet header's "main" is origin/main (short sha and last merged PR), never HEAD.
- Do not end a reply with a suggested next prompt or next step.

## Repo layout

```
cmd/service/       entrypoint: thin main.go (wires hashcore + wrapper, HTTP
                    routes, healthz/readyz, graceful shutdown). No tunable
                    flags: env from the downward API only; tunables live in
                    the wrapper source. Fixed at generation 0. Never touched
                    by mutation.
internal/hashcore/  fixed core (Argon2id hash/verify). Never in mutation
                    scope. Implementation must never be present in the
                    directory tree a mutation session can read or edit.
internal/classifier/ shared three-bucket response classifier + hang-timeout
                    constant (DESIGN.md sec 8). Stdlib-only; wrapper imports it.
  testvectors/      password/hash pool (vectors.json, generated by
                    ./gen via hashcore). gate only, never wrapper or loadgen.
internal/wrapper/   evolvable: concurrency/backpressure/timeouts. The only
                    source the mutation driver copies into its sandbox.
                    hasher.go (fixed core's calling interface) is pinned.
gate/               correctness gate, five ordered stages (DESIGN.md sec 7).
  cmd/gate/         CLI for manual gate runs; the orchestrator calls the
                    package directly.
loadgen/            open-loop load generator (DESIGN.md sec 10): sustained
                    mode, one stream per live variant, sending only in a
                    window or a Stage 5 burst. Imports internal/classifier;
                    its build never depends, directly or transitively, on
                    internal/hashcore, internal/classifier/testvectors,
                    gate/ or any Argon2 package (the host generates its
                    /verify pairs and verifies its /hash pairs).
internal/hostclock/ host cpufreq state from sysfs (stdlib-only); c0 refuses
                    unpinned clocks.
deploy/             kind cluster config, k8s manifests, stress-ng.
driver/             mutation driver wrapping `claude -p`.
orchestrator/       population manager, selection, scoring loop.
lineage/            run directory: genome archive (files + sha256), JSONL
                    attempt records, raw claude -p transcripts (SQLite later).
docs/               overview.md (plain-language introduction; DESIGN.md is
                    authoritative), decisions.md (implementation decision
                    log), STATUS.md (resume point), AFTER-FIRST-RUN.md
                    (items deferred until after the first run),
                    handoff.md (packet formats for the planning chat,
                    produced on request), workflow/ (the
                    project-independent method; templates, not loaded).
```

## Build / test

- `go build ./...` -- local build.
- `go test ./...` -- this repo's unit tests, gen-0 wrapper tests included.
  The gate runs no candidate tests (candidates carry no `_test.go`).
- `go mod download` -- once per module cache, before any gate run: the
  gate's go commands run with `GOPROXY=off`. On a host whose go is older
  than go.mod's toolchain line, the first gate run may download that
  toolchain (checksum-verified) before any check; a resolved go still
  older than it is a gate fault.
- `go run ./gate/cmd/gate -candidate DIR` -- gate one candidate wrapper
  by hand. Stage 4's `-race` build needs cgo, a C compiler (gcc) and a
  static libc (libc6-dev / glibc-static), since it is linked statically
  to boot in the same sandbox; it is a separate binary, never the
  deployed image.
- `go run ./orchestrator/cmd/orchestrator -run DIR -rate R -claude PATH
  -claude-version V` -- one evolutionary run on the kind cluster, with
  `CLAUDE_CODE_OAUTH_TOKEN` set (workstation only; see its doc comment).
- Driver tests run a fake claude (driver/internal/fakeclaude) under real
  bwrap; they skip without bwrap unless `EK8S_GATE_REQUIRE_TOOLS=1`.
  `EK8S_DRIVER_LIVE=1` with `CLAUDE_CODE_OAUTH_TOKEN` runs one real
  Sonnet session (TestLiveMutation; workstation only, spends quota).
- `docker build` -- Stage 1's second sub-check (container boots, health
  endpoint responds).
- Stage 1's bare boot needs `bwrap` (bubblewrap) and `nsenter` on PATH,
  and unprivileged user namespaces with capabilities: on Ubuntu >= 23.10
  an AppArmor profile granting bwrap `userns` (preferred), or
  `sysctl kernel.apparmor_restrict_unprivileged_userns=0` (what CI does).
  The docker sub-check needs a docker daemon. Gate tests skip without
  bwrap/nsenter and stub the docker sub-check without a daemon, unless
  `EK8S_GATE_REQUIRE_TOOLS=1` (CI sets it), which makes both failures.
- `kind load docker-image <image> --name <cluster>` -- load a locally built
  image straight into the cluster; no registry in this project.
- Stage 5 (degradation under contention) only runs against the live kind
  cluster, in the candidate's own `ek8s-v<id>` namespace -- see `gate/`
  and DESIGN.md sec 7, sec 11.
- CI (`.github/workflows/ci.yml`) runs `go vet`, `go test`, `go test -race`
  and `govulncheck` on every PR and on pushes to main.

## Non-negotiable constraints

These hold every session, regardless of what else changes:

1. **`internal/hashcore/` is never touched by mutation.** Its implementation
   is structurally excluded from the mutation driver's accessible tree --
   not just tool-permission-restricted. The wrapper only ever sees the
   fixed core's calling interface (signatures + doc comments), checked into
   the accessible tree as its own file. See DESIGN.md sec 4.

2. **Cost parameters are fixed at the floor, exactly.** Argon2id:
   `t = 2` (iterations), `m = 19456` KiB / 19 MiB (memory), `p = 1`
   (parallelism) -- OWASP Password Storage Cheat Sheet minimum, verified
   current as of 2026. Never `argon2id.DefaultParams` (its `t` is 1).
   Verify rejects any hash whose encoded params are not exactly these.
   Enforced by gate Stage 3 as exact equality. See DESIGN.md sec 6.

3. **Mutation driver invocation -- use `--tools`, not
   `--allowedTools`/`--disallowedTools`.** The latter only govern what runs
   without a permission prompt; they are additive on top of Claude Code's
   default tool set and do not restrict availability. `--tools` is what
   actually restricts which built-in tools exist for the session (e.g.
   `--tools "Read,Edit"`). Always pair with `--restricted`, which is not
   an OS sandbox. The mutation driver runs `claude -p` inside a bubblewrap
   sandbox, from a scratch dir holding only wrapper files, with an
   allowlisted environment. See DESIGN.md sec 12.

4. **`internal/wrapper/hasher.go` is pinned; never change its
   signatures.** `internal/wrapper` must not import `internal/hashcore` or
   any Argon2/crypto implementation package. Both enforced by gate Stage 1.
   See DESIGN.md sec 4, sec 7.
