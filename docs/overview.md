# eK8s: project overview

An introduction for readers who know Linux and Kubernetes basics but
not this project or Go. Implementation detail is left out.

[DESIGN.md](../DESIGN.md) is the authoritative design: if this page
and DESIGN.md disagree, DESIGN.md wins. For where the work is right
now, see [STATUS.md](STATUS.md).

## 1. What this is, in one paragraph

eK8s applies *evolution* to a small piece of software running on
Kubernetes. Each "generation", an AI (Claude, run from the command
line) proposes a change to a web service's code. An automated gate
throws out any version that is broken or insecure. Versions that pass
run side by side on a local Kubernetes cluster under real load,
including deliberate resource contention. They are scored, and the
weakest is removed. Over generations, the population should drift
toward code that handles load better. The AI replaces the blind random
mutation of a classic genetic algorithm with changes that understand
the code.

Everything runs on one Linux workstation (8 cores, 16 GB RAM) using
kind (Kubernetes-in-Docker). No cloud and no GPU are used.

## 2. The service being evolved

The service is a password-hashing API, the kind a login system would
call:

```
  POST /hash     {"password": "..."}               -> {"hash": "..."}
  POST /verify   {"password": "...", "hash": "..."} -> {"match": true}
  GET  /healthz  process alive?
  GET  /readyz   ready for traffic?
```

Hashing uses Argon2id, a deliberately slow, memory-hungry algorithm.
Each hash takes about 100 ms of CPU and 19 MiB of memory. That cost
makes the service easy to overload, and *how it copes with overload*
is what evolves.

## 3. What can evolve and what cannot

The code is split into three layers. Only the middle one is ever
changed by the AI.

```
  HTTP request
       |
       v
  +---------------------------------------------+
  | cmd/service  (FIXED)                        |
  |  routing, health checks, clean shutdown     |
  |  +---------------------------------------+  |
  |  | wrapper  (EVOLVES)                    |  |
  |  |  queueing, concurrency limits,        |  |
  |  |  timeouts, load shedding (503s)       |  |
  |  |  +---------------------------------+  |  |
  |  |  | hashcore  (FIXED, HIDDEN)       |  |  |
  |  |  |  Argon2id at fixed strength     |  |  |
  |  |  |  t=2, m=19456 KiB, p=1          |  |  |
  |  |  +---------------------------------+  |  |
  |  +---------------------------------------+  |
  +---------------------------------------------+
```

**Why the security core is locked away.** Evolution optimizes whatever
it is rewarded for. If the hashing itself could change, the fastest
"winner" would simply weaken it, for example with fewer iterations,
less memory, or a sloppy comparison that leaks timing. So the hashing
code is never even visible to the AI. The wrapper sees only a
two-function interface: `Hash` and `Verify`. The strength settings are
the OWASP minimum, and anything that does not match them exactly is
rejected.

## 4. Inside the wrapper: what is actually evolving

### Its job

Every request passes through the wrapper. For each one, it decides
three things:

- whether to call the expensive hashing core at all
- when to call it
- what to tell the client if it cannot serve the request in time

In other words, it is the service's traffic controller. The decision
points every wrapper has look like this:

```
 request
    |
    v
 [check input] -- bad JSON, empty or huge -----> 400
    |              password
    v
 [admission] is a hashing slot free?
    |  yes                      |  no
    |                           v
    |                    [wait in a queue]
    |                     |            |
    |               slot frees   queue full, or
    |                     |      waited too long
    v                     v            |
 [call hashcore: Hash or Verify]       v
    |                          503 + Retry-After
    v                          ("busy, retry soon")
 200 {"hash": ...}
  or {"match": ...}
```

The starting version, "generation 0", is one simple, hand-written
instance of this shape (see "Generation 0" at the end of this
section).

### The knobs evolution can turn

| Decision | Examples of what a mutant might try |
|----------|-------------------------------------|
| Concurrency limit | how many hashes run at once per pod; fixed or derived from the pod's CPU/memory limits |
| Queue | how many requests may wait, and for how long |
| Queue order | oldest first, or newest first (under overload, newest-first keeps some answers on time) |
| Shedding | when to give up early with a 503, and what Retry-After to suggest |
| Readiness | when `/readyz` should say "not ready", so Kubernetes sends traffic to sibling pods |
| Adaptivity | adjusting limits on the fly from observed latency |

The wrapper is given three inputs to work with:

- the pod's CPU and memory limits, read from the downward API
- the time ceiling a response must beat to count
- the two-function hashing interface

All tuning values live as literals in the wrapper's source code. That
source code is the "genome".

What it cannot change: the API contract, the hashing, the health
check, the port, or the shutdown behavior. It also cannot add files.
The AI can only edit the existing ones.

### Why this is a real optimization problem

The right amount of concurrency is a balancing act:

```
 hashes in flight per pod
 too few              sweet spot              too many
 |--------------------|-----------------------|
 CPU sits idle;       CPU busy,               CPU oversubscribed:
 requests wait        short queue,            every request slow.
 for no reason        fast answers            Memory: 19 MiB each,
                                              so too many -> OOMKill
```

A worked example, with **illustrative numbers** (real pod limits are
not set yet):

- A pod is limited to 1 CPU and 256 MiB.
- At about 100 ms of CPU per hash, it can finish at most about 10
  requests per second.
- If 15 per second arrive, 5 per second cannot be served.
- If they are queued, the queue grows by 5 every second. Soon every
  answer misses the time ceiling, which counts as "failed".
- Shedding the excess early with a 503 counts as "graceful_shed",
  which earns partial credit.
- Memory caps concurrency too: 256 / 19 is roughly 13 hashes at once,
  before Go's own overhead. More than that and the pod is OOM-killed.

The sweet spot also *moves*. When the noisy-neighbour pod (stress-ng)
steals CPU, a limit tuned for quiet conditions suddenly admits too
much. A good wrapper adapts. That is exactly what the contention half
of the scoring rewards.

### Generation 0: the starting genome

The wrapper source, as of commit a2d1ec1 on main, is four files:

| File | Role | Evolvable? |
|------|------|------------|
| `doc.go` | package description | yes (irrelevant) |
| `hasher.go` | the two-function interface to the core | no, pinned byte-for-byte |
| `tunables.go` | the knobs, as named constants | yes |
| `server.go` | the request-handling logic | yes |

`tunables.go` is the obvious control panel, but the genome is really
`tunables.go` plus `server.go`. A mutant asked for a structurally
different approach may rewrite `server.go`, as long as it keeps the
function signatures the fixed `cmd/service` calls.

Generation 0 is a **fixed-size semaphore with a time-limited wait**:

```
 request
    |
    v
 decode JSON -------- bad JSON / trailing data --> 400
    |
    v
 password 1-1024 bytes? (and hash non-empty,
 for /verify) ------------- no --------------> 400
    |
    v
 acquire a slot: wait up to acquireWait
    |            |                  |
  got one     timed out        client gave up
    |            |                  |
    |            v                  v
    |      503 + Retry-After: 1   write nothing
    v
 call Hash / Verify  (cannot be cancelled)
    |
    v
 release slot
    |
    v
 200 {"hash": ...} / {"match": ...}
 (a Verify error -> 400, never a match)
```

There is no explicit queue. Waiting requests are just blocked
goroutines, Go's lightweight threads. Their number is unlimited, but
each waits at most `acquireWait`.

The number of slots is computed once, at startup. GOMAXPROCS comes
from the pod's CPU limit:

```
 slots = max(1, min( GOMAXPROCS,
                     (memLimit * 1/2) / 20 MiB ))
```

Two examples with illustrative pod limits:

- 4 CPUs and 1 GiB gives 4 slots. CPU is the binding limit.
- 4 CPUs and 128 MiB gives 3 slots. Memory is the binding limit.

| Knob (`tunables.go`) | Gen-0 value | Meaning |
|----------------------|-------------|---------|
| `perHashMemory` | 20 MiB | memory estimate per hash (19 MiB + headroom) |
| `memoryFraction` | 1/2 | share of the pod's memory limit for hashing |
| `acquireWait` | HangTimeout / 4 | wait for a slot before shedding |
| `retryAfterSeconds` | 1 | "try again in" hint sent with a 503 |

How generation 0 compares with the knob table above:

| Knob | Generation 0 |
|------|--------------|
| Concurrency limit | fixed at startup from CPU and memory |
| Queue | implicit; no length cap, only a time cap |
| Queue order | not controlled by the code |
| Shedding | only after `acquireWait` runs out |
| Readiness | always "ready" |
| Adaptivity | none (the CPU-limit input is left unused, for mutants) |

Likely room for evolution to improve on:

- **Uncancellable work.** A hash cannot be stopped once started. If
  the client times out, the hash still runs and holds its slot.
  Under overload, generation 0 can spend capacity on answers nobody
  receives.
- **A fixed limit under contention.** When stress-ng steals CPU, each
  hash takes longer, and more waiters time out. Adaptive limits or
  newest-first admission are natural candidates.
- **Unused readiness.** Reporting "not ready" when saturated would
  let Kubernetes send traffic to a sibling replica.
- **Static timing.** One `acquireWait` value is used regardless of how
  long hashes are currently taking.

## 5. The evolution loop

There is one mutation at a time. The live population is 3 variants,
briefly 4 while a newcomer is on probation.

```
        +-------------------------------------------+
        |                                           |
        v                                           |
  [1] Select parent                                 |
      pick 2 at random from the archive of          |
      past winners; keep the better one             |
        |                                           |
        v                                           |
  [2] Mutate                                        |
      Claude edits the parent's wrapper code        |
        |                                           |
        v                                           |
  [3] Build                                         |
      go build -> docker build -> kind load         |
        |                                           |
        v                                           |
  [4] Gate (5 checks) ---- fail ---> discard ------>+
        | pass                                      |
        v                                           |
  [5] Go live (population now 4)                    |
        |                                           |
        v                                           |
  [6] Observe                                       |
      normal load, then load + noisy neighbour      |
        |                                           |
        v                                           |
  [7] Score all 4; cull the lowest ---------------->+
```

The archive holds every version that ever passed the gate, not just
the live ones. Parents are drawn from a capped pool of about 15-20 of
them: the oldest drop out, except the best ever. That keeps diversity,
so the search does not just hill-climb from the current best. If no new best appears for a while,
the AI is asked for a *structurally* different approach instead of a
small tweak.

The run stops when progress stalls for good, or when an AI-usage
ceiling is reached.

## 6. The gate: five checks, zero tolerance

Any single failure rejects the candidate. There is no partial credit.

| Stage | Where | What it checks |
|-------|-------|----------------|
| 1 Build/boot | local | It compiles and starts, uses only allowed libraries (it must not sneak in its own crypto), and the core interface file is unchanged |
| 2 Interop | local | Its hashes verify with a reference library and vice versa; bad input gets a clean 400, never a fake "match" |
| 3 Strength | local | Every hash it produced uses exactly the fixed Argon2id settings |
| 4 Concurrency | local | Under parallel requests with the race detector on, no answers leak between requests |
| 5 Under pressure | kind | It runs in its own namespace with a noisy neighbour, gets a short burst of load, and the pod must not crash and no response may be "failed" |

Stages 1-4 run on the host today. The planned direction (not built) is
to move Stages 2-4 into a pod in the cluster, so the host never runs
candidate code; the build and the static checks stay on the host.
The gate is a filter, not the defense: it only keeps obviously broken
candidates out. What protects the scores is checking live answers
(section 8), which a candidate cannot tell apart from real traffic.

## 7. How responses and variants are scored

Every response from a variant is put in one of three buckets by a
single shared judge (the *classifier*). The gate and the scoring use
the same judge, so they can never disagree about what "good" means.

```
  response
     |
     +--> success        200, in time, AND correct
     |                   (verify answer matches known truth;
     |                    hash is genuine, fresh, correct strength)
     |
     +--> graceful_shed  503 + valid Retry-After, in time
     |                   ("I'm busy, try again" -- done properly)
     |
     +--> failed         anything else: errors, timeouts,
                         wrong answers, malformed replies
```

A bare 503 does not count as graceful. Kubernetes itself returns one
when a Service has no ready pods, and that must not be mistaken for
deliberate shedding. Hence the Retry-After requirement.

**Fitness.** Each variant is measured in two halves, a normal half and
a contention half. Three numbers are taken in each:

- success rate
- p95 latency (the slow tail, not the average)
- CPU used per successful request

In the contention half, a graceful 503 earns partial credit, so
shedding load well is better than falling over. The final score is
the *worse* of the two halves. A variant that shines normally but
collapses under pressure is scored by the collapse.

Scores use fixed absolute scales, not "relative to the others".
That way a score from generation 5 can be compared fairly with one
from generation 50.

## 8. How answers are checked: salts and known answers

The judge has to know whether each answer is *correct*, not just
fast. How it does that differs between the two endpoints, and the
difference comes down to the salt.

### What a salt is

A salt is 16 random bytes that the hashing core generates fresh for
every `/hash` call. It is mixed into the Argon2 computation, then
stored in plain view inside the resulting hash string:

```
 password "hunter2"      random salt (new every call)
        \                 /
         v               v
   [ Argon2id  t=2  m=19456  p=1 ]
                 |
                 v
 $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
  algorithm  ver   cost settings  16 bytes 32 bytes
                                  base64   base64
```

So hashing the same password twice gives two *different* strings:

```
 "hunter2" -> $argon2id$...$c2FsdEFhYWFhYWFhYWFh$Xk3...
 "hunter2" -> $argon2id$...$b3RoZXJCYmJiYmJiYmJi$p9Q...
              (illustrative values)
```

Salts exist for two reasons:

- An attacker who steals a password database cannot precompute one
  "password -> hash" table and look everyone up at once.
- Two users with the same password do not get matching hashes.

Each hash has to be attacked separately.

Verifying needs nothing stored elsewhere, because the hash string
carries everything required:

```
 password + hash string
        |
        v
 read settings and salt out of the string
        |
        v
 re-run Argon2id(password, that salt)
        |
        v
 same key as in the string?  -> match / no match
```

### Checking /hash: no precomputed answers needed

Because of the salt, there is no single "expected" hash to
precompute. A correct answer is any hash that checks out. The load
generator sends a random password (16 characters up to 1024 bytes) and
checks the reply in two steps:

```
 loadgen: random password P --> /hash --> hash string H
                                              |
   every response (cheap, during the window): |
     - is H well-formed?                      |
     - exact t=2, m=19456, p=1?               |
     - salt not seen before from this       <-+
       variant (kept in loadgen's memory)?
   after the window, on the host (costly):
     - does Verify(P, H) say match?
```

The load generator never runs Argon2 itself; it only keeps the
(password, hash) pairs it saw. After each window the orchestrator
verifies them on the host with the real core, before the variant is
scored. For a variant's first K scored windows (to start, 3) every hash
is verified. After that only a sample is (to start, 10% of the
variant's hashes per window). Every hash in the Stage 5 burst is
verified too.

The salt doubles as a cheat detector. Every genuine hash carries a
brand-new random salt. With 16 random bytes, an accidental repeat is
effectively impossible. So a repeated salt is strong evidence that the
wrapper replayed a stored result instead of calling Argon2.

A salt check alone would miss a wrapper that caches "password -> hash",
because each password would still get a fresh-looking answer the first
time. So about 1% of `/hash` requests deliberately repeat a password
already sent to that variant in the same window (in a gate burst, the
two may go out in either order). A correct service hashes it again and
returns a different salt; a caching one returns the same hash, and is
caught.

### Checking /verify: fresh known answers

`/verify` is different. To judge a "match: true" reply as right or
wrong, the judge must already know the true answer. Before each
window the host generates fresh pairs, using the real hashing core:

- a *match* pair: a fresh password and a hash of it
- a *mismatch* pair: a fresh password and a random, well-formed hash
  that cannot belong to it

The load generator is handed the pairs and sends each one exactly once,
to one variant. Nothing repeats and nothing is shared between variants,
so a variant cannot memorize "this pair -> match" and skip Argon2. The
fixed pool of test passwords and hashes is used only by the gate, never
in live traffic. If the load generator restarts, it gets a new batch.

A small share of live traffic (and at least 5% of the Stage 5 burst) is
deliberately invalid: a malformed hash, or a well-formed one with the
wrong settings. The only acceptable answers are a 400 or a polite
"busy"; any success is a failure. Live traffic also sends three kinds
of bad `/hash` requests: an empty password, an oversize password and
invalid JSON. Each must get a 400.

## 9. Why so much anti-cheating, if the core is locked away?

### The short answer

Locking the core stops the AI from changing *how* a hash is computed.
It does nothing to stop the wrapper from simply *not computing one*.
The wrapper sits between the client and the core, and it writes every
reply:

```
            what the client sees
                    ^
                    |
 client ---> [  wrapper  ] ---> [ hashcore ]
              writes every       runs only if
              reply              the wrapper
                                 calls it
```

Think of a bank teller who cannot touch the vault's lock. The teller
still decides whether to walk to the vault at all, and what to tell
the customer afterwards. "Yes, your deposit is in" costs the teller
nothing to say.

### Two different threats

The protections fall into two groups, and they guard different things:

```
 threat                       when         defense
 ---------------------------  -----------  -----------------------
 the AI reads or edits what   mutation     hidden core, sandbox,
 it should not                time         Read/Edit tools only

 the code the AI wrote        run time     the gate, and the judge
 misbehaves when it runs                   checking every response
```

Sections 3 and 11 cover the first group. This section is about the
second. The hidden core keeps the *algorithm* honest; only checking
the *answers* keeps the *service* honest.

### Why a variant would cheat at all

**1. The incentive is enormous.** Argon2 is almost the whole cost of
a request. Fitness rewards speed, success rate and low CPU. Skipping
the hash is a bigger improvement than any honest tuning could ever
produce. Selection keeps whatever scores highest and does not care
why.

**2. No malice is needed.** Caching results, memoizing answers and
fast-path shortcuts are *good* engineering in most services. Asked to
"improve latency under load", a capable model may propose them in
good faith. Here they are disastrous:

- a cached hash means two users share a salt
- a cached verify answer means logging people in without checking
  their password

Escalation mode makes this more likely, not less, because it
explicitly asks for structurally different ideas.

**3. Cheating can be conditional.** A variant could take the shortcut
*only when overloaded*. It would pass every quiet-conditions test,
then win in exactly the contention half, which is where fitness is
decided (the worse of the two halves). This is why Stage 5 fully
checks every response under contention, and why the live contention
half is judged as strictly as the normal half. A variant could also
cheat only when it can tell it is *not* being tested; invalid hashes
in live traffic close that gap (see "Loopholes" below).

**4. One surviving cheat poisons the whole run.** It would become the
best-ever variant and be picked as a parent again and again, so its
descendants inherit the trick. The experiment's results would be
meaningless, and in a real deployment it would be an authentication
bypass.

### The cheats and what catches them

| Cheat | Why it would score well | What catches it |
|-------|-------------------------|-----------------|
| Always answer "match" on /verify | no Argon2, instant | the judge checks answers against pairs it built itself, including deliberate mismatches |
| Turn a core error into "match" (fail open) | looks robust | gate Stage 2 sends bad and wrong-strength hashes; the answer must be 400; the same invalid hashes are sent live and in Stage 5 (see "Loopholes") |
| Return a made-up hash string | no Argon2 | format and strength checks on every response; full verification on the host after each window (every hash for a variant's first K windows, a sample after) and for every Stage 5 hash |
| Re-send earlier hashes | no Argon2 | salt reuse is tracked while the load generator serves the variant; deliberately repeated passwords catch a password -> hash cache |
| Bring its own, weaker Argon2 | faster | Stage 1 library allowlist; Stage 3 exact strength check |
| Cheat only when overloaded | passes quiet tests | Stage 5 and the contention half use the same strict judge |
| Pull all its pods out of rotation | avoids load | its own dedicated traffic fails, and that is scored |
| Answer "busy" (503) to everything | never slow | only partial credit, and its success rate collapses |

**Why some checks are sampled, and when they run.** Fully verifying a
hash costs one Argon2 run, which would distort the measurements if it
shared the node with a window. So traffic flows only during observation
windows, and the gaps between them carry no load. Builds, gate runs and
Stage 5 happen in the gaps, and so does full verification, after a
window and before scoring. During a window only the cheap checks run on
every response: format, strength and salt reuse. A variant's first K
scored windows are verified in full; after that, a sample. A cheat
deliberately held back past those windows could slip through. That
limit is accepted, and a sampled catch still disqualifies. A variant
that fails an integrity check is culled at the end of the window,
before it is scored.

### Loopholes: decided, not yet built

The four loopholes tracked earlier now have a decided fix in
[DESIGN.md](../DESIGN.md); none is built until the load generator and
scoring are:

- **Reusing undelivered work:** a hash computed for a request that
  timed out could be handed to a later request. Any integrity failure
  (a wrong or reused hash, a fake "match", a success on an invalid hash)
  disqualifies the variant: the first one confirmed on the host means
  the variant is culled at the end of the window, before scoring, and
  never becomes a parent. It counts
  even when the answer arrives past the time ceiling: a slow cheat is
  still a cheat.
- **Memorizing verify answers:** fixed by fresh pairs, each sent once.
- **Replaying pool hashes:** fixed by keeping the pool out of live
  traffic; a hash replayed as a `/hash` answer fails full verification,
  since it does not match the password sent.
- **Behaving differently outside the gate:** a candidate can find
  small clues that it is running inside the gate (for example, no
  resource limits, or the sandbox's process list). It could return a
  clean 400 for bad hashes during the gate, but turn core errors into
  "match" once live. Fixed by sending invalid hashes in live traffic and
  in Stage 5, so such a candidate fails there as well. A candidate that
  detects the gate in other ways is an accepted limit: the live checks
  are what protect the scores.

## 10. Cluster layout

```
 +------------------ kind (single node) ------------------+
 |                                                        |
 |  ns ek8s-v12     ns ek8s-v15     ns ek8s-v17           |
 |  Deployment+HPA  Deployment+HPA  Deployment+HPA        |
 |  Service         Service         Service               |
 |     ^               ^               ^                  |
 |     | rate r        | rate r        | rate r           |
 |  +--+---------------+---------------+--+               |
 |  |         load generator             |               |
 |  +-------------------------------------+               |
 |                                                        |
 |  stress-ng        noisy neighbour (contention)         |
 |  metrics-server   CPU / memory telemetry               |
 +--------------------------------------------------------+
```

- Each variant gets its own namespace and identical resource limits.
- A variant's pods are meant to have no outbound connections (a network
  policy denies all egress) and to accept traffic only from the load
  generator; a CI smoke test checks this.
- One load generator, running for the whole run, serves every
  variant's stream; it sits outside the variants' namespaces and holds no
  cluster credentials.
- Each variant gets its own fixed-rate traffic stream. There is no
  shared load balancer splitting traffic, so variants never compete
  for requests, only for the node's resources.
- Traffic flows only during observation windows; the gaps between
  windows carry no load, and builds, gate runs and Stage 5 happen in
  them. A variant starts every window from idle, so cold start is
  scored the same for everyone.
- The load generator is *open loop*. It sends at a fixed rate whether
  or not the service keeps up, so overload shows up as real queueing
  and latency instead of being hidden.
- A newcomer's namespace is created by gate Stage 5. If the newcomer
  passes, it simply goes live in place with no redeploy (the pod is idle
  until its first window, which starts cold like every other). When a
  variant is culled, its namespace is deleted.

## 11. Keeping the AI contained

The AI that edits code runs in a tight sandbox. This guards the
mutation step; the gate and judge (section 9) guard the code it
produces.

```
 orchestrator
     |  launch directly (no shell)
     v
 +------------- bubblewrap sandbox --------------+
 |  claude -p  (tools: Read and Edit only)       |
 |  sees: a scratch copy of the wrapper only     |
 |  no repo, no hashing core, no kubeconfig,     |
 |  no docker socket, no stray credentials       |
 +-----------------------------------------------+
     |  copy back only the existing wrapper files
     v
 candidate -> build -> gate
```

Each call is fresh. The AI gets no memory between generations, so the
driver hands it four things:

- the parent's score breakdown, showing which number is weak
- a short summary of the last 3 attempts
- the interface it must use
- the time ceiling a response must beat, which it cannot otherwise
  see

It is also asked to explain its change. That explanation becomes the
"last 3 attempts" summary for later generations.

It is told explicitly never to touch hashing or comparison logic. That
instruction is a request, not a guarantee, which is why sections 6
to 9 exist.

## 12. Where the project is now

For the current state, see [STATUS.md](STATUS.md).

## 13. Expected outcome

The search space is a bounded set of tuning choices around
concurrency and backpressure, not open-ended invention. The expected
result is improvement over a handful of generations followed by a
plateau. That is enough to show the loop works end to end: AI-guided
mutation, a strict safety gate, and selection driven by real cluster
telemetry.
