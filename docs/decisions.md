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
- Follow-up (done in PR #21: every Stage 2 2xx goes through classifier.Run): gate Stage 2 must assert the 200-body contract when gate/ is built (DESIGN.md sec 8 says a mutant that breaks it fails Stage 2).

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

## 2026-10-09 · PR #18 · Known limit: net/http can read and exfiltrate files; contained by bwrap for the bare boot and a scratch image (updated PR #21)
- Rejected: dropping net/http from the allowlist (every wrapper serves HTTP).
- Reason: http.Dir("/").Open / FileServer read files and http.Client sends them; no import allowlist closes that. Stage 1's bare boot runs under bubblewrap (--clearenv, only the binary bound, ro; no /home, repo or /dev) in its own network namespace, so it has no network egress either (PR #21 bwrap entry); the container and pod see only a FROM scratch image, and keep bridge/pod egress.
- Reverse: n/a (a limit, not a choice); revisit if the boot ever runs unsandboxed or shares the host network namespace.

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

## 2026-10-09 · PR #20 · go.mod gains `toolchain go1.26.9` (go directive stays 1.26.0)
- Rejected: CI go-version-file now (the actions/go-versions manifest still lags 1.26.9; revisit with the CI unpin item in STATUS).
- Reason: under GOTOOLCHAIN=auto, local, cloud and gate resolveToolchain builds use >= 1.26.9, never 1.26.0's vulnerable stdlib. Each Go security patch needs this line bumped.
- Reverse: delete the toolchain line.

## 2026-10-09 · PR #21 · Gate fault when the resolved go is older than go.mod's toolchain line (Q3 item 15)
- Rejected: trusting the toolchain line alone (under GOTOOLCHAIN=local it is only a preference, so a 1.26.0-1.26.8 go would build the deployed binary).
- Reason: resolveToolchain compares `go env GOVERSION` with go.mod's toolchain line (the go directive when there is none); older is an error from Run, before any candidate check that builds.
- Reverse: drop the minGoVersion comparison in gate/assemble.go.

## 2026-10-09 · PR #21 · Bare boot: bwrap argv, no /dev, no --disable-userns
- Argv (gate/boot.go bwrapArgs): --unshare-all --unshare-user --die-with-parent --new-session --clearenv --setenv EK8S_VARIANT <label> --uid 65532 --gid 65532 --hostname hasher-<10>-<5> (random per run, pod-like: removes the fixed-hostname tell only; others remain, STATUS Parked) --ro-bind <bin> /service --proc /proc --chdir / --cap-drop ALL --info-fd 3 /service.
- Rejected: --dev /dev and --disable-userns: either makes the relay's `nsenter -U -n` fail with EPERM (bisected in the PR2 design session). Hashing works without /dev (verified); without syscall, os/exec or unsafe the candidate cannot call unshare/clone anyway.
- Reason: --unshare-user is explicit because --unshare-all only tries it. Verified as root and as an unprivileged user, but not under Ubuntu's AppArmor userns restriction (fails there; PR #21 CI entry).
- Reverse: edit bwrapArgs; adding --dev or --disable-userns needs another way to reach the candidate (Q1 options A/D).

## 2026-10-09 · PR #21 · Gate reaches the sandbox via a relay: nsenter -U -n into its namespaces, Unix socket in the gate tmp dir (Q1 option B)
- Rejected: --share-net (host egress, kind API, metadata endpoint; :8080 collisions); the Stage 2 client inside the sandbox (shares the candidate's uid, pool readable via http.Dir); in-process setns (needs CAP_SYS_ADMIN, so root).
- Reason: only option with no candidate egress, working unprivileged. gate/internal/relay is stdlib-only, go-built per run from the repo with the resolved toolchain; it stays in the host mount and pid namespaces. Not joined -> gate fault, unless the candidate already exited (boot failure). It exits on stdin EOF from a pipe the gate never writes, so a SIGKILLed gate does not leave it holding the dead candidate's namespaces (Pdeathsig fires on the creating OS thread's exit in Go).
- Reverse: --share-net in bwrapArgs, drop the relay and dial 127.0.0.1:8080.

## 2026-10-09 · PR #21 · Stage 1 order and docker sub-check (Q3 items 1-8, 12-14)
- Order: build -> bare boot + /healthz (100ms polls, 10s, fail at once on exit) -> docker build/run/health/rm (30s) -> Stages 2-3 on the bare process -> kill bwrap. Label "gate-" + 16 hex per run; EK8S_*_LIMIT unset (0) until deploy/. docker run -d --rm -p 127.0.0.1::8080 --read-only --cap-drop ALL --security-opt no-new-privileges --pids-limit 256 --memory 256m --memory-swap 256m --cpus 1 -e EK8S_VARIANT.
- Rejected: --network none (needs root to reach); treating docker CLI failures as candidate failures (daemon faults say nothing about it).
- Reason: the bare process has no egress and is the one Stages 2-3 need; the container only proves the image boots. Only its /healthz (or exiting before its port is published) is a candidate Result. Tag ek8s-wrapper:<12 hex of the binary's sha256>, built -trimpath so identical code gets one tag; kept on pass (Report.Image), rmi on fail unless it existed before the run. Output kept 64 KiB, in Detail on boot/Stage 2 failures.
- Reverse: edit gate/docker.go, gate/boot.go constants and Run's order.

## 2026-10-09 · PR #21 · Stage 2: 3 inputs per interop assertion, hashcore Verify as the real library, client timeout HangTimeout+0.5s; Stage 3 its own parser (Q3 items 9-11)
- Rejected: one input per assertion; relying on the classifier's exact params segment for Stage 3 (it already rejects any other).
- Reason: the pool's hashes came from hashcore, so hashcore.Verify is the reference; the client timeout sits above HangTimeout so the classifier reports hangs. Stage 3 parses m, t, p numerically against restated constants, so a classifier bug is not a single point of failure. Contract cases also cover a missing hash, non-object body and truncated hash; "password-bounds" hashes fresh 1- and 1024-byte passwords (never pool ones), fully verified, counted by Stage 3.
- Reverse: edit pairsPerAssertion, clientTimeout, contractCases and checkCostParams in gate/interop.go.

## 2026-10-09 · PR #21 · Missing bwrap, nsenter or docker daemon: gate fault; tests skip/stub unless EK8S_GATE_REQUIRE_TOOLS=1 (CI)
- Rejected: a Config knob to skip the docker sub-check; skipping all Stage 2 tests on hosts without docker (cloud sessions).
- Reason: tests swap the unexported dockerSubCheck var for a stub when no daemon runs, so Stages 2-3 are still tested; CI apt-installs bubblewrap and sets the env var, so there both are required.
- Reverse: drop the env var from ci.yml and setupHost in gate/stages_test.go.

## 2026-10-09 · PR #21 · CI lifts Ubuntu's AppArmor userns restriction (sysctl) for the bare boot
- Rejected: running the gate tests as root (root bypasses the restriction, hiding the unprivileged path operators use); a shared host net ns (excluded: candidate egress).
- Reason: on ubuntu-latest (24.04) bwrap got a userns without capabilities: "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted" (run 37903906043). `sysctl kernel.apparmor_restrict_unprivileged_userns=0` on the ephemeral runner is one line; operator hosts should prefer an AppArmor profile granting bwrap userns (CLAUDE.md, gate/doc.go; STATUS Parked).
- Reverse: drop the sysctl step from ci.yml (CI then fails at boot unless a bwrap profile is installed instead).

## 2026-10-09 · PR #21 · Sandbox preflight: boot a gate-built probe through the whole bare-boot path before any candidate
- Decision: gate/internal/probe (stdlib, /healthz like cmd/service) is booted with the same bwrapArgs, nsenter -U -n join, relay "ready" and /healthz, then torn down; any failure is a gate fault (error from Run, no "boot" Result) carrying bwrap's/nsenter's output.
- Rejected: classifying by a "bwrap:" prefix in the output (gameable: a candidate can print it and exit); --json-status-fd (cannot tell a bwrap setup failure from a candidate's exit 1).
- Reason: bwrap reports child-pid before it sets up loopback, so on the AppArmor-restricted runner a host failure read as the candidate's boot failure (run 37903906043). Residual gap: a host failure after a passing preflight still reads as a candidate boot failure.
- Reverse: drop the preflight call in Run and gate/internal/probe.

## 2026-10-12 · PR #22 · Newcomer overview in docs/overview.md; README.md is a short front door
- Rejected: the full overview as README.md; no overview (DESIGN.md only).
- Reason: DESIGN.md is a spec, not an introduction; a separate overview keeps the landing page to what/layout/build/links. The overview defers to DESIGN.md and docs/STATUS.md, and a CLAUDE.md bullet makes DESIGN.md and seed-wrapper PRs check it, so it cannot drift silently.
- Reverse: delete docs/overview.md, README.md's links to it, and the CLAUDE.md overview bullet.

## 2026-10-12 · PR #23 · Packets are the default end-of-task output (reverses PR #11's "only on request")
- Rejected: a prose report followed by a separate packet on request.
- Reason: the same facts were produced twice per task, and a packet already carries most of the (a)-(g) items.
- Reverse: restore the request-only wording in handoff.md and the standalone report rule in CLAUDE.md.

## 2026-10-12 · PR #23 · Packet header carries origin/main and the last merged PR; sync packet type; chat-side staleness check
- Rejected: relying on project-knowledge GitHub sync alone (manual; no sync control found in the iPad app or the claude.ai web UI, re-adding the files is the only refresh found).
- Reason: the planning chat wrote docs/overview.md from snapshots four PRs stale and nothing flagged it; the header exposes drift at the first packet.
- Reverse: drop the main field and the Sync packet and Chat side sections from handoff.md.

## 2026-10-12 · PR #23 · The start-of-PR reconcile goes in its own commit, first
- Rejected: bundling the reconcile with the task's commit.
- Reason: one logical change per commit; the bundling recurred in PRs #22 and #23.
- Reverse: drop the "own commit, first" clause from CLAUDE.md's reconcile bullet.

## 2026-10-12 · PR #25 · Stage 4 race build: static link, -tags netgo,osusergo, same bwrap sandbox (packet Q1 A)
- Rejected: a dynamic build with ld.so and libc --ro-bind'ed (per-distro paths, more in the sandbox); static without the tags (getaddrinfo static-link warning).
- Reason: the bwrap argv binds only the binary, so a dynamic race build fails "execvp /service: No such file or directory"; -linkmode=external -extldflags=-static boots and reports races there, as root and unprivileged. The race go list carries the same -tags (raceFlags). Needs gcc plus libc.a.
- Reverse: drop raceLinkFlags/raceTags in gate/boot.go and bind the libs in bwrapArgs; DESIGN.md sec 7 Stages 1 and 4 edit.

## 2026-10-12 · PR #25 · Race report: GORACE halt_on_error=1 exitcode=66, exit status primary, full-output marker scan (packet Q2 A)
- Rejected: scanning only the 64 KiB cappedBuffer (a candidate can log past it); the exit code alone.
- Reason: halt_on_error ends the process at the first report with 66, passed through by bwrap, and no allowlisted import can exit with a chosen code; raceWatch scans every byte (overlap of len(marker)-1), keeps an 8 KiB tail; the Detail is at most 4 KiB. Checked after the load plus a 1s grace; teardown stays SIGKILL.
- Reverse: edit goRace and raceWatch in gate/stage4.go.

## 2026-10-12 · PR #25 · Stage 4 load: N = 2 x NumCPU in [8, 64], fixed 1s+jitter retry, 15 sends, transport errors retried (packet Q4)
- Rejected: fixed N=32 (an unbounded-concurrency mutant hits cmd/service's 6s WriteTimeout on a 4-CPU host); honouring Retry-After (a mutant could stall the gate); failing at once on a transport error.
- Reason: constant per-CPU load and gen 0 sheds on any host; a shed or transport error is no evidence either way, but every request must get a final answer within 15 sends and 120s. A bare 503 is final and fails (graceful_shed minus HangTimeout).
- Reverse: edit the constants and sendWithRetry in gate/stage4.go; DESIGN.md sec 7 Stage 4 edit.

## 2026-10-12 · PR #25 · Stage 4 order: stop the Stage 2 process, race-build and boot the probe (gate fault), then the candidate (packet Q5 B)
- Rejected: relying on the Stage 1 preflight only (a missing gcc or libc.a would read as a candidate race-build failure); a docker sub-check of the race binary (never deployed).
- Reason: as in the PR #21 preflight, only gate code runs in the probe, so a failure there is the host's. sandbox.stop is now idempotent (sync.Once), so Run's deferred stop is safe after the explicit one.
- Reverse: drop the probe build and preflight call in runStage4.

## 2026-10-12 · PR #25 · Known gap: a wrapper can tell Stage 4 from its build info (A1)
- Decision: no mitigation in this PR; recorded in docs/STATUS.md Parked (gate detectability).
- Rejected: stripping or faking build info (it would change the deployed binary too, or diverge the two builds further).
- Reason: `go version -m` differs only in `-race=true`, `-tags=netgo,osusergo` and `CGO_ENABLED=1` (-ldflags not recorded), readable via runtime/debug.ReadBuildInfo (allowlisted); GORACE is readable from /proc/self/environ via http.Dir. A mutant could synchronize only under -race.
- Reverse: n/a (a limit); revisit with the gate-detectability item.

## 2026-10-12 · PR #25 · Stage 4 fails a candidate that exits after the load (owner review)
- Rejected: catching exits only through failed requests during the load.
- Reason: a mutant that answers everything and then exits (e.g. a panic in a goroutine) left no failed request; a crash under concurrency is a broken wrapper. Exit 66 stays a race-report; any other exit by the end of the load and the 1s grace fails concurrency. Gap: an exit later than that grace is not seen.
- Reverse: drop the sb.exited select after checkOutcomes in runStage4; DESIGN.md sec 7 Stage 4 edit.

## 2026-10-12 · PR #26 · README carries no status, only a pointer; overview.md's status section is tied to the STATUS.md rule
- Rejected: keeping README's status sentence and triggering its update on "changes what is built" too.
- Reason: README went stale after #25 (a gate PR that touched neither DESIGN.md nor the wrapper), and the sync packet's diff list omitted README.md.
- Reverse: restore the status sentence and the "(planned)" layout markers; drop the README.md and overview-status clauses from CLAUDE.md.

## 2026-10-12 · PR #26 · kind cluster: kind v0.33.0, one node, v1.37.0 by digest, name ek8s, 1 CPU / 2Gi reserved, no host ports (design packet Q1)
- Rejected: node v1.36.4 (closer to metrics-server's build deps, not a support statement); no reservation (allocatable = whole host); host loadgen via NodePort extraPortMappings (fixed port pool; the default listenAddress is 0.0.0.0).
- Reason: v1.37.0 is kind's tested default and inside metrics-server 0.9.x's 1.34+; kubeReserved + systemReserved 500m/1Gi each schedule pod requests against DESIGN.md sec 2's budget; loadgen runs in-cluster (DESIGN.md sec 10 edit), so nothing is published on the host.
- Reverse: edit deploy/kind/cluster.yaml (image, kubeadmConfigPatches) and recreate the cluster; DESIGN.md sec 10 edit for loadgen's location.

## 2026-10-12 · PR #26 · metrics-server v0.9.0 vendored, image by digest, --kubelet-insecure-tls (Q6)
- Rejected: the Helm chart (a new host tool); serverTLSBootstrap plus approving kubelet-serving CSRs on every cluster create.
- Reason: components.yaml is the unmodified release asset (sha256 in its kustomization); the digest comes from the k8s.io image promoter manifest; kind's kubelet cert is not signed by the cluster CA, acceptable with the API server on loopback only. HPA ships with the variant bundle in PR (2).
- Reverse: replace deploy/metrics-server/ and drop the patch.

## 2026-10-12 · PR #26 · stress-ng: upstream image by digest, kind-loaded under a digest tag; 0 <-> 1 replicas; dose in a ConfigMap; Guaranteed 3 CPU / 2Gi (Q7)
- Rejected: building FROM a distro base with apt (base pull plus apt network at build); a static build; an always-on pod driven by kubectl exec; a Job per stress half.
- Reason: colinianking/stress-ng@sha256:f59a28ca... is stress-ng 0.22.01; up.sh tags it ek8s-stress-ng:<12 hex> because kind load takes a tagged local image; pull Never, so the node never pulls. Runs as 65532 with a read-only root and an emptyDir /tmp (--temp-path), checked with docker. A small CPU request would make a weak neighbour.
- Reverse: edit deploy/stress/ and up.sh's stress_src/stress_tag.

## 2026-10-12 · PR #26 · The orchestrator owns stress state; Stage 5 requires stress-ng Running, else gate fault (Q7; DESIGN.md sec 7, sec 11)
- Rejected: Stage 5 turning stress on itself; Stage 5 waiting for a stress half.
- Reason: one owner for stress state; the orchestrator turns it on and marks the window excluded (sec 7 already excludes Stage 5 windows) before calling Stage 5. Anything but Running says nothing about the candidate.
- Reverse: DESIGN.md sec 7 Stage 5 and sec 11 edits.

## 2026-10-12 · PR #26 · Variant pods have no egress: NetworkPolicy deny-all egress, ingress 8080 from ek8s-system app=loadgen only (Q5; DESIGN.md sec 11)
- Updates the PR #18 net/http known-limit entry: pods no longer "keep bridge/pod egress" once PR (2) ships the policy; the container in the gate's docker sub-check still does.
- Rejected: Calico/Cilium (kind calls third-party CNIs unsupported); accepting egress.
- Reason: kindnet enforces NetworkPolicy since kind v0.24.0; egress enforcement is not stated in kube-network-policies' docs, so PR (2)'s smoke test must fail if egress is not denied.
- Reverse: DESIGN.md sec 11 edit; drop the NetworkPolicy template.

## 2026-10-12 · PR #26 · Variant resources: Burstable 250m/1000m CPU, 256Mi request = limit; HPA maxReplicas 2 (Q3; DESIGN.md sec 11)
- Rejected: Guaranteed 1 CPU (8 pods request 8 cores > ~7 allocatable, so scale-outs go Pending); Guaranteed 500m (GOMAXPROCS rounds a limit under 2 up to 2, so throttling dominates; go doc runtime.GOMAXPROCS, go1.26.9: never less than 2 unless the CPU count or affinity mask is below 2); maxReplicas 3.
- Reason: 4 variants x 2 pods = 8 pods: 2 cores requested, 2 GiB, 8 cores of limits (overcommit is the contention scored). 256Mi / 1 CPU equals the gate's docker sub-check. Implemented in PR (2)'s templates.
- Reverse: DESIGN.md sec 11 edit; edit the LimitRange and HPA templates.

## 2026-10-12 · PR #26 · Variant label "ek8s-<16 hex>", random per deployment, in gate and cluster (Q4; DESIGN.md sec 5)
- Rejected: the gate's "gate-<16 hex>" (a gate tell); a label carrying the variant id.
- Reason: one shape everywhere removes the label as a gate tell; the label -> ek8s-v<id> mapping lives in the orchestrator and lineage. PR (2) changes the gate's prefix (one line in gate/boot.go's newLabel).
- Reverse: DESIGN.md sec 5 edit; revert newLabel's prefix.

## 2026-10-12 · PR #26 · Merge gate for deploy/ PR (1): CI-size kind smoke run, not the workstation run
- Rejected: waiting for the owner's workstation (away for over a week).
- Reason: kind cannot run in the cloud container (cgroup v1, runc denied oom_score_adj), and CI's 2-CPU runner cannot hold the full-size reservation and stress dose; CI at CI size (cluster-ci.yaml, stress-ci overlay, EK8S_* parameters defaulting to full size) proves the mechanics but not the numbers, so the full-size run, budget numbers and host userns checks stay Parked, required before the first real orchestrator run.
- Reverse: drop .github/workflows/deploy.yml and the CI-size files; restore the local run as the merge gate.

## 2026-10-12 · PR #26 · CI (ci.yml) path filter: its own file, not all of .github/workflows/** (amends the PR #12 CI paths entry)
- Rejected: keeping '.github/workflows/**' (every deploy.yml-only change ran the Go suite and its flaky Stage 4 seeds, as on this PR).
- Reason: a change to another workflow says nothing about Go code; ci.yml still re-runs when ci.yml itself changes, and weekly and by hand as before.
- Reverse: restore '.github/workflows/**' in both paths lists of .github/workflows/ci.yml.

## 2026-10-13 · PR #27 · Stage 4 race boot: GOMAXPROCS = max(NumCPU, 4) via bwrap --setenv (amends the PR #25 Stage 4 load entry; answer packet O2)
- Rejected: O1 HTTP keep-alives (the answer packet's measurements did not show them closing the miss); O3 re-seeding the racy tests (it hides in-slot races instead of catching them); O4 more load; O5 accepting the flake.
- Reason: on a 2-CPU host gen 0 had 2 slots, so the seeded in-slot races needed two holders at once and were missed (CI run 38318638860). N is unchanged (2 x NumCPU in [8, 64]), still at least twice gen 0's slots up to 32 CPUs: 1-4 CPUs give N 8 and 4 slots, 8 CPUs N 16 and 8 slots. The probe gets the same env.
- Reverse: drop GOMAXPROCS from raceEnv in gate/stage4.go; DESIGN.md sec 7 Stage 4 edit.

## 2026-10-13 · PR #27 · Known gap: in-slot races need concurrent holders
- Decision: (a) the race detector orders every operation on a chan struct{} semaphore through one sync object, so one holder's release happens-before the next's acquire and an in-slot race is seen only between holders running at once; (b) a wrapper whose capacity is not derived from GOMAXPROCS (e.g. memory-limit sizing, 0 in the bare boot today) can still have 1 slot in the gate; (c) mitigated, not closed, by the GOMAXPROCS floor above.
- Rejected: changing gen 0 to chan byte (per-slot sync objects).
- Reason: mutants control the wrapper, so gen 0's semaphore type binds nothing.
- Reverse: n/a (a limit); revisit with the EK8S_*_LIMIT bare-boot item (docs/STATUS.md Parked).

## 2026-10-13 · PR #28 · Variant bundle: go:embed templates rendered by package deploy/variant, values checked against fixed shapes
- Decision: six *.yaml.tmpl in file-name order, text/template with missingkey=error; id canonical decimal (no leading zeros, at most 18 digits, so 7 and 07 cannot name two namespaces), label ^ek8s-[0-9a-f]{16}$, image ^ek8s-wrapper:[0-9a-f]{12}$; golden files; a CLI (cmd/render) for CI and hand runs.
- Rejected: kustomize overlays (need a per-variant directory or kubectl edits at run time); escaping values instead of rejecting them.
- Reason: Stage 5 and the orchestrator get one stdlib entry point; a value of a fixed shape cannot add YAML structure.
- Reverse: delete deploy/variant/ and apply hand-written manifests.

## 2026-10-13 · PR #28 · Variant pod spec details (design packet Q4)
- Decision: pod label variant=<label> read into EK8S_VARIANT by fieldRef; EK8S_CPU_LIMIT_MILLIS / EK8S_MEM_LIMIT_BYTES by resourceFieldRef (divisor 1m / 1); resources only from the LimitRange (min/max bound stated values); readiness /readyz every 2s (timeout 1s, 2 failures), liveness /healthz (cmd/service, not the genome) every 10s (timeout 5s, 3 failures); Service port 8080.
- Rejected: resources in the Deployment too (two places to keep equal); liveness on /readyz (a shedding wrapper would be restarted).
- Reason: readiness reacts within ~4s, under cmd/service's 5s drain; liveness tolerates a slow /healthz under a 1-CPU limit with hashes in flight. All starting values (DESIGN.md sec 15).
- Reverse: edit deploy/variant/40-deployment.yaml.tmpl and 20-limitrange.yaml.tmpl, then go test -update.

## 2026-10-13 · PR #28 · Variant smoke test: deploy/variant-smoke.sh in deploy.yml, curlimages/curl 8.22.0 by digest, positive controls
- Decision: fixed id 999999; ek8s-system created (PSA restricted) if absent; (c) removes app=loadgen from (b)'s pod and allows 30s for the policy to follow; curl pods run as 65532 with sleep and kubectl exec; a refusal is curl exit 7 or 28 only, and every egress target (1.1.1.1:80, the kubernetes ClusterIP:443, its endpoint) is first reached from ek8s-system; the image is curlimages/curl 8.22.0 (the curl project's own image, newest tag, published 2026-09-02) by its linux/amd64 manifest digest sha256:43366cd60f226c7655181a0f7e85c468a41d182fdd2dc2c1c3b872a2b9d05d7a (index sha256:58adaa4e...; both read from Docker Hub's registry API), kind-loaded as ek8s-curl:43366cd60f22.
- Rejected: the multi-arch index digest (owner review: kind load of a partly pulled index can fail; stress-ng's pin is an OCI index and has loaded fine so far); busybox wget (no exit code that tells a refusal from an HTTP error); targets by DNS name (DNS egress is denied too, so a failed lookup proves nothing).
- Reason: each check fails the job unless its claim holds, and the controls keep a broken network from passing as "denied".
- Reverse: drop the variant-smoke.sh step from deploy.yml.

## 2026-10-13 · PR #28 · Variant smoke runs in deploy.yml on pull requests, not a workflow_dispatch-only workflow (amends design packet Q8 B)
- Rejected: Q8 B's separate dispatch-only workflow (as STATUS.md Next had it).
- Reason: the owner's PR (2) task prompt put it in deploy.yml; the cluster is already up there, and the check that proves egress denial reruns on every change that could break it (deploy/**, cmd/service/**, internal/wrapper/**, deploy.yml).
- Reverse: move the variant-smoke.sh step to a workflow with only workflow_dispatch.

## 2026-10-13 · PR #28 · kindnet enforces egress: Q5's open condition met (DESIGN.md sec 11)
- Decision: treat egress denial as verified; DESIGN.md sec 11 says so. Evidence: deploy smoke run 38356331140, check (d): 1.1.1.1:80, 10.96.0.1:443 (API ClusterIP) and 172.18.0.2:6443 (node, API endpoint) each answered from ek8s-system and were refused from ek8s-v999999, each after the full 5s curl timeout (the ok lines are 5.3s apart), so packets were dropped.
- Rejected: Calico/Cilium (not needed now).
- Reason: the PR #26 Q5 entry made this run the proof; the smoke step keeps proving it on every deploy/ change.
- Reverse: n/a (a finding); if a later kind release stops enforcing egress, the smoke run fails and Q5 reopens.

## 2026-10-13 · PR #28 · Pod names: the gate draws hasher-<hash>-<5> as Kubernetes does; smoke check (a) accepts a 1-10 character hash
- Decision: newPodName's middle part is a random uint32's decimal digits through SafeEncodeString (k8s.io/apimachinery/pkg/util/rand alphanums, checked against its source; ComputeHash in pkg/controller), the suffix 5 uniform characters (rejection sampling, as rand.String is uniform).
- Rejected: a fixed 10 characters (a Deployment pod's hash is 10 characters for only ~77% of templates, so a short hostname proved "cluster", and check (a) would fail ~23% of runs).
- Reason: closes the pod-name tell instead of parking it (owner review).
- Reverse: restore the 15-byte draw in gate/boot.go newPodName; {1,10} -> {10} in variant-smoke.sh.

## 2026-10-13 · PR #29 · Loadgen is one long-lived Deployment in ek8s-system; salts restored after a restart (design packet Q1 A+(i); DESIGN.md sec 10)
- Decision: the orchestrator PUTs each live variant's salts back on a new boot_id (window excluded for all variants; sends to a variant blocked until restored); loadgen also journals salts to an emptyDir.
- Rejected: a Job per observation window (Run state persisted and restored every window, scheduling jitter at window start, two pods' requests at once during a burst); per-variant loadgen pods (4-5 Guaranteed pods do not fit the sec 2 budget, no pair sharing).
- Reason: a restart would empty every Run, so a mutant could replay hashes from before it; the journal covers a container restart, the PUT a pod recreation.
- Reverse: DESIGN.md sec 10 edit; drop the control API's restore call.

## 2026-10-13 · PR #29 · A variant's classifier Run starts at its Stage 5 burst, not at promotion (design packet O2; amends the PR #15 salt-reuse entry; DESIGN.md sec 7, sec 8)
- Rejected: the PR #15 entry's "Stage 5's burst uses its own Run" (a mutant could replay a burst hash once live).
- Reason: the burst is the variant's first traffic, so salts from it must count; the Run continues through promotion to cull.
- Reverse: DESIGN.md sec 8 edit; start a separate Run at promotion.

## 2026-10-13 · PR #29 · Loadgen control API: separate port, ClusterIP Service, reached via the API-server service proxy; no API credentials (design packet Q2 A; DESIGN.md sec 10)
- Decision: owner approves this new in-cluster listener (CLAUDE.md: ask before exposing network services). Records carry wall-clock time (phase attribution) and monotonic time (lag, latency).
- Rejected: kubectl exec subcommands (exec streams need their own framing and reconnects); a ConfigMap/CR plus pod logs (loadgen needs a token and watch RBAC; log rotation and restarts lose records silently); kubectl port-forward (host listener, SPDY tunnel drops under load); a host port (sec 10: none published); a ServiceAccount for loadgen.
- Reason: the gate and orchestrator stay on the host and already reach the cluster through the API server.
- Reverse: DESIGN.md sec 10 edit.

## 2026-10-13 · PR #29 · Loadgen sizing: Guaranteed 750m / 256Mi, shared pool (design packet Q3 A; DESIGN.md sec 10)
- Decision: Argon2 FullVerify runs off the send path, token-bucket capped; the static CPU manager only if the workstation run excludes more than about 5% of halves for lag.
- Rejected: the static CPU manager up front.
- Reason: about 0.95 CPU of requests is left after control plane, metrics-server, stress-ng 3 and 8 variant pods at 250m (estimate; the workstation run measures it). The static CPU manager also makes stress-ng's integer 3 CPUs exclusive, so it would stop time-sharing with variants unless moved to 2900m (reopens the PR #26 Q7 entry), and 1 more exclusive CPU exceeds 7 allocatable.
- Reverse: DESIGN.md sec 10 edit; edit the loadgen Deployment's resources.

## 2026-10-13 · PR #29 · Global exclusion on loadgen lag (design packet Q4 A; DESIGN.md sec 10, sec 11, sec 15)
- Decision: a half is excluded for all variants on lag p99 > 10 ms, any lag > 100 ms, a skipped send or a ring gap (starting guesses); after 3 consecutive excluded halves the orchestrator flags and halts.
- Rejected: CFS throttling as a trigger (a diagnostic only: throttling that delays no send is harmless); per-variant exclusion (breaks probation's same-window comparison, sec 11); measuring latency from intended send time (charges loadgen's own stalls to the variant).
- Reason: loadgen is one shared process, so a stall hits every stream equally.
- Reverse: DESIGN.md sec 10 edit; change the thresholds in the orchestrator.

## 2026-10-13 · PR #29 · Loadgen draws every verify pair fresh; the pool is gate-only; contract negatives are in live traffic (design packet Q5 C; DESIGN.md sec 5, 7, 8, 10)
- Decision: match = hashcore.Hash of a fresh password (each pair once per variant); mismatch = random canonical PHC at exact params + fresh password; Run.Reserve on every hash sent to /verify; /hash passwords crypto/rand, 1-1024 bytes; mix 50/48/2; negatives (both classes) in sustained traffic and >= 5% of the burst.
- Rejected: pool pairs in live traffic (memoizable, replayable); Stage 2 as the only negative check (a gate-detecting mutant fails open outside it); pairs from the variant's own /hash output (the STATUS Parked suggestion): the mutant produced h for p, so it can cache h -> p at hash time and answer without Argon2.
- Reason: closes the Parked verify-caching, pool-replay and fail-open items. Negatives pass as 400 or graceful_shed, fail as negative_accepted on any 2xx, and are outside success_rate, p95 and cpu_per_success.
- Reverse: DESIGN.md sec 5/7/8/10 edit; restore the pool import in loadgen.

## 2026-10-13 · PR #29 · The first confirmed integrity failure disqualifies a variant (design packet Q6 A; DESIGN.md sec 9, sec 11)
- Decision: re-checked on the host with hashcore; cull at once, never a parent; malformed_body counts only when the body was read to a clean EOF.
- Rejected: scoring an integrity failure as one failed request; a threshold of k failures or a rate (tolerates cheating at a low rate; no benign source to tolerate).
- Reason: a mutant can relabel undelivered Argon2 output as later hashes; sampled FullVerify catches it rarely, so one catch is enough evidence.
- Reverse: DESIGN.md sec 9 edit; fold the classes into `failed`.

## 2026-10-13 · PR #29 · Readiness probe timeoutSeconds 2, failureThreshold 3, period 2s; probe Events are diagnostics only (design packet Q7 B; amends the PR #28 pod-spec entry; DESIGN.md sec 11)
- Decision: the template change lands in the deploy PR; probe Events never exclude a window. The PR #28 reason "readiness reacts within ~4s, under cmd/service's 5s drain" no longer holds: 3 x 2s is about 6s, past the drain. Caveat: removal of a terminating pod then relies on pod deletion dropping it from endpoints (Kubernetes behaviour, not yet checked; the deploy PR should check it).
- Rejected: exclusion on probe-timeout Events (gameable: /readyz is evolvable, so a genome could turn bad windows into excluded ones); readiness on /healthz (removes the genome's readiness lever); timeout 1s / threshold 2.
- Reason: under stress-ng and a 1000m CFS quota a healthy /readyz can exceed 1s, pulling a pod for reasons the genome does not control. Gen 0's /readyz is constant, so the workstation acceptance check is zero readiness failures for gen 0 at 4 x 2 pods, stress on, rate r, over >= 3 windows.
- Reverse: edit 40-deployment.yaml.tmpl, then go test -update.

## 2026-10-13 · PR #29 · Stream rate r = 0.5 x C0, frozen per run (design packet Q8 A; DESIGN.md sec 9, sec 10)
- Decision: C0 = highest rate at which gen 0 on a single pod (HPA min = max = 1, no stress) sheds <= 1% with p95 <= HangTimeout/4; a new r means a new run (one run = one comparability epoch; archived scores compare only within their run; a later run may seed from archived genomes, not their scores); provisional r from a workstation Argon2 benchmark; CI smoke r = 2.
- Rejected: changing r within a run (fixed-bound scores are comparable only at one r, DESIGN.md sec 9).
- Reason: the factor 0.5 and the shed/p95 bounds are starting guesses (sec 15); the workstation run measures C0.
- Reverse: DESIGN.md sec 10 edit.

## 2026-10-13 · PR #29 · Loadgen disables HTTP keep-alives (design packet O1; DESIGN.md sec 10)
- Rejected: pooled keep-alive connections.
- Reason: kube-proxy balances per connection, so keep-alive pins a stream to one replica and HPA's second pod and readiness changes act only on new connections.
- Reverse: DESIGN.md sec 10 edit; drop DisableKeepAlives from loadgen's transport.

## 2026-10-13 · PR #29 · Variant namespaces get the label ek8s.io/role: variant (design packet O5; DESIGN.md sec 11)
- Decision: lands in the deploy PR's namespace template; selects the class of variant namespaces without carrying the id.
- Rejected: selecting by the ek8s-v name prefix.
- Reason: a NetworkPolicy namespaceSelector matches labels, not name prefixes, so loadgen's egress policy needs a label to select variant namespaces.
- Reverse: DESIGN.md sec 11 edit; drop the label from 10-namespace.yaml.tmpl.

## 2026-10-13 · PR #30 · Classifier Kind Negative: a 503 without one valid Retry-After is bad_status (task prompt; DESIGN.md sec 8)
- Decision: order transport error, any 2xx = negative_accepted (at any latency, see the "Integrity reasons win" entry), over_hang_timeout, 503 + one valid Retry-After = graceful_shed, 400 = success, else bad_status. The body is never read.
- Rejected: reusing bare_503 / bad_retry_after for negatives (the Hash/Verify reasons); the task prompt names bad_status for "anything else".
- Reason: neither reason is an integrity failure, so the split only changes diagnostics.
- Reverse: route a Negative's 503 through the Hash/Verify 503 branch in Classify.

## 2026-10-13 · PR #30 · Run.Reserve returns ErrNotCanonical for a hash it cannot parse, and records nothing (DESIGN.md sec 8)
- Decision: same canonicalSalt check as a hash response; reserving an already-recorded salt is a no-op returning nil.
- Rejected: panic (loadgen reserves contract negatives' hashes too if it reserves every /verify hash); recording the salt of a non-exact-params string (it could never come back as success: non_canonical_phc is checked first).
- Reason: the caller decides whether a non-canonical reserve is a bug (match/mismatch pair) or expected (negative).
- Reverse: change Reserve's signature (callers: loadgen only).

## 2026-10-13 · PR #30 · malformed_body is integrity only via a caller contract on Observation.Body/Err (DESIGN.md sec 9)
- Decision: Observation.Body must be read to a clean EOF; a read error or a size-cap cut sets Err (transport_error). Integrity(reason) then lists malformed_body unconditionally. gate/interop.go's post already does this.
- Rejected: an Observation flag "body complete" (a second way to say what Err already says).
- Reason: the classifier cannot see how the body was read; one rule keeps it the caller's.
- Reverse: add the flag and make Integrity take the Observation.

## 2026-10-13 · PR #30 · Integrity reasons win over the hang-timeout (owner review; amends the PR #15 "HangTimeout bounds every bucket" entry; DESIGN.md sec 8)
- Decision: after a transport error, every 2xx runs the checks that can yield an integrity reason, at any latency; only if none fires does latency > HangTimeout give over_hang_timeout. HangTimeout only decides whether a passing response can be success or graceful_shed. Late hash salts are still recorded; FullVerify still runs on a late hash.
- Rejected: latency first (the PR #15 order, and this PR's first draft).
- Reason: a mutant could fail open, or fake results, only past HangTimeout and never be disqualified; negatives are outside every metric, so a slow fail-open costs nothing.
- Reverse: DESIGN.md sec 8 edit; move the latency check above integrity2xx in Classify.

## 2026-10-13 · PR #30 · Loadgen's HTTP client timeout outlasts cmd/service's WriteTimeout (owner review; reverses the reviewer's note 6 decline; amends the design packet's 2 x HangTimeout client timeout; DESIGN.md sec 10)
- Decision: client timeout > WriteTimeout (3 x HangTimeout, 6s), e.g. WriteTimeout + 1s, so every response the service can ever write is observed and classified.
- Rejected: 2 x HangTimeout (4s).
- Reason: a shorter timeout turns a slow fail-open or faked result, one a real client waiting longer would receive, into a transport_error instead of an integrity failure (DESIGN.md sec 8 "Integrity reasons apply at any latency").
- Reverse: DESIGN.md sec 10 edit.
