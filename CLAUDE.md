# CLAUDE.md

Operational notes for working in this repo. Full design/rationale: `DESIGN.md`.

## Session workflow (standing rule)

Before every commit in this repo, show the exact diff (or full file
contents for new files) and the proposed commit message, and wait for
explicit approval before running `git commit`. Never push without a
separate explicit approval after that. This rule applies to this session
and all future sessions in this repo.

## Repo layout

```
cmd/service/       entrypoint: thin main.go (config/flags, wires hashcore +
                    wrapper, HTTP routes, healthz/readyz, graceful shutdown).
                    Never touched by mutation.
internal/hashcore/  fixed core (Argon2id hash/verify). Never in mutation
                    scope. Implementation must never be present in the
                    directory tree a mutation session can read or edit.
internal/wrapper/   evolvable: concurrency/backpressure/timeouts. The only
                    directory the mutation driver's --add-dir may point at.
gate/               correctness gate, five ordered stages (DESIGN.md sec 7).
deploy/             kind cluster config, k8s manifests, stress-ng.
driver/             mutation driver wrapping `claude -p`.
orchestrator/       population manager, selection, scoring loop.
lineage/            SQLite (structured) + JSONL (raw claude -p transcripts).
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
  cluster in a scratch namespace -- see `gate/` and DESIGN.md sec 7.

## Non-negotiable constraints

These hold every session, regardless of what else changes:

1. **`internal/hashcore/` is never touched by mutation.** Its implementation
   is structurally excluded from the mutation driver's accessible tree --
   not just tool-permission-restricted. The wrapper only ever sees the
   fixed core's calling interface (signatures + doc comments), checked into
   the accessible tree as its own file. See DESIGN.md sec 4.

2. **Cost-parameter floor is a floor, never lowered.** Argon2id:
   `t >= 2` (iterations), `m >= 19456` KiB / 19 MiB (memory), `p >= 1`
   (parallelism) -- OWASP Password Storage Cheat Sheet minimum, verified
   current as of 2026. A mutation may legally increase cost, never decrease
   it below this floor. Enforced by gate Stage 3. See DESIGN.md sec 6.

3. **Mutation driver invocation -- use `--tools`, not
   `--allowedTools`/`--disallowedTools`.** The latter only govern what runs
   without a permission prompt; they are additive on top of Claude Code's
   default tool set and do not restrict availability. `--tools` is what
   actually restricts which built-in tools exist for the session (e.g.
   `--tools "Read,Edit"`). Always pair with `--restricted` (strips
   command/code-execution tools and WebFetch unless named individually) and
   `--add-dir` scoped to `internal/wrapper/` only:

```
   claude -p --tools "Read,Edit" --restricted --add-dir internal/wrapper \
     --model <pinned-model> --permission-prompts none --max-turns <N> \
     --output-format json "<constructed prompt>"
```

   See DESIGN.md sec 12 for the full rationale and per-call context this
   invocation depends on.
