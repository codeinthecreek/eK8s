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

## Result so far

Four evolutionary runs and two capacity measurements are done. A capacity
measurement ("c0", tool `orchestrator/cmd/c0`) finds the highest request rate
one pod serves within the shed and latency limits. One pod running the fixed
Argon2id core serves about 32 successes/s whatever the wrapper around it does,
so evolution found no extra throughput. It found a shorter wait for a hashing
slot (in run 4, 500 ms down to 67 ms), which cut the stress-half p95 latency
about 3 times at the same throughput. The evidence and the limits of these
claims are in [docs/RESULTS.md](docs/RESULTS.md).

## The loop

```
  select parent (archive) -> mutate (claude -p) -> build
        ^                                            |
        |                                            v
  score & cull <- observe under load <- go live <- gate
                                                 (fail: discard)
```

## Where to read next

Suggested reading order: this page, then [docs/RESULTS.md](docs/RESULTS.md)
(what was found), then [docs/overview.md](docs/overview.md) (how it
works), then [deploy/README.md](deploy/README.md) and the commands below
(how to run it). Read [DESIGN.md](DESIGN.md) when you need the detail.

| Document | What it is |
|----------|------------|
| [docs/RESULTS.md](docs/RESULTS.md) | What runs 1-4 and the capacity (c0) measurements showed. Read first. |
| [docs/overview.md](docs/overview.md) | Plain-language explanation of how it works, with diagrams. Read after RESULTS.md. |
| [DESIGN.md](DESIGN.md) | The settled architecture. Authoritative. |
| [deploy/README.md](deploy/README.md) | The cluster, host setup, smoke tests, the c0 tool. |
| [docs/decisions.md](docs/decisions.md) | Why each implementation choice was made. |
| [docs/STATUS.md](docs/STATUS.md) | Where the work stopped. |
| [docs/AFTER-FIRST-RUN.md](docs/AFTER-FIRST-RUN.md) | The open backlog: deferred and unfinished items. |
| [CLAUDE.md](CLAUDE.md) | Working agreement and non-negotiables for Claude Code sessions. |

## How this was built

The owner planned and reviewed the work in a planning chat, while Claude
Code sessions implemented it in this repo. The method is in
[docs/workflow/WORKFLOW.md](docs/workflow/WORKFLOW.md), which also lists how
it changed and holds a retrospective. Each design and implementation choice,
with who made it, is in [docs/decisions.md](docs/decisions.md).

**Process terms.** The docs use some terms from how this repo was
built. The *owner* is the repo's maintainer. The *planning chat* is the
separate chat where the owner planned and reviewed work. A *Code session*
is one Claude Code session working in this repo. A *packet* is a
fixed-format report a Code session hands back for the planning chat
(formats in `docs/handoff.md`). "*(inferred)*" marks a statement made
without checking it against the tree. The method is described in
[docs/workflow/](docs/workflow/).

## Repo layout

```
cmd/service/          fixed entrypoint: routes, health, shutdown
internal/hashcore/    fixed Argon2id core; never visible to mutation
internal/classifier/  shared success/shed/failed judge + time ceiling
  testvectors/        known password/hash pairs (gate only)
internal/hostpairs/   host-side /verify pairs and full verification
internal/hostclock/   host CPU clock state; c0 and the orchestrator
                      refuse unpinned clocks
internal/stallrun/    stall-run detector shared by the gate and the orchestrator
internal/wrapper/     the evolvable layer: concurrency, backpressure
gate/                 five-stage correctness gate
fitness/              per-window scoring from loadgen's records
loadgen/              open-loop load generator
deploy/               kind config, manifests, stress-ng
driver/               sandboxed claude -p mutation driver
orchestrator/         population, selection, scoring loop
lineage/              run directory: genome archive, JSONL attempt
                      records, raw transcripts (JSONL; SQLite was
                      not built)
docs/                 overview, results, decisions, status, backlog,
                      handoff formats, workflow/ (project-independent
                      method)
```

## Build and test

Requires Go 1.26 or later (see `go.mod`). These are the commands CI
(`.github/workflows/ci.yml`) runs; CI also runs `govulncheck`. The gate
and driver tests skip without `bwrap` and `nsenter`; the gate also needs
gcc, a static libc and docker, and `go mod download` once. Setup is in
[deploy/README.md](deploy/README.md) ("Host setup") and `CLAUDE.md`
("Build / test").

```
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

Known toolchain issue: on go1.27.x (seen on Arch Linux's go1.27.3), the
driver's `TestSandboxTree` fails, because the toolchain's vendored
x/crypto (TLS ciphers, no argon2) is visible in the sandbox. It passes on
CI's Go 1.26.10. The fix was never made.

## Bring up the cluster

Needs docker, kind and kubectl; versions and checks are in
[deploy/README.md](deploy/README.md). The deploy smoke workflow
(`.github/workflows/deploy.yml`, run by hand) runs these from the repo
root:

```
deploy/up.sh
deploy/smoke.sh
```

`deploy/up.sh` creates the kind cluster (name `ek8s`) if it is absent and
applies the manifests; `deploy/down.sh` deletes it. An evolutionary run
also needs the load generator (`deploy/loadgen-up.sh <r>`) and a
workstation with pinned CPU clocks. It is started with:

```
go run ./orchestrator/cmd/orchestrator -run DIR -rate R -claude PATH -claude-version V
```

`-run` is a new run directory (it must not exist). `-rate` is the request
rate per variant, r, as deployed by `loadgen-up.sh`; it is set from the
c0 measurement (runs 3 and 4 used r = C0 = 31). `-claude` and
`-claude-version` are the `claude` binary and the version it must report.

Launch it from a terminal where `CLAUDE_CODE_OAUTH_TOKEN` is exported
(workstation only; see the command's doc comment). See also
`deploy/README.md`. To repeat the capacity measurement, see its "C0"
section.

## Non-negotiables

- The hashing core (`internal/hashcore/`) is never touched by mutation
  and never visible to it.
- Argon2id cost parameters are fixed at exactly t=2, m=19456 KiB, p=1.
- The evolvable wrapper may not import the core or any crypto
  implementation.

Details and enforcement: [CLAUDE.md](CLAUDE.md), [DESIGN.md](DESIGN.md)
sec 4, 6 and 7.
