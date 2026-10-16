# Decision log: implementation-level decisions, oldest first (architecture lives in DESIGN.md)

From the PR #34 entry on, each entry ends with
`- Origin: owner | chat | Code`, who first proposed the choice (every
entry is still owner-approved). Origin does not count toward the 5-line
max. Earlier entries are not backfilled.

## 2026-10-14 · PR #32 · Workflow docs start in docs/workflow/ for later extraction; templates are named *.template.md
- Decision: docs/workflow/ holds WORKFLOW.md, chat-instructions.md and templates/*.template.md, linking only within the folder. eK8s's own CLAUDE.md, docs/handoff.md and .claude/agents/reviewer.md stay unchanged. Subdirectory CLAUDE.md files load lazily when Claude touches that subdirectory (code.claude.com/docs/en/memory), and the subagent docs (code.claude.com/docs/en/sub-agents) list the locations subagents load from (managed settings, --agents, .claude/agents/, ~/.claude/agents/, plugin agents/ directories), none of which is docs/, so the .template.md names keep Claude Code from auto-loading them.
- Rejected: a separate repo now (nothing to extract from until the folder settles); templates named CLAUDE.md / reviewer.md in place.
- Reason: the method is product-independent; moving a self-contained folder later is one commit.
- Reverse: delete docs/workflow/ and the workflow/ mentions in CLAUDE.md and README.md layouts.

## 2026-10-14 · PR #32 · chat-instructions.md carries method rules only
- Rejected: also carrying the owner's formatting preferences for shell commands and replies.
- Reason: those are personal, differ per owner, and belong in the owner's account preferences; the method must work for any owner who pastes the block in.
- Reverse: add a preferences section to docs/workflow/chat-instructions.md.

## 2026-10-14 · PR #32 · Every Code prompt carries a Model/Effort/Why line
- Decision: `Model: <x> | Effort: <y> | Why: <one line>` starts each prompt the chat drafts; Haiku/low for information and mechanical edits, Sonnet/medium for judgement on a settled design, Opus/high for design packets and security-relevant work (docs/workflow/WORKFLOW.md sec 7).
- Rejected: one model and effort for the whole project.
- Reason: usage windows are hit even on a small project; matching model and effort to the prompt is the cheapest saving.
- Reverse: drop the rule from chat-instructions.md and WORKFLOW.md sec 7.

## 2026-10-14 · PR #33 · Three concerns: containment, artifact security, fitness integrity; the gate is a filter (DESIGN.md sec 7)
- Decision: the algorithm and its parameters are structural (fixed hashcore, exact params, no crypto imports); the wrapper's other security properties are enforced by integrity checks on live traffic in the cluster; the gate filters broken candidates and need not be indistinguishable from the cluster.
- Rejected: the gate as the security boundary for wrapper behaviour, kept honest by closing every gate tell.
- Reason: a mutant can always find another tell, so gate-only checks bind only mutants that do not look; live checks see what is scored.
- Reverse: DESIGN.md sec 7 principle paragraph edit.

## 2026-10-14 · PR #33 · Traffic only during observation windows; builds, gate runs and Stage 5 in the gaps; cold start scored (amends the PR #26 stress-state entry; DESIGN.md sec 7, sec 11)
- Decision: gaps carry no load; nothing but scored traffic runs in a window, so Stage 5 and gate builds no longer exclude windows (the PR #26 entry's "marks the window excluded" before Stage 5 becomes "in a gap"). No warm-up exclusion: every variant starts each window from an idle gap.
- Rejected: continuous traffic with windows excluded around builds and probes; a warm-up exclusion at window start.
- Reason: no scored window shares the node with a build, gate run or burst, and host-side pair generation and verification get the gap; cold start is part of the behaviour scored, and the same for all. Gap order: verify, score, generate next pairs, then start the gate, so host Argon2 never competes with Stages 2-4 or Stage 5 and fails a correct candidate on the hang-timeout.
- Reverse: DESIGN.md sec 7 Stage 5 and sec 11 edits; restore the exclusion reasons in sec 11.

## 2026-10-14 · PR #33 · /verify pairs generated on the host per window, each sent once to one variant (amends the PR #29 Q5 C fresh-pairs entry and the PR #31 FailOpen entry; DESIGN.md sec 5, 8, 10, 11)
- Decision: match = hashcore.Hash of a fresh password, mismatch = random canonical PHC at exact params with a fresh password, both made on the host before each window (a window starts only when they are ready); a loadgen restart gets a new batch, never the old one. Pool gate-only. No cross-variant reuse in v1, so egress denial is containment only (the PR #31 entry's "cross-variant pair sharing relies on it" no longer holds).
- Rejected: loadgen generating pairs with hashcore (Argon2 on the node during scored windows); pairs shared across variants.
- Reason: pair generation costs Argon2 time; doing it on the host in the gap keeps it out of scored windows and loadgen free of hashcore.
- Reverse: DESIGN.md sec 10 edit; give loadgen back its hashcore import.

## 2026-10-14 · PR #33 · /hash: structural checks and a per-variant salt set in loadgen memory; ~1% repeated passwords; passwords 16 characters to 1024 bytes (amends the PR #15 salt-reuse entry and the PR #29 Q5 C and O2 entries; DESIGN.md sec 8, 10)
- Decision: every response is checked for format, exact params and salt against the variant's salt set, held in loadgen memory while loadgen serves the variant (no journal, no restore); about 1% of /hash requests repeat a password sent to that variant in the same window, and the same salt twice is salt_reused; /hash passwords >= 16 random characters, <= 1024 bytes (was 1-1024 bytes).
- Rejected: a classifier Run spanning Stage 5 to cull plus a salt journal and restore (see the "Dropped" entry); no repeats (a password -> hash cache verifies, so only a repeat exposes it).
- Reason: salts catch cross-window replay cheaply without Argon2; repeats catch caching; the floor makes a collision with a (1-1024 byte) pool password negligible.
- Reverse: DESIGN.md sec 8, sec 10 edit.

## 2026-10-14 · PR #33 · Full verification on the host after each window, before scoring; loadgen does no Argon2 (amends the PR #29 Q3 A sizing entry; DESIGN.md sec 5, 8, 10)
- Decision: loadgen hands its recorded (password, hash) pairs to the orchestrator; every hash is verified during a variant's first K scored windows, a sample after (only scored windows count toward K); every Stage 5 burst hash is verified. Loadgen no longer imports hashcore; the PR #29 Q3 A entry's off-send-path FullVerify and token bucket go.
- Rejected: sampled verification in loadgen during windows (Argon2 on the node competes with scored variants); verifying everything for a variant's whole life (host time per window grows with the population).
- Reason: a mutant faking results early is caught with certainty while it is new; verification in the gap cannot distort scores.
- Reverse: DESIGN.md sec 8, sec 10 edit; restore FullVerify in loadgen with a token bucket.

## 2026-10-14 · PR #33 · Integrity failures cull at window end, before scoring (amends the PR #29 Q6 A disqualification entry; DESIGN.md sec 9, sec 11)
- Decision: the first confirmed integrity failure still disqualifies, but the variant is culled at window end, before scoring, not at once; a sampled catch after K still disqualifies.
- Rejected: culling mid-window (the population changes inside a scored window).
- Reason: verification now runs after the window, so most failures are only confirmed then; one rule for all of them.
- Reverse: DESIGN.md sec 9, sec 11 step 7 edit.

## 2026-10-14 · PR #33 · Dropped: Run.Reserve, the salt journal and restore, the Stage-5-to-cull classifier Run, full verification during windows (reverses the PR #30 Run.Reserve entry, the PR #29 Q1 A+(i) restore clause and the PR #29 O2 entry; DESIGN.md sec 8, 10)
- Decision: a /verify hash replayed as a /hash answer is caught by host verification instead of Run.Reserve; a restart only loses detection power (salt set) and that window's unverified hashes (the window is excluded). Classifier code cleanup is a later PR (docs/STATUS.md Next).
- Rejected: keeping them alongside the new checks.
- Reason: each existed to cover something host-side verification or the in-memory salt set now covers; the journal and restore added a control-API write path and an exclusion reason for little gain.
- Reverse: DESIGN.md sec 8, sec 10 edit; restore Reserve's use and the restore call.

## 2026-10-14 · PR #33 · Live contract negatives gain three 400 classes: empty password, >1024-byte password, invalid JSON (amends the PR #29 Q5 C entry's "both classes"; DESIGN.md sec 7, 8, 10)
- Decision: five classes in sustained traffic and in the Stage 5 burst (>= 5%, every class at least once); all pass as 400 or graceful_shed, any 2xx is negative_accepted.
- Rejected: only the two Verify-error classes live (the 400 classes would be checked by the gate only).
- Reason: under the sec 7 principle the gate binds only mutants that do not detect it; input rejection is a security property, so it is checked live.
- Reverse: DESIGN.md sec 8, sec 10 edit.

## 2026-10-14 · PR #33 · Gate direction: Stages 2-4 move to a pod; Stage 1 checks and builds stay on the host (planned, not built; DESIGN.md sec 7 "Planned")
- Decision: pre-build checks, go vet and go build on the host, no candidate execution there; Stages 2-4 in a pod, replacing the bare boot; a host build-only probe keeps toolchain faults gate faults. Its own design packet and PR series; Stage 1's docker run sub-check is a planned removal (the gate pod proves the image boots). The bare-boot text and the PR #21 / #25 bare-boot entries stay until it lands, then that series supersedes them.
- Rejected: bounding the bare boot with rlimits or cgroups (STATUS Parked; the gate shares the host with the live population).
- Reason: the kubelet already bounds a pod's memory and CPU, and a pod sees the same limits as a variant.
- Reverse: drop the "Planned" subsection.

## 2026-10-14 · PR #33 · Accepted limits; no further gate-tell closing (amends the PR #25 A1 entry's "revisit", and the reasons of the PR #26 label and PR #28 pod-name entries; DESIGN.md sec 15)
- Decision: accepted: a cheat delayed past K windows can go uncaught; a mutant can detect the gate; a mutant can log plaintext passwords; a loadgen restart mid-window loses that window's unverified hashes. The label shape and pod-name draw stay as built but are no longer a goal; A1 is not revisited.
- Rejected: continuing to close gate tells (GOMAXPROCS vs NumCPU, build info, environment).
- Reason: tells cannot all be closed (A1 needs a different build); live checks protect fitness regardless.
- Reverse: reopen the gate-detectability item in docs/STATUS.md Parked.

## 2026-10-14 · PR #33 · Starting values: K = 3, repeat share 1%, /hash password floor 16 characters (amends the PR #29 Q3 A entry; DESIGN.md sec 15)
- Decision: K = 3, repeat share 1%, floor 16; after K, 10% of a variant's hashes verified per window; the old sustained-mode sample rate and the token-bucket rate leave sec 15's list; per-window pair-generation time and post-window verification time are workstation measurements to take.
- Rejected: none; values are starting guesses.
- Reason: the two measurements bound how long a gap must be.
- Reverse: DESIGN.md sec 15 edit.

## 2026-10-14 · PR #34 · Review packets lead with intent and list added complexity; hypothetical-threat guards are parked; reviewer flags over-engineering as advisory; entries carry Origin
- Rejected: 2-3 lines of intent and guards built for any threat by default; over-engineering as a must-fix finding.
- Reason: the owner reviews intent before the diff, and each guard names the threat it answers, so speculative code is visible and parked; hypotheticals on hashcore isolation, protected paths, fail-closed classification or containment go to the owner with the guard kept meanwhile, so no fail-closed rule weakens; fitness integrity rests on the live checks and the sec 15 accepted limits (DESIGN.md sec 7), so its hypotheticals park like any other.
- Reverse: revert the edits to docs/handoff.md, reviewer.md (item 13, advisory), CLAUDE.md (Self-review bullet, Origin clause), the chat Review line and the decisions.md Origin note, here and in docs/workflow/.
- Origin: chat

## 2026-10-15 · PR #35 · Fixed commit trailers via a CLAUDE.md rule, with no model name
- Decision: every commit message ends with `Co-Authored-By: Claude <noreply@anthropic.com>` and `Claude-Session: <this session's claude.ai/code link>` (the second omitted when a session has no link); no model name or identifier in commit messages, PR titles or PR bodies; new commits only, never amend to change trailers. The same bullet is in docs/workflow/templates/CLAUDE.template.md.
- Rejected: `attribution.commit` in .claude/settings.json (no evidence the harness reminder reads it); the reminder's model-named trailer (it conflicts with Claude Code's built-in ban on model identifiers in pushed artifacts); amending PR #34's commits (CLAUDE.md: never amend or re-author).
- Reason: the harness reminder says a CLAUDE.md rule takes precedence over it, so one rule satisfies both sources; commit identity is unchanged (PR #8 and PR #9 entries).
- Reverse: drop the bullet from CLAUDE.md and docs/workflow/templates/CLAUDE.template.md.
- Origin: chat

## 2026-10-15 · PR #36 · Loadgen/orchestrator split: raw records, halves and canary on the orchestrator side (amends the PR #29 Q4 A lag entry and the PR #31 FailOpen entry; DESIGN.md sec 10, 11) (ring and pairs details amended by the PR #41 ring and records-carry-pairs entries)
- Decision: loadgen returns raw per-request records (each with its verdict, wall-clock and monotonic intended send time) and the recorded pairs; the orchestrator assigns records to halves by intended send time against its own stress-ng switch times and computes every statistic and exclusion; it triggers the per-half canary (connected or refused); the ring holds one window plus one burst, read only in gaps, a gap still excludes; one clock (the kind node is a host container).
- Rejected: loadgen computing per-half summaries; loadgen tracking halves (per-half signals, or a schedule in the window-start call).
- Reason: statistics and thresholds live in one place and change without a loadgen restart (a restart excludes a window); raw records can be re-checked; one clock.
- Reverse: DESIGN.md sec 10, 11 edit.
- Origin: chat

## 2026-10-15 · PR #36 · Orchestrator stress waits: a stress half starts once stress-ng pods are Running, a baseline half only once they are gone (DESIGN.md sec 11)
- Decision: the orchestrator waits for the stress-ng pods to be Running before a stress half, and gone before a window (so its baseline half) starts; details are settled with the orchestrator design.
- Rejected: assigning halves by the time of the scale call.
- Reason: stress-ng pods take time to start and stop, e.g. Stage 5's stress running into the next window's baseline half.
- Reverse: DESIGN.md sec 11 edit.
- Origin: chat

## 2026-10-15 · PR #37 · Trailer rule covers commit messages; PR bodies keep the harness's standard footer and session link, with no model name (amends the PR #35 entry; CLAUDE.md and docs/workflow/templates/CLAUDE.template.md)
- Decision: the trailer rule covers commit messages; PR bodies keep the harness's standard footer and session link, with no model name.
- Rejected: dropping the footer (earlier PRs carry it); leaving the rule as worded and asking for the footer in each PR prompt.
- Reason: "replaces the harness's attribution reminder" could be read as also dropping the PR footer; one sentence makes it explicit.
- Reverse: restore the PR #35 sentence in CLAUDE.md and the template.
- Origin: chat

## 2026-10-15 · PR #38 · Loadgen formats malformed and non-exact-params hash strings itself (DESIGN.md sec 10)
- Decision: loadgen formats both negative hash strings from crypto/rand bytes; no Argon2.
- Rejected: the host batch carries them (more control-API payload, and it couples negatives to pair generation).
- Reason: no Argon2 is needed, and the hashcore agreement test catches drift.
- Reverse: DESIGN.md sec 10 edit plus moving string construction into the host batch.
- Origin: chat

## 2026-10-15 · PR #38 · Loadgen has its own negative and password builders; gate/interop.go keeps its own (DESIGN.md sec 10)
- Decision: two independent builders, one in loadgen and one in gate/interop.go.
- Rejected: a shared internal package (gate's cases are a fixed set built on the pool, loadgen's are random per request, and the shared code is about three one-liners).
- Reason: loadgen cannot import gate/; each side has a coverage test against DESIGN.md sec 8's classes.
- Reverse: extract the one-liners into a package both import.
- Origin: chat

## 2026-10-15 · PR #38 · /hash password alphabet and lengths (DESIGN.md sec 10, sec 15)
- Decision: crypto/rand.Text output (base32), cut to a uniform 16 to 1024 characters.
- Rejected: printable ASCII (encoding/json escapes some of it, so wire size drifts from password size); arbitrary UTF-8 (characters and bytes differ).
- Reason: the 16-character floor and the 1024-byte cap then mean the same unit.
- Reverse: DESIGN.md sec 10 and sec 15 edit; a different alphabet changes the loadgen password builder only.
- Origin: chat

## 2026-10-15 · PR #38 · Loadgen's import rule is enforced by a go list -deps test, with test files exempt (DESIGN.md sec 10)
- Decision: a go list -deps test over loadgen's build checks it never depends on hashcore, testvectors, gate/ or an Argon2 package; _test.go files may import hashcore.
- Rejected: direct imports only (misses a transitive import through internal/classifier).
- Reason: the rule is about the binary, and the negative-class tests need hashcore.
- Reverse: drop the test and restate the rule as direct imports.
- Origin: chat

## 2026-10-15 · PR #38 · internal/classifier gets a stdlib-only deps test (enforces the PR #15 stdlib-only decision)
- Decision: a test lists internal/classifier's transitive dependencies and allows only the standard library.
- Rejected: relying on the wrapper's Stage 1 allowlist alone.
- Reason: that allowlist checks direct imports only, so a crypto or hashcore import inside classifier would reach the wrapper with no check firing.
- Reverse: delete the test; the PR #15 entry then rests on review.
- Origin: chat

## 2026-10-15 · PR #38 · Bursts repeat ~1% of /hash passwords; pairs go out in random order; each Stream's class rotation starts at a random class (DESIGN.md sec 8, 10)
- Decision: a Stage 5 burst repeats about 1% of its /hash passwords as a window does (one shared helper); Stream and Burst shuffle a copy of the host's pairs; a Stream's first negative class is random.
- Rejected: all-fresh burst passwords (a burst could not catch a password -> hash cache); host order (it would decide where matches fall); a fixed first class.
- Reason: burst and window traffic look alike, and nothing a wrapper sees depends on host order or a fixed start.
- Reverse: Burst back to newPassword(); NewStream without shuffled(); the rotation starting at 0; DESIGN.md sec 8, 10 edits.
- Origin: chat

## 2026-10-15 · PR #39 · Loadgen reuses internal/classifier unchanged: one NewRun(nil) per variant, FullVerify always false (DESIGN.md sec 8, 10)
- Decision: loadgen's Checker wraps a classifier Run with no Verifier; nothing under internal/classifier changes.
- Rejected: a loadgen-own classifier (it would drift from the gate's rules).
- Reason: the same code applies the same rules in the gate and in live traffic, and needs no Argon2.
- Reverse: a separate classifier in loadgen.
- Origin: chat

## 2026-10-15 · PR #39 · A /hash pair is recorded for 2xx with reason none or over_hang_timeout (DESIGN.md sec 8, 10) (extended by the PR #40 status entry: bad_status also records) (Pairs() half superseded by the PR #41 records-carry-pairs entry)
- Decision: Checker records a pair only for KindHash, status 2xx and reason none or over_hang_timeout (records() in loadgen/check.go).
- Rejected: Success only (a slow bad hash would skip host verification).
- Reason: over_hang_timeout on a 2xx comes only after the integrity checks pass, and the host verifies late hashes too.
- Reverse: restrict records() to Success.
- Origin: chat

## 2026-10-15 · PR #39 · The salt set lasts a variant's whole time in loadgen; the Checker's lifecycle belongs to PR 4 (DESIGN.md sec 8, 10) (sizing moved: PR 3 benchmarks, the key type and any cap go to the classifier cleanup PR, docs/STATUS.md Next) (key type and cap settled by the PR #43 salt-key and no-cap entries)
- Decision: one Checker per variant for as long as loadgen serves it; the control API (PR 4) owns when it is made and dropped. No cap yet; PR 3 sizes it.
- Rejected: a salt set per window (a salt reused across windows goes uncaught).
- Reason: DESIGN.md sec 8 puts the set across windows, so a mutant cannot replay an earlier window's hash.
- Reverse: a Checker per window.
- Origin: chat

## 2026-10-15 · PR #39 · Checks take a loadgen Response, with no HTTP in PR 2; a capped or failed body read is a transport error (DESIGN.md sec 8, 10) (cap half superseded by the PR #40 cap entry) (Check's return extended by the PR #41 records-carry-pairs entry)
- Decision: Check takes a Response{Status, Header, Body, Latency, Err}; a sender whose body read failed or hit its size cap sets Err.
- Rejected: malformed_body for a cut-short body (a client-side cap would count as an integrity failure).
- Reason: malformed_body is an integrity reason that culls a variant, so only the variant's own bytes may cause it.
- Reverse: classify truncated bodies as malformed_body.
- Origin: chat

## 2026-10-16 · PR #40 · Strict single-member decode: a v1 json.Decoder token walk; a duplicate key is malformed_body (DESIGN.md sec 8)
- Decision: the classifier walks the body with encoding/json's Decoder tokens and accepts one object with exactly one member; a second member, a duplicate included, is malformed_body.
- Rejected: encoding/json/v2 (needs GOEXPERIMENT=jsonv2 in go1.26.9, outside the compatibility promise); last value wins (a first-wins client sees a different value than the one checked).
- Reason: the checked value must be the value every client reads.
- Reverse: back to the map decode.
- Origin: chat

## 2026-10-16 · PR #40 · Body matching is JSON-equivalent, not byte-exact; invalid UTF-8 is malformed_body (DESIGN.md sec 8)
- Decision: escaped names and characters and surrounding whitespace are accepted; invalid UTF-8 is malformed_body; a lone-surrogate escape in a hash stays non_canonical_phc.
- Rejected: byte-exact matching (every conforming parser reads the equivalent forms the same, so there is no client divergence to guard; it would constrain wrappers beyond the spec).
- Reason: reject what a client could read differently, nothing more.
- Reverse: compare raw bytes.
- Origin: chat

## 2026-10-16 · PR #40 · Only 200 is success; a non-200 2xx is bad_status after the integrity checks, and loadgen records its pair (DESIGN.md sec 8, 10)
- Decision: a non-200 2xx gets the integrity checks first (salt recorded), then over_hang_timeout if late, else bad_status; loadgen records its /hash pair.
- Rejected: rejecting non-200 2xx before reading the body (a 201 hash's salt goes unrecorded and can be replayed as a 200; clients that accept any 2xx would use a wrong hash undetected); non-200 2xx as an integrity reason (culls a variant for a correct hash).
- Reason: the host must verify every hash a client could accept, and a status slip alone is not cheating.
- Reverse: is2xx for Success; drop bad_status from loadgen records().
- Origin: chat

## 2026-10-16 · PR #40 · The classifier owns a 4 KiB MaxBodyBytes; a 2xx body over it is malformed_body (DESIGN.md sec 8, 10; supersedes the size-cap half of the PR #30 caller-contract entry and the PR #39 capped-body entry)
- Decision: callers read MaxBodyBytes+1 bytes and pass them with Err nil; a 2xx body over MaxBodyBytes is malformed_body; a failed read stays transport_error.
- Rejected: the caller setting Err at its own cap (lets a variant turn an integrity failure into a transport error by padding); 64 KiB (loadgen buffers up to the cap per in-flight request against 256Mi).
- Reason: only the variant's own bytes may cause malformed_body, and an over-cap body is its own.
- Reverse: caller-side cap with Err.
- Origin: chat

## 2026-10-16 · PR #40 · Content-Type stays unchecked (DESIGN.md sec 8)
- Decision: the classifier ignores Content-Type, since the contract states no 2xx headers.
- Rejected: requiring application/json (a rule the spec does not state).
- Reason: a check the spec does not state would cull variants for no stated reason.
- Reverse: add it to the contract and the classifier.
- Origin: chat

## 2026-10-16 · PR #41 · Open-loop schedule and record times (DESIGN.md sec 10)
- Decision: one scheduler goroutine per target; send i is intended at start + i/r, from the index; each send is its own goroutine; a late send goes out at once with its lag recorded; a send is skipped only for a due /verify with ErrOutOfPairs; no in-flight cap. Monotonic times are durations since a process-start base; IntendedWall is the window's wall start + i/r in UTC, monotonic reading stripped; ActualMono is taken just before client.Do and Latency runs to the full body read.
- Rejected: sleeping one period between sends (drift accumulates); skipping late sends (hides lag); a fixed worker pool (turns closed-loop when saturated).
- Reason: the intended time does not depend on earlier sends, so lag measures loadgen alone and a slow variant cannot throttle its own load.
- Reverse: DESIGN.md sec 10 edit; scheduler in loadgen.
- Origin: chat

## 2026-10-16 · PR #41 · Records carry the recorded pairs; Checker.Pairs is dropped (DESIGN.md sec 10)
- Decision: Check returns the recorded pair with the Verdict and the record carries it (Seq, Target, RunID, Index, Kind, Class, times, Verdict, Skipped, Fault, Pair, Evidence); Checker.pairs and Pairs() go; the sender builds a new Stream per window.
- Rejected: a separate drained pair store (a second store needing its own gap detection); Checker.Pairs never clearing (grows for a variant's whole life).
- Reason: ring gap detection then covers the pairs, and a pair lives only as long as its record.
- Reverse: restore Checker.pairs and Pairs(); a drained store.
- Origin: chat

## 2026-10-16 · PR #41 · Integrity records carry evidence, the first 8 per target per window or burst (DESIGN.md sec 9, 10)
- Decision: a record whose reason is an integrity reason carries the request body, the response status and the response body (at most MaxBodyBytes+1), for the first 8 such records per target per window or burst; later ones carry the verdict only.
- Rejected: evidence on every integrity record (a variant failing every request fills memory); no evidence (the host cannot confirm loadgen-side reasons, sec 9).
- Reason: sec 9 culls on the first host-confirmed failure, so a few records per window are enough. What the host checks per reason is settled in the orchestrator design.
- Reverse: change the count in loadgen; DESIGN.md sec 10 edit.
- Origin: chat

## 2026-10-16 · PR #41 · A Check error is a record Fault that excludes the half (extends the PR #29 global-exclusion entry; DESIGN.md sec 10, 11) (extended by the PR #41 build-fault entry)
- Decision: a Checker.Check error yields a record with Fault set and its Verdict ignored; the half is excluded for all variants, like a skipped send.
- Rejected: recording the zero Verdict (charges a loadgen bug to the variant as failed); dropping the record (hides the fault and undercounts sends).
- Reason: a Check error is loadgen's, so it must neither count against a variant nor vanish.
- Reverse: DESIGN.md sec 10, 11 edit; drop the trigger in the orchestrator.
- Origin: chat

## 2026-10-16 · PR #41 · Client timeout 3 x HangTimeout + 1s, pinned to WriteTimeout by a go/parser test (extends the PR #30 client-timeout entry; DESIGN.md sec 10)
- Decision: one http.Client over a clone of http.DefaultTransport with DisableKeepAlives true and Proxy nil, Content-Type application/json; Timeout = 3*classifier.HangTimeout + 1s (7s), a loadgen const; a go/parser test fails unless cmd/service/main.go's WriteTimeout field still reads 3 * classifier.HangTimeout.
- Rejected: exporting WriteTimeout from internal/classifier (a protected-path change for one constant); a bare 7s with no check (drifts silently).
- Reason: the test turns a silent drift of the "timeout > WriteTimeout" rule into a failing build. Proxy nil keeps variant traffic off any environment proxy.
- Reverse: DESIGN.md sec 10 edit; drop the const and the test.
- Origin: chat

## 2026-10-16 · PR #41 · One record ring per target, owned with its Checker (amends the PR #36 ring wording; DESIGN.md sec 10)
- Decision: one ring per target, made and dropped with that target's Checker (PR 4); capacity round(r x W) + the largest burst n; Seq is assigned when a record is written; Read(since) returns the records from since onward with a gap flag if the oldest retained Seq is past since; a gap on any target excludes the half for all variants; a window is done only when every send has completed or timed out; PR 4 drops a target only after its records are read.
- Rejected: one ring for all targets (a burst competes with streams for space; its lifecycle is separate from the Checker's).
- Reason: a culled target's records and memory go with its Checker, and no target's sends can evict another target's records.
- Reverse: DESIGN.md sec 10 edit; one shared ring.
- Origin: chat

## 2026-10-16 · PR #41 · Loadgen follows no redirect; a 3xx is the variant's own response (DESIGN.md sec 10)
- Decision: loadgen's client sets CheckRedirect to return http.ErrUseLastResponse, so a 3xx comes back as the response and classifies as bad_status.
- Rejected: net/http's default (follows up to 10 redirects and re-sends the POST body on 307 and 308).
- Reason: a variant could answer 307 to a peer variant's Service, which loadgen's egress allows, and score the peer's real hash as its own; host verification passes it, and the peer's salts sit in the peer's Checker.
- Reverse: drop CheckRedirect in loadgen/send.go; DESIGN.md sec 10 edit.
- Origin: Code

## 2026-10-16 · PR #41 · A request loadgen cannot build or generate is a Fault too (extends the PR #41 Check-fault entry; DESIGN.md sec 10, 11)
- Decision: a failed request build, and a request-generator error other than ErrOutOfPairs, give a Fault record with nothing sent and Check not called; a Fault excludes the half like a Check error.
- Rejected: charging a failed build to the variant as transport_error (loadgen's own bad URL would count against the variant).
- Reason: only the variant's own behaviour may count against it.
- Reverse: loadgen/send.go returns the build error as resp.Err; for a generator error, drop the `err != nil` case in run.go's schedule and say what replaces it (the zero Request would otherwise be sent).
- Origin: Code

## 2026-10-16 · PR #42 · Cut per-PR doc reads: reconcile greps decisions.md, pre-#32 entries archived, Recent capped at 5, overview refreshed on request
- Decision: the reconcile greps decisions.md instead of reading it; entries before #32 are in docs/decisions-archive.md; STATUS Recent keeps the last 5 PRs; overview.md and README.md refresh only on request, with a stale line in STATUS Next; docs/workflow/ is frozen until extraction.
- Rejected: per-PR overview checks (reads ~32 KB of overview on every DESIGN or wrapper PR).
- Reason: usage windows are hit regularly, and reading decisions.md (~103 KB) for every reconcile and overview.md (~32 KB) on every DESIGN or wrapper PR were the largest fixed reads per PR; the stale line keeps overview drift visible without the read.
- Reverse: restore the three CLAUDE.md bullets and drop the archive bullet; merge docs/decisions-archive.md back into docs/decisions.md.
- Origin: chat

## 2026-10-16 · PR #43 (inferred) · The salt set is keyed on the decoded [16]byte salt (DESIGN.md sec 8)
- Decision: classifier.Run's salt set is a map[[16]byte]struct{} of the decoded salt, measured at 25.23 B per salt (BenchmarkCheckerSalts).
- Rejected: string (a substring pins the 97-byte hash, 149.3 B; a clone is 61.3 B); uint64 (the same 25.2 B, and it misses reuse when two salts differ only in the dropped 8 bytes).
- Reason: canonical base64 is one-to-one, so the decoded bytes catch exactly the reuses the salt segment did.
- Reverse: key on the salt segment string.
- Origin: chat

## 2026-10-16 · PR #43 (inferred) · No last-M-windows cap on the salt set (DESIGN.md sec 8, 10)
- Decision: the set keeps every salt for the variant's whole time in loadgen.
- Rejected: a last-M-windows cap (a salt reused more than M windows apart goes uncaught).
- Reason: 4 variants x 750,000 salts x 25.23 B is 72.2 MiB; with rings (no burst; one of 3,000 records adds ~12 MiB) and Evidence, ~84 MiB live. loadgen sets no GOMEMLIMIT, so at GOGC=100 the heap can reach about twice that, ~170-195 MiB, under 256Mi. The 750k, rate and window figures are planning guesses.
- Revisit: if the measured rate or window count is well above them (STATUS Parked, Pending workstation, Q8).
- Reverse: add a cap.
- Origin: chat

## 2026-10-16 · PR #43 (inferred) · Stage 4 shares Stage 2's classifier Run on purpose (DESIGN.md sec 8)
- Decision: gate Stage 4 classifies through the Run Stage 2 made, not a new one.
- Rejected: a fresh Run for Stage 4 (salts repeated across the two boots go uncaught).
- Reason: Stage 4 FullVerifies every /hash answer, so a replayed Stage 2 hash already fails as verify_mismatch; the shared salt set adds catching a salt that repeats across the two boots (for example a fixed-seed salt RNG).
- Reverse: give Stage 4 its own NewRun.
- Origin: chat
