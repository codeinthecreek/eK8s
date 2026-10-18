# eK8s: Evolutionary Kubernetes Credential-Hashing Service

**Status:** design settled, v1 scope. Ready for implementation.
Fitness-first redesign (2026-10-14): traffic only in observation
windows, host-generated `/verify` pairs, host-side full verification
after each window, and the gate as a filter for broken candidates, with
live integrity checks protecting fitness (sections 7-11, 15). The gate
change (Stages 2-4 in a pod, section 7 "Planned") is planned, not built;
section 7's Stage 1-4 text describes what is built today.

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
    testvectors/         # the gate's test-vector pool (precomputed,
                          # embedded). Imported by gate only; never by
                          # wrapper or loadgen (loadgen's pairs are
                          # generated fresh on the host, sec 10).
  gate/                  # correctness gate, five ordered stages.
  loadgen/               # open-loop load generator (sec 10): sustained
                          # mode (one stream per live variant, sending
                          # only in a window or a Stage 5 burst), one
                          # long-lived Deployment. Imports
                          # internal/classifier; its build never depends,
                          # directly or transitively, on internal/hashcore,
                          # internal/classifier/testvectors, gate/ or any
                          # Argon2 package (it does no Argon2: pairs and
                          # full verification are the host's, sec 10).
                          # Never in the mutation driver's sandbox.
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

The gate's test-vector pool (section 7) lives in the
internal/classifier/testvectors/ subpackage, as precomputed data
generated once by a small tool that uses internal/hashcore/.
internal/wrapper/ never imports the subpackage (the Stage 1 allowlist,
section 7, forbids it), and internal/classifier/ itself never imports
internal/hashcore/. Loadgen never imports it either: a finite pool lets a
mutant memoize (password, hash) -> match, or replay a pool hash as a
`/hash` answer, so every pair loadgen sends is generated fresh on the
host for its window (section 10). The pool
also holds a small negative set: well-formed PHC strings at non-exact
parameters (section 7, Stage 2 contract conformance), generated with the
reference library -- except very-large-`m` entries, which are
hand-constructed, since computing one would allocate that memory. Loadgen
builds its own contract negatives (section 10) to the same shape rules. A
hand-constructed entry
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
- The variant label has one shape in the gate and in the cluster,
  `ek8s-<16 hex>`, random per deployment: a pod label in the cluster,
  passed in as `EK8S_VARIANT`. It names no variant id, so a candidate
  cannot tell gate from cluster by its shape. The label -> `ek8s-v<id>`
  mapping is kept by the orchestrator and lineage (section 14).

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

**Principle: three concerns, kept apart.** *Containment* of untrusted
code (a candidate is mutant-written: what it can read, reach and send);
*artifact security* (the deployed service must stay a safe password
hasher); *fitness integrity* (a variant's score must come from real work,
not faked results). For artifact security, the algorithm and its
parameters are structural: the fixed hashcore (section 4), the exact
parameters (section 6) and the ban on crypto imports (Stage 1 below)
hold whatever a candidate does. The wrapper's other security properties
(rejecting bad input, never failing open, really hashing) are enforced
by integrity checks on live traffic in the cluster (sections 8-10),
which also protect fitness integrity. The gate is a filter for broken
candidates, so they never cost a live slot or an observation window; it
is not required to be indistinguishable from the cluster. A candidate
that detects the gate can behave differently there (section 15), so a
gate-only check binds only candidates that do not detect it, and the live
checks are what protect fitness.

Stages 1-4 below describe the current implementation (the bare boot).
The "Planned" subsection after Stage 4 replaces the bare boot for Stages
2-4; until that PR series lands, the Stage 1-4 text stays authoritative
for what is built.

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
   and Stage 4's race build (`CGO_ENABLED=1 -race -tags netgo,osusergo`,
   the flags it is built with). Exactly these,
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
So the bare boot below runs under bubblewrap (`--clearenv`, only the
binary bound, read-only; no `/home`, repo or `/dev`) in its own network
namespace, so it has no network egress either; the gate reaches its :8080
through a relay joined to that namespace with `nsenter`, over a Unix
socket in the gate's private temp dir. The image is `FROM scratch`.

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
the error paths of the section 8 HTTP contract. Three groups of
assertions, all zero-tolerance. Stage 2 talks HTTP to the process Stage 1 booted, one
request at a time; every response must carry `X-EK8s-Variant` equal to
that run's label (section 5).

**Interoperability**: four assertions. The `RealHash` pair uses the gate's
password/hash test-vector pool (section 5; gate-time and run-time
correctness share the classifier's ground-truth rule, section 8, not a
pool). The `MutantHash` pair uses fresh random passwords (`NoMatch`:
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

**Password bounds**: `/hash` with fresh 1-byte and 1024-byte passwords
(never pool passwords) must classify as `success`, fully verified; their
hashes count toward Stage 3.

**Contract conformance** (section 8): each case must return exactly 400,
never 2xx:
1. `/verify` with a malformed hash string.
2. `/verify` with a well-formed hash at non-exact parameters -- at least
   one with `t=1` and one with a very large `m`, from the pool's gate-only
   negative set (section 5). This is the fail-open case: all four interop
   assertions use correct-params hashes, so hashcore's Verify never errors
   there, and a wrapper that turns a Verify error into a match would
   otherwise pass them, Stage 3, Stage 5 and fitness. A wrapper that detects the
   gate could still fail open outside it, so loadgen sends contract
   negatives, these classes and three 400 classes from cases 3-5, in
   sustained traffic and in the Stage 5 burst (sections 8, 10).
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
other status (a 400 included), or any race report in the process's
output (stdout and stderr).
`HangTimeout` is not applied under `-race`.

Mechanics:
- The race build is linked statically (`-race -tags netgo,osusergo
  -ldflags='-linkmode=external -extldflags=-static'`), so it boots in the
  same bubblewrap sandbox as Stage 1's bare boot, with only the binary
  bound; it needs gcc and a static libc (libc6-dev, glibc-static). Before
  it, the gate's probe is race-built and booted the same way; any failure
  there is a gate fault. The Stage 2 process is stopped first.
- `GORACE="halt_on_error=1 exitcode=66 atexit_sleep_ms=0"` and
  `GOMAXPROCS` = max(the gate host's CPUs, 4) (both via bwrap
  `--setenv`). GOMAXPROCS has a floor of 4 so that a wrapper sizing its
  concurrency from it (generation 0's slots) has several requests in
  flight at once even on a 1- or 2-CPU host: the race detector orders
  every operation on a `chan struct{}` semaphore through one sync
  object, so it sees a race inside the slot only between holders that
  run at once. The first race report ends the process with status 66. A
  report is that exit status, or "WARNING: DATA RACE" anywhere in the
  output, scanned in full (a candidate can log to stderr, so no prefix cap
  hides it). Checked once the load ends (or the process exits) and a 1s
  grace has passed.
- N = 2 x the gate host's CPUs, clamped to [8, 64]: at least twice
  generation 0's slots (one per GOMAXPROCS, so 4 at 1-4 CPUs) up to 32
  CPUs, so it sheds there, at a constant per-CPU load from 4 to 32 CPUs.
  Half `/hash` with fresh passwords (never pool ones), half `/verify`
  with pool pairs (distinct passwords, alternately match and mismatch),
  interleaved and released together.
- A "503 shed" is a 503 that classifies as `graceful_shed` (section 8)
  apart from the `HangTimeout` check; a bare 503 fails. It is retried
  after 1s plus up to 250ms of jitter (Retry-After is validated, not
  honoured), at most 15 sends per request. A transport error (no
  response status) is retried within the same bound, never once the
  process has exited; a response whose body cannot be read is final.
  The stage has a 120s deadline and each request a 30s client timeout.
  cmd/service's fixed `WriteTimeout` (3 x `HangTimeout`, 6s) still
  applies under `-race`: a response written later is lost as a
  transport error.
- The process must still be running after the load and the 1s grace:
  any exit fails (status 66 as a race report, any other in the checks
  below).
- After the load, every final response must be exactly 200, carry exactly
  one `X-EK8s-Variant` equal to that boot's label, and classify as
  `success` (latency aside) in Stage 2's classifier Run, hash responses
  fully verified against their own password.

### Planned: Stages 2-4 in a pod (not built)

Direction only; the implementation is a later PR series with its own
design packet, and the bare-boot text above stays until it lands.
- Stage 1's pre-build checks, `go vet` and `go build` stay on the host,
  with no candidate execution there.
- Stages 2-4 move to a pod in the cluster, replacing the bare boot (and
  with it bwrap, the nsenter relay and the race build's bare boot on the
  host).
- A build-only probe stays on the host, so a host toolchain fault (no
  go, no gcc or static libc for the race build) is still a gate fault,
  not a candidate failure.
- Stage 1's `docker run` sub-check is removed: the gate pod proves the
  image boots, so the host stops executing the candidate in a container.

Open items for that design packet:
- which namespace gate pods run in;
- how the gate client reaches the pod, given the variant ingress policy
  (TCP 8080 from loadgen only, section 11);
- the race pod's resource limits against the variant LimitRange (a
  `-race` hash costs about 3x a plain one's memory);
- race-report scanning (exit status 66, the output marker) and restart
  counts on a pod;
- cluster faults (Pending, image pull) as gate faults, never candidate
  failures.

### Stage 5 -- Degradation under contention (kind, candidate namespace)

The one stage that must run on the cluster: it tests behavior under
resource scarcity *enforced by the kubelet* (cgroup limits), which a local
process cannot faithfully simulate without re-implementing what k8s
already provides.

Procedure:
- Deploy the candidate into its own namespace, `ek8s-v<id>` (section 11),
  with the same resource requests/limits every variant gets (the
  per-namespace LimitRange). Before the burst the orchestrator registers
  it as a loadgen target (a PUT, section 10), so the burst's salts enter
  its salt set. A registered target gets no traffic until a start names
  it, so it receives no live traffic before passing.
- Wait for the candidate's Deployment to report Ready before sending a
  single request (prevents a startup race from being misclassified as a
  gate failure).
- Point a short, bounded load-generator burst (tens of seconds, not the
  multi-minute observation-window duration -- Stage 5 only needs a
  boolean pass/fail, not statistical confidence) directly at the
  candidate's own Service, started through the long-lived loadgen's
  control API (section 10). The burst rate is b = 2 x C0 (= 4r) and it
  lasts a fixed D = 30 s; its size n = round(b x D) follows from b and
  is not a second input. No live stream targets it, so this traffic
  never mixes with anything currently live. Stage 5 runs in a gap between
  observation windows (section 11), so it never overlaps a scored window.
  The burst's `/verify` pairs are generated on the host before it, as for
  a window (section 10). Contract negatives (section 8) are at least 5% of
  the burst, every negative class (section 8) at least once. On a Stage 5
  fail, the orchestrator DELETEs the candidate's target after it has read
  the burst's records.
- The existing stress-ng Deployment provides contention (kind is
  single-node, so contention is a global resource -- no second stress-ng
  instance needed). The orchestrator owns stress state (section 11): it
  turns stress-ng on, in the gap, before it calls Stage 5. Stage 5 only
  checks that stress-ng is Running; anything else is a gate fault, never
  a candidate failure.

**Pass condition (two independent, zero-tolerance assertions):**
1. Pod health: `restartCount == 0` and no container status reason of
   `OOMKilled`, `CrashLoopBackOff`, or `Error`.
2. Every observed response classifies as `success` or `graceful_shed` via
   the shared three-bucket classifier (section 8) -- never `failed`; a
   contract negative passes as a 400 or a `graceful_shed`, and any 2xx
   to one is `negative_accepted`, a failure. Every hash response in the
   burst is fully verified on the host after the burst (section 8), not
   sampled.

Pass iff both hold. On pass, there is no shared Service to relabel into:
**the variant stays in the namespace Stage 5 created** and becomes live
when the orchestrator names it in window starts (section 10); its target
already exists in loadgen from Stage 5. Still no redeploy -- the image is
already loaded and the pod is already running. It is idle until its first
window, which starts cold like every other. (Full namespace lifecycle in
section 11.)

**Shared node, handled by scheduling:** live variants share the single
kind node with whatever else is running, and a Stage 5 probe (or a gate
build's CPU spike) affects cluster-wide resource availability. So builds,
gate runs and Stage 5 run only in the gaps between observation windows,
which carry no load, and never overlap a scored window (section 11).

## 8. Shared three-bucket response classifier

One function, used by both Stage 5 (boolean: any `failed`?) and the
fitness function (graded: what fraction in each bucket?), implemented in
internal/classifier/ (section 5).

- **`success`** -- status 200, arriving within the hang-timeout ceiling
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

**Integrity reasons apply at any latency.** After a transport error, the
checks that can yield an integrity reason (section 9) run on every 2xx
response whatever its latency, and win over the hang-timeout: a late
fail-open or faked result is still a bypass. For a response that passes
them, the hang-timeout decides only whether it can be `success` (or
`graceful_shed`) rather than `failed`.

**Contract negatives** are loadgen's deliberate bad requests (section
10), five classes: `/verify` with a malformed hash string (hashcore's
`ErrMalformedHash` class), `/verify` with a well-formed hash at non-exact
parameters (`ErrUnsupportedParams`), an empty password, a password over
1024 bytes, and invalid JSON. Each must get a 400 under the HTTP
contract below. A negative passes as a 400 within the hang-timeout or as
a `graceful_shed`; any 2xx to one, at any latency, is
`negative_accepted` (the fail-open case, section 7 Stage 2), and
anything else is `failed`. Negatives are outside `success_rate`, p95 and
`cpu_per_success` (section 9).

For `verify` calls specifically, "success" additionally requires the
*objectively correct* accept/reject determination against the pair's
known ground truth (section 10: a match pair is a host-generated
`hashcore.Hash` of a fresh password, a mismatch pair a random canonical
PHC at the exact parameters with a fresh password) -- a fast, well-formed
response that returns "valid" for a should-reject pair is a `failed`
classification regardless of latency, not a success. (This closes a
specification-gaming risk: without ground-truth checking, a mutant could
shed queue pressure under contention by short-circuiting verification to
always return success, which would show up as *high* success_rate in
exactly the stress-active half where the fitness signal is most
trusted.)

`hash` calls get the same anti-gaming treatment. A hash response is
`success` only if all of the following hold: 200 within the hang-timeout
ceiling; a well-formed PHC string; exactly the section 6 parameters; a
salt not already seen from that variant; and it verifies against the
password sent.
- **Structural checks, every response, in loadgen during the window:**
  format, exact parameters, and the variant's salt set. The salt set is
  per variant, held in loadgen's memory for as long as loadgen serves
  that variant, so a mutant cannot cache a hash string from an earlier
  window and replay it later. Stage 5 burst salts go into the variant's
  salt set too: loadgen runs the burst through its control API, so the
  set sees every `/hash` response the variant gives. It has no journal
  and no restore: a loadgen restart empties it, which only loses
  detection power (section 10).
  A salt is recorded for every 2xx answer to a `/hash` request that is
  not a contract negative, whose body passes the body checks and whose
  hash is canonical, on time or late (so a late hash cannot be replayed
  on time), whatever full verification then finds. A malformed or
  over-cap body, or a non-canonical hash, records nothing.
  The set is keyed on the decoded 16-byte salt.
- **Repeated passwords:** about 1% of `/hash` requests repeat a password
  already sent to that variant in the same window, or another `/hash`
  request's password in the same Stage 5 burst (shuffled, so either may
  go first). The two responses must carry distinct salts; the same salt
  twice is an integrity failure (`salt_reused`), so a wrapper that caches
  password -> hash is caught (a cached hash still verifies, so full
  verification alone would miss it).
- **Full verification, on the host, after each window and before
  scoring:** loadgen hands its recorded (password, hash) pairs to the
  orchestrator, which checks them with hashcore. Every hash is verified
  during a variant's first K scored windows (section 15), a sample after
  that (10% of the variant's hashes per window to start, section 15);
  only scored windows count toward K. Every Stage 5 burst hash is
  verified, by the gate on the host. A hash that answers `/hash` by
  replaying a `/verify` pair's hash fails here, since it does not verify
  against the password sent.
  Verification runs off the node's scored time: windows carry the only
  load, and the host does its Argon2 in the gap.

internal/wrapper/'s own request-timeout enforcement imports the same
constant from internal/classifier/, so wrapper-side timeout behavior and
gate/fitness classification can never drift apart.

**HTTP contract.** Fixed by the design, not by the wrapper: the wrapper
implements it, and a mutant that breaks it fails Stage 2.
- `POST /hash`, JSON `{"password": string}` -> 200 `{"hash": string}`.
- `POST /verify`, JSON `{"password": string, "hash": string}` -> 200
  `{"match": bool}`.
- A 200 body is exactly the contract object: one JSON object holding only
  its one key (matched case-sensitively; a second member, a duplicate key
  included, is `malformed_body`), with a non-null value of the stated
  type, valid UTF-8, at most `MaxBodyBytes` (4 KiB), and nothing after it
  but whitespace. JSON-equivalent forms (escaped names or characters,
  surrounding whitespace) are accepted. A 2xx other than 200 gets the
  same body checks; if they pass and its hash is canonical its salt is
  recorded, and it is then `bad_status`. Content-Type is not part of the
  contract.
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
  count of successful responses. Contract negatives (section 8) are
  outside all three metrics. Since the fixed core's cost is constant
  across genomes, differences here are almost entirely wrapper overhead
  (goroutine scheduling, queueing, concurrency-control bookkeeping) -- a
  clean signal for exactly what is being evolved.

**Stress-half bucketing:** `graceful_shed` responses get partial credit
(starting point: 0.5, tune later) rather than being scored identically to
`failed` -- otherwise a wrapper that evolves a genuinely good
load-shedding strategy is penalized the same as one that simply falls
over, which works against the resilience premise of the project.

**Integrity failures disqualify.** `verify_mismatch`, `verify_error`,
`wrong_match`, `non_canonical_phc`, `salt_reused`, `negative_accepted`,
and a `malformed_body` 2xx are not one failed request but evidence the
wrapper is faking results (for example, relabelling undelivered Argon2
output from timed-out clients as later hashes). The first one, once
confirmed on the host with hashcore, disqualifies the variant: it is
culled at window end, before scoring, and is never a parent (section
11). A catch by the sampled verification after K windows (section 8)
disqualifies just the same. A failed body read (a timeout or reset) is
`transport_error`, an ordinary `failed`; a 2xx body over `MaxBodyBytes`
is `malformed_body`. The classifier owns that cap: callers read
`MaxBodyBytes`+1 bytes and pass them with no error.

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

Likewise the per-variant stream rate `r` is frozen for a run (section
10): a new `r` means a new run, and one run is one comparability epoch.
Archived scores are compared only within their run; a later run may seed
from archived genomes but not from their scores.

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

**Request mix -- must include deliberate mismatches and contract
negatives.** 50% `/hash`, 48% `/verify`, 2% contract negatives (starting
guesses, section 15). There is no shared pool: every pair is generated
fresh, so a mutant has nothing finite to memoize or replay.

**`/verify` pairs** are generated on the host, fresh for each window, and
handed to loadgen through its control API. Each pair is sent once, to one
variant; no pair is reused across variants in v1. Loadgen sends a batch
in random order, whatever order the host hands it over in. A window
starts only when its pairs are ready. After a loadgen restart the
orchestrator hands over a new batch, never the old one. The test-vector
pool stays gate-only (section 5).
- Match pair: `hashcore.Hash` of a fresh password.
- Mismatch pair: a random canonical PHC string at the exact parameters
  with a fresh password. Sending only matching pairs would never exercise
  the rejection path under load, the specification-gaming gap described
  in section 8.

A window batch holds P = ceil(0.48 N + sqrt(N ln(1e9) / 2)) pairs, capped
at N, where N is the window's send count: a stream draws `/verify` on
each send with probability 0.48, so by Hoeffding's inequality one stream
runs out of pairs in one window with probability at most 1e-9. A start
needs each target's batch to hold at least P (a window) or the burst's
`/verify` count (a burst), both given by `GET /config`, and refuses a
shorter batch. A batch is per target, uploaded separately, and consumed
at start. At most one batch is pending per target: an upload under
another `run_id` replaces it. An upload is refused while its target is in
a run, and under a `run_id` a run already used. A batch left pending
under a `run_id` that has since been used is never sent, and the next
upload replaces it. Every pair must carry `password`, `hash` and `match`.
The upload body is capped at max(window pairs, burst pairs) x 1161 + 4 KiB
(a 1024-byte password, a 97-byte hash and the field names, per pair), and
a start body at 64 KiB.

**Contract negatives** (section 8): all five classes in sustained
traffic -- `/verify` with a malformed hash and with a well-formed hash
at non-exact parameters, an empty password, a password over 1024 bytes,
and invalid JSON. Loadgen builds every negative itself, hash strings
included. Every random byte of a password, salt or key that loadgen
generates comes from `crypto/rand` (the host makes the `/verify` pairs);
`math/rand/v2` makes only the structural choices: request kind and
order, which pairs a burst uses, the order pairs go out in, a stream's
first negative class, which password repeats, lengths, a negative's form
and the defect it carries (cut point, whether salt or key is altered,
newline position, the value set in the unused low bits, wrong type,
trailing data). The malformed and non-exact-parameters hash strings need
no Argon2, since hashcore rejects both before hashing. The classes
rotate within each stream, from a random starting class. A Stage 5 burst
of n requests holds max(ceil(0.05 n), 5) negatives, every class at least
once. They pass as 400 or `graceful_shed`; a 2xx is `negative_accepted`.
Because they are in live traffic too, a wrapper that detects the gate
and fails open only outside it is caught (the PR #6 fail-open bypass).

**`/hash` payloads:** passwords are `crypto/rand.Text` output (RFC 4648
base32: A-Z, 2-7), concatenated and cut to a length drawn uniformly from
16 to 1024 inclusive. They are ASCII, so characters equal bytes, and
`encoding/json` escapes none of them. A fresh one per request, so a
collision with a pool password is negligible; about 1% repeat a password
already sent to that variant in the same window, or another `/hash`
request's password in the same Stage 5 burst (section 8).

**Loadgen does no Argon2.** It runs the structural checks on every
response (section 8) with `internal/classifier` and no verifier, one salt
set per variant for as long as loadgen serves it. It records the
(password, hash) pair of a `/hash` request answered 2xx, 200 or not,
whose hash passed every check it makes, on time or late (those hashes
are verified on the host too); the pair rides on that request's record.
The sender reads `MaxBodyBytes`+1 bytes and passes them with no error: a
2xx body over the cap is `malformed_body`, and only a failed read is a
transport error. The orchestrator reads the pairs, on the records,
through the control API after the window and fully verifies them on the
host (section 8). Loadgen's build never depends, directly or
transitively, on `internal/hashcore`, `internal/classifier/testvectors`,
`gate/` or any Argon2 package, and a `go list -deps` test checks this.
Its `_test.go` files may import hashcore to check that each negative
gets the intended hashcore error; they are not in the binary.

**Two operating modes, one generator component:**
- Sustained mode, for the multi-minute observation window (baseline +
  stress-active halves): one fixed-rate open-loop stream per live
  variant, each against that variant's own Service, at the same rate `r`
  for every variant ever scored -- keeps scores comparable across time
  (section 9). Total offered load therefore scales with live population
  (up to 4 streams during probation, section 11); `r` must be sized for
  that plus stress-ng.
- A short, bounded-count burst for the Stage 5 gate probe, at its own
  rate b for a fixed D (below).
Same request-mix logic and classifier in both; only duration and target
rate differ. Loadgen sends nothing outside a window or a Stage 5 burst
(section 11).

**The rate `r`** is `0.5 x C0`. C0 is the highest rate at which generation
0 on a single pod (HPA `minReplicas` = `maxReplicas` = 1, no stress)
sheds at most 1% with p95 at most `HangTimeout`/4. It is measured once,
and frozen for the run; a new `r` means a new run, and one run is one
comparability epoch (section 9). Until C0 is measured, a provisional `r` comes from a
workstation Argon2 benchmark; the CI smoke run uses `r` = 2.

**The burst shape** is frozen per run like `r`: its rate b is set by the
start-up flag `-burst-rate` (b = 2 x C0 = 4r, section 15), its duration D
is the fixed 30 s, and its size is n = round(b x D). The ring's largest
burst is n (below). Start-up refuses n > 3,000, because the memory sum
(docs/decisions.md, PR #43 no-cap entry) covers no more.

**Schedule.** One scheduler goroutine per target. Send i is intended at
start + i/`r`, computed from the index and never from a running sleep, so
drift cannot accumulate. Each send runs in its own goroutine, so a slow
response delays no later send. A late send goes out at once and its lag is
recorded; sends are never skipped to catch up. A window is round(`r` x W)
sends; a burst uses the same scheduler with its own rate and count n. A
send is skipped only when a `/verify` is due and the stream has no pair
left (`ErrOutOfPairs`); it becomes a skipped record. In-flight requests
are not capped.

**Schedule lag and global exclusion.** Every send records its intended and
actual send time. A scoring half is excluded for all variants (loadgen is
one shared process) when lag p99 exceeds 10 ms, any lag exceeds 100 ms, a
send is skipped, a send faults (a `Check` error, or a request loadgen
could not build or generate: a loadgen fault, not the variant's
verdict), or a record ring has a gap (records overwritten before the
orchestrator read them; a gap on any target excludes the half for all
variants). Each target has its own ring, made and dropped with that
target's `Checker`, with room for round(`r` x W) records plus the
burst's n. `Read(since)` returns the records from `since` onward, with a
gap flag if the oldest retained `Seq` is past `since`. The orchestrator
reads the rings only in gaps, and a window is done only when every send
has completed or timed out; a target is dropped only after its records
have been read. The thresholds
are starting guesses (section 15). CFS throttling of loadgen is logged as
a diagnostic, not a trigger. After 3 consecutive excluded halves the
orchestrator flags the run and halts.

**Records, not summaries; loadgen has no halves.** Loadgen returns raw
per-request records, each carrying its per-request verdict (section 8)
and, for a `/hash` the host must verify, its recorded (password, hash)
pair. Each record carries its intended send time in wall-clock time
(assigns it to a half) and in monotonic time (lag, latency), plus its
actual send time. Monotonic times are durations since a process-start
base, so they compare only within one `boot_id`. The intended monotonic
time comes from the index; the intended wall-clock time is the window's
wall-clock start + i/`r`, in UTC with the monotonic reading stripped. The
actual time is taken immediately before `client.Do`, and latency runs
from there to the full body read. The orchestrator assigns each
record to a half by intended send time, against its own stress-ng switch
times, and computes every per-half statistic (lag p99, `success_rate`,
`p95_latency`, verdict counts) and every exclusion decision. Loadgen and
the orchestrator read one clock: the kind node is a container on the host.

**Evidence.** A record whose reason is an integrity reason (section 9)
carries the request body, the response status and the response body (at
most `MaxBodyBytes`+1 bytes), for the first 8 such records per target per
window or burst; later ones carry the verdict only, so a variant that
fails every request cannot fill loadgen's memory. Section 9 requires the
host to confirm an integrity failure before it culls; what the host
checks per reason is settled with the orchestrator design.

**Runs in the cluster**, as one long-lived Deployment in namespace
`ek8s-system` serving every variant's stream and every Stage 5 burst.
Guaranteed QoS, 750m CPU / 256Mi, in the shared pool; the static CPU
manager is used only if the workstation run excludes more than about 5%
of halves for lag. It reaches each variant by Service DNS (section 11)
with HTTP keep-alives disabled and no proxy (one `http.Client` over a
clone of `http.DefaultTransport` with `Proxy` nil), sending
`Content-Type: application/json`, so every request opens its own
connection: kube-proxy balances per connection, so a kept-alive stream
would pin to one replica, and HPA's second pod and readiness changes
would act only on new connections. It follows no redirect: a 3xx is the
variant's own response (`bad_status`), and following it would send the
request wherever the variant names, a peer variant included. Its HTTP
client timeout is longer than cmd/service's fixed `WriteTimeout` (3 x
`HangTimeout`, 6s): 3 x `HangTimeout` + 1s (7s), a loadgen constant, and
a `go/parser` test fails unless cmd/service/main.go's `WriteTimeout`
field still reads `3 * classifier.HangTimeout`. So every response the
service can ever write is observed and classified: a shorter timeout
would turn a slow fail-open or faked result, one a real client waiting
longer would receive, into a `transport_error` instead of an integrity
failure (section 8, "Integrity reasons apply at any latency"). No host
ports are published; the gate and orchestrator stay on the host and
drive the cluster through the API server on loopback.

**Control API.** A listener on a separate port, the fixed `:9090`, behind
a ClusterIP Service, reached from the host through the API server's
service proxy. Loadgen holds no Kubernetes API credentials. Through it
the orchestrator and gate add and remove targets, hand over a window's
`/verify` pairs, start a window or a burst, read records (each with its
per-request verdict and recorded (password, hash) pair), run the
per-half policy canary (section 11), and read `boot_id`. "Verdicts" means
per-request verdicts only. The owner approved this in-cluster listener
(2026-10-13).

The endpoints are `GET /config`, `GET /targets`, `PUT` and `DELETE
/targets/{id}`, `GET /targets/{id}/records`, `PUT
/targets/{id}/pairs/{run_id}`, `POST /runs/{run_id}/start`, `GET
/runs/{run_id}`, `POST /runs/{run_id}/cancel` and `POST /canary`.
`boot_id` is on every response, as the `Ek8s-Boot-Id` header and in every
JSON body; the mux's own 404 and 405 bodies are plain text, with the
header only. A target is named by its id only: loadgen builds the URL
from its `-target-url` template, and checks the id against the variant id
format. Every flag (`-rate`, `-window`, `-burst-rate`, `-target-url`,
`-canary-addr`) is required, with no default.

Runs:
- One run is active at a time. A `run_id` cannot start with a dot, and is
  never reused within a boot.
- A start is all or nothing: if any named target cannot start, none does
  and no batch is consumed. Loadgen picks the start time (now + 200 ms).
- A target's state in a run is `running`, `done`, `cancelled` or `error`.
  Cancel ends the run's context, and a target leaves `running` only after
  every send it launched is recorded.
- The orchestrator reads records only when no target is `running`, and
  excludes the run on `cancelled` or `error`.
- `DELETE ?through=S` is refused while records past S are unread or the
  target is in a run.

Records come in pages of at most 1,000. The wire format has snake_case
names, buckets and reasons as names, durations in ns, times in RFC 3339
UTC and bytes in base64. An absent reason means none, and a skipped or
fault record carries no bucket or reason.

On SIGTERM loadgen cancels every run, then waits up to 10 s for its
requests and for the sends in flight to be recorded.

**Restart.** A variant's salt set lives only in loadgen's memory: no
journal, no restore. A restart empties it, which only loses detection
power: a hash replayed from before the restart is no longer caught by
its salt, only by full verification, since it does not verify against the
new password. A restart during a
window loses that window's unverified hashes, so the orchestrator, on
seeing a new `boot_id`, excludes the window for all variants and hands
over a new batch of pairs for the next one, never the old batch.

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
  gate probe, labelled `ek8s.io/role: variant`.
- **Stage 5 fail:** the orchestrator reads the burst's records, then
  DELETEs the target (section 10), then the namespace is deleted
  (cascading everything in it); otherwise the target's Checker, salt set
  and Ring stay in loadgen's memory, outside the PR #43 memory sum.
- **Stage 5 pass:** nothing changes in the cluster. The orchestrator marks
  the variant live and names it in window starts (through loadgen's
  control API, section 10); the target already exists from Stage 5, and
  loadgen builds its URL from the id. The pod keeps running but is idle
  until its first window, which starts cold like every other; no
  redeploy. It takes traffic from the next observation window on.
- **Culled after scoring:** the orchestrator reads the variant's last
  records, then DELETEs its target, then the namespace is deleted; the
  DELETE is what frees the target's Checker, salt set and Ring, which
  stay outside the PR #43 memory sum while loadgen holds them.

**Why the original relabel-in-place design was dropped:** it could never
have worked. A Service only selects pods in its own namespace, so a
shared front-facing Service could never have picked up pods in a separate
scratch namespace; and changing a Deployment's pod-template labels
triggers a rollout, which would have lost the warm pod anyway.

A per-namespace LimitRange enforces identical resource limits on every
variant. `role: hasher` remains as an inventory label only; it no longer
selects anything. Telemetry is attributed by namespace.

**Variant pods have no egress.** A per-namespace NetworkPolicy denies all
egress and admits ingress only on TCP 8080 from loadgen's pods in
`ek8s-system` (kind's built-in kindnet enforces NetworkPolicy, egress
included: deploy/variant-smoke.sh, run in CI, fails unless a pod in a
variant namespace is refused an external address, the API server's
ClusterIP and the node address it serves on). A wrapper
can read files with `net/http` (section 7, Stage 1 known limit) but cannot
send them anywhere.

**loadgen's pods take no ingress from any pod.** Their NetworkPolicy
(deploy/ek8s-system) has no ingress rule, and allows egress only to TCP
8080 in namespaces labelled `ek8s.io/role: variant` and DNS to kube-dns.
The API server's service proxy still reaches the control port, because
kindnet never applies policy to traffic from a root-owned socket on the
node. No policy can single the proxy out, so control-port ingress is
"node root processes only": the API server, the kubelet, the control plane
and root hostNetwork pods, all of which can already exec into loadgen.
The bypass covers only packets made on loadgen's own node, which holds
because the cluster has one node. The control port is the only one
reachable because loadgen listens on no other, not because of the policy.
deploy/variant-smoke.sh, run in CI, fails unless the proxy reaches a
stand-in on the control port, a variant pod and an `ek8s-system` pod
without `app=loadgen` are refused there, and the stand-in reaches a
variant by Service DNS but not an external address, an outside DNS server
or the API server.

**Known limit: kindnet's policy enforcement fails open.** kindnet runs
its policy queue FailOpen: if kindnet's userspace stops reading the
queue, packets pass unchecked, so every NetworkPolicy fails open. That
includes variant egress denial, now a containment measure only: fitness
integrity no longer relies on it, since no `/verify` pair is shared
across variants (section 10). So at the start of each half the
orchestrator calls loadgen's control API for the policy canary: loadgen
makes one connection attempt its own policy must refuse and returns
connected, refused or error. It dials the `-canary-addr` flag, the API
server's ClusterIP, which must be an IP literal (a failed lookup would
read as refused), with a 1s timeout. The result is refused only for a
timeout, ECONNREFUSED, EHOSTUNREACH or ENETUNREACH. EAGAIN, which on
connect is local, and any other error give error, which the orchestrator
treats like connected: enforcement is not shown to be on, and the half is
excluded. The dial is detached from the request's context, so a caller's
deadline cannot read as refused. The canary is allowed during a run.
The orchestrator also checks the kindnet DaemonSet is Ready, as a cheap
first look; Ready alone does not show the queue is being read.

**Observation windows and gaps.** Traffic flows only during observation
windows; the gaps between windows carry no load. Builds, gate runs and
Stage 5 happen in gaps, so they never overlap a scored window. A window
starts only when its `/verify` pairs are ready (section 10), and full
verification and scoring follow it, in the next gap (section 8). There is
no warm-up exclusion: every variant starts each window from an idle gap,
so cold start is scored, identically for every variant.

The gap runs in a fixed order: verify the previous window's pairs on the
host, score it, generate the next window's `/verify` pairs while the
mutation runs, and start the gate only after pair generation finishes.
Host Argon2 saturating the cores during gate Stages 2-4 (host) or Stage 5
could push a correct candidate past the hang-timeout, failing it for the
host's reasons.

**Stress state is owned by the orchestrator.** It scales the stress-ng
Deployment on for each stress half and off for each baseline half, and on
in a gap before calling gate Stage 5 (section 7). A stress half starts
once the stress-ng pods are Running; a window, and so its baseline half,
starts only once the stress-ng pods are gone. Details are settled with
the orchestrator design.

**Excluded windows.** The orchestrator marks a window excluded from
scoring for a loadgen restart during it (section 10) or a loadgen
exclusion trigger (lag, a skipped send, a loadgen fault, a ring gap, a
policy canary that connects). Probe Events (readiness failures,
restarts) are diagnostics only, never a reason to exclude: a variant's
own readiness behaviour is part of what is scored.

**Readiness probe.** `/readyz` every 2s, `timeoutSeconds` 2,
`failureThreshold` 3. Under a 1-CPU limit with hashes in flight a 1s
timeout can flap a loaded variant out of its own Service; 3 failures at a
2s period react in about 6s, past `cmd/service`'s 5s drain delay, so the
probe no longer sees the drain's 503 in time to be what removes a
terminating pod from rotation. The pod's deletion does that instead:
deploy/variant-smoke.sh, run in CI, fails unless a deleted pod's
EndpointSlice entry turns `ready=false`, `terminating=true` within 2s
while its sibling stays ready.

**Readiness dodging punishes itself.** A variant that pulls all its pods
from rotation empties its own Service, and its own traffic fails. Within
a variant, one replica going unready shifts load to its sibling replicas
-- a legitimate strategy, scored fairly, because the variant bears all of
its consequences.

**Live population cap: 3.** A resource/deployment constraint given the
8-core/16GB workstation, not a statement about genome-pool size (see
below). HPA `maxReplicas` per variant: 2 (4 variants x 2 replicas during
probation is the binding case for the section 2 budget).

### Generational process (steady-state, single active mutation at a time)

1. **Select parent** via tournament selection (k=2, see Selection below)
   from the full genome archive (not only the current live 3 -- see
   below).
2. **Mutate**: invoke the driver (section 12) on the parent's wrapper
   source.
3. **Build**: `go build` -> `docker build` -> `kind load docker-image`,
   in a gap (see Observation windows and gaps above), as is step 4.
4. **Gate** (section 7): fail at any stage -> log which stage, discard,
   return to step 1 (cap consecutive failures before flagging the lineage
   stalled -- do not halt the whole run on one bad mutation).
5. **Provisional promotion**: pass -> mark live and name the variant in
   window starts (see Namespace lifecycle above). Population is briefly
   4, not 3 (see Probation below).
6. **Observe**: one full observation window (baseline + stress-active
   halves), same shape as any live variant's.
7. **Verify, score & cull**: after the window, fully verify its hashes on
   the host (section 8). A variant with a confirmed integrity failure
   (section 9) is culled before scoring and is never a parent. Then score
   the remaining live variants together on that window's data and cull
   the lowest scorer back to 3; if the integrity culls already restore
   the cap, there is no further cull.
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
- A variant disqualified for an integrity failure (section 9) is archived
  with its record but is not eligible as a parent.

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
  `r` and the 0.5 x C0 factor, the 50/48/2 request mix and the
  match:mismatch split, the Stage 5 negative share (5%), loadgen's
  750m/256Mi sizing, the lag thresholds (p99 10 ms, any 100 ms), the 5%
  lag-exclusion trigger for the static CPU manager, 3 consecutive excluded
  halves, readiness-probe timings, K = 3 fully verified scored windows per
  variant, the `/hash` repeat share (1%), the `/hash` password floor (16
  characters), the `/hash` password length distribution (uniform, 16 to
  1024), the sampled verification share after K (10% of a variant's
  hashes per window), the Stage 5 burst factor (2 x C0) and duration (D =
  30 s), the 200 ms run start lead, the 1e-9 bound on a stream running out
  of `/verify` pairs, the 10 s shutdown bound, and the control API
  server's 2-minute read and write timeouts) are all explicitly
  starting guesses pending real telemetry, not derived values -- expect to
  tune all of them once the loop is running.
- Workstation measurements still to take (they bound the gap between
  windows): the time to generate one window's `/verify` pairs on the host,
  and the time to fully verify one window's hashes on the host after it
  (section 8).

**Accepted limits (fitness integrity, section 7 principle):**
- A cheat deliberately delayed past a variant's first K scored windows
  can go uncaught: after K, full verification is a sample. A sampled
  catch still disqualifies (section 9).
- A mutant can detect the gate (for example from build info, environment
  or CPU counts; Stage 4 runs under `-race`). Gate-only checks bind only
  mutants that do not detect it; the live checks (sections 8-10) are what
  protect fitness. No further work goes into closing gate tells.
- A mutant can log the plaintext passwords it receives (the driver never
  passes candidate output into a prompt, docs/STATUS.md Parked).
- A loadgen restart during a window loses that window's unverified
  hashes; the window is excluded (section 10), and the restart empties the
  salt sets.
