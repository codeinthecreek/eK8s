# eK8s

Evolution of a Kubernetes service, with an AI as the mutation
operator. Each generation, Claude (run via `claude -p` in a sandbox)
edits the concurrency and load-shedding layer of a small Argon2id
password-hashing service. A strict correctness gate discards broken or
insecure variants. Survivors run side by side on a single-node kind
cluster under real load and deliberate resource contention, and the
weakest is culled. The hashing core itself is locked: evolution can
change how the service copes with overload, never how securely it
hashes.

Everything runs on one Linux workstation. There is no cloud, GPU or
container registry.

**Status:** early. The generation-0 service, the locked hashing core,
the shared response classifier, the test-vector pool and gate Stages
1-3 are built and tested in CI. The rest of the evolution machinery
(gate Stages 4-5, load generator, mutation driver, orchestrator) is
not built yet. For the current state, see
[docs/STATUS.md](docs/STATUS.md).

## The loop

```
  select parent (archive) -> mutate (claude -p) -> build
        ^                                            |
        |                                            v
  score & cull <- observe under load <- go live <- gate
                                                 (fail: discard)
```

## Where to read next

| Document | What it is |
|----------|------------|
| [docs/overview.md](docs/overview.md) | Plain-language introduction, with diagrams. Start here. |
| [DESIGN.md](DESIGN.md) | The settled architecture. Authoritative. |
| [docs/decisions.md](docs/decisions.md) | Why each implementation choice was made. |
| [docs/STATUS.md](docs/STATUS.md) | Where the work is right now. |
| [CLAUDE.md](CLAUDE.md) | Working agreement and non-negotiables for Claude Code sessions. |

## Repo layout

```
cmd/service/          fixed entrypoint: routes, health, shutdown
internal/hashcore/    fixed Argon2id core; never visible to mutation
internal/classifier/  shared success/shed/failed judge + time ceiling
  testvectors/        known password/hash pairs (gate, loadgen only)
internal/wrapper/     the evolvable layer: concurrency, backpressure
gate/                 five-stage correctness gate
loadgen/              open-loop load generator (planned)
deploy/               kind config, manifests, stress-ng (planned)
driver/               sandboxed claude -p mutation driver (planned)
orchestrator/         population, selection, scoring loop (planned)
lineage/              SQLite records + raw transcripts (planned)
docs/                 overview, decisions, status, handoff formats
```

## Build and test

Requires Go 1.26 or later (see `go.mod`).

```
go build ./...
```

```
go vet ./...
```

```
go test ./...
```

```
go test -race ./...
```

CI (`.github/workflows/ci.yml`) also runs `govulncheck`.

## Non-negotiables

- The hashing core (`internal/hashcore/`) is never touched by mutation
  and never visible to it.
- Argon2id cost parameters are fixed at exactly t=2, m=19456 KiB, p=1.
- The evolvable wrapper may not import the core or any crypto
  implementation.

Details and enforcement: [CLAUDE.md](CLAUDE.md), [DESIGN.md](DESIGN.md)
sec 4, 6 and 7.
