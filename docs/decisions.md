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

## 2026-09-27 · PR #6 · OPEN: gen-0 wrapper tests are copied into the mutation sandbox (closed by the PR #18 _test.go entry)
- Question: DESIGN.md sec 12 copies "the wrapper's existing .go files", which includes _test.go, so mutants can edit gen-0 tests, and gen-0-specific tests would fail legitimate mutants if Stage 1 ran them.
- Options: exclude _test.go from the copy and from gate runs, or keep them as mutant-editable, non-gating tests.
- To be settled when the driver and gate are built; recorded here so it is not lost.

## 2026-09-27 · PR #6 · Stage 1 import allowlist checks direct imports only (DESIGN.md sec 7 edit done in PR #18)
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

## 2026-09-27 · PR #6 · X-EK8s-Variant writer exposes Unwrap(); unwrapping can bypass the header (reason superseded by the PR #18 header entry)
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

## 2026-10-02 · PR #15 · classifier: full hash verification is an injected Verifier; the package stays stdlib-only
- Rejected: splitting the ceiling into its own package (B); importing argon2id in classifier with a direct-imports-only Stage 1 check (C); exporting a check from hashcore (D).
- Reason: internal/wrapper imports classifier, so any Argon2/crypto import there would undercut the Stage 1 backstop; injection keeps DESIGN.md sec 5's "never imports hashcore" literal with no package split.
- Reverse: give classifier its own Verify (option B or C), with a DESIGN.md sec 5/sec 7 edit.

## 2026-10-02 · PR #15 · classifier reimplements hashcore's canonical-PHC check
- Rejected: exporting checkParams (or a parse-only variant) from hashcore.
- Reason: keeps hashcore's API unchanged; drift is caught by PR B's hashcore-vs-classifier agreement test, and Stage 5 always runs hashcore Verify on every hash response.
- Reverse: export a check from hashcore and inject it alongside the Verifier.

## 2026-10-02 · PR #15 · HangTimeout bounds every bucket, graceful_shed included
- Rejected: accepting a 503 + Retry-After at any latency.
- Reason: a shed that arrives after the ceiling has still hung the client; otherwise a mutant could hold queued requests indefinitely and still score as shedding gracefully.
- Reverse: DESIGN.md sec 8 edit (graceful_shed and failed bullets); move the latency check after the 503 branch in Classify.

## 2026-10-02 · PR #15 · graceful_shed needs exactly one Retry-After that parses (delay-seconds or HTTP-date)
- Rejected: presence alone (DESIGN.md sec 8's earlier wording); the first of several values.
- Reason: an empty or garbage value is no usable retry hint; a repeated Retry-After header is rejected, since RFC 9110 defines it as a single-value field. Both fail closed. Gen 0 sends "1".
- Reverse: DESIGN.md sec 8 edit (graceful_shed bullet, HTTP contract); replace the parse check in Classify with a presence check.

## 2026-10-02 · PR #15 · Strict 2xx bodies: one JSON object, exactly the contract key (case-sensitive), non-null right type, no trailing data
- Rejected: encoding/json struct decoding (matches keys case-insensitively, accepts null as absent); ignoring unknown fields.
- Reason: the HTTP contract (DESIGN.md sec 8) is fixed, so extra or misspelled fields mean a broken mutant. Duplicate keys are not detected (last wins); the wrapper's encoder never emits them.
- Reverse: DESIGN.md sec 8 edit (HTTP contract 200-body bullet); let decodeField ignore keys other than the contract one.
- Follow-up: gate Stage 2 must assert the 200-body contract when gate/ is built (DESIGN.md sec 8 says a mutant that breaks it fails Stage 2).

## 2026-10-02 · PR #15 · Salt-reuse scope: one classifier Run per variant live lifetime (promotion to cull); late 2xx hash salts recorded too
- Rejected: a Run per observation window; ignoring salts from responses over HangTimeout.
- Reason: per-window Runs let a mutant cache hash strings from an earlier window and replay them later, skipping Argon2, caught only by sampled FullVerify; a late hash not recorded could be replayed once on time. Stage 5's burst uses its own Run.
- Reverse: DESIGN.md sec 8 edit (hash success conditions); drop the late-salt recording in Classify.

## 2026-10-02 · PR #16 · Test-vector pool in internal/classifier/testvectors; Stage 1 allowlist matches internal/classifier exactly
- Rejected: the pool inside internal/classifier (DESIGN.md sec 5's earlier wording); a prefix match on internal/classifier.
- Reason: wrapper imports internal/classifier; with the pool reachable, a mutant could answer /verify by table lookup, skipping Argon2. See DESIGN.md sec 5, sec 7 Stage 1.
- Reverse: move vectors.json and its loader into internal/classifier; DESIGN.md sec 5/sec 7 edit.

## 2026-10-02 · PR #16 · Mismatch pairs are stored in vectors.json, not derived by callers
- Rejected: storing only matching pairs and letting gate/loadgen pair passwords with other hashes.
- Reason: one definition of "should reject" for gate and loadgen, checked once by the pool-validity test; callers cannot pick a pairing that accidentally matches.
- Reverse: drop Match=false entries from gen and the format; derive mismatches in callers.

## 2026-10-02 · PR #16 · Pool-validity test (every pair through hashcore Verify) always runs in non-race test runs, including under -short; skipped under -race only
- Rejected: skipping it under -short as a slow Argon2 test (~6s); running it under -race too (~60s on 4 cores).
- Reason: the pool is the ground truth for gate and loadgen; a stale or corrupt vectors.json must fail every non-race run. Under -race it (and TestAgreement) has no shared mutable state to race on, and CI's plain go test step runs both.
- Reverse: add `if testing.Short() { t.Skip(...) }` to TestPoolVerify; drop the raceEnabled skips (race_test.go, norace_test.go).

## 2026-10-02 · PR #17 · Pre-packet self-review by a read-only reviewer subagent (.claude/agents/reviewer.md) on Go changes
- Rejected: relying on the planning chat alone to review.
- Reason: the chat cannot read the repo, so each finding it makes costs a relay round trip, and most were checkable in-repo. One run per packet on the final tree, not per edit, to keep usage down; docs-only changes skip it.
- Reverse: delete .claude/agents/reviewer.md, the self-review bullet in CLAUDE.md's working agreement, and the Self-review: section in docs/handoff.md; restore `.claude/` in .gitignore.

## 2026-10-02 · PR #17 · Measure the reviewer by tagging chat Feedback lines [missed] / [repo-unseeable] / [dup]
- Rejected: seeded-defect runs on a scratch branch as the routine measure (kept for an occasional check once gate/ exists).
- Reason: zero extra reviewer runs; after a few Go PRs the [missed] count shows whether the reviewer earns its cost, and each [missed] item is a concrete checklist edit. Self-review `declined:` lines give its false positives.
- Reverse: drop the tagging bullet from docs/handoff.md's rules for every packet.

## 2026-10-09 · PR #18 · gate/ in four PRs: Stage 1 pre-build checks; Stage 1 build/boot + Stages 2-3; Stage 4; Stage 5
- Rejected: Stages 1-3 in one PR; all local stages in one PR; deferring the docker sub-check to deploy/.
- Reason: the pre-build checks are the sec 4 backstop and get a review of their own; each PR fits one review packet. Stage 5 waits for deploy/ and loadgen burst mode; namespace lifecycle is already settled by DESIGN.md sec 11.
- Reverse: merge the remaining gate PRs into fewer.

## 2026-10-09 · PR #18 · Build tree: fresh temp dir with the fixed tree + candidate; go env built from scratch (DESIGN.md sec 7 Stage 1)
- Rejected: building in the repo checkout; inheriting the operator's environment or go env file.
- Reason: inherited GOFLAGS can carry -toolexec, and the go env file can set CC, CGO_*, GOEXPERIMENT, GOCACHEPROG; so GOENV=off, GOFLAGS=-mod=readonly, GOPROXY=off, GOWORK=off, GOTOOLCHAIN=local, and only PATH, GOMODCACHE, GOCACHE (resolved once via the operator's `go env`) carried over; without HOME or XDG_CONFIG_HOME no go env file can be located. That one operator `go env` may download the go directive's toolchain (checksum-verified) on an older host go. GOPROXY=off makes `go mod download` a prerequisite. GOARCH is the gate host's (kind runs on the same host).
- Reverse: edit toolchain.env in gate/assemble.go.

## 2026-10-09 · PR #18 · Candidate files: exactly doc.go, hasher.go, server.go, tunables.go; regular files only, 256 KiB cap each
- Rejected: copying whatever .go files the candidate dir holds; following symlinks.
- Reason: a mutant cannot create files (sec 12), so any other entry is a driver or tampering fault. Entries are typed by lstat (ReadDir), then opened O_NOFOLLOW|O_NONBLOCK and re-checked on the descriptor, so a swap after the listing cannot be followed. A test keeps the set equal to gen 0's non-test files.
- Reverse: edit candidateFiles / maxCandidateFileBytes in gate/assemble.go.

## 2026-10-09 · PR #18 · Canonical hasher.go is a SHA-256 constant in gate/, kept equal to the repo file by a test
- Rejected: comparing against the working tree's internal/wrapper/hasher.go at gate time; an embedded copy in gate/.
- Reason: a working-tree file could have been overwritten; //go:embed cannot reach ../internal/wrapper, and a copy would be a second source. Changing hasher.go now takes a deliberate gate edit.
- Reverse: compare against the repo file in checkHasherPin.

## 2026-10-09 · PR #18 · Stages 2-4 talk HTTP to the booted binary; X-EK8s-Variant becomes load-bearing in the gate (supersedes the PR #6 Unwrap entry's reason)
- Rejected: in-process tests of the candidate package (a generated harness run with go test).
- Reason: same binary as the image, through cmd/service's mux and body limit. With fixed :8080, every response must carry X-EK8s-Variant equal to a per-run label, so a stale listener cannot answer; a mutant that writes via Unwrap and drops the header now fails Stage 2 (DESIGN.md sec 5 edited). Unwrap stays, for http.ResponseController.
- Reverse: DESIGN.md sec 5, sec 7 edit; drop the header check from Stages 2-4.

## 2026-10-09 · PR #18 · Stage 1 allowlist finalized in DESIGN.md sec 7 (closes "Stage 1 import allowlist checks direct imports only")
- Rejected: a denylist; a -deps check; one go list under default tags only.
- Reason: listed under CGO_ENABLED=0 and CGO_ENABLED=1 -race, each re-checking imports and CgoFiles/IgnoredGoFiles, plus a lexical ban on //go:build and // +build lines, so no constraint hides a file's imports from either build. os is excluded (env and limits come via cmd/service). Imports are checked before load errors: go list -e still reports a forbidden import that fails to resolve.
- Reverse: edit allowedImports / listings in gate/precheck.go and DESIGN.md sec 7 together.

## 2026-10-09 · PR #18 · Known limit: net/http can read and exfiltrate files; contained by bwrap for the bare boot and a scratch image
- Rejected: dropping net/http from the allowlist (every wrapper serves HTTP).
- Reason: http.Dir("/").Open / FileServer read files and http.Client sends them; no import allowlist closes that. Stage 1's bare boot (PR2) runs under bubblewrap (--clearenv, binary bound ro, no /home or repo); the container and pod see only a FROM scratch image.
- Reverse: n/a (a limit, not a choice); revisit if the boot ever runs unsandboxed.

## 2026-10-09 · PR #18 · Gen-0 _test.go files are excluded from the sandbox copy and the gate (closes OPEN "gen-0 wrapper tests are copied into the mutation sandbox")
- Rejected: copying them as mutant-editable, non-gating tests.
- Reason: gen-0 tests would fail legitimate mutants and add nothing the gate does not check; they stay in the repo as CI coverage of gen 0. DESIGN.md sec 12 and CLAUDE.md's Build/test lines edited (the gate runs no candidate tests).
- Reverse: DESIGN.md sec 12 edit; add test files to candidateFiles.

## 2026-10-09 · PR #18 · Stage 2: fresh random passwords for MutantHash, sequential requests, success-only, one classifier Run (closes the PR #15 2xx-body follow-up)
- Rejected: pool passwords for /hash with an ordering rule (all /hash before any /verify).
- Reason: never sending a pool password to /hash leaves a mutant no pool pair to replay, with no ordering to get wrong. Each 2xx goes through one classifier.Run with FullVerify (strict body, canonical PHC, salt reuse); a graceful_shed on one request to an idle process fails. Contract cases assert exactly 400.
- Reverse: DESIGN.md sec 7 Stage 2 edit.

## 2026-10-09 · PR #18 · Image: host-built static binary, FROM scratch, USER 65532:65532; build context holds only Dockerfile + binary
- Rejected: building inside a golang image; a distroless base.
- Reason: no image pulls or network, the bare-booted binary is the one deployed, matching sec 11's go build -> docker build -> kind load; a minimal context keeps the repo out of the image.
- Reverse: change the Dockerfile (PR2) and DESIGN.md sec 7 Stage 1.

## 2026-10-09 · PR #18 · Stage 4: retry shed requests; fail on cross-talk, any status but 200 or a 503 shed (a 400 included), or any race report; race build needs cgo + gcc
- Rejected: counting a 503 shed as failure; applying HangTimeout under -race.
- Reason: a shed proves nothing either way, but every password must eventually succeed; Stage 4 sends only valid distinct passwords, so a 400 is a broken wrapper. Verified: CGO_ENABLED=0 go build -race fails ("-race requires cgo"), and with CGO_ENABLED=1 and no gcc on PATH it fails in runtime/cgo; go list -race needs no gcc. CI's existing go test -race step shows ubuntu-latest has a C compiler.
- Reverse: DESIGN.md sec 7 Stage 4 edit.

## 2026-10-09 · PR #18 · gate API: Run returns a Report of per-check Results plus an error only for gate faults; CLI at gate/cmd/gate
- Rejected: one pass/fail bool; reporting gate faults as candidate failures; no CLI.
- Reason: Stage 1's checks are logged distinctly (sec 7); a gate fault (no go, I/O) says nothing about the candidate. A go list that exits non-zero is a candidate failure, since the tree differs from a good one only in the candidate. Nothing is persisted until lineage/ exists.
- Reverse: change gate.Run's signature and its callers.

## 2026-10-09 · PR #18 · Gate tests mutate a temp copy of gen 0 instead of keeping testdata fixture dirs
- Rejected: gate/testdata/candidates/<case>/ with full copies of the wrapper.
- Reason: fixtures would be stale copies of gen 0; each case is one edit to the live gen-0 files. PR1's tests are pure go list runs, fine under -race.
- Reverse: add fixture dirs and point TestRejects at them.

## 2026-10-09 · PR #18 · CI pins go-version 1.26.9 in ci.yml (departs from the PR #7 "latest patch" entry)
- Rejected: '1.26.x' with check-latest (setup-go resolves it from the actions/go-versions manifest, which lags: newest 1.26 there is 1.26.8).
- Reason: govulncheck fails on stdlib advisories fixed in 1.26.9, but setup-go resolves '1.26.x' to the runner's cached 1.26.8 and its manifest has nothing newer; a version missing from the manifest is downloaded from go.dev instead.
- Revisit: switch back to '1.26.x' once the manifest lists >= 1.26.9.
