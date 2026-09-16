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
  mutants before they reach a live Deployment. Kubernetes' own mechanisms
  (readiness probes, HPA-style scaling, kube-proxy endpoint splitting)
  provide continuous, fitness-proportional selection among viable variants
  already live.
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

Two independent layers, not one:

1. **Tool-availability restriction on the mutation driver** (see section 9)
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
   implementation, because the bytes are not in the accessible tree.

**Backstop, not primary defense:** Stage 2 of the correctness gate (section
7) would almost certainly catch a mutant that tried to bypass the fixed
core entirely (e.g. reimplementing hashing from scratch) -- a from-scratch
implementation would not interoperate with the real reference library. This
is a safety net for something the design is meant to prevent outright, not
the mechanism relied on to prevent it.

## 5. Repo layout

```
repo/
  cmd/service/           # entrypoint: thin main.go -- config/flags, wires
                          # fixed core + wrapper, HTTP routes, healthz/readyz,
                          # graceful shutdown. Never touched by mutation.
  internal/hashcore/     # fixed core. Never in mutation scope.
  internal/wrapper/      # evolvable: concurrency/backpressure/timeouts.
  gate/                  # correctness gate, five ordered stages.
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

## 6. Fixed-core parameters

Argon2id, OWASP Password Storage Cheat Sheet floor (verified current as of
2026): **t=2 (iterations), m=19456 KiB / 19 MiB (memory), p=1
(parallelism)**. This is the cost-parameter floor the gate's Stage 3
enforces as a minimum, not a fixed value -- a mutation could legally
increase cost, never decrease it below the floor.

## 7. Correctness gate

Five ordered stages, each zero-tolerance (a single failure at any stage
fails the gate outright -- no partial-credit / statistical acceptance
sampling anywhere in the gate). A shared three-bucket response classifier
(section 8) is used by both Stage 5 and the fitness function, so "what
counts as graceful degradation" cannot silently diverge between the two.

### Stage 1 -- Build / boot (local)

Two sub-checks, kept distinct in the lineage log because they fail for
different reasons:
1. `go build` succeeds, then the compiled binary boots as a bare local
   process and its health endpoint responds.
2. `docker build` + `docker run` succeeds locally, health endpoint
   responds from the container.

No kind involved.

### Stage 2 -- Interoperability (local)

Tests that the mutated wrapper has not corrupted data in flight between
its concurrency/request-handling layer and the (unchanged) fixed core --
not re-validating the hashing algorithm itself. Four assertions, all
zero-tolerance, using a shared password/hash test-vector pool (also used
by the load generator, section 10, so gate-time and run-time correctness
share one definition of "correct"):

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

### Stage 3 -- Cost-parameter floor (local)

Parse the encoded Argon2 string from Stage 2's calls and assert
`m >= 19456, t >= 2, p >= 1`. Pure assertion against data already
produced by Stage 2 -- no separate HTTP round trip needed.

### Stage 4 -- Concurrency safety (local)

Fire N concurrent requests with distinct passwords, assert no cross-talk
(password A's result never validates against password B). Build a
separate `-race`-instrumented binary for this stage only -- race-detector
overhead would distort Stage 5's timing behavior, so it is never the
image that gets deployed.

### Stage 5 -- Degradation under contention (kind, scratch namespace)

The one stage that must run on the cluster: it tests behavior under
resource scarcity *enforced by the kubelet* (cgroup limits), which a local
process cannot faithfully simulate without re-implementing what k8s
already provides.

Procedure:
- Deploy the candidate into an ephemeral scratch namespace with the same
  resource requests/limits it would get in production, but **not** yet
  carrying the `role: hasher` label -- it must not join the shared Service
  or receive live traffic before passing.
- Wait for the scratch Deployment to report Ready before sending a single
  request (prevents a startup race from being misclassified as a gate
  failure).
- Point a short, bounded load-generator burst (tens of seconds, not the
  multi-minute observation-window duration -- Stage 5 only needs a
  boolean pass/fail, not statistical confidence) directly at the scratch
  Service, bypassing the shared Service so this traffic never mixes with
  anything currently live.
- The existing stress-ng Deployment provides contention (kind is
  single-node, so contention is a global resource -- no second stress-ng
  instance needed).

**Pass condition (two independent, zero-tolerance assertions):**
1. Pod health: `restartCount == 0` and no container status reason of
   `OOMKilled`, `CrashLoopBackOff`, or `Error`.
2. Every observed response classifies as `success` or `graceful_shed` via
   the shared three-bucket classifier (section 8) -- never `failed`.

Pass iff both hold. On pass, **relabel the scratch Deployment in place**
to join the shared Service's selector -- no redeploy, the image is already
loaded and the pod already warm.

**Known adjacent gap, deliberately not solved in v1:** live variants share
the single kind node with whatever else is running. A Stage 5 probe (and a
gate build's CPU spike) transiently affects cluster-wide resource
availability. The orchestrator must exclude telemetry from the affected
window from live-population scoring rather than let it become unexplained
noise (see section 9).

## 8. Shared three-bucket response classifier

One function, used by both Stage 5 (boolean: any `failed`?) and the
fitness function (graded: what fraction in each bucket?).

- **`success`** -- 2xx status, arriving within the hang-timeout ceiling
  (same constant as the fitness function's latency-normalization ceiling,
  section 9 -- one number serving both purposes rather than two thresholds
  that could drift apart).
- **`graceful_shed`** -- HTTP 503 **and** a `Retry-After` header present.
  Both parts of the contract required; a bare 503 (e.g. from the Service
  reporting no ready endpoints) must not be indistinguishable from the
  wrapper's own deliberate load-shedding.
- **`failed`** -- anything else: connection refused/reset, response
  exceeding the hang-timeout, a 5xx without the `Retry-After` contract, a
  malformed response.

For `verify` calls specifically, "success" additionally requires the
*objectively correct* accept/reject determination against the shared
test-vector pool's ground truth -- a fast, well-formed response that
returns "valid" for a should-reject pair is a `failed` classification
regardless of latency, not a success. (This closes a specification-gaming
risk: without ground-truth checking, a mutant could shed queue pressure
under contention by short-circuiting verification to always return
success, which would show up as *high* success_rate in exactly the
stress-active half where the fitness signal is most trusted.)

## 9. Fitness function

Computed per half-window (baseline / stress-active):

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
2. Tournament selection (section 10) draws parents from an archive that
   spans the whole run's history, not just the current live set -- the
   same cross-time comparability requirement applies.

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
may discard headers by default to keep memory bounded.)

**Request mix -- must include deliberate mismatches, not only valid
credentials.** A shared test-vector pool (also used by gate Stage 2,
section 7) of valid password+hash pairs, plus the same passwords paired
with a different hash to force a should-reject case. Sending only
matching pairs to `verify` would never exercise the rejection path under
load, and is the specification-gaming gap described in section 8.

**Hash-call payloads:** randomly generated passwords of varying length per
request, not a fixed repeated test vector.

**Two operating modes, one generator component:**
- Sustained fixed-rate open-loop generation for the multi-minute
  observation window (baseline + stress-active halves).
- A short, bounded-count burst for the Stage 5 gate probe.
Same request-mix logic and classifier in both; only duration and target
rate differ.

## 11. Orchestrator

### Population topology

Several viable variants run concurrently as sibling Deployments, each its
own Service, sharing a common label (`role: hasher`) so one front-facing
Service's endpoint list pools pods from every live variant. kube-proxy
splits traffic across whichever variants are currently Ready; a variant
with more ready replicas (scaled up by its own HPA) gets a proportionally
larger traffic share. This is the literal "fitness-proportional by native
mechanism" piece. Note: k8s mechanisms (readiness probes, HPA) cull the
obviously broken and provide elastic capacity -- they do not rank viable
variants against each other. Explicit fitness scoring (section 9) is what
does the ranking.

**Live population cap: 3.** A resource/deployment constraint given the
8-core/16GB workstation, not a statement about genome-pool size (see
below). HPA `maxReplicas` per variant: 2-3.

### Generational process (steady-state, single active mutation at a time)

1. **Select parent** via tournament selection (k=2, section 11) from the
   full genome archive (not only the current live 3 -- see below).
2. **Mutate**: invoke the driver (section 12) on the parent's wrapper
   source.
3. **Build**: `go build` -> `docker build` -> `kind load docker-image`.
4. **Gate** (section 7): fail at any stage -> log which stage, discard,
   return to step 1 (cap consecutive failures before flagging the lineage
   stalled -- do not halt the whole run on one bad mutation).
5. **Provisional promotion**: pass -> relabel into the shared Service.
   Population is briefly 4, not 3 (see Probation below).
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

```
claude -p --tools "Read,Edit" --restricted --add-dir <wrapper-dir-only> \
  --model <pinned-model> --permission-prompts none --max-turns <N> \
  --output-format json "<constructed prompt>"
```

**Correction from earlier assumption:** `--allowedTools`/`--disallowedTools`
only govern what runs *without a permission prompt* -- they are additive
on top of Claude Code's default tool set and do not restrict availability.
`--tools` is what actually restricts which built-in tools exist for the
session (`--tools "Read,Edit"`; empty string disables all). `--restricted`
is purpose-built for this kind of harness use ("an evaluation harness
drives claude on a shared machine"): it strips command/code-execution
tools and WebFetch unless named individually in `--tools`, and confines
file tools to the declared working directories. Use both together, plus
`--add-dir` scoped to the wrapper directory only (see section 4 for why
this is necessary but not the primary defense).

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
  stagnation soft/hard thresholds, usage ceiling) are all explicitly
  starting guesses pending real telemetry, not derived values -- expect to
  tune all of them once the loop is running.
