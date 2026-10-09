# eK8s: Evolutionary Kubernetes Credential-Hashing Service

**Status:** design settled, v1 scope. Ready for implementation.

## 1. Motivation and background

This project brings self-organizing-systems / evolvable-hardware interests
(morphogenetic evolvable hardware, variable-length chromosomes, growth
coupled to real gate-level resource state) forward to current technology.
It applies AlphaEvolve's *spirit* -- LLM-guided mutation as a
semantically-aware replacement for blind random mutation -- to a Kubernetes
substrate, using no specialized hardware (FPGA/CGRA/wafer-scale). Resourcing
is a single Linux workstation.

**Accepted v1 limitation:** genome space is a bounded engineering-parameter
space (concurrency/backpressure tuning), not open-ended algorithmic search.
Convergence within a handful of generations followed by a plateau is the
expected and acceptable outcome -- this proves the loop works, it does not
target open-ended novelty.

## 2. Hardware target

Single Linux workstation: 8 physical CPU cores, 16 GB RAM, 4 GB Nvidia GPU.

The GPU is not used anywhere in this design. Argon2's memory-hardness is
specifically intended to blunt GPU/ASIC cracking, so an idle GPU next to the
hashing endpoint is the algorithm working as intended, not wasted potential.

kind's control-plane overhead (etcd, apiserver, controller-manager,
scheduler, kubelet, containerd, kube-proxy, CoreDNS) on a single-node
cluster costs roughly 1 core and 1.5-2 GB baseline. Budget the rest
(~7 cores / ~13-14 GB) across: orchestrator process, load generator,
stress-ng, metrics-server, and the live variant population.

## 3. Architecture summary

- **Substrate:** Kubernetes, single-node kind cluster.
- **Genome:** source of a small Go service, packaged as a container image.
  No registry -- `kind load docker-image` loads locally-built images
  straight into the cluster's containerd, skipping push/auth/registry
  lifecycle entirely for this PoC.
- **Mutation operator:** `claude -p` (Claude Code CLI), driven by a Claude
  Pro subscription (not metered API billing).
- **Selection:** hybrid. An explicit correctness gate kills non-viable
  mutants before they reach a live Deployment. kube-proxy does not split
  traffic across variants. Kubernetes' own mechanisms (readiness probes,
  HPA-style scaling) still act natively, but *within* a variant, across
  its own replicas. Ranking *between* viable variants comes only from
  explicit fitness scoring (section 9) on per-variant load streams
  (sections 10, 11).
- **Fitness signal:** real cluster telemetry (latency/CPU/memory via
  metrics-server), not a synthetic benchmark.
- **Resilience angle:** a `stress-ng` noisy-neighbour pod creates resource
  contention, standing in for adversarial/DDoS-style pressure without
  needing real attack traffic infrastructure.

### Target task

A password/credential hashing endpoint (Argon2id).

- **Fixed core (never mutated):** the hash algorithm call, cost-parameter
  floor, and the comparison/verify call (library's own constant-time
  compare). Isolated in its own package, structurally excluded from what
  the mutation operator can see or edit. Exists to prevent evolution from
  "optimizing" toward weaker security (faster-but-insecure hashing, or a
  reintroduced timing side-channel via naive comparison).
- **Evolvable wrapper:** concurrency model, request handling,
  backpressure/load-shedding strategy, timeout/queue/worker tuning --
  everything around the fixed core, nothing inside it.

## 4. Fixed-core isolation -- enforcement mechanism

Three independent layers, not one:

1. **Tool-availability restriction on the mutation driver** (see section 12)
   -- necessary but not sufficient; a permission-prompt mechanism is not a
   filesystem boundary.
2. **Structural isolation (the real boundary):** the fixed-core package's
   implementation is never present in the directory tree the mutation
   session can read or edit. The wrapper is given only the fixed core's
   calling interface (signatures + doc comments, checked into the
   accessible tree as a real Go file) -- enough to call it correctly,
   nothing that reveals or allows editing the implementation. The
   correctness gate's build step assembles fixed-core-impl + evolved-wrapper
   before compiling. Even a tool-permission bug cannot expose the
   implementation, because the bytes are not in the accessible tree. The
   driver also runs `claude -p` from a per-run scratch directory outside
   the repo, holding copies of the wrapper's files only (section 12) --
   the repo checkout is never the session's working tree.
3. **OS sandbox:** the `claude -p` process runs inside bubblewrap (section
   12), so what it can reach -- filesystem, environment, namespaces -- is
   bounded by the kernel, not by Claude Code's own tool logic.
   `--restricted` is explicitly *not* an OS sandbox.

**Hiding the bytes does not prevent imports.** Go's `internal/` rule lets
`internal/wrapper` import `internal/hashcore` directly, and
`golang.org/x/crypto/argon2` is already in `go.sum` via
`alexedwards/argon2id` (section 6). A faithful reimplementation at the
same parameters would produce interoperable hashes and pass Stage 2
cleanly -- Stage 2 only catches an *unfaithful* one.

**Backstop, not primary defense:** the Stage 1 import allowlist (section
7), not Stage 2, rejects a wrapper that imports the fixed core or any
Argon2/crypto implementation package, before anything is built. This is a
safety net for something the design is meant to prevent outright, not the
mechanism relied on to prevent it.

## 5. Repo layout

```
repo/
  cmd/service/           # entrypoint: thin main.go -- wires fixed core +
                          # wrapper, HTTP routes, healthz/readyz, graceful
                          # shutdown. Fixed at generation 0 (see below).
                          # Never touched by mutation.
  internal/hashcore/     # fixed core. Never in mutation scope.
  internal/wrapper/      # evolvable: concurrency/backpressure/timeouts.
                          # hasher.go (fixed core's calling interface) is
                          # pinned -- see below.
  internal/classifier/   # shared three-bucket response classifier (sec 8)
                          # + the shared hang-timeout/latency ceiling
                          # constant. Imported read-only by wrapper, gate,
                          # loadgen, and orchestrator. Never in mutation
                          # scope.
    testvectors/         # the shared test-vector pool (precomputed,
                          # embedded). Imported by gate and loadgen only;
                          # never by wrapper.
  gate/                  # correctness gate, five ordered stages.
  loadgen/               # open-loop load generator (sec 10): per-variant
                          # sustained streams + Stage 5 burst mode.
                          # Imports internal/classifier.
  deploy/                # kind cluster config, k8s manifests, stress-ng.
  driver/                # mutation driver wrapping claude -p.
  orchestrator/          # population manager, selection, scoring loop.
  lineage/               # SQLite (structured record) + JSONL (raw
                          # claude -p transcripts).
```

`cmd/service/` is intentionally boring: keeping wiring logic out of it is
what allows the mutation driver's `--add-dir` scope to include only
`internal/wrapper/` without losing the ability to evolve anything that
matters.

internal/classifier/ exists because sections 8 and 9 require gate Stage 5
and the fitness function to share one definition of success/graceful_shed
/failed and one hang-timeout ceiling constant -- neither can live inside
internal/wrapper/ (evolvable, so untrustworthy as a source of a stable
threshold) or internal/hashcore/ (unrelated). internal/wrapper/ imports it
read-only, to enforce the same ceiling in its own request-timeout
behavior; it is never copied into the mutation driver's sandbox (section
12), and the mutation driver never sees or edits it.

The shared test-vector pool (sections 7 and 10) lives in the
internal/classifier/testvectors/ subpackage, as precomputed data
generated once by a small tool that uses internal/hashcore/.
internal/wrapper/ never imports the subpackage (the Stage 1 allowlist,
section 7, forbids it), and internal/classifier/ itself never imports
internal/hashcore/. The pool also holds a small gate-only negative set,
not used by loadgen: well-formed PHC strings at non-exact parameters
(section 7, Stage 2 contract conformance), generated with the reference
library -- except very-large-`m` entries, which are hand-constructed,
since computing one would allocate that memory. A hand-constructed entry
differs from a valid hash only in the parameter under test: the
`$argon2id$v=19$` prefix, the other parameters at their exact values, a
16-byte salt and a 32-byte key, both in the library's own encoding
(unpadded standard base64). Otherwise a rejection could come from the
syntax check rather than the parameter check.

**The fixed core's calling interface is `internal/wrapper/hasher.go`**
(the "real Go file" of section 4):

```go
type Hasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) (bool, error)
}
```

It is pinned: gate Stage 1 requires it byte-identical to the canonical
version (section 7), so a mutation may read it but any edit to it fails
the gate. The canonical version is a SHA-256 constant in gate/, kept equal
to this file by a gate test, so changing it takes a deliberate gate edit.

**`cmd/service/` responsibilities -- all fixed at generation 0**, because
`cmd/service/` can never change later:
- Wires `hashcore.New()` into `wrapper.New(hasher, env)`, where `env`
  carries only environmental facts: the pod's CPU and memory limits, read
  from the downward API. All evolvable tunables live as literals in the
  wrapper source (the genome), not as flags.
- Mounts the wrapper's `HashHandler`, `VerifyHandler`, and `ReadyHandler`.
  `/readyz` returns 503 once SIGTERM is received, regardless of the
  wrapper.
- Sets `GOMEMLIMIT` from the downward-API memory limit (~90%). CPU needs
  nothing: Go >= 1.25 sets `GOMAXPROCS` from the cgroup CPU limit.
- Sets an `X-EK8s-Variant` response header from the variant label (via
  the downward API), in middleware that wraps the `ResponseWriter` so the
  handler cannot overwrite it. Load-bearing in the gate only: Stages 2-4
  require it on every response, equal to the per-run label, so a stale
  listener on :8080 cannot answer for the candidate (section 7). A mutant
  that writes through the writer's `Unwrap` and drops it fails the gate.

## 6. Fixed-core parameters

Argon2id, OWASP Password Storage Cheat Sheet floor (verified current as of
2026): **t=2 (iterations), m=19456 KiB / 19 MiB (memory), p=1
(parallelism)**.

internal/hashcore/ uses `alexedwards/argon2id` with explicit parameters:

```go
argon2id.Params{Memory: 19456, Iterations: 2, Parallelism: 1,
	SaltLength: 16, KeyLength: 32}
```

Never `argon2id.DefaultParams` -- its `Iterations` is 1, below the floor.

**Verify accepts only hashes whose encoded parameters exactly equal
these; anything else is rejected.** The library takes parameters from the
hash string itself, so this blocks both downgrade (weak hashes accepted as
valid) and memory exhaustion (a hash claiming a huge `m`). Stage 3
correspondingly asserts exact equality, not `>=` floor.

## 7. Correctness gate

Five ordered stages, each zero-tolerance (a single failure at any stage
fails the gate outright -- no partial-credit / statistical acceptance
sampling anywhere in the gate). A shared three-bucket response classifier
(section 8) is used by both Stage 5 and the fitness function, so "what
counts as graceful degradation" cannot silently diverge between the two.

### Stage 1 -- Build / boot (local)

The gate assembles a fresh build tree outside the repo (section 4): the
repo's go.mod, go.sum, `cmd/service/`, `internal/hashcore/` and
`internal/classifier/` (non-test files, top level only, so never the
test-vector pool), plus the candidate's files as `internal/wrapper/`. Every
go command on it runs with an environment built from scratch: only `PATH`
and the module and build caches (resolved once from the operator's
`go env`) are carried over; the rest is fixed (`GOENV=off`,
`GOFLAGS=-mod=readonly`, `GOPROXY=off`, `GOWORK=off`, `GOTOOLCHAIN=local`,
`GOOS=linux`, `GOARCH` of the gate host, `CGO_ENABLED` per build). So the
module cache must already be populated (`go mod download` in the repo).
That one operator `go env` runs under the operator's `GOTOOLCHAIN`: on a
host whose go is older than go.mod's `toolchain` line (the go directive if
there is none), the first run may download that toolchain
(checksum-verified) before any check. A resolved go still older than it
is a gate fault: the deployed binary never gets an older stdlib.

Pre-build checks first, zero-tolerance, each logged distinctly from the
build/boot sub-checks below:
1. The candidate is exactly `doc.go`, `hasher.go`, `server.go`,
   `tunables.go`: regular files (no symlinks, checked without following
   them), each under a size cap; anything else, `_test.go` included,
   fails.
2. `internal/wrapper/hasher.go` is byte-identical to the canonical
   version (section 5).
3. No build-constraint line (`//go:build` or `// +build`, also after a
   leading BOM) anywhere in the candidate, so no file can hide its
   imports from one build configuration; check 5 backstops it.
4. Import allowlist for `internal/wrapper`, checked via `go list` on
   direct imports only, under both the deployed build (`CGO_ENABLED=0`)
   and Stage 4's race build (`CGO_ENABLED=1 -race`). Exactly these,
   matched exactly, not as a prefix: `bytes`, `container/heap`,
   `container/list`, `context`, `encoding/json`, `errors`, `fmt`, `io`,
   `log/slog`, `maps`, `math`, `math/rand/v2`, `net/http`, `runtime`,
   `runtime/debug` (legitimate tuning), `runtime/metrics`, `slices`,
   `sort`, `strconv`, `strings`, `sync`, `sync/atomic`, `time`,
   `unicode/utf8`, `internal/classifier`. So its subpackages are
   forbidden: a wrapper importing the test-vector pool
   (`internal/classifier/testvectors`, section 5) could answer `/verify`
   by table lookup, skipping Argon2. Also outside it, among others:
   `internal/hashcore`, `golang.org/x/crypto/...`,
   `github.com/alexedwards/argon2id`, `crypto/...`, `os`. Direct-only is
   sound because `unsafe` (needed for `//go:linkname`), cgo (`"C"`),
   `os/exec`, `syscall` and `plugin` are all outside it; a -deps check
   would reject every wrapper, since `net/http` itself reaches crypto.
   This is the backstop described in section 4.
5. Both listings compile exactly the candidate's files as plain Go: no
   cgo, ignored, invalid or test files, no load errors.

Known limit: `net/http`, which every wrapper needs, can read files
(`http.Dir`) and send them out (`http.Client`); no allowlist closes that.
So the bare boot below runs under bubblewrap (`--clearenv`, binary bound
read-only, no `/home` or repo bound), and the image is `FROM scratch`.

Then two sub-checks, kept distinct in the lineage log because they fail for
different reasons:
1. `go build` (static, `CGO_ENABLED=0`) succeeds, then the compiled binary
   boots as a bare local process under bubblewrap (above) and its health
   endpoint responds.
2. `docker build` + `docker run` succeeds locally, health endpoint
   responds from the container. The image is `FROM scratch`, the same
   host-built binary, `USER 65532:65532`; the build context is a temp dir
   holding only the Dockerfile and the binary.

No kind involved.

### Stage 2 -- Interoperability (local)

Tests that the mutated wrapper has not corrupted data in flight between
its concurrency/request-handling layer and the (unchanged) fixed core --
not re-validating the hashing algorithm itself -- and that it implements
the error paths of the section 8 HTTP contract. Two groups of assertions,
all zero-tolerance. Stage 2 talks HTTP to the process Stage 1 booted, one
request at a time; every response must carry `X-EK8s-Variant` equal to
that run's label (section 5).

**Interoperability**: four assertions. The `RealHash` pair uses the shared
password/hash test-vector pool (also used by the load generator, section
10, so gate-time and run-time correctness share one definition of
"correct"). The `MutantHash` pair uses fresh random passwords (`NoMatch`:
a different fresh one), so the gate never sends a pool password to
`/hash` and a mutant has no pool pair to replay. Every 2xx response goes
through one classifier run (section 8), hash responses fully verified,
and must classify as `success`: a `graceful_shed` on a single request to
an unloaded process fails Stage 2.

1. `TestInterop_MutantHash_RealVerify_Match` -- hash via mutant wrapper,
   verify via real reference library, matching password: must succeed.
2. `TestInterop_RealHash_MutantVerify_Match` -- hash via real reference
   library, verify via mutant wrapper, matching password: must succeed.
3. `TestInterop_MutantHash_RealVerify_NoMatch` -- hash via mutant wrapper,
   verify via real reference library, wrong password: must correctly
   reject.
4. `TestInterop_RealHash_MutantVerify_NoMatch` -- hash via real reference
   library, verify via mutant wrapper, wrong password: must correctly
   reject.

**Contract conformance** (section 8): each case must return exactly 400,
never 2xx:
1. `/verify` with a malformed hash string.
2. `/verify` with a well-formed hash at non-exact parameters -- at least
   one with `t=1` and one with a very large `m`, from the pool's gate-only
   negative set (section 5). This is the fail-open case: all four interop
   assertions use correct-params hashes, so hashcore's Verify never errors
   there, and a wrapper that turns a Verify error into a match would
   otherwise pass them, Stage 3, Stage 5 and fitness.
3. `/verify` with an empty hash; `/hash` and `/verify` with an empty or
   missing password.
4. `/hash` and `/verify` with a password over 1024 bytes.
5. Invalid JSON, wrong JSON types, and trailing data after the object.

Excluded: 405 (owned by the fixed `cmd/service` mux, not the wrapper) and
the Hash 500 path (a randomness failure, not inducible black-box).

### Stage 3 -- Cost parameters (local)

Parse the encoded Argon2 string from Stage 2's calls and assert exact
equality: `m == 19456, t == 2, p == 1` (section 6) -- not `>=` floor.
Pure assertion against data already produced by Stage 2 -- no separate
HTTP round trip needed.

### Stage 4 -- Concurrency safety (local)

Fire N concurrent requests with distinct passwords, assert no cross-talk
(password A's result never validates against password B). Build a
separate `-race`-instrumented binary for this stage only -- race-detector
overhead would distort Stage 5's timing behavior, so it is never the
image that gets deployed. The race build needs cgo (`CGO_ENABLED=1`) and a
C compiler on the gate host. A 503 shed is no evidence either way: shed
requests are retried a bounded number of times, and every password must
eventually succeed. Stage 4 sends only valid, distinct passwords, so the
only allowed statuses are 200 and a 503 shed: fail on any cross-talk, any
other status (a 400 included), or any race report on the process's
stderr.
`HangTimeout` is not applied under `-race`.

### Stage 5 -- Degradation under contention (kind, candidate namespace)

The one stage that must run on the cluster: it tests behavior under
resource scarcity *enforced by the kubelet* (cgroup limits), which a local
process cannot faithfully simulate without re-implementing what k8s
already provides.

Procedure:
- Deploy the candidate into its own namespace, `ek8s-v<id>` (section 11),
  with the same resource requests/limits every variant gets (the
  per-namespace LimitRange). Its Service is **not** yet on the load
  generator's target list -- it must not receive live traffic before
  passing.
- Wait for the candidate's Deployment to report Ready before sending a
  single request (prevents a startup race from being misclassified as a
  gate failure).
- Point a short, bounded load-generator burst (tens of seconds, not the
  multi-minute observation-window duration -- Stage 5 only needs a
  boolean pass/fail, not statistical confidence) directly at the
  candidate's own Service. No live stream targets it, so this traffic
  never mixes with anything currently live.
- The existing stress-ng Deployment provides contention (kind is
  single-node, so contention is a global resource -- no second stress-ng
  instance needed).

**Pass condition (two independent, zero-tolerance assertions):**
1. Pod health: `restartCount == 0` and no container status reason of
   `OOMKilled`, `CrashLoopBackOff`, or `Error`.
2. Every observed response classifies as `success` or `graceful_shed` via
   the shared three-bucket classifier (section 8) -- never `failed`.
   Every hash response in the burst is fully verified (section 8), not
   sampled.

Pass iff both hold. On pass, there is no shared Service to relabel into:
**the variant stays in the namespace Stage 5 created** and becomes live
when the orchestrator adds its Service to the load generator's target
list. Still no redeploy -- the image is already loaded and the pod already
warm. (Full namespace lifecycle in section 11.)

**Known adjacent gap, deliberately not solved in v1:** live variants share
the single kind node with whatever else is running. A Stage 5 probe (and a
gate build's CPU spike) transiently affects cluster-wide resource
availability. The orchestrator must exclude telemetry from the affected
window from live-population scoring rather than let it become unexplained
noise (see section 11).

## 8. Shared three-bucket response classifier

One function, used by both Stage 5 (boolean: any `failed`?) and the
fitness function (graded: what fraction in each bucket?), implemented in
internal/classifier/ (section 5).

- **`success`** -- 2xx status, arriving within the hang-timeout ceiling
  (same constant as the fitness function's latency-normalization ceiling,
  section 9 -- one number serving both purposes rather than two thresholds
  that could drift apart).
- **`graceful_shed`** -- HTTP 503 **and** exactly one `Retry-After` value
  that parses as delay-seconds or an HTTP-date (RFC 9110 sec 10.2.3; it is
  a single-value field, so a repeated header fails), arriving within the
  hang-timeout ceiling. Both parts of the contract required; a bare 503
  (e.g. from the Service reporting no ready endpoints) must not be
  indistinguishable from the wrapper's own deliberate load-shedding.
- **`failed`** -- anything else: connection refused/reset, any response
  arriving after the hang-timeout (a 503 shed included), a 5xx without the
  `Retry-After` contract, a malformed response (including a 2xx body that
  is not exactly the contract object below).

For `verify` calls specifically, "success" additionally requires the
*objectively correct* accept/reject determination against the shared
test-vector pool's ground truth -- a fast, well-formed response that
returns "valid" for a should-reject pair is a `failed` classification
regardless of latency, not a success. (This closes a specification-gaming
risk: without ground-truth checking, a mutant could shed queue pressure
under contention by short-circuiting verification to always return
success, which would show up as *high* success_rate in exactly the
stress-active half where the fitness signal is most trusted.)

`hash` calls get the same anti-gaming treatment. A hash response is
`success` only if all of the following hold: 2xx within the hang-timeout
ceiling; a well-formed PHC string; exactly the section 6 parameters; a
salt not repeated within the variant's live lifetime; and it verifies
against the password sent. "Live lifetime" means one classifier run from
promotion to cull, not one per observation window, so a mutant cannot
cache hash strings from an earlier window and replay them later to skip
Argon2 (only sampled full verification would catch that). Stage 5's burst
uses its own run. Salts from 2xx hash responses that arrive after the
hang-timeout are recorded too, so a late hash cannot be replayed on time.
Structural checks (format, parameters, salt reuse) apply to every
response. Full verification is mandatory for every Stage 5 response; in
the sustained observation window it is applied to a sample, since a full
verify costs one Argon2 run on the same node.

internal/wrapper/'s own request-timeout enforcement imports the same
constant from internal/classifier/, so wrapper-side timeout behavior and
gate/fitness classification can never drift apart.

**HTTP contract.** Fixed by the design, not by the wrapper: the wrapper
implements it, and a mutant that breaks it fails Stage 2.
- `POST /hash`, JSON `{"password": string}` -> 200 `{"hash": string}`.
- `POST /verify`, JSON `{"password": string, "hash": string}` -> 200
  `{"match": bool}`.
- A 200 body is exactly the contract object: one JSON object holding only
  its one key (matched case-sensitively), with a non-null value of the
  stated type, and nothing after it but whitespace.
- Malformed input, an empty or >1024-byte password on either endpoint
  (`/hash` or `/verify`), or a hash whose parameters hashcore rejects
  -> 400.
- Deliberate load shedding -> 503 with exactly one valid `Retry-After`
  value (the `graceful_shed` contract above).
- `GET /healthz` -> 200 while the process is up; `GET /readyz` -> 200
  when ready, 503 otherwise.

## 9. Fitness function

Computed per variant, from that variant's own dedicated load stream
(section 10). Within that, computed per half-window (baseline /
stress-active):

- `success_rate` -- fraction of requests classifying as `success` (via
  section 8's classifier).
- `p95_latency` -- 95th percentile latency of successful responses.
  Recorded via an HdrHistogram-style recorder under open-loop load
  generation (section 10) -- not a mean, since mean hides the tail
  behavior the evolvable wrapper is meant to affect.
- `cpu_per_success` -- pod CPU-seconds (from metrics-server) divided by
  count of successful responses. Since the fixed core's cost is constant
  across genomes, differences here are almost entirely wrapper overhead
  (goroutine scheduling, queueing, concurrency-control bookkeeping) -- a
  clean signal for exactly what is being evolved.

**Stress-half bucketing:** `graceful_shed` responses get partial credit
(starting point: 0.5, tune later) rather than being scored identically to
`failed` -- otherwise a wrapper that evolves a genuinely good
load-shedding strategy is penalized the same as one that simply falls
over, which works against the resilience premise of the project.

**Normalization: fixed absolute bounds, not against the live population.**
This is a non-negotiable design choice, not a simplification. It is
required for two separate reasons that turn out to be the same underlying
requirement (comparability across time):
1. The stagnation counter (section 11) compares the global-best-ever score
   against candidates found many generations later, under a possibly very
   different live population composition. Population-relative
   normalization would make "1.0" mean different things at different
   points in the run, silently.
2. Tournament selection (section 11) draws parents from an archive that
   spans the whole run's history, not just the current live set -- the
   same cross-time comparability requirement applies.

A served-share term (a variant's fraction of total traffic served) was
considered and rejected: it is population-relative by construction, which
violates this fixed-absolute normalization requirement.

Bounds: a latency floor near the theoretical minimum single-hash time
under the Stage-3 cost floor, and a ceiling past which a response is
scored as unacceptably slow (this ceiling is the same constant used as
the hang-timeout in section 8). CPU-seconds floor/ceiling analogously.
Exact bound values and per-component weights are starting guesses to be
set once real telemetry spread is visible -- not derived from first
principles.

**Per-half score** = weighted sum of the three normalized components
(starting point: roughly equal weighting, slight lean toward
`success_rate` given availability is arguably most load-bearing).

**Combine across halves: `fitness = min(score_baseline, score_stress)`,
not an average.** This operationalizes the resilience claim explicitly: a
variant's fitness is bounded by its worst regime. Averaging would let a
variant coast on baseline numbers while quietly collapsing under
contention -- exactly the failure mode the noisy-neighbour setup exists to
surface.

**Pooled across endpoints (hash and verify), not scored separately.** The
fixed core's cost is symmetric between the two operations (verify
re-derives the same Argon2id computation; the comparison step is
negligible against ~100ms hash cost), and nothing in the evolvable
parameters is endpoint-specific, so there is no mechanism that would ever
produce genuine per-endpoint divergence -- only traffic-mix artifacts from
the load generator's hash:verify ratio, which is a separate experiment to
run later if desired, not a reason to split the metric now.

## 10. Load-generation harness

**Open-loop, not closed-loop.** A closed-loop generator (fixed worker
count, each waiting for a response before sending the next) implicitly
self-throttles to match whatever rate the service can sustain, which
hides exactly the queueing/latency blowup under contention this project
is trying to measure (the "coordinated omission" problem). Generate at a
fixed target rate regardless of response timing (e.g.
`golang.org/x/time/rate`, request-sending goroutines decoupled from
response latency) and record what actually happened, including stalls.

**Tool: custom Go generator**, not a canned CLI tool -- the shared
classifier (section 8) needs to inspect the `Retry-After` header on 503s
per-request, which off-the-shelf summary-output tools are not guaranteed
to retain. (Vegeta's Go library is a real alternative worth checking
directly against its current source if wanted, since its `Result` struct
may discard headers by default to keep memory bounded.) Lives in
`loadgen/` (section 5).

**Request mix -- must include deliberate mismatches, not only valid
credentials.** A shared test-vector pool (also used by gate Stage 2,
section 7) of valid password+hash pairs, plus the same passwords paired
with a different hash to force a should-reject case. Sending only
matching pairs to `verify` would never exercise the rejection path under
load, and is the specification-gaming gap described in section 8.

**Hash-call payloads:** randomly generated passwords of varying length per
request, not a fixed repeated test vector.

**Two operating modes, one generator component:**
- Sustained mode, for the multi-minute observation window (baseline +
  stress-active halves): one fixed-rate open-loop stream per live
  variant, each against that variant's own Service, at the same rate `r`
  for every variant ever scored -- keeps scores comparable across time
  (section 9). Total offered load therefore scales with live population
  (up to 4 streams during probation, section 11); `r` must be sized for
  that plus stress-ng.
- A short, bounded-count burst for the Stage 5 gate probe.
Same request-mix logic and classifier in both; only duration and target
rate differ.

## 11. Orchestrator

### Population topology

Each variant is its own Deployment, HPA, and Service in its own
namespace, `ek8s-v<id>`. There is no pooled front-facing Service: each
live variant receives its own dedicated load stream (section 10).
Readiness probes and HPA still act natively, but within a variant, across
its own replicas -- they cull the obviously broken and provide elastic
capacity, they do not rank viable variants against each other. Explicit
fitness scoring (section 9) is what does the ranking.

**Namespace lifecycle:**
- **Created** by gate Stage 5, when the candidate is deployed for its
  gate probe.
- **Stage 5 fail:** the namespace is deleted (cascading everything in
  it).
- **Stage 5 pass:** nothing changes in the cluster. The orchestrator marks
  the variant live and adds `<svc>.ek8s-v<id>.svc.cluster.local` to the
  load generator's target list. The pod is still warm; no redeploy.
- **Culled after scoring:** the namespace is deleted.

**Why the original relabel-in-place design was dropped:** it could never
have worked. A Service only selects pods in its own namespace, so a
shared front-facing Service could never have picked up pods in a separate
scratch namespace; and changing a Deployment's pod-template labels
triggers a rollout, which would have lost the warm pod anyway.

A per-namespace LimitRange enforces identical resource limits on every
variant. `role: hasher` remains as an inventory label only; it no longer
selects anything. Telemetry is attributed by namespace.

**Readiness dodging punishes itself.** A variant that pulls all its pods
from rotation empties its own Service, and its own traffic fails. Within
a variant, one replica going unready shifts load to its sibling replicas
-- a legitimate strategy, scored fairly, because the variant bears all of
its consequences.

**Live population cap: 3.** A resource/deployment constraint given the
8-core/16GB workstation, not a statement about genome-pool size (see
below). HPA `maxReplicas` per variant: 2-3.

### Generational process (steady-state, single active mutation at a time)

1. **Select parent** via tournament selection (k=2, see Selection below)
   from the full genome archive (not only the current live 3 -- see
   below).
2. **Mutate**: invoke the driver (section 12) on the parent's wrapper
   source.
3. **Build**: `go build` -> `docker build` -> `kind load docker-image`.
4. **Gate** (section 7): fail at any stage -> log which stage, discard,
   return to step 1 (cap consecutive failures before flagging the lineage
   stalled -- do not halt the whole run on one bad mutation).
5. **Provisional promotion**: pass -> mark live and add the variant's
   Service to the load generator's target list (see Namespace lifecycle
   above). Population is briefly 4, not 3 (see Probation below).
6. **Observe**: one full observation window (baseline + stress-active
   halves), same shape as any live variant's.
7. **Score & cull**: score all currently-live variants together on that
   window's data, cull the lowest scorer back to 3.
8. **Log**: lineage record for this attempt regardless of outcome.
9. Repeat from 1, using the (possibly unchanged) best as a candidate
   parent alongside the rest of the archive.

### Probation, not evict-then-promote

Evicting an incumbent *before* the newcomer's window would compare
variants using stale data measured under different conditions -- the same
problem as cross-time tournament comparisons, but avoidable here. Instead:
the newcomer runs its full observation window alongside the existing 3
(population briefly 4), and only then is the lowest of all 4 scorers
culled. The overshoot is bounded (one window) and never compounds, since
mutation is sequential. A newcomer that loses its own probation window is
archived having had exactly one live window -- a real, logged outcome
("gate-passed but functionally weak"), not a discarded non-event.

Newcomers enter probation at HPA `minReplicas` (cold start), consistent
with how every other variant enters the pool -- artificially warming a
newcomer would itself be a source of measurement inconsistency.

### Genome archive (separate from live population)

Every gate-passing candidate is archived permanently regardless of
whether it stays live: wrapper source (a git commit) plus its lineage
record. **Tournament selection samples from this archive, not only the
current live 3** -- mutating a parent does not require it to be currently
running (the driver operates on stored source, not a live pod), so the
genome pool available to selection is much larger than the deployment
cap.

**Bounded FIFO with a hall-of-fame exception**, to keep the eligible pool
from growing unbounded while avoiding the failure mode a naive
top-K-by-fitness cap would cause (collapsing the pool toward near-
duplicates of one lineage, defeating the point of having a larger pool at
all):
- Every gate-passing candidate enters the eligible pool.
- Past a cap (starting point: 15-20, tune empirically), evict the
  **oldest** entry -- plain recency, no genotype-distance computation.
- Exception: the single best-scoring candidate ever produced is never
  evicted by the FIFO rule.

**Known, accepted limitation:** an archived variant's fitness score was
measured under whatever live conditions existed at the time -- not
perfectly comparable to a freshly-scored variant. Accepted for v1 rather
than solved (re-scoring an archived parent before it's eligible would cost
a live slot and add real complexity); logged as a candidate finding for
the writeup either way.

### Selection: tournament (k=2), not argmax

Sampling, not always selecting the best-scoring live/archived variant --
argmax-only parent selection is a simple hill-climb with no diversity
pressure, likely to converge too quickly on a local optimum, and
undermines the point of demonstrating LLM-guided mutation as more than
blind random search. Tournament selection: sample k=2 at random from the
eligible pool, take the better of the two as parent.

**Stagnation escalation (ties selection to mutation-prompt content,
section 12):** track generations since the last new global-best fitness
(against best-ever, not live population -- see section 9's normalization
rationale for why this must be a stable comparison). Past a soft
threshold, escalate: switch the mutation prompt to request a structurally
different approach, and/or bias parent sampling toward the archive rather
than only recently-live variants. The counter only resets on an actual new
global best, not on escalation itself -- so the gap between soft and hard
threshold (section 13) is "how many escalated attempts before conceding."

## 12. Mutation driver

### Invocation

Sandboxed: `claude -p` runs inside bubblewrap, from a per-run scratch
directory outside the repo, never from the repo checkout (section 4).

**Per-run directories:**
- `$RUN/work` -- the session's cwd. Real copies (no symlinks) of the
  wrapper's existing non-test `.go` files only (`doc.go`, `hasher.go`,
  `server.go`, `tunables.go`). Gen-0 tests stay in the repo as CI
  coverage of gen 0; the gate runs no candidate tests.
- `$RUN/home` -- `HOME`, outside the file tools' reach. A fresh `HOME`
  also means no user-level `CLAUDE.md` is loaded.

**bubblewrap:** `--unshare-all --share-net --die-with-parent
--new-session --cap-drop ALL --clearenv`. Bind only:
- `$RUN/work` and `$RUN/home` (rw);
- the resolved, versioned `claude` binary (ro) -- which also pins the
  Claude Code version;
- system libs, certs, `resolv.conf` (ro).

Never bind `/home`, `/run` (docker.sock), or `/var`. bwrap sets
`no_new_privs`, so setuid binaries cannot escalate.

**Environment:** built empty, then allowlisted back: `HOME`, `TMPDIR`,
`PATH`, `LANG`, `CLAUDE_CODE_OAUTH_TOKEN` (a dedicated token from
`claude setup-token`, revocable independently), `CLAUDE_CODE_RESTRICTED=1`,
plus the autoupdate and nonessential-traffic disables. Must never be
present:
- `ANTHROPIC_API_KEY` -- in `-p` mode it is always used when present,
  which means metered billing, breaking the Pro-subscription premise
  (section 3);
- `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`;
- `KUBECONFIG`, `DOCKER_HOST`, `SSH_AUTH_SOCK`, `GH_TOKEN`,
  `GITHUB_TOKEN`.

**Launch:** the orchestrator launches via exec with an explicit argv,
never through a shell -- the prompt contains prior generations'
LLM-written rationale (see Per-call context below) and is treated as
untrusted text. The process group is killed on a wall-clock timeout, as a
backstop beyond `--max-turns`. Inside the sandbox, cwd `$RUN/work`:

```
claude -p --tools "Read,Edit" --restricted \
  --model <pinned-model> --permission-prompts none --max-turns <N> \
  --output-format json "<constructed prompt>"
```

**Copy-back:** only the pre-existing `.go` filenames are copied back from
`$RUN/work`; symlinks and anything else are rejected. Consequence of
`--tools "Read,Edit"`: the mutant cannot create files, so structural
rewrites must fit in the existing files.

**To verify at driver implementation time:** exact env var names for
disabling autoupdate / nonessential traffic; bwrap flags against the
installed bubblewrap version.

**Correction from earlier assumption:** `--allowedTools`/`--disallowedTools`
only govern what runs *without a permission prompt* -- they are additive
on top of Claude Code's default tool set and do not restrict availability.
`--tools` is what actually restricts which built-in tools exist for the
session (`--tools "Read,Edit"`; empty string disables all). `--restricted`
is purpose-built for this kind of harness use ("an evaluation harness
drives claude on a shared machine"): it strips command/code-execution
tools and WebFetch unless named individually in `--tools`, and confines
file tools to the declared working directories. Use both together,
inside the sandbox above (see section 4 for why this is necessary but not
the primary defense).

`--permission-prompts none` and `--max-turns` bound an unattended run so it
cannot hang waiting on a prompt nobody will answer. `--output-format json`
returns `total_cost_usd`, `usage` (input/output/cache tokens), `num_turns`,
`duration_ms`, `session_id` per invocation -- parsed straight into the
lineage record (section 14).

### Per-call context (no session continuity across generations)

Each call is a fresh process against a freshly checked-out parent (which,
per the archive, may not be the previously-live variant) -- there is no
`--resume`/`--continue` chain, so all "semantic awareness" must be
constructed and injected by the driver, not assumed to persist in the
model:

- **Fitness breakdown by component** for the parent (success_rate,
  p95_latency, cpu_per_success, separately per half) -- lets the operator
  target the actual weak axis rather than guessing.
- **Short natural-language summary of the last 3 attempts from this
  lineage** (small on purpose, to keep the prompt tight and avoid
  diluting the fitness-breakdown signal) -- what each changed and its
  outcome. This is what actually distinguishes the approach from blind
  mutation; without it, one edit per generation with no memory of what
  was already tried is not meaningfully better than random search.
- **The fixed core's calling interface as a real, readable file**
  (signatures + doc comments only) rather than described only in prompt
  text.
- **The hang-timeout ceiling value, stated explicitly** -- the mutant
  cannot see internal/classifier/, where the constant lives.
- **Rationale requested back in the response text** -- comes free in the
  `result` field already being parsed for cost/usage, and becomes next
  generation's "recent attempts" input. The lineage narrative is then
  self-documenting rather than reconstructed by diffing source.

### Prompt content

**Normal mode:** small, targeted tuning adjustment aimed at the identified
weak fitness component. Prefer minimal, explainable changes.

**Escalation mode** (stagnation past soft threshold): the lineage has
stagnated for N generations despite tuning attempts (summarized); propose
a structurally different approach to concurrency/backpressure/request-
handling rather than an incremental tweak.

**Non-negotiable line in both prompt variants, escalation especially:**
any interaction with credential hashing or comparison must go through the
provided fixed-core interface only -- never implement, reimplement, or
bypass hashing/comparison logic directly. "Structurally different" is
scoped strictly to the request-handling/concurrency/backpressure layer.
This exists because an unqualified invitation to think unconventionally is
exactly the kind of prompt that could otherwise motivate bypassing the one
property in this system explicitly designed never to regress. (See
section 4 for the structural backstop that also protects against this.)

## 13. Stopping criteria

Run halts on whichever comes first, with the reason logged explicitly on
the run-level summary record (`stagnation` / `usage_ceiling` / other):

1. **Stagnation hard threshold**: the generations-since-global-best counter
   (section 11) exceeds a hard threshold even through escalated attempts.
2. **Usage ceiling**: cumulative `num_turns` and/or token usage (from
   section 12's parsed `--output-format json` fields) crosses a ceiling.

**On the usage ceiling specifically:** `total_cost_usd` from the CLI is a
client-side *estimate* computed as if billed per-token; under a Pro
subscription (not metered API billing) this figure does not correspond to
actual spend or an actual limit being approached. It remains useful as a
*relative* metric between generations, but `num_turns` and token counts
are the more honest metrics for an absolute ceiling, since they reflect
real subscription-quota consumption. The ceiling's exact value should be
set empirically after observing typical per-generation usage across a
handful of real runs, not asserted in advance; current Pro-plan usage
limits are account/plan-specific and should be checked against current
Anthropic documentation if an authoritative number is needed.

The two mechanisms are complementary: the usage ceiling is a hard backstop
independent of whether the stagnation logic is itself working correctly.

## 14. Lineage / data model

- **SQLite** -- structured, unbounded, one row per generation attempt:
  generation_id, parent_id (may reference an archived, non-live genome),
  timestamp, gate result (pass/fail + failing stage if any), fitness score
  and its components (both halves), action taken (promoted / discarded /
  archived), selection mode (tournament-normal vs. stagnation-escalated),
  ever-live / currently-live flags, usage fields from section 12/13.
  Keep this table unbounded -- it is the basis for convergence plots and
  writeup analysis.
- **JSONL** -- raw `claude -p --output-format json` transcripts per
  generation, archived but rarely queried directly.
- **Eligible parent pool** (section 11) is a *view* over the SQLite table
  (gate-passed, within the FIFO cap or hall-of-fame), not a separate
  store.

## 15. Known limitations and deliberately deferred items (v1)

- Cross-time fitness comparability rests on fixed-bound normalization
  holding up in practice; if telemetry proves noisier than expected under
  varying live-population composition, this may need revisiting.
- Archived-genome re-scoring before eligibility is not implemented --
  accepted staleness in cross-time tournament comparisons (section 11).
- Endpoint-pooled (not split) latency/CPU scoring -- revisit only if a
  deliberate traffic-mix experiment motivates it (section 9).
- Exact numeric starting points (fitness component weights, normalization
  bounds, stress-ng dosing, per-pod resource limits, FIFO archive cap,
  stagnation soft/hard thresholds, usage ceiling, per-variant stream rate
  `r`, sustained-mode hash verification sample rate) are all explicitly
  starting guesses pending real telemetry, not derived values -- expect to
  tune all of them once the loop is running.
