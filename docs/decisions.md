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
