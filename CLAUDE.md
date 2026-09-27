# CLAUDE.md

Operational notes for working in this repo. Full design/rationale: `DESIGN.md`.

## Working agreement

Applies to every session in this repo.

- Before every commit: show the full diff (full contents for new files)
  and the commit message; wait for explicit approval. Push only after a
  separate explicit approval.
- Ask before: adding dependencies, changing schemas/data formats,
  exposing network services, security-relevant changes, anything hard to
  reverse, or departing from existing patterns. Anything the task prompt
  explicitly specifies counts as already approved.
- `DESIGN.md` holds settled architecture; `docs/decisions.md` logs
  implementation-level decisions. A decision that changes architecture
  needs a `DESIGN.md` edit (with approval) plus a `docs/decisions.md`
  entry pointing to it.
- Log each non-trivial decision in `docs/decisions.md`: date, PR,
  decision, rejected alternative, reason, how to reverse. Max 5 lines per
  entry.
- End every task with: (a) what changed, (b) assumptions made but not
  verified, (c) files I must understand vs boilerplate, (d) exact
  commands to verify it works, (e) a brief explanation of any Go idiom or
  concurrency pattern used that a non-Go-specialist wouldn't know.
- One logical change per commit; the message explains why, not what.

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
internal/wrapper/   evolvable: concurrency/backpressure/timeouts. The only
                    source the mutation driver copies into its sandbox.
                    hasher.go (fixed core's calling interface) is pinned.
gate/               correctness gate, five ordered stages (DESIGN.md sec 7).
loadgen/            open-loop load generator: per-variant sustained streams +
                    Stage 5 burst mode. Imports internal/classifier.
deploy/             kind cluster config, k8s manifests, stress-ng.
driver/             mutation driver wrapping `claude -p`.
orchestrator/       population manager, selection, scoring loop.
lineage/            SQLite (structured) + JSONL (raw claude -p transcripts).
docs/               decisions.md: implementation-level decision log.
```

## Build / test

- `go build ./...` -- local build.
- `go test ./...` -- unit tests (gate Stage 1's local sub-check; Stage 4's
  `-race` build is a separate binary, never the deployed image).
- `docker build` -- Stage 1's second sub-check (container boots, health
  endpoint responds).
- `kind load docker-image <image> --name <cluster>` -- load a locally built
  image straight into the cluster; no registry in this project.
- Stage 5 (degradation under contention) only runs against the live kind
  cluster, in the candidate's own `ek8s-v<id>` namespace -- see `gate/`
  and DESIGN.md sec 7, sec 11.

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
