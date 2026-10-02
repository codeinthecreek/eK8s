# Decision log: implementation-level decisions, oldest first (architecture lives in DESIGN.md)

## 2026-09-16 · PR #2 · `go mod init` at scaffold time (module github.com/codeinthecreek/eK8s)
- Rejected: waiting until real code lands.
- Reason: CLAUDE.md documents `go build ./...` / `go test ./...` as standard checks; stubs should build from day one.
- Reverse: delete go.mod.

## 2026-09-16 · PR #2 · cmd/service stub `main()` panics "not yet implemented"
- Rejected: comment-only doc.go (package main can't build without `main`), or a silent no-op `main`.
- Reason: `go build ./...` must pass, and a running stub must be unmistakably unfinished.
- Reverse: replaced by the real `main` in the generation-0 PR.

## 2026-09-17 · PR #3 · Shared classifier + hang-timeout ceiling in its own package, internal/classifier
- Rejected: defining them in internal/wrapper (evolvable, so untrustworthy as a stable threshold) or internal/hashcore (unrelated).
- Reason: gate Stage 5, the fitness function and wrapper timeouts must share one success/graceful_shed/failed definition and one ceiling (DESIGN.md sec 5, 8), so they can never drift apart.
- Reverse: architectural -- DESIGN.md sec 5, 8 edit; move the classifier into gate/ and pass the ceiling to the wrapper another way.

## 2026-09-27 · PR #4 · Argon2 params: exact equality (t=2, m=19456, p=1), not a floor
- Rejected: floor with "a mutation may increase cost".
- Reason: params come from the hash string, so a floor still allows memory exhaustion via a huge m; exact match blocks both downgrade and exhaustion.
- Reverse: relax hashcore Verify and gate Stage 3 to `>=` checks plus an upper bound on m (DESIGN.md sec 6, sec 7).

## 2026-09-27 · PR #4 · Fitness load: dedicated fixed-rate stream per variant
- Rejected: pooled Service plus a served-share fitness term.
- Reason: share is population-relative, violating fixed absolute normalization (DESIGN.md sec 9); dedicated streams also make readiness dodging self-punishing.
- Reverse: architectural -- DESIGN.md sec 3, 9, 10, 11 edit, with a pooled Service and a fitness term that is not population-relative.

## 2026-09-27 · PR #4 · Variant topology: own namespace ek8s-v<id>, live when added to loadgen targets
- Rejected: relabel in place into a shared Service.
- Reason: Services only select pods in their own namespace, and pod-template relabels trigger a rollout (losing the warm pod).
- Reverse: architectural -- DESIGN.md sec 7, 11 edit; would need a same-namespace design and accept a redeploy on promotion.

## 2026-09-27 · PR #4 · Mutation driver: scratch dir + bubblewrap sandbox, empty allowlisted env, dedicated setup-token
- Rejected: `--add-dir` from the repo root.
- Reason: `--restricted` confines file tools to the working directories including cwd, and is not an OS sandbox.
- Reverse: DESIGN.md sec 12 edit; drop bwrap and run from the repo with `--add-dir internal/wrapper` (weakens sec 4 layers 2-3).

## 2026-09-27 · PR #4 · Wrapper isolation backstop: Stage 1 import allowlist + pinned hasher.go
- Rejected: relying on Stage 2 interop.
- Reason: a faithful reimplementation via golang.org/x/crypto/argon2 would pass interop.
- Reverse: remove the Stage 1 pre-build checks (DESIGN.md sec 4, sec 7) -- not recommended.

## 2026-09-27 · PR #4 · go directive 1.25.0
- Rejected: staying on go 1.24.7.
- Reason: Go >= 1.25 sets GOMAXPROCS from the cgroup CPU limit (container-aware), which cmd/service relies on.
- Reverse: lower the directive and have cmd/service set GOMAXPROCS from the downward-API CPU limit.

## 2026-09-27 · PR #4 · Sandboxed `claude -p` invocation has no `--add-dir`
- Rejected: `--add-dir $RUN/work` (or any wrapper path).
- Reason: cwd is already `$RUN/work`, which holds only the wrapper files; a second directory would only widen scope.
- Reverse: add `--add-dir <dir>` to the argv in DESIGN.md sec 12.

## 2026-09-27 · PR #4 · .gitignore: lineage data + driver run dirs `driver/runs/`, `runs/`
- Rejected: ignoring all of lineage/ (would drop lineage/README.md).
- Reason: data files (SQLite incl. WAL/SHM `*.db-*`, JSONL) must never be committed; the run-dir names are a guess, since `$RUN` is meant to live outside the repo.
- Reverse: edit .gitignore once the driver fixes its real run-dir location.

## 2026-09-27 · PR #6 · .gitignore: drop `driver/runs/`, `runs/`; lineage data ignored at any depth under lineage/ only
- Rejected: keeping the run-dir entries as a safety net; repo-wide `*.jsonl`; top-level-only db/sqlite patterns.
- Reason: `$RUN` lives outside the repo (DESIGN.md sec 12) and unanchored `runs/` hides any `runs/` dir anywhere; recursive `lineage/**/` variants keep data files out at any depth without hiding JSONL elsewhere in the repo.
- Reverse: re-add the run-dir entries; drop the `lineage/**/` db/sqlite variants.

## 2026-09-27 · PR #6 · HTTP contract fixed in DESIGN.md sec 8, not left to the wrapper
- Rejected: letting the gen-0 wrapper's behavior define the contract implicitly.
- Reason: the wrapper is evolvable; gate, loadgen and classifier need a stable contract, and a mutant that breaks it must fail Stage 2.
- Reverse: architectural -- DESIGN.md sec 8 edit, plus matching gate/loadgen changes.

## 2026-09-27 · PR #6 · Gate Stage 2 gains HTTP error-path contract assertions (DESIGN.md sec 7)
- Rejected: interop-only Stage 2.
- Reason: interop never sends a hash hashcore rejects, so a mutant that fails open (Verify error -> 200 match:true) passes Stages 2, 3, 5 and fitness: a total auth bypass.
- Reverse: DESIGN.md sec 7 edit to drop the group (reopens the bypass).

## 2026-09-27 · PR #6 · hashcore Verify: accept only the canonical encoding (exact param segments, re-encoded salt/key)
- Rejected: trusting argon2id.DecodeHash's parse (Params struct equality, decoded salt/key as-is).
- Reason: its Sscanf ignores trailing input and accepts non-canonical numbers (`m=019456`, `p=1,junk`, `v=19x`), and base64 decoding ignores CR/LF even in Strict mode; so segments 1-3 are compared as exact strings and salt/key must re-encode to their segments. Wrong variant/version -> ErrUnsupportedParams (downgrade); syntax or non-canonical salt/key -> ErrMalformedHash.
- Reverse: drop the segment and re-encode comparisons in checkParams, relying on DecodeHash + struct equality.

## 2026-09-27 · PR #6 · golang.org/x/crypto pinned at v0.55.0, not argon2id's v0.14.0 minimum or latest
- Rejected: v0.14.0 (2023, known advisories in other subpackages); v0.56.0+ (requires go 1.26, would bump the go directive).
- Reason: newest x/crypto compatible with the go 1.25.0 directive (PR #4 entry).
- Reverse: `go get golang.org/x/crypto@<version>`; moving past v0.55.0 needs the go directive raised first.

## 2026-09-27 · PR #6 · go directive 1.26.0, golang.org/x/crypto v0.57.0 (supersedes "golang.org/x/crypto pinned at v0.55.0, not argon2id's v0.14.0 minimum or latest")
- Rejected: staying on go 1.25.0 with x/crypto pinned at v0.55.0.
- Reason: 1.25 is out of security support since Go 1.27 (2026-08-19), and the pin blocks x/crypto fixes (v0.56.0+ needs go 1.26); the PR #4 "go directive 1.25.0" GOMAXPROCS rationale still holds (>= 1.25).
- Reverse: lower the directive to 1.25.0 and `go get golang.org/x/crypto@v0.55.0`.

## 2026-09-27 · PR #6 · wrapper: HashHandler/VerifyHandler/ReadyHandler are methods returning http.Handler
- Rejected: methods that are themselves http.HandlerFunc.
- Reason: a returned http.Handler lets a mutant wrap its handlers in its own middleware without changing the fixed signatures cmd/service calls.
- Reverse: change the three methods and cmd/service's calls together (cmd/service is fixed at gen 0, so this is architectural).

## 2026-09-27 · PR #6 · wrapper.Env: 0 in a field means unconstrained/unknown
- Rejected: a separate "known" flag per field, or a non-zero default.
- Reason: a bare local run (gate Stage 1 boot) has no downward API; cmd/service passes 0 for absent limits and exits on present-but-invalid ones, so 0 is never a real limit.
- Reverse: add explicit known flags to Env and cmd/service (changes the fixed API).

## 2026-09-27 · PR #6 · wrapper gen 0: validate the request body before waiting for a hashing slot
- Rejected: acquiring the semaphore first.
- Reason: bad input must never queue or consume a slot, and a 400 is then independent of load.
- Reverse: move acquire above decode in the handlers (a mutant may do so; the contract still holds).

## 2026-09-27 · PR #6 · Error responses: JSON {"error": "<short fixed message>"}
- Rejected: plain-text bodies; empty bodies.
- Reason: the DESIGN.md sec 8 contract is silent on error bodies; JSON matches the success bodies, and fixed messages never echo passwords or hashes. Clients must rely on the status code, not the message.
- Reverse: change writeError in the wrapper (evolvable; not part of the contract).

## 2026-09-27 · PR #6 · OPEN: gen-0 wrapper tests are copied into the mutation sandbox
- Question: DESIGN.md sec 12 copies "the wrapper's existing .go files", which includes _test.go, so mutants can edit gen-0 tests, and gen-0-specific tests would fail legitimate mutants if Stage 1 ran them.
- Options: exclude _test.go from the copy and from gate runs, or keep them as mutant-editable, non-gating tests.
- To be settled when the driver and gate are built; recorded here so it is not lost.

## 2026-09-27 · PR #6 · Stage 1 import allowlist checks direct imports only (DESIGN.md sec 7 edit pending)
- Rejected: checking the full dependency closure (`go list -deps`).
- Reason: net/http transitively imports crypto/* and std's vendored x/crypto, so a -deps check would reject every HTTP-serving wrapper. Direct-only is sound only if the allowlist excludes unsafe (the compiler allows `//go:linkname` only in files that import unsafe), cgo ("C"), os/exec, syscall and plugin.
- Reverse: n/a until gate/ exists; amend DESIGN.md sec 7 when it is built.

## 2026-09-27 · PR #6 · Contract password limit (1-1024 bytes) applies to both endpoints and is checked by Stage 2
- Rejected: leaving the limit unchecked by the gate.
- Reason: the contract says a mutant that breaks it fails Stage 2, so every stated 400 case must be tested (DESIGN.md sec 7, sec 8).
- Reverse: drop the Stage 2 case and the "either endpoint" wording.

## 2026-09-27 · PR #6 · cmd/service listens on fixed :8080
- Rejected: a flag or env var for the port.
- Reason: cmd/service takes only downward-API facts, never config (DESIGN.md sec 5); the port is set once in the manifests, and a fixed value keeps Stage 1's boot check trivial.
- Reverse: edit the constant and every manifest/probe that names 8080.

## 2026-09-27 · PR #6 · /healthz = process alive, owned by cmd/service, not the wrapper
- Rejected: delegating liveness to the wrapper, or tying it to load.
- Reason: a mutant that could fail liveness under load would get its pods restarted (restartCount fails Stage 5) instead of shedding; readiness (/readyz) is the wrapper's load-aware signal.
- Reverse: route /healthz through a wrapper method (changes the fixed API).

## 2026-09-27 · PR #6 · X-EK8s-Variant writer exposes Unwrap(); unwrapping can bypass the header
- Rejected: no Unwrap (breaks http.ResponseController Flush/deadlines for mutants); forcing the header into the underlying writer up front (a handler could still delete it).
- Reason: the header is a sanity check, not load-bearing (DESIGN.md sec 5); a mutant writing through Unwrap to hide it gains nothing scored.
- Reverse: drop Unwrap from variantWriter in cmd/service/main.go.

## 2026-09-27 · PR #6 · Shutdown: 5s readiness-503 drain, then http.Server.Shutdown with a 20s deadline
- Rejected: Shutdown immediately on SIGTERM (requests still routed during endpoint removal get refused).
- Reason: endpoint removal propagates asynchronously; 5s + 20s = 25s stays under the default 30s terminationGracePeriodSeconds. Shutdown timeout -> Close and exit 1.
- Reverse: change drainDelay/shutdownTimeout in cmd/service/main.go (keep their sum under the pod's grace period).

## 2026-10-01 · PR #7 · CI on GitHub Actions: vet, test, -race, govulncheck on Go 1.26.x (latest patch)
- Rejected: `go-version-file: go.mod` (gives exactly 1.26.0); a third-party govulncheck action.
- Reason: 1.26.0's fixed stdlib vulns would make govulncheck noisy; `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0` keeps dependencies minimal. Runners reach vuln.go.dev, which cloud sessions can't.
- Note: Raise setup-go's go-version whenever the go directive is raised: setup-go sets GOTOOLCHAIN=local, so CI fails with 'go.mod requires go >= ...' until it is (local sessions with GOTOOLCHAIN=auto would instead download the directive's exact, possibly old, patch).
- Reverse: delete .github/workflows/ci.yml.

## 2026-10-01 · PR #7 · Accepted govulncheck finding: GO-2026-5932 (x/crypto/openpgp)
- Rejected: dropping or forking x/crypto to clear the finding.
- Reason: module-level only; the advisory has no fixed version; the repo builds only x/crypto's argon2 and blake2b packages (`go list -deps -test ./...`), never openpgp.
- Revisit: if anything ever imports golang.org/x/crypto/openpgp (gate Stage 1's allowlist will forbid x/crypto in the wrapper, DESIGN.md sec 7; not enforced until gate/ exists).

## 2026-10-01 · PR #8 · Keep the platform Stop hook; answer its feedback with a one-line acknowledgement (CLAUDE.md)
- Rejected: `disableAllHooks` in .claude/settings.json (removes the safety net, and disables the owner's user-level hooks in this repo on their workstation); editing launcher files (regenerated each session).
- Reason: the hook's unpushed-work report is a useful safety net; acting on it would bypass CLAUDE.md's commit/push approval. Upstream: anthropics/claude-code #96146, #82624.
- Reverse: drop the Stop hook bullet from CLAUDE.md's working agreement.

## 2026-10-01 · PR #9 · Cloud environment: drop GIT_COMMITTER_*, keep GIT_AUTHOR_* (owner authors, Claude commits)
- Rejected: keeping both (the platform's SSH key signs commits whose committer is the owner, so GitHub shows them Unverified); re-authoring in-session (env vars override git config).
- Reason: GitHub validates a signature against the committer; with GIT_COMMITTER_* gone, git falls back to the environment's git config (Claude <noreply@anthropic.com>), which matches the signing key. Authorship and contribution credit stay with the owner. Confirmed: f55adbf shows Verified on GitHub.
- Reverse: re-add GIT_COMMITTER_NAME/GIT_COMMITTER_EMAIL in the cloud environment's settings.

## 2026-10-01 · PR #10 · docs/STATUS.md is the resume point, maintained by Code sessions; planning chat reads it, not transcripts
- Rejected: planning chat reconstructing state from session transcripts.
- Reason: transcripts are long and scattered; a short Now/Next/Parked/Recent file updated in every PR is cheap to read after a long gap.
- Reverse: delete docs/STATUS.md and the STATUS.md bullet in CLAUDE.md's working agreement.

## 2026-10-01 · PR #11 · Chat and Code keep separate roles; docs/handoff.md defines what crosses between them
- Rejected: pasting session transcripts or command logs into the planning/review chat.
- Reason: Code implements; the chat plans, reviews and approves but cannot read the repo, so it needs evidence (quoted lines, diffs, check results), not transcripts. Packets are produced only on request.
- Reverse: delete docs/handoff.md and the handoff bullet in CLAUDE.md's working agreement.

## 2026-10-01 · PR #12 · CI runs only on Go/module/testdata/workflow changes, plus weekly (Mon 06:17 UTC) and by hand
- Rejected: `paths-ignore` (new non-Go files would trigger runs by default); no schedule (govulncheck would miss advisories published while Go code is unchanged).
- Reason: docs-only PRs need no vet/test/govulncheck. main has no branch protection or required checks (checked 2026-10-01; rulesets unavailable on this plan), so skipped runs leave nothing Pending.
- Revisit: if a required status check is ever added, path-filtered skips would block merges; drop the filter or add an always-run job.
- Reverse: remove the `paths:`, `schedule:` and `workflow_dispatch:` keys from .github/workflows/ci.yml.

## 2026-10-01 · PR #13 · Each PR starts by reconciling STATUS.md and decisions.md with git log origin/main
- Rejected: a separate housekeeping PR after each merge.
- Reason: entries are written before a PR is opened, so its number starts as "(inferred)"; fixing them in the next task's PR keeps the record accurate without extra PRs.
- Reverse: drop the reconcile bullet from CLAUDE.md's working agreement.

## 2026-10-02 · PR #14 · Review packet full diff: -U10 for Go tests, docs/STATUS.md omitted unless asked, ~600-line threshold
- Rejected: -W on all Go files and the STATUS.md diff always included, with a ~1500-line threshold.
- Reason: whole test functions and STATUS.md churn crowd out the code under review; ~600 lines keeps a packet to one paste. STATUS.md still shows in --stat and under Omitted:, so the chat can ask for it.
- Reverse: restore the two-part full diff and the 1500-line threshold in docs/handoff.md.
