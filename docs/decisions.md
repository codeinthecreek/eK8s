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

## 2026-10-14 · PR #33 · /verify pairs generated on the host per window, each sent once to one variant (amends the PR #29 Q5 C fresh-pairs entry and the PR #31 FailOpen entry; DESIGN.md sec 5, 8, 10, 11) (mismatch method amended by the PR #46 host-pairs entry)
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

## 2026-10-16 · PR #43 · The salt set is keyed on the decoded [16]byte salt (DESIGN.md sec 8)
- Decision: classifier.Run's salt set is a map[[16]byte]struct{} of the decoded salt, measured at 25.23 B per salt (BenchmarkCheckerSalts).
- Rejected: string (a substring pins the 97-byte hash, 149.3 B; a clone is 61.3 B); uint64 (the same 25.2 B, but two salts that differ only in the dropped 8 bytes would be a false salt_reused).
- Reason: canonical base64 is one-to-one, so the decoded bytes catch exactly the reuses the salt segment did.
- Reverse: key on the salt segment string.
- Origin: chat

## 2026-10-16 · PR #43 · No last-M-windows cap on the salt set (DESIGN.md sec 8, 10) (memory reasoning updated by the PR #45 GOMEMLIMIT and W entries)
- Decision: the set keeps every salt for the variant's whole time in loadgen.
- Rejected: a last-M-windows cap (a salt reused more than M windows apart goes uncaught).
- Reason: 4 variants x 750,000 salts x 25.23 B is 72.2 MiB; with rings (no burst; one of 3,000 records adds ~12 MiB) and Evidence, ~84 MiB live. loadgen sets no GOMEMLIMIT, so at GOGC=100 the heap can reach about twice live: ~170 MiB, or ~195 MiB with a 3,000-record burst, under 256Mi. The 750k, rate and window figures are planning guesses.
- Revisit: if the measured rate or window count is well above them (STATUS Parked, Pending workstation, Q8).
- Reverse: add a cap.
- Origin: chat

## 2026-10-16 · PR #43 · Stage 4 shares Stage 2's classifier Run on purpose (DESIGN.md sec 8)
- Decision: gate Stage 4 classifies through the Run Stage 2 made, not a new one.
- Rejected: a fresh Run for Stage 4 (salts repeated across the two boots go uncaught).
- Reason: Stage 4 FullVerifies every /hash answer, so a replayed Stage 2 hash already fails as verify_mismatch; the shared salt set adds catching a salt that repeats across the two boots (for example a fixed-seed salt RNG).
- Reverse: give Stage 4 its own NewRun.
- Origin: chat

## 2026-10-18 · PR #44 · The Stage 5 burst shape is fixed per run (DESIGN.md sec 7, 10, 15)
- Decision: b = 2 x C0 (= 4r), D = 30 s constant, n = round(b x D), derived from -burst-rate; start-up refuses n > 3,000.
- Rejected: per-call n and rate through the API (gate results would not compare within a run); n as a second flag (it could disagree with b).
- Reason: one shape per run keeps every candidate's Stage 5 result comparable, and a single flag leaves n no way to disagree with b.
- Revisit: raise the 3,000 limit only with a new memory sum (PR #43 no-cap entry); the factor 2 and D after the workstation run (Q8).
- Reverse: DESIGN.md sec 7, 10, 15 edits; loadgen/config.go.
- Origin: chat

## 2026-10-18 · PR #44 · Targets are named by id: loadgen builds the URL, and the target is PUT before the Stage 5 burst (amends DESIGN.md sec 7 and sec 11 target-list wording)
- Decision: loadgen builds each URL from the -target-url template and the id; the target is PUT before its Stage 5 burst; "live" means named in window starts.
- Rejected: a URL in the PUT (an id/URL mismatch would silently score one variant's responses as another's); adding the target only after a pass (the burst's salts would miss its set).
- Reason: the URL follows from the id, so no request can pair one variant's id with another's URL; and the burst's salts enter the candidate's own salt set.
- Reverse: DESIGN.md sec 7, 11 edits; take the URL in the PUT.
- Origin: chat

## 2026-10-18 · PR #44 · DELETE /targets/{id}?through=S is refused while records past S are unread or the target is in a run (DESIGN.md sec 10)
- Decision: DELETE answers 409 while records past S are unread or the target is in a run. It implements the drop rule of the PR #41 ring entry (a target is dropped only after its records are read).
- Reason: a target's records go with its ring, so a DELETE before they are read would lose them, and one during a run would drop a Checker that still has sends in flight.
- Reverse: DESIGN.md sec 10 edit; drop the through check and the 409s in loadgen/control.go.
- Origin: chat

## 2026-10-18 · PR #44 · Runs: per-target pair batches, one active run, an all-or-nothing start chosen by loadgen (DESIGN.md sec 10)
- Decision: batches are per target, uploaded separately and consumed at start, one pending per target; one active run, and a run_id never reused within a boot; a start is all or nothing at now + 200 ms, chosen by loadgen; per-target states and cancel.
- Rejected: pairs inside the start request (one large request); an orchestrator-chosen start time; leaving run overlap to the orchestrator.
- Reason: only loadgen sees its active run, the run_ids it has used and its own clock, so it enforces no overlap, no reuse and one shared start itself.
- Reverse: DESIGN.md sec 10 edit; loadgen/control_run.go.
- Origin: chat

## 2026-10-18 · PR #44 · Window batch size P = ceil(0.48N + sqrt(N ln(1e9)/2)), capped at N (DESIGN.md sec 10)
- Decision: a window batch holds P pairs, N being the window's send count, so one stream runs out of pairs with probability at most 1e-9 per window (Hoeffding).
- Rejected: a fixed margin (it does not grow with N).
- Reason: only /verify sends use a pair, so a window needs P pairs rather than N; this P keeps the shortfall at 1e-9 per window whatever N is, and the cap at N means no send can run out.
- Reverse: change P in loadgen/config.go; DESIGN.md sec 10 edit.
- Origin: Code

## 2026-10-18 · PR #44 · boot_id on every control API response (DESIGN.md sec 10)
- Decision: boot_id is the Ek8s-Boot-Id header and a field in every JSON body; the mux's 404 and 405 carry the header only.
- Reason: a restart loses targets, batches and records, and the header lets the orchestrator see it on any answer.
- Reverse: DESIGN.md sec 10 edit; drop the header and the body field.
- Origin: chat

## 2026-10-18 · PR #44 · Policy canary mechanics (amends the mechanics in the PR #31 FailOpen entry; DESIGN.md sec 11)
- Decision: POST /canary dials -canary-addr, an IP literal; refused only for a timeout, ECONNREFUSED, EHOSTUNREACH or ENETUNREACH; EAGAIN and any other error give error, which excludes like connected; the dial is detached from the request's context.
- Rejected: any dial error as refused (a local failure would hide a fail-open policy); a DNS name (a failed lookup would read as refused).
- Reason: refused must mean the policy dropped the connection, so every other outcome reads as connected or error, which exclude the half; a dial detached from the request cannot turn its caller's deadline into refused.
- Reverse: DESIGN.md sec 11 edit; loadgen/canary.go.
- Origin: chat

## 2026-10-18 · PR #44 · Wire format and request limits (DESIGN.md sec 10)
- Decision: separate wire structs; buckets and reasons as names; a skipped or fault record carries no bucket or reason; records pages of at most 1,000; every uploaded pair must carry password, hash and match; upload body cap max(window_pairs, burst_pairs) x 1161 + 4 KiB (1024-byte password, 97-byte hash, 40 bytes of names, per pair), start body 64 KiB.
- Reason: the wire shape is the orchestrator's contract, so it does not follow internal struct or enum changes; the caps bound what one request can make loadgen hold, the upload cap being the largest batch at its largest pair size.
- Reverse: DESIGN.md sec 10 edit; loadgen/control_http.go and control_run.go.
- Origin: Code

## 2026-10-18 · PR #44 · The loadgen binary: required flags, fixed :9090, cancel-then-wait shutdown (DESIGN.md sec 10, 15)
- Decision: every flag required, with no default; the listener is the fixed :9090; on SIGTERM, cancel every run, then wait up to 10 s (Control.Wait) for requests and in-flight sends to be recorded; server read and write timeouts of 2 minutes.
- Reason: a missing flag fails at start-up instead of running with a value nobody chose; a fixed :9090 matches the control Service's named targetPort, so listener and Service cannot drift and there is no second listener to configure; a send outlives its run's context, so cancel-then-wait is what lets every in-flight send be recorded before exit.
- Reverse: cmd/loadgen/main.go; DESIGN.md sec 10, 15 edits.
- Origin: Code

## 2026-10-18 · PR #44 · The control API is tested in-process; kind-level checks wait for PR 5 (DESIGN.md sec 10)
- Decision: httptest, with gen-0 wrapper handlers and the real hashcore in e2e_test.go; kind-level checks (STATUS Next, loadgen PR 5) come with the deployment.
- Rejected: kind-level checks in this PR (there is no loadgen deployment until PR 5).
- Reason: the control API's behavior needs no cluster, and httptest runs it with the real classifier, hashcore and gen-0 wrapper handlers.
- Reverse: add kind-level checks to PR 5's smoke script.
- Origin: chat

## 2026-10-21 · PR #45 · loadgen sets GOMEMLIMIT to 90% of its memory limit (DESIGN.md sec 10)
- Decision: loadgen reads EK8S_MEM_LIMIT_BYTES (downward API, limits.memory) and sets debug.SetMemoryLimit(limit - limit/10), as cmd/service does; absent means no limit.
- Rejected: no limit (the PR #43 no-cap entry's premise).
- Reason: at r = 2 live memory is ~77 MiB, so the GOGC=100 peak stays under ~230 MiB and the limit never binds; at maxRate 19 (~103-117 MiB live) it may just bind, and past the estimate GC CPU (capped near 50% by the runtime) shows as send lag and excluded halves before an OOM kill that empties loadgen. A live heap near the limit still OOMs.
- Reverse: drop the SetMemoryLimit call in loadgen/cmd/loadgen/main.go and the env entry in deploy/loadgen.
- Origin: chat

## 2026-10-21 · PR #45 · loadgen's only listener is control 9090, checked three ways (DESIGN.md sec 10, 11)
- Decision: a go/parser test counts exactly one listen call site in loadgen/ and cmd/loadgen non-test files; forbiddenDep (go list -deps, transitive) refuses net/http/pprof and expvar by exact path; the kind smoke reads /proc/net from an ephemeral container (one TCP LISTEN, :2382; no bound UDP port), which also catches a listener a dependency opens (the manifest also declares one containerPort).
- Rejected: an import check in the parser test (direct imports only; forbiddenDep is transitive).
- Reason: every root process on the node reaches any loadgen port (kindnet's root-socket bypass), so the listener set, not the policy, is the boundary.
- Reverse: delete the three checks and the two forbiddenDep paths.
- Origin: chat

## 2026-10-21 · PR #45 · -canary-addr comes from kubelet's KUBERNETES_SERVICE_HOST/PORT (DESIGN.md sec 11)
- Decision: args carry -canary-addr=$(KUBERNETES_SERVICE_HOST):$(KUBERNETES_SERVICE_PORT) with enableServiceLinks: false.
- Rejected: the deploy step writing the ClusterIP into the manifest (kept as the fallback if the kind run shows no expansion).
- Reason: kubelet injects the default/kubernetes Service vars even with service links off, and expands args against them (kubelet v1.37.0 source); an unexpanded value is literal text, which validCanaryAddr refuses at start.
- Reverse: render the ClusterIP into the args.
- Origin: chat

## 2026-10-21 · PR #45 · loadgen's grace period is 15 s (DESIGN.md sec 10)
- Decision: terminationGracePeriodSeconds: 15, its 10 s shutdown bound plus 5 s; a renderer test requires > 10.
- Rejected: leaving the 30 s default implicit (a later edit could lower it unnoticed); an explicit 30 s (the same unless loadgen overruns its 10 s bound, and slower deletes).
- Reason: the shutdown wait lets in-progress control requests and in-flight sends finish; it does not preserve records, which die with the process.
- Reverse: change the template value and the test bound.
- Origin: chat

## 2026-10-21 · PR #45 · loadgen's Deployment uses strategy Recreate (DESIGN.md sec 10)
- Decision: strategy Recreate, one replica.
- Rejected: the default RollingUpdate (surges a second loadgen, with a second boot_id, behind the control Service, and a second Guaranteed 750m pod may not fit the node).
- Reason: one loadgen at a time behind the Service; a hand-deleted pod can still overlap its replacement (Kubernetes docs), which a new boot_id already covers.
- Reverse: drop the strategy field.
- Origin: chat

## 2026-10-21 · PR #45 · loadgen has a tcpSocket readiness probe and no liveness probe (DESIGN.md sec 10)
- Decision: readinessProbe tcpSocket on port control; no livenessProbe.
- Rejected: a liveness probe (a restart empties loadgen's targets, records and salt sets); an httpGet readiness probe on /config (a slow answer under load could take loadgen out of the Service mid-run).
- Reason: readiness makes Ready mean listening, so a rollout wait is a usable start signal; the kernel accepts the probe's connection from the listen backlog whatever loadgen's CPU or GC is doing; the orchestrator's control-call timeouts see a hung loadgen.
- Reverse: add a livenessProbe to the template, or switch the probe to httpGet.
- Origin: chat

## 2026-10-21 · PR #45 · the deploy step takes r alone and fills -rate, -window and -burst-rate (DESIGN.md sec 10)
- Decision: deploy/loadgen renders from r: b = 4r, W the renderer constant, both formatted with strconv.FormatFloat(x, 'g', -1, 64); r must already be in that canonical form; the smoke run uses r = 2; the orchestrator refuses to start if GET /config's r, window_s or burst_rate differ from its own, compared exactly.
- Rejected: values fixed in the manifest (a provisional r would be baked in, and a new r is a new run).
- Reason: one input leaves b and W no way to disagree with r; shortest round-trip formatting (and an exact x4) makes the flag, the JSON and the orchestrator's value one float64.
- Reverse: take W and b as renderer inputs.
- Origin: chat

## 2026-10-21 · PR #45 · loadgen's manifest is rendered by Go, like the variant bundle (DESIGN.md sec 10)
- Decision: deploy/loadgen renders the Deployment from Params{Rate, Image}; Render refuses an r not in FormatFloat 'g' -1 form, then runs loadgen.Config.Validate with the real rate, window, burst rate and target URL and the stand-in canary 192.0.2.1:443, so loadgen's maxRate and n cap apply.
- Rejected: kustomize (cannot compute 4r); sed/envsubst (no validation); a renderer-side rate limit (two budgets could disagree).
- Reason: Validate is the one definition of what loadgen starts with; the real canary value exists only after kubelet expansion, so validCanaryAddr checks it at pod start and the smoke run checks it, and a test pins the template arg to the $(KUBERNETES_SERVICE_HOST):$(KUBERNETES_SERVICE_PORT) literal.
- Reverse: replace with a static manifest plus a sed step.
- Origin: Code

## 2026-10-21 · PR #45 · W = 5 min; loadgen refuses r above maxRate = 19, from the memory sum (DESIGN.md sec 10, 15) (in-flight term extended by the PR #47 header-cap entry)
- Decision: the renderer constant W is 5 min; loadgen/config.go's maxRate is 19, next to maxBurstSends, and Config.Validate refuses a higher r, so the binary enforces its own budget and the renderer inherits it.
- Rejected: 1-2 min (not the "multi-minute" window of DESIGN.md sec 10); maxRate 25 from n <= 3,000 alone (110.16 MiB benchmarked, ~128 MiB with inferred transients).
- Reason: at r = 19, 4 targets, W = 5 min, n = 3,000 and 750k salts per variant, live memory is 102.94 MiB benchmarked, ~117 MiB with inferred transients: about half the ~230 MiB GOMEMLIMIT, GOGC=100's headroom; salts dominate.
- Revisit: salt growth past 750k per variant is open with the orchestrator design (docs/STATUS.md Next).
- Reverse: raise maxRate in loadgen/config.go after the workstation heap measurement; a new W is a new run.
- Origin: Code

## 2026-10-21 · PR #45 · loadgen logs its applied GOMEMLIMIT; GET /config is unchanged (DESIGN.md sec 10)
- Decision: the start log line adds mem_limit_bytes and go_mem_limit; the smoke run reads them and canary_addr from kubectl logs.
- Rejected: new GET /config fields (a wire change no API caller needs).
- Reason: same as cmd/service, whose start line variant-smoke.sh already reads.
- Reverse: add the fields to wireConfig.
- Origin: Code

## 2026-10-21 · PR #45 · the reviewer subagent runs once per task, on the final tree (CLAUDE.md Self-review)
- Decision: steps and their commit-approval diffs skip the reviewer subagent; it runs once after the last step, before the push relay or any review or approval packet; on a must-fix finding Code stops and reports, and fixes only after approval.
- Rejected: a review before every Go commit-approval diff (the previous CLAUDE.md rule); fixing must-fix findings without approval (the previous rule's "Fix").
- Reason: one review of the final tree costs one subagent run per PR instead of one per step, and stop-and-report keeps every fix under the owner's approval.
- Reverse: restore the previous Self-review bullet in CLAUDE.md.
- Origin: chat

## 2026-10-22 · PR #45 · the smoke's canary check waits up to 60 s for policy enforcement (DESIGN.md sec 11)
- Decision: deploy/loadgen-smoke.sh polls POST /canary every 2 s for up to 60 s after Ready and needs refused twice in a row; connected and error mean poll again; check (1)'s later canary stays strict.
- Rejected: asserting refused at Ready (CI read connected at a pod age of about 0.7 s).
- Reason: kindnet enforces a new pod's policy only after a short lag, the gap the canary exists to detect; variant-smoke (f) shows the policy covers the API server path, ClusterIP and endpoint, once enforced.
- Reverse: replace the poll in check (3) with a single assertion.
- Origin: Code

## 2026-10-24 · PR #46 · Stage 5 lands as three PRs (amends docs/decisions-archive.md, PR #18 "gate/ in four PRs") (amended by the PR #48 five-PR entry)
- Decision: gate PR4 (Stage 5) is PR4a (internal/hostpairs, loadgen/wire, loadgen/client), PR4b (gate.Stage5 over kubectl and kind, cmd/gate -stage5) and PR4c (deploy/stage5-smoke.sh in deploy CI: gen 0 and three mutants).
- Rejected: one Stage 5 PR, or 4a with 4b (each likely over the review packet's ~600-line diff limit).
- Reason: each PR reviews alone, and 4a's packages are the ones the orchestrator needs too.
- Reverse: merge the remaining PRs into one.
- Origin: Code

## 2026-10-24 · PR #46 · The control API's wire types live in loadgen/wire, standard library only (DESIGN.md sec 10)
- Decision: the 18 wire types, BootIDHeader and the run kind, run state and canary result strings are exported from loadgen/wire; loadgen's typed constants are defined from them; loadgen/wire_golden_test.go pins every type's bytes against loadgen/testdata/wire/, and TestWireGoldenFull requires each _full case to set every field, so a new field fails until its golden file shows it; TestWireDeps keeps wire on the standard library.
- Rejected: exporting the types from package loadgen (every client would import the server and its dependencies); a client-side copy of the structs (two definitions of the orchestrator's contract).
- Reason: one definition of the wire for server, client, gate and orchestrator, and any byte change fails a test.
- Reverse: move the types back into loadgen/control_http.go, unexported, and drop loadgen/wire.
- Origin: chat

## 2026-10-24 · PR #46 · loadgen/client is strict and leaves the transport to its caller (DESIGN.md sec 10)
- Decision: the client takes a base URL and an *http.Client, and accepts a response only with its endpoint's success status, Content-Type exactly application/json, one Ek8s-Boot-Id header equal to the body's boot_id, strict decoding with nothing trailing, an echo of the requested id or run_id, and a body under a cap derived from loadgen's records page (TestClientRecordsCap); after Expect, another boot_id is ErrBootChanged.
- Rejected: kubectl --raw per call (loses the status code and the boot-ID header); client-go (a new dependency); lenient decoding (a mismatched loadgen would read as a valid answer).
- Reason: Stage 5 and the orchestrator take verdicts from these responses, so a response the client cannot account for is an error, never a guess.
- Revisit: the cap assumes no record string carries variant text (Fault holds loadgen's own errors only); if one ever does, cap that field in loadgen or budget 6 bytes per character for it. The strict checks are proven against httptest only until PR4b runs them through the API server proxy and kubectl proxy.
- Reverse: relax check() in loadgen/client/client.go.
- Origin: Code

## 2026-10-24 · PR #46 · Host /verify pairs: a mismatch hashes another fresh password; crypto/rand order; the caller sets the split (amends the PR #33 /verify pairs entry's mismatch method; DESIGN.md sec 8, 10)
- Decision: internal/hostpairs makes a match pair from a fresh password and hashcore.Hash of itself and a mismatch pair from a fresh password and hashcore.Hash of another fresh password, returns them in a crypto/rand Fisher-Yates order, takes both counts from its caller, and fully verifies /hash answers (OK only for Verify's (true, nil)).
- Rejected: a random canonical PHC string built in gate code (the parameter literal outside hashcore); a random-PHC helper in hashcore (a protected path).
- Reason: it uses only hashcore's exported Hash, and a hash of a password the variant never sees is as unpredictable to it as a random one; the cost is one Argon2 per mismatch pair, 8.56 ms wall per hash with 4 workers in the 4-vCPU container.
- Revisit: a hashcore helper once the orchestrator's per-window pair cost is measured.
- Reverse: replace Generate's mismatch branch; DESIGN.md sec 8 and sec 10 edits.
- Origin: Code

## 2026-10-25 · PR #47 · loadgen caps response headers at 1 KiB, 1xx included; an over-cap response is transport_error (amends the PR #45 memory-sum entry's in-flight term; DESIGN.md sec 10)
- Decision: loadgen's client sets MaxResponseHeaderBytes to 1 KiB (maxResponseHeaderBytes, loadgen/send.go); with no httptrace Got1xxResponse hook, net/http counts every 1xx response toward the same limit; an over-cap response fails the request as the variant's transport_error. BenchmarkStalledRequest measures the heap held per stalled request in four shapes; the worst, a body stall after bare-LF lines repeating one name, holds about 56 KiB, about 80 KiB with goroutine stacks.
- Rejected: an in-flight cap (it changes the open-loop design); not counting a Stage 5 boot_id change toward halting the run after repeated gate faults (it leaves live windows exposed and charges nothing to the variant); 4 KiB (about 180 KiB per stalled request, so about 304 MiB with four stalling live variants plus a burst, over 256Mi); 2 KiB (about 115 KiB, so about 236.5 MiB in that case, over the ~230 MiB GOMEMLIMIT).
- Reason: with Go's default 10 MiB limit, a variant sending large or many tiny headers could make loadgen hold 10-50 MiB per in-flight request, so a few dozen stalled requests could OOM-kill it (256Mi). At 1 KiB a burst at maxRate 19 (about 532 in flight x ~80 KiB = ~42 MiB) brings PR #45's ~117 MiB to about 159 MiB live, and four stalling live variants plus a burst (about 1,064 in flight) to about 200 MiB, under the ~230 MiB GOMEMLIMIT. gen 0's largest header block is 164 B, about 6 times under the cap.
- Reverse: drop tr.MaxResponseHeaderBytes in loadgen/send.go, with its tests and the benchmark.
- Revisit: a variant that legitimately needs more header bytes (it fails as transport_error); the 1,064 case is measured only as a heap benchmark, not in the cluster; whether kernel socket buffers count against the pod's memcg is unproven.
- Origin: Code

## 2026-10-25 · PR #48 · Stage 5 lands as five PRs: 4a, 4b1, 4b2, 4b3, 4c (amends the PR #46 "Stage 5 lands as three PRs" entry) (amended by the PR #49 six-PR entry)
- Decision: after 4a (#46) and the loadgen header cap (#47): 4b1 cluster plumbing (gate/tool.go runTool; gate/kube.go pod classifier, kubectl and kind wrappers, Ready wait); 4b2 the kubectl proxy transport and burst evaluation; 4b3 gate.Stage5; 4c cmd/gate -stage5 and deploy/stage5-smoke.sh in deploy CI.
- Rejected: one PR4b holding gate.Stage5 with its transport and cluster plumbing.
- Reason: each part reviews alone under the ~600-line packet limit, and 4b1's files carry no Stage 5 coupling, so the gate-pod series can reuse them.
- Reverse: merge the remaining PRs into fewer.
- Origin: chat

## 2026-10-25 · PR #48 · The gate runs docker, kubectl and kind through one exec helper, runTool; kubectl and kind as commands, no client-go
- Decision: gate/tool.go runTool(ctx, stdin, name, args...) runs every external tool; an ended ctx gives an error wrapping ctx.Err() without stderr, any other failure carries stderr; cmd.WaitDelay (toolWaitDelay, 5 s) bounds Wait when a process the tool started holds its output pipes. docker() wraps it with its error text unchanged; kubectl and kind are host prerequisites (gate/doc.go). kubectl() appends --request-timeout 30s (kubectl's default is none) and refuses args containing "--".
- Rejected: client-go (a large dependency tree for a few calls, and a second path to the cluster beside the kubectl the deploy scripts use).
- Reason: one place holds the ctx-versus-stderr split and the pipe bound; ci.yml installs no kubectl or kind, so the tests use fake scripts on PATH and never skip.
- Reverse: replace kubectl and kindLoad in gate/kube.go with client-go calls; inline runTool back into docker().
- Revisit: runTool kills only the direct child, so a process a tool starts (kind load's image export and import) outlives a cancel, and a SIGKILLed gate leaves a running kind load or docker command behind (kubectl ends within its request timeout); a process-group kill is parked in docs/STATUS.md.
- Origin: chat

## 2026-10-25 · PR #48 · Ready wait and pod health: which pod states are gate faults and which are candidate failures
- Decision: gate/kube.go readyCheck judges the first pod's Ready wait (readyTimeout 120 s, a poll every 1 s, each kubectl call cut off readyGrace 30 s past the deadline) and podHealth judges every pod after load. Gate faults: Deployment ReplicaFailure (admission refusal), ErrImageNeverPull, Evicted, DisruptionTarget, deletionTimestamp, pod phase Failed (kubelet admission rejection such as OutOfcpu; the pod stays Failed after its replacement is Ready, so any admission rejection during the wait or the load, an HPA scale-up included, is a fault), and at the timeout Pending, Unschedulable or any other waiting reason. Candidate failures: restartCount > 0, OOMKilled or Error, CrashLoopBackOff, any non-init container exit (Completed included), and at the timeout Running but never Ready. Every fault check precedes any failure check; Pending and not-Ready pods are judged only in the Ready wait.
- Rejected: one classifier with a phase switch (it mixes the two rule sets); Unschedulable as a fault at once (a previous candidate's pods may still be releasing resources).
- Reason: only the candidate's own behaviour may fail it; a node, admission or cluster problem says nothing about it, and a gate fault retries the candidate once, then halts the run (the retry rule lands in DESIGN.md sec 11 with gate.Stage5). No message field is decoded (terminated.message is candidate-written) and reasons print from an allowlist. The HPA's default 300 s scale-down stabilisation keeps a scale-down's deletionTimestamp out of the load window (inferred); readyTimeout and readyGrace are inferred (to be checked against real pod start times on the workstation).
- Reverse: edit readyCheck and podHealth in gate/kube.go.
- Revisit: Evicted and phase Failed as faults assume the candidate cannot drive node pressure (a variant with no ephemeral-storage limit that floods its log could be evicted under DiskPressure; the worst case is a halted run, never a pass); restarts and Running but never Ready are scored without checking the node, so a NotReady node or a node restart (ContainerStatusUnknown) counts against the candidate until gate.Stage5 checks node health first.
- Origin: chat

## 2026-10-26 · PR #49 · The gate reaches loadgen's control API through kubectl proxy on a unix socket in a private directory
- Decision: gate/controlproxy.go StartControlProxy runs kubectl --context C proxy --unix-socket <dir>/s --accept-paths '^/api/v1/namespaces/ek8s-system/services/loadgen:control/proxy(/.*)?$' (no --port; kubectl's default --reject-paths and --accept-hosts kept) in a 0700 directory from MkdirTemp under TMPDIR, or under /tmp when the socket path would be over 100 bytes or MkdirTemp fails there; it waits up to proxyStartTimeout (10 s) for the line "Starting to serve on <socket path>" and returns a loadgen/client Client whose http.Client dials only the socket, uses no proxy and HTTP/1.1, follows no redirect and turns any 3xx into an error; Close first stops all dialling (a dial already under way finishes first; any later one fails), then kills kubectl and removes the directory, so a Client used after Close cannot reach a socket another local user recreated at the removed path. Every ControlProxy error is a gate fault.
- Rejected: a TCP port (any local user could connect to it); kubectl get --raw per request (a process per call, and its output hides the response headers loadgen/client checks: Content-Type and the boot_id header); client-go (as in the PR #48 runTool entry).
- Reason: the directory limits the socket to the gate's uid (and root), and --accept-paths limits it to loadgen's control Service proxy path. On a kind cluster (kubectl v1.37.1 client, v1.37.0 node) the path filter applied on the socket (other paths 403), a non-localhost Host was refused, the socket was srwx------, exactly one Ek8s-Boot-Id and a Content-Type of application/json passed through (the API server adds Audit-Id and Cache-Control, so no exact header set is assumed), and a path with ".." segments got a 307 to the cleaned path, which can lie outside --accept-paths; kubectl's server, a Go ServeMux, sends that redirect for any unclean path before kubectl's filter runs (inferred from the go1.26 net/http source), so the client must never follow a redirect.
- Reverse: replace StartControlProxy with another transport; loadgen/client takes any *http.Client, so only gate/controlproxy.go changes.
- Revisit: a SIGKILLed gate leaves kubectl proxy serving the socket, and the directory on disk, until something kills it, and a world-writable TMPDIR without the sticky bit would let another user swap the directory between the privacy check and kubectl's bind (both in docs/STATUS.md Parked); kubectl proxy is given no --request-timeout, so only the http.Client's Timeout bounds a request; proxyStartTimeout is inferred; the Go client through the socket is tested only against a fake kubectl so far, and on a cluster only with curl.
- Origin: chat

## 2026-10-26 · PR #49 · Stage 5 lands as six PRs: the transport alone, then burst evaluation (amends the PR #48 five-PR entry) (amended by the PR #50 eight-PR entry)
- Decision: 4b2 is the kubectl proxy transport alone (gate/controlproxy.go); then 4b3 burst evaluation (gate/burst.go), 4b4 gate.Stage5, and 4c cmd/gate -stage5 and deploy/stage5-smoke.sh in deploy CI.
- Rejected: 4b2 as planned, holding the transport and burst evaluation (about 2,000 lines); burst evaluation inside the gate.Stage5 PR (over 1,000 lines).
- Reason: the transport with its tests came to about 1,300 lines with its review fixes, over the ~600-line review packet limit on its own.
- Reverse: merge burst evaluation back into the transport PR.
- Origin: chat

## 2026-10-26 · PR #50 · Stage 5 lands as eight PRs: burst evaluation splits into verdicts, pre-run steps and the burst run (amends the PR #49 six-PR entry)
- Decision: after 4b2 (#49): 4b3a the burst verdicts (gate/burstverdict.go, Stage 5 checks "responses" and "hash-verify"); 4b3b the Stage 5 pre-run steps over loadgen/client (the GET /config and GET /targets checks, Expect, PUT pairs, the canary) with a fake loadgen; 4b3c the burst run (start, the bounded poll, the records read and its fault checks, the drain); then 4b4 gate.Stage5, and 4c cmd/gate -stage5 and deploy/stage5-smoke.sh in deploy CI.
- Rejected: 4b3 as one PR (about 1,250 lines); two PRs, the run protocol with its fake loadgen (about 1,000 lines) and the verdicts; the verdicts inside the gate.Stage5 PR (about 860 lines).
- Reason: each part reviews alone under the ~600-line packet limit; the verdicts work on records alone and need no fake loadgen, and 4b3c holds the poll bound, the cancel and the drain for review on their own.
- Reverse: merge 4b3a, 4b3b and 4b3c back into one PR.
- Origin: chat

## 2026-10-26 · PR #50 · Stage 5 burst verdicts: any failed bucket fails "responses", every /hash pair is host-verified
- Decision: gate/burstverdict.go adds the Stage 5 check "responses" and, only if it passes, "hash-verify". A record fails "responses" if and only if its Bucket is "failed" (negative_accepted and bad_status included); success and graceful_shed pass. Every record with a Pair (once "responses" has passed, only success /hash records) goes to hostpairs.Verify, and a hash that does not verify, or that hashcore rejects as malformed or at other parameters, fails "hash-verify". A Skipped or Fault record, an empty or unknown Bucket, a success /hash record without a Pair, a Pair on a record of another kind, an empty record list, and ctx ending during Verify are gate faults; completeness (one record per index, the run's RunID and Target, no gap) is for the caller to check before burstVerdicts, in the burst run (4b3c). Detail holds counts, record indices (the first 8, ascending) and reason names from an allowlist of the classifier's reasons ("other" otherwise), never Pair, Evidence, Class or other text as given.
- Rejected: trusting 4b3c's record checks with no re-check here of the fields these verdicts read; verifying the pairs of failed records too; a hash that hashcore rejects as a gate fault (drift between the classifier and hashcore); Reason text printed as given.
- Reason: only the candidate's own responses may fail it, and a record loadgen cannot account for says nothing about the candidate; every hash is verified, not a sample (DESIGN.md sec 7); a failed candidate is discarded at "responses", and integrity culls are the orchestrator's (DESIGN.md sec 9); Detail must carry no candidate-controlled or free text.
- Reverse: edit gate/burstverdict.go.
- Revisit: a success hash that hashcore rejects is inferred unreachable (loadgen's classifier checks the hash first); host Verify time at r = 19 is unmeasured.
- Origin: Code

## 2026-10-26 · PR #51 · The gate-pod series (DESIGN.md sec 7 "Planned") waits until after the first evolutionary run
- Decision: the first run's gate uses the bare boot (bwrap, the nsenter relay, Stage 1's docker run sub-check); the "Planned" text stays in DESIGN.md sec 7 with one sentence saying so, and the gate-pod design packet item moves to docs/AFTER-FIRST-RUN.md.
- Rejected: building Stages 2-4 in a pod before the first run.
- Reason: the bare boot is built and tested; the pod series is a design packet plus several PRs that a first, fitness-ranked run does not need.
- Reverse: move the gate-pod item back to docs/STATUS.md Next and drop the sec 7 sentence.
- Origin: chat

## 2026-10-26 · PR #51 · Autonomous working mode: Code works along STATUS.md Next and asks the owner only on direction questions
- Decision: CLAUDE.md "Working mode: autonomous": Code plans, implements, self-reviews, opens each PR and continues; technical decisions are Code's (Origin: Code); it asks the owner, in one batched message, only on the non-negotiables, DESIGN.md direction or scope, off-path work, or what it cannot do or verify; findings are fixed in the PR, put in Next if they block the first run, or appended to docs/AFTER-FIRST-RUN.md; Next and Parked capped at 10. Commit, push, packet and chat-approval rules in CLAUDE.md, docs/handoff.md and reviewer.md now apply only when the owner asks.
- Rejected: the per-step chat relay (a chat-drafted prompt, a packet and an owner approval for each commit, push and step).
- Reason: owner time and quota; the owner accepts Code's technical recommendations.
- Reverse: drop the "Working mode: autonomous" section and restore the PR #51 edits to the commit, push, ask-before, DESIGN.md-approval, end-of-task packet, chat-material and self-review rules in CLAUDE.md, the packet-default, "Which packet" and hypothetical-routing lines in docs/handoff.md, and reviewer.md's description.
- Origin: chat

## 2026-10-26 · PR #52 · Owner answers to the skeleton plan batch (PR #51 comment)
- Decision: the 9-step plan approved; S1-S3 and S6-S12 yes (Stage 5 PRs merged into two; JSONL lineage, no SQLite; genomes as files plus sha256; full host verification of every window; live cap 3 (4 in probation); FIFO eviction, hall of fame, stagnation, escalation, a retry/re-PUT, docker rmi, salt growth and HPA tuning deferred; mutation model pinned to Sonnet 5.5); N1-N4 no; N5 accepted for the first run (bare boot unbounded; pressure on the workstation, not an escape).
- S4 yes, conditional: one baseline window and no stress half, only if none of the three fitness components S5 keeps (success_rate, p95 latency, cpu_per_success) needs the stress half (DESIGN.md sec 9); otherwise ask the owner before building fitness. Restore the stress half before any run that makes resilience claims.
- Rejected: N1-N4 (each weakens a non-negotiable); a cgroup-bound PR for N5 before the first run.
- Reverse: re-ask the owner.
- Origin: owner

## 2026-10-26 · PR #52 · Escalation list gains any third-party dependency
- Decision: CLAUDE.md "Working mode: autonomous" stop-and-ask list adds "adds any third-party dependency (a Go module or a tool)".
- Rejected: Code deciding dependencies itself and logging them (the earlier Working agreement default).
- Reason: owner's call; supply-chain exposure in a run that executes model-written code.
- Reverse: drop the clause from the escalation list.
- Origin: owner

## 2026-10-26 · PR #52 · Model-tier rule: Code stops when the next PR needs another tier
- Decision: CLAUDE.md "Working mode: autonomous" adds: mid tier (Sonnet, medium) for skeleton PRs 1-4 and 8; top tier (Opus, high) for PRs 5-7 (driver sandbox, orchestrator integrity and verification); Code stops and tells the owner when the next PR needs a different tier than the session runs.
- Rejected: one tier for every PR.
- Reason: owner's call; spend top-tier effort where the non-negotiables bite.
- Reverse: drop the "Model tier" bullet.
- Origin: owner

## 2026-10-26 · PR #53 · Every stop ends with a ready-to-paste resume prompt
- Decision: CLAUDE.md "Working mode: autonomous" adds a "Stop message" bullet: each stop's final message ends with one fenced block (Model/Effort/Why header, Prompt ID EK-RESUME-<UTC YYYYMMDD-HHMM>, the Guard line, what to do next, END OF PROMPT), preceded by "Or reply 'continue' in this session" when the same session can carry on.
- Rejected: free-form stop messages; a stored counter for prompt IDs.
- Reason: owner's call; a fresh session resumes with no chat relay, and the timestamp ID is unique without stored state.
- Reverse: drop the "Stop message" bullet.
- Origin: owner

## 2026-10-26 · PR #53 · Stage 5 burst run: every failure is a gate fault; the host's pairs are half matches; the canary is polled
- Decision: gate/burstrun.go runBurst does config check (r, window_s, burst_rate exactly), a fresh target, host pairs (half match, half mismatch), PUT, canary polled until refused (30 s bound), start, a bounded poll (burst length + 7 s client timeout + 15 s slack), then a records read that requires one record per index, this run and target, no gap. On bound or ctx end it cancels on a detached context and drains. Errors carry no Result and only cut, quoted text; the target is left for the caller to DELETE through burstResult.Through.
- Rejected: a candidate-failure Result for a run that ends error or cancelled; failing the gate at the first canary not refused; no drain; a real loadgen in the tests (a burst lasts 30 s).
- Reason: only the candidate's responses may fail it; kindnet enforces a new pod's policy after a lag (DESIGN.md sec 11); the 50/50 split follows the pool pairs (DESIGN.md sec 7).
- Reverse: edit gate/burstrun.go; merging 4b3b and 4b3c follows S1 (the PR #52 entry).
- Origin: Code

## 2026-10-26 · PR #54 · gate.Stage5: the orchestrator owns stress and namespace cleanup; a pod problem on a NotReady node is a fault; create, not apply
- Decision: gate.Stage5(ctx, Stage5Config, *Report) checks nodes Ready and the stress-ng pods Running, loads the image, `kubectl create`s the variant bundle, waits Ready, runs runBurst, then adds "deploy-ready" (on a failure), "pod-health", "responses", "hash-verify". A pod problem (never Ready, restarts) first re-checks nodes: a NotReady node makes it a fault. ContainerStatusUnknown is a fault. On a candidate failure it DELETEs the loadgen target through the last Seq; the namespace stays for the caller. Each cluster step runs under a 2 min stepTimeout. A fault ends the stage with an error and no Result (halt, S10).
- Rejected: Stage5 deleting the namespace (DESIGN.md sec 11 gives it to the orchestrator); `apply` (merges into a namespace that exists); the retry-once rule in code (S10).
- Reason: only the candidate's own behaviour may fail it; the stage must not start or stop stress itself (DESIGN.md sec 7).
- Reverse: edit gate/stage5.go. Resolves the node-check Revisit of the PR #48 pod-state entry. DESIGN.md sec 7 pass condition 1 and sec 11 "Gate faults" now state the pod-state and retry rules (overview.md stale line extended).
- Origin: Code

## 2026-10-26 · PR #55 · lineage: JSONL attempt table, file genome archive, no candidate text in a record
- Decision: package lineage keeps a run directory: genomes/<id>/ (files plus a sha256 over names, lengths and bytes, written once by rename, digest re-checked on read), attempts.jsonl (one validated Attempt per line, append-only; a later line with the same generation_id supersedes, which is how live flags change), transcripts/<id>.json (written once). A record has the gate stage and check name only, never a Detail; its other strings are ids, enum values, lowercase tokens, or the model's rationale (cut to 8 KiB); a gate-failed attempt is discarded with no fitness. Open refuses a torn last line.
- Rejected: SQLite and git commits for the first run (S2, S3); storing gate Detail or evidence (candidate-controlled, never for a prompt); update-in-place of a record.
- Reason: no dependency; the file loads into SQLite later; the prompt builder reads records, so they must not carry candidate text.
- Reverse: replace lineage's storage; the Attempt struct is the contract. DESIGN.md sec 14 says so (overview.md stale line extended).
- Origin: Code

## 2026-10-26 · PR #56 · Fitness: S4 condition holds (no component needs the stress half); kubelet counters; exact p95; starting bounds
- Decision: DESIGN.md sec 9's three components (success_rate, p95, cpu_per_success) and the 0.5 shed credit are each defined per half and computable on a baseline half alone, so S4 proceeds: fitness/ scores one half, Combine keeps the min rule for later. success_rate = (successes + 0.5 sheds) / non-negative records; p95 nearest rank over the successes (exact, no histogram); cpu_per_success from the kubelet's pod_cpu_usage_seconds_total delta, a vanished pod, a reset or a skipped or fault record being errors. Latency and CPU terms count in proportion to the served share (successes / requests), so shedding nearly everything cannot keep full marks on both. Starting guesses: latency 50 ms to the hang timeout, CPU 0.05 to 0.5 s per success, weights 0.4, 0.3, 0.3.
- Rejected: metrics-server (a rate, not a counter); an HdrHistogram dependency (at most a few hundred records a window; no new dependency); an own Prometheus library (a 60-line parser of one series).
- Reason: DESIGN.md sec 9 wants cumulative CPU-seconds (resolves the STATUS Parked item); the smoke checks that the kubelet serves the series.
- Reverse: edit fitness/; restoring the stress half needs only a second Score and Combine's stress argument.
- Origin: Code

## 2026-10-26 · PR #57 · Driver invocation: acceptEdits, stream-json with an init check, "--" before the prompt
- Decision: driver/ runs `claude -p --tools "Read,Edit" --restricted --model claude-sonnet-5-5 --permission-mode acceptEdits --permission-prompts none --max-turns 30 --strict-mcp-config --no-session-persistence --output-format stream-json --verbose -- <prompt>`, 15 min wall clock. It kills the session as soon as its first event is not an init with tools exactly Edit and Read, no MCP server, the pinned model, acceptEdits, cwd /sandbox/work and the pinned version; the result must be the last event, success, on the pinned model (modelUsage). Copy-back reads only the parent's names, as regular files (O_NOFOLLOW); extras are counted, never read. Usage is returned with every error that got as far as a result.
- Rejected: DESIGN.md's argv as written (default mode with prompts none denies every Edit; json output shows the tool set only after the session ran); a prompt that could parse as a flag; trusting the flags without reading what the session reports.
- Reason: verified on Claude Code 2.1.310 in the sandbox: the init event lists the real tool set before any API call, and `--` ends option parsing. DESIGN.md sec 12 updated (overview.md stale line extended).
- Reverse: edit driver/sandbox.go claudeArgs and driver/stream.go. A session killed before its result event has no usage to count, so the usage ceiling undercounts such timeouts.
- Origin: Code

## 2026-10-26 · PR #57 · Driver sandbox: fixed in-sandbox paths, token through --args, process-group kill, version pin by config
- Decision: bwrap gets `--unshare-all --unshare-user --share-net --disable-userns --die-with-parent --new-session --cap-drop ALL --clearenv`, binds work and home at /sandbox/{work,home}, the resolved binary at /sandbox/bin/claude, lib dirs (host symlinks recreated), certs and resolver files, /proc, --dev, a 256 MiB tmpfs /tmp; options go through `--args 3` on a pipe so the token is in no command line. bwrap runs in its own process group, SIGKILLed on timeout or cancel; --die-with-parent (also bwrap's own death signal if the driver dies) and the pid namespace take the rest. Certificates are bound by directory, never /etc/ssl or /etc/pki whole (private keys). Config carries the expected version; New checks `--version` in the sandbox, and each call refuses a binary whose file changed.
- Rejected: --unshare-user-try (falls back silently); the token in argv (/proc/*/cmdline); binding /usr or /etc whole; a hard-coded version (the owner's installer moves on).
- Reason: bound the tree to what claude needs (tested: a walk of the sandbox finds no module path, hashcore, test vectors, go.mod or .git); no Pdeathsig of our own (gate/internal/relay's reason: it can fire when the forking OS thread exits; --die-with-parent carries the same caveat, harmless while no goroutine locks and exits a thread).
- Reverse: edit driver/sandbox.go sandboxOptions and runSandboxed.
- Origin: Code

## 2026-10-26 · PR #57 · Prompt builder: lineage records only; the lineage is the attempts made from the parent's ancestors
- Decision: BuildPrompt(all attempts, parent id, fitness.Params) states the rules (non-negotiable line, pinned hasher.go, edit-only, fixed signatures, gate Stage 1's import list, hang timeout), the parent's components per half with normalized scores (fitness.Params.Normalize, shared with Score) and the weakest by weighted shortfall, and the last 3 attempts whose parent is the parent or an ancestor, each with outcome from ids, enums and numbers and its rationale cut to 600 bytes, one line, quotes and control characters removed, framed as data. Normal mode only (escalation deferred, PR #52).
- Rejected: last 3 attempts of the run (other lines); the parent's descendants only (gen 0 has none); importing gate for the import list (gate's build holds hashcore and the pool; the driver's must not, TestDeps), so the list is copied and a test compares it with gate.AllowedImports.
- Reason: DESIGN.md sec 12 per-call context; no input type carries gate Detail or candidate output.
- Reverse: edit driver/prompt.go.
- Origin: Code

## 2026-10-26 · PR #58 · Owner answers to the PR #57 driver questions: all four yes
- Decision: the four PR #57 questions are approved as built: the extra flags (acceptEdits, --strict-mcp-config, --no-session-persistence, "--"); stream-json with the init check; the bwrap hardening beyond DESIGN.md sec 12; the claude version pinned by config. The three PR #57 entries above stand.
- Rejected: none proposed by the owner.
- Reason: owner's call (session prompt, "I accept all the recommendations in PR #57"); no PR comment.
- Reverse: re-ask the owner.
- Origin: owner

## 2026-10-27 · PR #58 · Orchestrator window mechanics: one Session per loadgen, run-wide exclusions, CPU fail-closed per variant
- Decision: orchestrator.Session checks GET /config's r, window_s and burst_rate exactly and expects its boot_id. RunWindow uploads one host Batch per live variant (batches name no target, so the gap makes live + 1 before the gate), stops stress and waits until its pods are gone, excludes without starting on a kindnet DaemonSet not Ready or a canary not refused (polled up to 30 s after a new boot_id), samples the kubelet's CPU counters around the run, polls it, reads every record, and excludes run-wide for a run not done, a ring gap, a skipped or faulted send, lag p99 over 10 ms or any lag over 100 ms. A restart is an exclusion; Recover re-PUTs the live targets. Drop DELETEs through the last Seq read, then the namespace. A variant whose pod vanished or whose counter reset scores at the CPU ceiling; every variant's at once (more than one) is a fault. A batch is refused if used before. Excluded is sorted. Streak halts after 3 excluded windows. First run: a window is one baseline half, every hash verified every window (PR #52 S4, S9).
- Rejected: excluding the window for a variant's own CPU failure (a variant could stall the run at will); running a window already excluded by its canary (5 min of load for nothing); batches bound to ids before the gate (a newcomer would have none); every kubectl or loadgen error as an exclusion (the first run halts on faults, S10).
- Reason: DESIGN.md sec 10, sec 11; every Session error is a harness fault, never a variant's.
- Reverse: edit orchestrator/session.go and window.go; DESIGN.md sec 11 "Stress state" and "Excluded windows" edits (overview.md stale line extended).
- Origin: Code

## 2026-10-27 · PR #58 · Host confirmation of integrity claims: the shared classifier on the evidence, a host salt history, unconfirmed claims a fault
- Decision: the host re-checks each claim's evidence with internal/classifier: a /hash answer on the variant's host salt history (a classifier.Run with no Verifier fed every recorded pair hash: the Stage 5 burst's via gate.Report.BurstHashes, then each window's, never emptied on a loadgen restart), then hashcore Verify; a /verify answer against the host's own batch truth; a negative's 2xx only if its request bytes break the contract on the endpoint they went to, read as gen-0 reads them (encoding/json, unknown members ignored); for that, evidence carries the request path (wire.Evidence.path, a wire format addition). Pairs that fail full verification or repeat a salt are confirmed by construction. Any host-found integrity reason confirms, excluded window or not; claims of which none confirm are a fault. loadgen/client gains ExpectAny, so Recover can read a new boot_id.
- Rejected: taking loadgen's reason (DESIGN.md sec 9 asks for the host); host checks written apart from the classifier (two implementations could drift); culling on an unconfirmed claim (a loadgen bug would cull an honest variant); a strict reading of requests (an honest wrapper that ignores unknown members could be culled); checking a negative on every endpoint its class allows, without the path (verify_wrong_type_hash reads as a valid /hash body, so a mutant failing open on it would leave only unconfirmed claims and could halt the run at will); dropping unconfirmed claims silently (a disagreement nobody sees).
- Reason: settles DESIGN.md sec 10 "Evidence" (PR #41 entry: "settled in the orchestrator design"); a salt first seen in the burst needs the burst's hashes on the host.
- Reverse: edit orchestrator/integrity.go; DESIGN.md sec 10 "Evidence".
- Origin: Code

## 2026-10-27 · PR #59 · Orchestrator loop: windows only after a promotion, until scored; a loadgen restart halts
- Decision: orchestrator.Loop gates and scores generation 0 alone, then per generation: tournament (k = 2, distinct, ties to the first drawn) over passed, undisqualified, scored genomes; driver.Mutate while Session.Batches makes live + 1 batches, the gate only after; StressOn, Stage 5, Adopt; then probation windows (fresh batches each) until one is scored or the newcomer is culled; integrity culls every window, scored or not; the lowest score culled back to 3, a tie culling the newer. Recorded fitness is the latest scored window's. No candidate (a session that failed or edited nothing): recorded, no gate, no window, batches cancelled; a Mutate error with no session started is a harness fault. Stress-ng is off at start and again right after Stage 5. Halts: S10 loadgen restart, from a window or a cull's DELETE (Session.Recover unused in the first run, superseding the STATUS Next wording), gate fault (no attempt record: not the candidate's outcome), 3 excluded windows, no parent; stops: N generations, a num_turns or token ceiling (all four token kinds).
- Rejected: Recover after a restart (owner's S10 says halt); a window after every generation (5 min with nothing to cull); culling on an excluded window's scores, or leaving 4 live past it; the mean over windows as fitness (mixes conditions further; revisit with the archive design); halting on a failed session (a model's bad session is an outcome, not a fault); stress left on until the next window (the next host pairs and gate run would share the host with it).
- Reason: DESIGN.md sec 11 steps 1-9 with PR #52 S4, S8-S10; the gap order of sec 11.
- Reverse: edit orchestrator/loop.go; DESIGN.md sec 11 and sec 13 "First run" paragraphs.
- Origin: Code

## 2026-10-27 · PR #59 · Lineage: a mutation reason for attempts with no candidate, and summary.json; cmd/orchestrator start-up guards
- Decision: lineage.Attempt gains `mutation` (`no-change`, `session-failed`), set only with a zero gate result, discarded, no fitness, flags, genome or reason; driver's prompt names those outcomes. Run.WriteSummary writes summary.json once (reason token, generations, total usage, time). cmd/orchestrator reads CLAUDE_CODE_OAUTH_TOKEN and unsets it before any child runs, and refuses to start with a variant namespace or a loadgen target left from an earlier run; a halted run leaves its variants for inspection. A failing gate Detail (candidate-controlled) goes to the operator's stderr only, %q-quoted (no terminal escapes) and cut to 1 KiB; never to lineage or a prompt.
- Rejected: recording a driver failure as a gate failure at some stage (a lie in the table); no record for it (DESIGN.md sec 11 step 8 records every attempt); the run summary as an attempts.jsonl line (it is not an attempt); the token as a flag (visible in ps); no Detail anywhere (a failed candidate is not archived, so the log is the only clue to why it failed).
- Reason: DESIGN.md sec 13 summary record and sec 14 schema; schema additions only, older records still read.
- Reverse: drop Attempt.Mutation and lineage/summary.go; edit cmd/orchestrator/main.go.
- Origin: Code

## 2026-10-27 · PR #60 · End-to-end dry run: fakeclaude dry-run mode, 30 s windows, 2 generations
- Decision: deploy/orchestrator-smoke.sh runs the real orchestrator with fakeclaude as -claude; the fake treats a real BuildPrompt prompt (first word "You") as dry-run mode: comment edit, or a broken build when the prompt lists exactly one earlier attempt. Loadgen's `-window` is patched to 30 s in the smoke, not changed in the renderer; 2 generations (ORCH_GENERATIONS overrides), checks by jq on attempts.jsonl and kubectl.
- Rejected: a renderer `-window` flag (a production knob only CI uses); a state file for the fake (the sandbox is fresh per call); 3 generations by default (three live variants at CI size risk Unschedulable pods).
- Reason: exercises Kube, the loop's waits, gate Stages 1-5 and lineage once per CI run without quota (no cull: the cap is 3, which does not fit the CI node); the prompt already carries what the fake needs.
- Reverse: delete the script, its deploy.yml step and fakeclaude's dryRun.
- Origin: Code

## 2026-10-28 · PR 9 (first real run, docs PR) · Run 1 recorded: r = 11 provisional, baseline-only windows; findings
- Decision: none new; records run 1 (owner's Arch workstation, exit 0, reason generations, 10 of 10 mutations passed the gate, 38 window scores, 8 culls, 0 exclusions, 43 turns, 1 h 22 min). r = 11 was provisional: the argon2 CLI on the host measured 43 ms per hash at t=2, m=19456 KiB, p=1, so 23 hashes/s per core, r = 0.5 x that, rounded down. Windows were baseline-only (no stress), so success_rate was 1 for every variant and C0 was never measured.
- Findings: the CPU floor 0.05 s is above this host's cpu_per_success (0.0424), so the CPU term clamped to 1 for every variant; with latency scored from 50 ms to HangTimeout, 1 ms of p95 moves a score about 0.00015, so run 1's culls ranked p95 jitter; the cull rule has no minimum margin; "a tie culls the newer" holds only if l.live is ordered oldest-first (orchestrator/loop.go:468-480), unverified. Follow-ups: docs/AFTER-FIRST-RUN.md, 2026-10-28.
- Rejected: changing the floors or the cull rule in this PR (the owner decides host-calibrated floors).
- Reverse: n/a (a record, no change).
- Origin: Code

## 2026-10-28 · PR #65 · C0 counts shed and failed together, negatives out, scored windows only; maxRate not raised (DESIGN.md sec 10)
- Decision: C0's "sheds at most 1%" counts graceful_shed and failed together, negatives excluded, and needs a scored window; a pass at maxRate (19) gives C0 >= 19, and maxRate is not raised for C0.
- Rejected: graceful_shed alone (a step of timeouts would pass); raising maxRate for a single target (the PR #45 memory sum is unmeasured).
- Reason: an excluded window's offered rate was not r, and fitness scores failed below shed, so a C0 that ignores failures overstates capacity.
- Reverse: DESIGN.md sec 10 edit.
- Later: C0 was measured once above maxRate with an uncommitted single-target patch, reverted afterwards; maxRate itself is unchanged (see the 2026-10-29 C0 = 31 entry).
- Origin: chat

## 2026-10-28 · PR #65 · C0 tool: HPA merge patch, loadgen redeployed per step, excluded step not measured, no exported API changed
- Decision: orchestrator/cmd/c0 sets the HPA to 1/1 with a kubectl merge patch after creating the bundle, not a variant.Params field; each step runs deploy/loadgen-up.sh (a rollout restart if the manifest was unchanged) for a new boot_id and an empty loadgen; a step whose window is excluded or whose pod/HPA/restart checks fail is not measured (rerun once, then halt); a gen 0 disqualification halts. orchestrator, gate, fitness, loadgen/client and deploy/variant are used as exported, unchanged; maxRate is checked through deploy/loadgen's Params.Validate (loadgen's own Config.Validate).
- Rejected: a Params field for the HPA bounds (only this tool wants a single-pod bundle, and the golden files and Stage 5 would carry it); one loadgen across steps (its salt sets and target would carry over); treating an excluded window as a fail (its offered rate was not r); exporting maxRate.
- Reason: the brief ruled out refactoring the reused packages; a fresh loadgen per step keeps each window independent, as in the run.
- Reverse: add the HPA bounds to variant.Params and drop the patch; keep one loadgen and Drop the target per step.
- Origin: Code

## 2026-10-29 · PR #65 · C0 = 31, r = 15.5 for run 2 (b = 62), measured with pinned clocks
- Decision: C0 = 31, so r = 15.5 and b = 62 for run 2; measured on the owner's workstation with pinned clocks and an uncommitted single-target patch (maxRate 40, maxBurstSends 4800, reverted afterwards); the committed limits stay 19 and 3000. Record: 0 shed and 0 failed in every pass, p95 33 ms, CPU per success 0.0296-0.0311 s from r 8 to 31; r = 32 failed on p95 550 ms, r = 31 passed 3 times.
- Rejected: the unpinned results (C0 >= 19, and Unresolved: 28 passed once, then failed its confirm window at 15% shed, p95 644 ms).
- Reason: CPU per success moved 2x with clock state, and C0 is set by the 1-CPU limit at about 0.0305 s per request (about 0.94 core at r = 31).
- Reverse: re-measure if the host, the clocks or the Argon2 parameters change.
- Origin: chat

## 2026-10-29 · PR #65 · Host clocks are pinned for C0, calibration and every run; c0 refuses otherwise
- Decision: governor performance on every CPU and turbo off (intel_pstate no_turbo = 1, or cpufreq boost = 0) for C0, calibration and every run; internal/hostclock reads them and c0 records them in its JSON header and refuses unless pinned (-allow-unpinned overrides and is recorded; no readable governor counts as unpinned).
- Rejected: leaving the clocks to the OS and averaging the noise out.
- Reason: unpinned clocks moved gen 0's CPU per success 2x (0.0225 to 0.047 s) and its p95 from 27 to 67 ms across runs on one host.
- Reverse: drop the preflight check; keep recording the state.
- Origin: chat

## 2026-10-29 · PR #66 · The orchestrator checks the host clocks once at start and records them in run.json
- Decision: the orchestrator calls hostclock.Preflight (moved from c0) once at start, before any cluster action, and records host_clock and allow_unpinned in run.json; lineage.Create marshals run.json before creating anything, so an unrecordable value leaves no run directory.
- Rejected: a check before every window (adds a halt path, and a reboot ends the run anyway); summary.json only (lost when a run halts or crashes); EPP in the refusal rule (recorded only); a typed host_clock field (it would add internal/hostclock to the driver's dependencies, TestDeps).
- Reverse: drop the Preflight call and the run.json fields.
- Origin: chat

## 2026-10-29 · PR #67 · Code runs PRs end to end; reply rules in CLAUDE.md
- Decision: Code commits, self-reviews once, pushes, opens the PR and replies with a review packet without approval; it stops only for a design question with options, the escalation list, or an owner-only action. CLAUDE.md "Reply rules" fix what a reply carries.
- Rejected: approval before every commit; a prompt-by-prompt restatement of output rules.
- Reason: per-commit round trips and incomplete packets cost most of a week's quota by Thursday (2026-10-29); the owner wants to sit between chat and Code only for decisions, workstation actions and merges.
- Reverse: restore the replaced CLAUDE.md wording.
- Origin: owner

## 2026-10-29 · PR #68 · Prompt guard: header line, END OF PROMPT, a prompt log on the host
- Decision: CLAUDE.md "Working agreement" gains a Guard bullet: a prompt with a Prompt ID opens with `Model: <x> | Effort: <y> | Why: <one line>` and ends with END OF PROMPT; Code replies only WRONG SETTINGS (model or get_session effort_level differs), TRUNCATED (no END OF PROMPT) or ALREADY RUN (ID in /tmp/ek8s-prompts.log), else appends the ID and `date -u` to that log.
- Rejected: the guard restated in every prompt only (lost when a prompt is pasted without it).
- Reason: owner's decision (2026-10-29): a wrong tier, a cut paste or a re-sent prompt costs a session's quota before anyone notices.
- Reverse: drop the bullet.
- Origin: owner

## 2026-10-29 · PR #68 · Owner answers to the R2-SH stress-half questions: Q1-Q4 A
- Decision: Q1 A: a window is two loadgen runs of W each, a baseline run then a stress run (W, C0, r and the loadgen limits unchanged). Q2 A: a window is scored only when both halves are; the stress half is not run once the baseline half is excluded; the streak counts windows. Q3 A: CI runs both halves at the stress-ci dose (fall back to a lower CPU_LOAD only if CI shows lag exclusions, logged here). Q4 A: stress-ng off right after the stress run, before its records are read and verified; one host verification per window. Code's two decisions accepted (weakest component from the half that sets the min; lineage records both halves).
- Rejected: W/2 halves (reverses the PR #45 W entry; C0 was measured at W = 5 min); one run with a mid-run switch (variable half length, a transition band, CPU charged across the switch); scoring the remaining half (a baseline-only fitness sits above min()); re-running an excluded half.
- Reason: owner's call (2026-10-29, design packet R2-SH).
- Reverse: re-ask the owner.
- Origin: owner

## 2026-10-29 · PR #68 · Stress half: two runs per window, per-half judgement, RunBaseline for C0 (DESIGN.md sec 9-11)
- Decision: Session.RunWindow runs runHalf twice (batches[2i], [2i+1] for live[i], each uploaded just before its run, since an upload replaces a pending batch): upload, stress off (baseline) or on and Running (stress), kindnet, canary, CPU, run, CPU, stress off (a defer also stops it on any early return), records, exclusions. The host verifies both halves' pairs in one hostpairs call, then judges each half in order on one salt history (a stress salt repeating a baseline one is caught); claims a half confirms none of stay a fault, per half as per run before; a restart in the stress half still judges the baseline half's records. Each half scores with its own CPU samples; every variant's CPU unmeasured at once in a half is a fault. RunBaseline keeps C0's one-half, no-stress window. WindowResult names each half's run_id and exclusions; VariantResult carries Baseline and Stress HalfScores and the half a disqualification came from.
- Rejected: a Windows interface change for halves (the loop only needs twice the batches); judging the baseline half before the stress run (Q4 B); a window-wide unconfirmed-claims rule (a confirmed half could hide a disagreement in the other).
- Reason: owner answers above; loadgen stays unaware of halves (PR #36 entry).
- Reverse: edit orchestrator/window.go and loop.go; c0 back to RunWindow only if C0 ever needs stress.
- Origin: Code

## 2026-10-29 · PR #68 · lineage.Half gains its score; the prompt's weakest component is the lower half's
- Decision: lineage.Half has `score` (the half's fitness.Score); Fitness.Score is the min of both. driver.BuildPrompt shows each half's score when both are scored, marks the lower (a tie goes to stress) as the one that sets the fitness, and names the weakest component from that half only.
- Rejected: deriving the half score in the prompt from the stored components (Served is not stored, AFTER-FIRST-RUN); the largest shortfall across both halves (a shortfall in the higher half cannot raise min()).
- Reason: schema addition only; run-1 records read with score 0 and have one half, so nothing compares them.
- Reverse: drop Half.Score and the bind logic in driver/prompt.go.
- Origin: Code

## 2026-10-29 · PR #68 · Fitness floors set just under gen 0's pinned baseline
- Decision: DefaultParams CPUFloor 0.05 s to 0.025 s and LatencyFloor 50 ms to 25 ms; ceilings unchanged. Gen 0's pinned baseline (CPU per success 0.0296-0.0311 s, p95 33 ms) now sits above both floors, so gen-0-like variants no longer clamp to 1.0 at baseline.
- Rejected: calibrating from stress data first (run 2 is the stop-or-go check); floors at gen 0's own values (gen 0 still clamps).
- Reason: owner decision; the 0.05 s and 50 ms floors clamped gen 0 to 1.0 on both terms, so run 1's culls ranked p95 jitter.
- Reverse: restore CPUFloor 0.05 and LatencyFloor 50 ms in fitness/params.go.
- Origin: owner


## 2026-10-31 · PR #69 (r3a-static-cpu) · Run 2 recorded: STOP, load far below capacity (amended by the 2026-11-02 Stage 5 stall entry)
- Decision: run 2 (r = 15.5, b = 62, -generations 2) is a STOP. First attempt: gen 0 failed its own Stage 5 gate (220 of 1860 over_hang_timeout, from index 1428), 0 turns; by hand gen 0 then passed Stage 5 at full and near-zero stress dose, and run2b passed Stage 5 3 of 3: read as a rare flake at b = 2 x C0 with zero tolerance.
- run2b: exit 0, 2 generations, 13 turns, 3 windows, 6 halves, 0 excluded. Gen 0 stress half success_rate 1 (no sheds, no failures), p95 40/43/53 ms as live grew 1/2/3, score 0.9930/0.9914/0.9892; gens 1-2 matched gen 0 within noise; window-to-window drift (~0.004) >> spread between variants in one window (~0.0002).
- Cause: load far below capacity (~0.5 core per variant, idle cores beside the 3-CPU stress dose).
- Origin: owner

## 2026-11-01 · PR #69 (r3a-static-cpu) · Harsher environment via load, in two PRs
- Decision: PR A (this one): static CPU manager, loadgen exclusive core. PR B: r = C0 = 31, maxRate 31, b decoupled from r (b decided after PR A from about 10 Stage 5 runs of gen 0 at b = 62 on the rebuilt cluster). Then run 3, -generations 2: Go if gen 0 sheds or fails under stress and variants spread more than window drift.
- Rejected: one PR changing everything (cannot attribute a result to one change).
- Reason: owner decision after run 2.
- Reverse: drop cpuManagerPolicy and restore the PR #29 sizing (see the "Static CPU manager, loadgen 1 CPU / 384Mi" entry).
- Origin: owner

## 2026-11-01 · PR #69 (r3a-static-cpu) · Static CPU manager, loadgen 1 CPU / 384Mi, stress-ng 2750m request
- Decision: cluster.yaml `cpuManagerPolicy: static` (CI keeps none); loadgen requests = limits = cpu 1, memory 384Mi (GOMEMLIMIT 362387866); stress-ng CPU request 2750m, limit "3", memory unchanged. CI size: `EK8S_LOADGEN_CPU_REQUEST=100m`, set once in deploy.yml, read by loadgen-up.sh and passed to the renderer as `-cpu-request` (limit stays 1); stage5-smoke.sh's own patch is gone. CI: Burstable 100m/1, cpuManagerPolicy none; workstation: Guaranteed 1/1, exclusive core (static policy).
- Rejected: keeping 256Mi (at r = 31 the PR #45/#47 estimates reach the ~230 MiB GOMEMLIMIT, and GC pressure shows as send lag and excluded halves, confounding run 3's Go test); stress-ng limit 2750m (CFS throttling pulses the neighbour every 100 ms and lowers the dose, changing two things versus run 2); the static policy in CI (does not fit a 2-CPU runner); a post-deploy kubectl patch for CI size (a second Recreate rollout, two mechanisms).
- Reason: the Guaranteed integer CPU takes loadgen out of the shared pool so the neighbour cannot starve the sender; stress-ng stays in the shared pool (non-integer request) and the node budget is 6800m of 7000m.
- Reverse: remove cpuManagerPolicy and restore 750m / 256Mi and the stress-ng 3 CPU request; reverses the PR #29 sizing entry (docs/decisions-archive.md).
- Origin: Code

## 2026-11-01 · PR #69 (r3a-static-cpu) · Smoke check (7) and the CPU request bound
- Decision: loadgen-smoke.sh check (7) reads the kubelet's cpu_manager_state (docker exec on the node) and, under static, requires one exclusive CPU outside defaultCpuSet and loadgen's Cpus_allowed_list equal to it (ephemeral container, --target=loadgen, process found by comm); under none it prints a skip. The renderer accepts a CPU request of "1" or 1m-1000m only.
- Rejected: jq for the state file (a new tool for this script; sed/grep/awk suffice); assuming pid 1; an unbounded request (fails at apply, far from the cause).
- Reason: nothing else proves the static policy took effect.
- Reverse: delete check (7); restore cpuRequestRE in deploy/loadgen/loadgen.go.
- Origin: Code

## 2026-11-02 · PR #70 · Stage 5 stalls on an HPA pod add are a harness defect (amends the 2026-10-31 run 2 entry's "rare flake")
- Decision: recorded. At b = 62, stress on, 10 of 37 gen-0 Stage 5 runs failed on one contiguous ~213-225 over_hang_timeout run (~5 s stall of new connections), only when the HPA's second pod appeared mid-burst; kindnet logged "Could not receive message ... netlink receive: no such file or directory" (controller.go:305) in 5/5 failing runs, 0/7 passing. Run 2's first-attempt Stage 5 failure (220 of 1860 from index 1428) is this defect, not a flake.
- Cause candidate: kindnet's vendored kube-network-policies (v1.0.1-0.20260528073847-f67f0fb35e2b, kind v0.33.0) deletes and recreates its nftables table on every sync, and deleting a base chain drops every packet waiting in an nfqueue. Changed upstream in v1.1.2 (rules replaced in place, table kept); not yet confirmed as the cause; no kindnetd image carries v1.1.2 (newest kindest/kindnetd tag v20260820-69b56db7, ours).
- Rejected: lowering b or the gate's tolerance (hides the defect); changing the environment around it (design packet option 3).
- Reason: zero-tolerance Stage 5 must fail only for the candidate, and the same stall can hit live windows whenever an HPA scales.
- Reverse: n/a (a finding); the fix is decided after the experiment (Q2).
- Origin: owner

## 2026-11-03 · PR #70 · Owner answers to the kindnet-stall design packet: Q1 yes, Q2 deferred, Q3 yes, Q4 yes
- Decision: Q1 yes: run deploy/diag/stage5-nopolicy.sh on the workstation cluster (both policies on the burst's path removed and the canary on 192.0.2.1:443 in its OFF arm, gen 0 only, guards G1-G5). Q2 (switch to Calico v3.33.0, CI parity, reversing the PR #31 "another CNI out of scope" rejection, DESIGN.md sec 11 rewrite) deferred until the experiment's result. Q3 yes: re-measure C0 on any new CNI before PR B. Q4 yes: option 4 now as detection and recording only (D3); its retry and exclusion rules go to docs/AFTER-FIRST-RUN.md.
- Rejected: Calico now (unconfirmed cause); retry-once and cross-variant exclusion now (fault-vs-fail semantics, S10).
- Reason: owner's call (2026-11-03, prompt EK-D1-STALL-20261103).
- Reverse: re-ask the owner.
- Origin: owner

## 2026-11-03 · PR #70 · deploy/diag/stage5-nopolicy.sh: an alternating ON/OFF experiment in bash, no Go change
- Decision: one workstation-only script runs real `gate -stage5` bursts on gen 0 at b = 4r, alternating ON (cluster as deployed) and OFF (NetworkPolicies ek8s-system/loadgen and the diag's ek8s-v999997/variant deleted, canary arg patched to 192.0.2.1:443 with a JSON-patch test op), at most 15 runs per arm, never extended. Each run asserts its arm's state (policies, start line canary_addr, POST /canary refused twice, loadgen's and hasher IPs against kindnet's podips-v4 set about every second, records read in every judged run); OFF restores ON after every run and on exit (G5). Per run it keeps loadgen's netns TcpExt TCPSynRetrans and Tcp RetransSegs (/proc/<pid>/net via the node; per-netns counters, kernel snmp_counter.rst and tcp_output.c), the raw kindnet log lines, sync-line times, and records reduced to index, kind, send time, latency, bucket, reason, skipped, fault.
- Rejected: a build-tag hook in gate/ (a no-isolation path in gate source beside fail-closed code); nstat (not on every host; /proc read directly); raw records on disk (pairs carry passwords and hashes); one ON-to-OFF switch for the whole experiment (time drift confounds the arms).
- Reason: packet item 3 and owner feedback F2-F6; kindnet queues a connection when either end is policy-selected (vendored controller.go), so both policies must go, and the canary with loadgen's.
- Reverse: delete deploy/diag/ and its deploy/README.md section.
- Origin: Code

## 2026-11-03 · PR #70 · Diag review: G4 ancestry, stress-ng asserted per run, r from the deployed loadgen
- Decision: G4 also refuses unless HEAD is an ancestor of origin/main or of PR #70's head (`git fetch` of both, refs/pull/70/head, first; a failed fetch refuses; the result goes to meta.txt). Before every gate start, both arms: exactly one stress-ng pod, Running and ready, with origin/main's deploy/stress requests, limits and stress-dose data (read by `git show`); after the gate, the same pod, no restart; otherwise a violation, the run not judged; summary.tsv records it. -rate defaults to the deployed loadgen's -rate argument (required with none); the recovery message prints the r used and its source.
- Rejected: HEAD checks against a named branch (a branch can carry anything); comparing the dose with the working tree's deploy/stress (the checkout may be older than main); a default r in the script (a second copy PR B would have to change).
- Reason: owner review of #70 (R1-R3, prompt EK-D1-REVIEW-20261103).
- Reverse: drop g4_ancestry, stress_state and deployed_rate from deploy/diag/stage5-nopolicy.sh.
- Origin: owner

## 2026-11-03 · PR #70 · Diag second self-review: the restore checks every step; G4 counts untracked and ignored files
- Decision: restore_on, set_canary, check_on and check_off check each step (the restore runs with errexit off), and wait_set refuses an empty IP; G4's tree check counts untracked and ignored files and covers deploy/ek8s-system, which the restore applies; a pgrep error refuses; stress-ng is restarted once at start so its dose is the checked ConfigMap's; the r and its source are printed 5 s before anything is deployed; the output dir and meta.txt are made before G4, so a refusal is recorded.
- Rejected: running the restore in its own session (setsid) so a second Ctrl-C cannot kill a kubectl in it (a killed step already ends in POLICY NOT RESTORED); guarding against deliberate workstation tampering (replace refs, skip-worktree, GOFLAGS overlays).
- Reason: second reviewer pass (owner R4).
- Reverse: revert the commit.
- Origin: Code

## 2026-11-03 · PR #70 · Diag: loadgen's host pid read by path (crictl go-template); -rate read only when unique
- Decision: loadgen_pid reads `crictl inspect -o go-template --template '{{.info.pid}}'` (cri-tools v1.36.0, kind v0.33.0's base) and requires digits, then keeps the /proc/<pid>/comm == loadgen check; deployed_rate prints the -rate argument only when exactly one exists. The other JSON reads were audited and kept (packet P2).
- Rejected: first-match grep of `"pid"` (hit the nested `"pid": 1` before info.pid on the owner's dry run); a deeper regex on indentation; adding jq or python to the tool check.
- Reason: owner dry run of EK-D1-PIDFIX-20261103; the template path is exact, needs no new tool, and fails on a missing key.
- Reverse: revert the commit.
- Origin: Code

## 2026-11-03 · PR #70 · No-policy experiment: the Stage 5 stall is kindnet's policy path; Q2 yes (switch CNI)
- Decision: recorded. Owner's run diag-nopolicy-20261103T094410Z (workstation; gen 0; r = 15.5, b = 62; stress-ng at main's dose; 15 ON + 15 OFF alternating). ON: 7 stalls in 10 mid-burst-add runs (longest over_hang_timeout run 200-222), 0 in 5 runs without an add. OFF: 0 stalls in 9 mid-burst-add runs, 0 in 6 without. kindnet's "Could not receive message" lines (1-2 per run) appear in all 7 ON stall runs and in 1 of 8 ON passes (run 29, a mid-burst add); none in OFF. TCPSynRetrans delta 0 in every stall run (not SYN drops with client retransmit); RetransSegs > 0 in several ON runs, almost never in OFF (run 6: 1). Exact mechanism unknown. The script's verdict is INCONCLUSIVE only because OFF had 9 mid-burst adds, not the criterion's 10; the owner accepts the result as confirmed: the 10 was set for power at p ~ 0.45, and at ON's observed 7/10, P(0 of 9) ~ 2e-5; Fisher exact on add runs (7/10 vs 0/9), one-sided p ~ 0.002. Q2 (2026-11-02 design packet): yes, switch to Calico (D2), with the F7 (a)-(e) checks as preconditions; Cilium only if Calico fails them.
- Rejected: topping up OFF runs to reach 10 adds (the margin is not in doubt); keeping kindnet with a detect-and-retry net only (the defect stays in every HPA scale-up).
- Reason: a pod add stalls new connections only with kindnet's policy path in place.
- Reverse: re-run the experiment; re-ask the owner.
- Origin: owner

## 2026-11-03 · PR #71 · Calico v3.33.0 replaces kindnet as the CNI and NetworkPolicy engine (reverses PR #31's "another CNI" rejection)
- Decision: both kind configs set disableDefaultCNI and podSubnet 10.244.0.0/16; Calico v3.33.0's plain manifest (iptables dataplane, no operator, no Typha) is the CNI and policy engine on CI and the workstation alike. F7 checked from upstream source and docs (D2 design packet): (a) Kubernetes 1.37 supported; (b) the node budget fits (E3); (c) a pod Felix has not programmed is closed ("Unknown interface" drop, felix/rules/dispatch.go); (d) DefaultEndpointToHostAction ACCEPT admits only pod-to-node traffic the pod's own egress policy passed, and the kubelet port is now a refused target; (e) ek8s-stress deny-all is probed (variant-smoke (j)). DESIGN.md sec 11 rewritten: control-port ingress is processes in the node's network namespace, any uid, and hostNetwork pods, all root here (Q4).
- Rejected: staying on kindnet (it fails open, and no kindnetd image carries kube-network-policies v1.1.2); tigera-operator (another pod, image and CRD layer on one node); Cilium (only if Calico fails F7 or the owner's validation runs, Q2).
- Reason: PR #70's experiment put the Stage 5 stall on kindnet's policy path. Reverses decisions-archive.md PR #31 ("another CNI (out of scope for v1)") and the PR #26 and PR #28 Calico/Cilium rejections.
- Reverse: revert PR #71, then kind delete cluster --name ek8s and deploy/up.sh.
- Origin: owner (Q2)

## 2026-11-03 · PR #71 · deploy/calico: the release manifest vendored unmodified, images by digest, four patches
- Decision: deploy/calico/calico.yaml is v3.33.0's manifests/calico.yaml byte for byte (sha256 2de8f475...4da5; URL, tag commit and re-check command in kustomization.yaml), applied server-side (its CRD bundle is too large for a client-side apply); its three images from quay.io by index digest, equal to Docker Hub's (owner-checked, Q1). Patches: pool 10.244.0.0/16, IPIP Never, FELIX_NFTABLESMODE Disabled, CNI policy_setup_timeout_seconds 10 (a pod starts only once Felix reports its policy programmed; past 10 s the CNI logs "Unblocking pod creation", which fails variant-smoke and the deploy CI). FELIX_DEFAULTENDPOINTTOHOSTACTION stays the manifest's ACCEPT (Q2).
- Rejected: Docker Hub images (anonymous pull limits); nftables mode left to Auto (a detection change would switch dataplanes silently); DROP or RETURN to host (breaks metrics-server, coredns and calico-kube-controllers on one node).
- Reason: the deploy/metrics-server pattern, every byte pinned; the policy wait makes pod start wait for its rules, and a timeout is visible.
- Reverse: edit deploy/calico/kustomization.yaml; an upgrade replaces calico.yaml, its sha256 and the three digests together.
- Origin: Code (artifacts and patches owner-approved, Q1 and Q2)

## 2026-11-03 · PR #71 · calico-node requests 100m CPU on CI and the workstation alike
- Decision: deploy/calico patches calico-node's CPU request to 100m (the manifest has 250m), kindnet's own request, everywhere; no CI overlay and no EK8S_CALICO_KUSTOMIZE. calico-kube-controllers keeps the manifest's (no request).
- Rejected: the manifest's 250m (workstation spare 200m to 50m; CI holds one variant pod fewer, so orchestrator-smoke's Stage 5 candidate could stay Pending); a CI-only overlay deploy/calico-ci (the design packet's proposal).
- Reason: owner's Q3 change: no CI divergence, the workstation keeps its 200m spare, and kindnet ran at 100m in the same shared pool. calico-node's real use is unmeasured (`kubectl top pod -n kube-system` after the rebuild).
- Reverse: drop the resources patch from deploy/calico/kustomization.yaml.
- Origin: owner (Q3)

## 2026-11-03 · PR #71 · The CNI first look reads calico-node; its exclusion token is cni-not-ready
- Decision: Cluster.KindnetReady becomes CNIReady (DaemonSet kube-system/calico-node, the same Ready logic); ExcludedKindnet "kindnet-not-ready" becomes ExcludedCNI "cni-not-ready". The token appears in the orchestrator's stderr window line and c0.json's "excluded" list (attempts.jsonl holds only a bool). A run dir or c0.json written before this PR may say kindnet-not-ready: both tokens mean the CNI first look failed.
- Rejected: keeping the old name and token (they name a DaemonSet that no longer exists); a CNI-neutral lookup by label (one CNI, named exactly).
- Reason: with kindnet gone its check errors and every half becomes a fault, so the rename ships with the infra change.
- Reverse: rename back, with kindnet's constants.
- Origin: Code

## 2026-11-03 · PR #71 · The policy canary and its post-boot poll stay; a connected canary fails loadgen-smoke at any pod age
- Decision: the per-half canary stays, unchanged in Go, and so does the poll after a new boot_id: under Calico, connected means its rules are absent altogether. loadgen-smoke.sh (3) fails on any connected, from its first poll once loadgen is Ready (it was allowed 60 s for kindnet's lag): the policy is enforced as soon as loadgen can be reached. smoke.sh checks Felix's "Unknown interface" DROP rules are installed, the fail-closed path for a pod started without its rules. Trimming the poll is in docs/AFTER-FIRST-RUN.md.
- Rejected: dropping the canary (Calico's rules can still be missing: the v3.33.0 release note 13425 fixed that failure in nftables mode); trimming the poll now (harmless, and not on the first-run path).
- Reason: the canary is the only per-half proof that enforcement is on; the stricter smoke catches a fail-open start.
- Reverse: restore loadgen-smoke.sh (3)'s 60 s allowance.
- Origin: Code

## 2026-11-03 · PR #71 · deploy/diag/stage5-runs.sh replaces stage5-nopolicy.sh for the owner's D2 validation (stop rule and MET superseded by 2026-11-04 · PR #71 · stage5-runs.sh verdict, rule B: a Felix drop or an Unblocking is not a PASS)
- Decision: git mv and adapt (Q5, with the owner's change): one arm, the cluster as deployed, no OFF arm, canary patch or policy removal; G3 and G4 kept (G4: HEAD on origin/main or PR #71, no changes under cmd/service, internal, gate, loadgen, deploy, go.mod, go.sum); G1, G2 and G5 and EK8S_DIAG_NOPOLICY dropped with the isolation removal they guarded. Kept: per-run stress-ng assertion, loadgen pid, netns TCP counters, record reduction (raw records off disk), mid-burst-add detection, summary.tsv. Added per run: iptables-save -c deltas of Felix's "Unknown interface" and end-of-tier drop rules (before the gate, at the first record read, after), calico-node's log for the run (raw, and a warning/error count), the CNI "Unblocking" count.
- Stop rule, fixed in advance: at least 15 runs, past 15 only while mid-burst adds < 10, at most 25; the first stall ends it. MET = 0 stalls with >= 10 mid-burst-add runs, STALLED on a stall, else INCONCLUSIVE. -try runs one run with no verdict.
- Rejected: keeping stage5-nopolicy.sh (it asserts kindnet's nft set); a fresh script (the kept code was reviewed three times in PR #70); an opt-in variable (nothing it guarded remains).
- Reason: the D2 validation plan needs PR #70's measurements on Calico, plus Calico's own drop and log signals to tell a stall from pod-start lag.
- Reverse: delete deploy/diag/stage5-runs.sh; git has stage5-nopolicy.sh (merge 4ec4b4a).
- Origin: owner (Q5)

## 2026-11-03 · PR #71 · up.sh: kind create without --wait; a cluster that runs kindnet is refused
- Decision: up.sh creates the cluster without --wait, applies deploy/calico, then waits for the calico-node and calico-kube-controllers rollouts, every node Ready and coredns; on an existing cluster it refuses when DaemonSet kube-system/kindnet exists (delete and recreate it).
- Rejected: --wait (with no CNI the node stays NotReady, and kind v0.33.0's waitforready.go only warns on its timeout, so it adds its full length to every create); layering Calico onto a kindnet cluster or migrating it in place.
- Reason: the Ready wait belongs after the CNI exists; up.sh never recreates a cluster, so an old one must be refused, not half-converted.
- Reverse: restore `--wait 180s` and drop the kindnet check from deploy/up.sh.
- Origin: Code

## 2026-11-03 · PR #71 · The reviewer subagent runs on every change that is not docs-only (Markdown only)
- Decision: CLAUDE.md's Self-review rule and .claude/agents/reviewer.md now trigger on every change that is not docs-only, docs-only meaning only Markdown files change; "Docs-only changes may skip it." is deleted. Shell, YAML and workflow changes, such as the deploy/ scripts that set up or check isolation, now trigger it; reviewer.md lets it run `bash -n` on changed scripts (not shellcheck: not a repo tool).
- Rejected: "any change touching Go code" (left shell, YAML and workflow changes undefined, and this PR is mostly those); an "isolation-relevant" test (a judgement call).
- Reason: D2 design packet item 6, owner Q7.
- Reverse: restore the two lines in CLAUDE.md, reviewer.md's description, and drop its `bash -n` line.
- Origin: owner (Q7)

## 2026-11-04 · PR #71 · stage5-runs.sh verdict, rule B: a Felix drop or an Unblocking is not a PASS
- Decision: a run is PASS only with gate exit 0 and zero Unknown-interface drops, zero in-burst end-of-tier drops and zero CNI "Unblocking pod creation" lines; the first run that is not PASS ends the set; MET needs no stall, every run PASS, >= 15 runs and >= 10 mid-burst adds; any non-PASS run without a stall is INCONCLUSIVE; a count that cannot be read ("-") is not 0, so that run is DROPS.
- Rejected: MET on zero stalls alone with the drop and Unblocking counts only printed; an unread count taken as 0 or as FAULT.
- Reason: a SYN dropped to a new pod for less than the hang timeout never reads as a stall, so zero stalls alone could pass a run whose pod was denied.
- Reverse: drop the DROPS branch in run_one and the notpass test in verdict, and restore the stall-only stop rule in another_run (deploy/diag/stage5-runs.sh).
- Origin: owner

## 2026-11-04 · PR #72 · D2 validation: Calico removes the Stage 5 stall (rule B MET)
- Decision: the owner's run (main 5c4e5fb, pinned clocks, gen 0 image ek8s-wrapper:cf1338c176f5, r = 15.5, b = 62, stress-ng at origin/main's dose): -try (diag-runs-20261104T013254Z) PASS; full set (diag-runs-20261104T013447Z) "runs 15, mid-burst adds 13, stalls 0 (0 in add runs), other FAILs 0, DROPS 0: MET"; every run over_hang_timeout, syn_retrans, retrans_segs, unknown_if_drops, end_of_tier_drops_burst, calico_warn_err, cni_unblocking all 0 (owner-checked against summary.tsv). Against kindnet's 7/10 stalls in add runs (PR #70): Fisher exact, one-sided, p about 5e-4; 0/13: one-sided 95% upper bound on Calico's stall rate in mid-burst-add runs about 21% (exact binomial, 1 - 0.05^(1/13)). Also: up.sh re-run on the existing cluster exit 0, calico-node UID and restarts unchanged; calico-node idle 25m CPU, 122Mi; iptables-legacy holds no cali- rules (Calico is in the nft tables, which stage5-runs.sh reads). Closes #71 packet (b)'s three unverified assumptions. D3 follows (Cilium, Q2, is not needed).
- Rejected: none (a measurement).
- Reason: the set met the fixed stop rule and rule B (2026-11-04, PR #71).
- Reverse: n/a; a later stall reopens D2.
- Origin: owner

## 2026-11-04 · PR #72 · The C0 re-measure moves into PR B (option B)
- Decision: committed maxRate is 19 and maxBurstSends 3000 (loadgen/config.go), so c0 on main cannot reach C0 = 31; PR #65's 31 came from an uncommitted, reverted patch (maxRate 40, maxBurstSends 4800). Order: D3; PR B part 1 raises maxRate and maxBurstSends to cover a ladder past 31; the owner runs c0 on merged main, clocks pinned; PR B part 2 sets r = the measured C0, maxRate to match, b decoupled from r (from the gen-0 Stage 5 runs); run 3.
- Rejected: re-creating the local patch (uncommitted, unreviewed numbers behind a committed result); c0 on main up to 19 (cannot show C0 above its cap).
- Reason: a measurement must run on committed code.
- Reverse: STATUS Next reorder; raise the limits in a PR of its own.
- Origin: owner

## 2026-11-04 · PR #72 · stage5-smoke.sh waits for its namespace to be gone and fails closed
- Decision: cleanup() deletes the namespace with --wait=true --timeout=180s, then reads it back with get --ignore-not-found -o name; a failed delete, an unreadable namespace or a leftover one each fail the smoke (status 1 if it was 0).
- Rejected: --wait=false (PASS with ek8s-v999998 Terminating, so G3 refused the next run); treating a failed get as gone (fail-open when the API is unreachable).
- Reason: a smoke must leave nothing the next step refuses on, and an unknown state is not a pass.
- Reverse: restore the --wait=false delete in cleanup() (deploy/stage5-smoke.sh).
- Origin: Code

## 2026-11-04 · PR #73 · D3 is split into D3a (stall counts) and D3b (the calico-node log archive)
- Decision: D3a records the stall counts in the gate and the orchestrator, written to a new run-dir file stalls.jsonl; attempts.jsonl (DESIGN.md sec 14) is unchanged, no DESIGN.md edit (Q1 definitions D1-D7, Q3, Q5 c0 out of D3, Q6 only the longest run's start). A failed read or write of stalls.jsonl or a log file is a stderr line (for logs also a .failed file), never a fault, exclusion or halt (Q4). D3b archives calico-node's log for every gate Stage 5 burst too (Q2), each file capped at 1 MiB with a cut marker line (Q7); per window it is read once, from the baseline half's start, after the window returns, into one file, untrimmed, stalls.jsonl carrying each half's start and end (B); on a window fault the same read runs before the halt (C).
- Rejected: one D3 PR (counts and archive in one review); per-half log files trimmed by timestamp; a recording failure that faults or excludes.
- Reason: owner answers to the D3 plan (prompt EK-D3-P2-20261104); D3 is detection and recording only (Q4, 2026-11-02), so recording must never change a verdict.
- Reverse: STATUS Now/Next; a later entry overrides any answer.
- Origin: owner

## 2026-11-04 · PR #73 · internal/stallrun: stall-run definitions D1-D7
- Decision: internal/stallrun.Detect(recs, rate) counts one (loadgen run, target)'s records: D1 an OHT record is Bucket failed with Reason over_hang_timeout, any other record (another reason, success, graceful_shed, Skipped, Fault) breaks a run; D2 ordered by Index (a copy; window records arrive by Seq), a missing index ends a run; D3 an OHT run is maximal at consecutive indices, length 1 included; D4 a stall run is an OHT run of >= Threshold(rate) = ceil(rate x HangTimeout) (31 at 15.5, 124 at 62; at least 1) with a recovery record at a later index, a recovery record being a classified response (not Skipped, not a Fault) that is not OHT (R38: a Skipped or Fault record means loadgen got no answer); D5 TailRun is the last OHT run's length when no recovery record has a higher index, else 0, and the tail is never a stall; D6 the longest run (tail included, ties to the earliest), its start Index and IntendedWall (UTC), else 0, -1, zero time; D7 Stats holds records, OHT, stall runs, tail, longest, start and threshold.
- Rejected: a missing index bridged as OHT (would join two runs across a send loadgen did not record); any later non-OHT record as recovery, Skipped and Fault included (a wedged candidate followed by skipped sends would count as a stall); a threshold from the planned send interval rounded down (a run shorter than one hang timeout of sends would count); per-target filtering inside Detect (the caller passes one target's records).
- Reason: owner Q1 on the D3 plan (prompt EK-D3-P2-20261104); one package so a gate burst and a window half count by the same rules; stdlib, loadgen/wire and internal/classifier only, outside Stage 1's import allowlist.
- Reverse: remove its callers (gate.Report.Stage5Stall, orchestrator VariantResult.BaselineStall/StressStall, the stalls.jsonl writes), then delete internal/stallrun.
- Origin: Code (approved by the owner as Q1)

## 2026-11-04 · PR #73 · stalls.jsonl: a run-dir file for the stall counts
- Decision: lineage.Run.AppendStall appends one JSON line per (loadgen run, target) to stalls.jsonl, as AppendAttempt does (mode 0600, synced, refused after a torn tail), the file made by the first append: kind burst | baseline | stress, run_id, window_run_id (halves only), generation_id, the stallrun.Stats fields in snake_case, span_start and span_end for halves (RFC 3339 UTC; these and longest_start_wall omitted when zero); validation is ids by lineage's patterns, the kind, counts >= 0, longest_start_index >= -1. The Loop writes the burst line once Stage 5 returns with Stage5Stall set (pass, fail or fault, before any halt; gate.Report.Stage5RunID names the run) and one line per variant per half that ran once RunWindow returns. A failed write is one log line on the run's stderr. Nothing in the run reads the file.
- Rejected: the counts in attempts.jsonl (owner Q3; BuildPrompt reads it); a reader, or a torn-tail check in Open (nothing reads the file in D3a); making the file in Create (lazily made, so a run dir opened from before D3a appends without a migration).
- Reason: owner answers Q3, Q4 and B (prompt EK-D3-P2-20261104); a run-dir format addition, logged per CLAUDE.md.
- Reverse: drop AppendStall and the Loop calls (and gate.Report.Stage5RunID); a stalls.jsonl already written is inert.
- Origin: Code

## 2026-11-04 · PR #74 · cni/: a run-dir directory for calico-node's log
- Decision: lineage.Run.WriteCNILog(name, log) writes cni/<name>.log and WriteCNIFailed(name, msg) cni/<name>.failed, name by the run-id pattern (nameRE), once each (synced temp file linked into place, ErrExists on a second write, mode 0600; cni/ 0700, made by the first write). A log over MaxCNILog (1048576) keeps its first MaxCNILog bytes, a newline if they lack one, then the line "ek8s: cut at 1048576 bytes\n"; at or under the cap the bytes are kept as read. A .failed holds the error cut to 1024 bytes plus a newline.
- Rejected: a file capped at 1 MiB in total, marker included (the marker would then not say where the log was cut); making cni/ in Create (lazy, as stalls.jsonl, so an older run dir needs no migration); one file kind holding both (a reader tells a failed read by its suffix).
- Reason: owner answers Q4 and Q7 (prompt EK-D3-P2-20261104, D3 split entry); a run-dir format addition, logged per CLAUDE.md.
- Reverse: delete lineage/cni.go and its callers; a cni/ already written is inert.
- Origin: Code

## 2026-11-04 · PR #74 · Kube.CNILog reads calico-node's log through an untrimmed kubectl path
- Decision: Kube.CNILog runs `kubectl -n kube-system logs ds/calico-node -c calico-node --timestamps --since-time=<since> --limit-bytes=<2 x lineage.MaxCNILog>` (since: RFC 3339 UTC, floored to the second, minus 1 s; a zero since refused; twice the cap since the API's limitBytes may return slightly more or less than asked, so a read of cap+1 could come back short and a longer log go unmarked) through Kube.kubectlRaw, which is Kube.kubectl's checks and flags without the TrimSpace; kubectl now trims what kubectlRaw returns, and runKubectl and the run hook return stdout as written. It is a Cluster method; the Loop calls it (next entry).
- Rejected: --limit-bytes=cap+1 (limitBytes is approximate, R45); reading through the trimming kubectl (a byte past the cap that is white space would be trimmed away and the cut go unmarked, and "the bytes as read" would lose the final newline); client-go (a dependency).
- Reason: owner prompt EK-D3B-P1 (Q7's read past the cap), the 2 x cap from review feedback R45; the trim moves up one call, so every other Kube read sees the same trimmed string as before.
- Reverse: delete CNILog and cniLogSince, fold kubectlRaw back into kubectl with runKubectl trimming again.
- Origin: Code

## 2026-11-04 · PR #74 · The Loop archives calico-node's log per burst, per window and on a window fault
- Decision: Loop.archiveCNI(ctx, name, since) calls Cluster.CNILog (added to the interface) and writes cni/<name>.log, or cni/<name>.failed with the read's error; either failure is one log line, never a fault, exclusion or halt; no read once ctx has ended. Stage 5: since = l.now() just before Gate.Stage5, name Stage5RunID, when it is set (pass, fail, fault), before any halt. Window: since BaselineSpan.Start when set, name res.RunID, after windowStalls and before the restart check. Window fault: since = l.now() just before RunWindow, name fault-<unix seconds> (matches lineage's run-id pattern; loadgen ids start w- or gate-; a fault halts the run, so one per run), before the error returns.
- Rejected: a separate optional LoopConfig field for the read (a nil would silently skip the archive; Kube already is the Cluster); a .failed on a failed write (the write is what failed); a fault name from the window's run id (RunWindow returns none with its error).
- Reason: owner answers Q2, Q4, B and C (D3 split entry; prompt EK-D3B-P2-20261104); deploy/orchestrator-smoke.sh (g) checks every stalls.jsonl burst run_id and window_run_id has a cni/<id>.log (empty allowed: calico-node may log nothing in a short span) and no .failed.
- Reverse: delete archiveCNI, faultLogName and their three calls, CNILog from Cluster, and smoke check (g).
- Origin: Code

## 2026-11-04 · PR #75 · CI minutes: draft PRs skip ci.yml and deploy.yml, one run per PR, no push-to-main run (amends the PR #12 CI-trigger entry)
- Decision: both workflows run on pull_request types opened, synchronize, reopened, ready_for_review; the job is skipped while the PR is a draft (`github.event_name != 'pull_request' || !github.event.pull_request.draft`, so schedule and dispatch still run); concurrency group `workflow-(PR number or ref)`, cancel-in-progress only for pull_request events (`github.event_name == 'pull_request'`), so a dispatch never cancels the weekly or another non-PR run; ci.yml drops its push-to-main trigger. Path filters unchanged. Code opens every PR as a draft and marks it ready only after the owner approves.
- Rejected: running tests once with -race only (the plain build compiles the !race files in gate and internal/classifier/testvectors, so it tests different code); faster orchestrator tests now (shared pre-generated batches; deferred, docs/AFTER-FIRST-RUN.md).
- Reason: 342 min in the first 4 days of the month (deploy.yml 202 min / 17 runs, ci.yml 140 min / 13), about 2,500 a month projected; every push to an open PR ran both workflows. The PR #12 entry recorded no required checks on main (2026-10-01, not re-checked); a skipped job counts as passing under branch protection either way; CI no longer re-runs on main after a merge.
- Reverse: drop `types`, the job `if`, the concurrency block, and restore ci.yml's `push: branches: [main]` with the same paths.
- Origin: owner

## 2026-11-04 · PR #76 · deploy smoke runs by hand only (amends the PR #75 entry, deploy.yml part; ci.yml unchanged)
- Decision: deploy.yml's only trigger is workflow_dispatch. The pull_request trigger, its types and path list, and the job's draft `if` are removed; the path list stays as documentation in deploy/README.md (CI paragraph), with the instruction to dispatch the smoke by hand before merging a PR that touches it. The concurrency block is simplified to group per ref (`${{ github.workflow }}-${{ github.ref }}`) with cancel-in-progress false: a newer dispatch replaces a pending one, and a run in progress is never cancelled. The path list is "usually warrant a run", not a mandatory rule; the planning chat asks for a run in review.
- Rejected: per-step path selection inside deploy.yml (small saving, extra logic); a self-hosted runner (no spare host now; revisit if minutes are still too high).
- Reason: owner decision, 2026-11-04. deploy.yml was 202 of 342 Actions minutes in the first 4 days of November (17 runs) and ran on almost every code PR; the owner wants CI kept small. The smoke scripts are unchanged.
- Reverse: restore the pull_request trigger, its paths and the job `if` from the PR #75 version (`git show 49b0e00:.github/workflows/deploy.yml`) and undo the deploy/README.md paragraph.
- Origin: owner

## 2026-11-05 · PR #77 · PR B memory sum: loadgen 768Mi, maxRate 50, maxBurstSends 6000 (amends the PR #45 W and PR #47 header-cap entries' sums; DESIGN.md sec 2, 10, 15)
- Decision: loadgen memory request = limit 768Mi (GOMEMLIMIT 724775732 = limit - limit/10), maxRate 50, maxBurstSends 6000 (4 x 50 x 30), c0 default ladder 8,16,24,28,32,36,40,44,48. At 512Mi (owner option B) the sum failed at r 50 (272.19 MiB vs GOMEMLIMIT/2 230.40) and passed only to r 38; the owner then chose 768Mi.
- Sum (4 targets, W 300 s, n = 120r, today's mem_test.go figures): salts 4 x 750k x 25.36 B; ring slots 4 x (rW + n) x 193 B; record pairs 0.51 x (4rW + n) x 857 B; /verify batches 4 x P(rW) x 739.4 B (measured, 16-1024 char passwords, plus the Stream copy); burst 0.49n x 739.4 B + n x 700 B; evidence 5 x 8 x 5703 B; live = sum x 117/102.94 (PR #45 transients); stalled = 28r in flight (4 targets or one burst at 4r, never both: Stage 5 is in gaps, sec 11) x 80 KiB (PR #47). Gate: live <= GOMEMLIMIT/2 and live + stalled <= GOMEMLIMIT/2. Result at r 50, 768Mi: live 162.81 MiB <= 345.60 PASS; live + stalled 162.81 + 109.38 = 272.19 MiB <= 345.60 PASS. At r 19 the model gives 99.82 MiB benchmarked, PR #45 had 102.94. Node budget with the HPA pin, control plane + kube-system measured on the fresh cluster (1050m, 440Mi): CPU 5800m of 7000m, memory 4280Mi of 13802Mi requested (DESIGN.md sec 2).
- Reason: c0 must climb past the old C0 of 31 and run 3 sets r = C0 with b = 4r, so loadgen must accept r up to 50 and n up to 6,000; 768Mi is what keeps both half-GOMEMLIMIT tests passing there.
- Rejected: 512Mi with maxRate 38 (owner chose 768Mi to keep the ladder to 48); comparing live + stalled with the whole GOMEMLIMIT as PR #47 did (the owner's rule is half, GOGC=100's headroom); counting every slot of every ring as a /hash record with a pair, a burst in all four rings at once (over-counts).
- Reverse: set maxRate, maxBurstSends, the 768Mi and the smoke's (2)/(6) values back; a higher maxRate needs this sum redone with a measured loadgen heap.
- Origin: owner (768Mi, maxRate 50, the half-GOMEMLIMIT rule), Code (the sum)

## 2026-11-05 · PR #77 · Variant HPA pinned to minReplicas = maxReplicas = 1 (DESIGN.md sec 11)
- Decision: deploy/variant/60-hpa.yaml.tmpl keeps the HPA object with maxReplicas 1 (minReplicas was already 1); variant-smoke.sh check (h), which needs two pods, now patches min and max to 2 (the API refuses min 2 over max 1).
- Rejected: deleting the HPA (a larger change, and an unpin later is one field); a Deployment replicas field (the HPA owns the count).
- Reason: in the D3b live check (run dir $HOME/workspace/scratch/ek8s-runs/d3b-live-20261104T211756Z, window w-fb258046f9f62824) the HPA scaled gen 0 from 2 to 1 inside the stress half and deleted hasher-7476775b48-slsgx; fitness.CPUSeconds lost its counter and score() charged the CPU ceiling (stress 0.6982 vs baseline 0.9947, with success_rate 1 and p95 37 ms). Pinning also makes live windows match C0's single-pod basis. The HPA was never part of the genome.
- Reverse: set maxReplicas back to 2 in the template and goldens, restore the smoke's minReplicas-only patch and the DESIGN.md sec 11 wording.
- Origin: owner

## 2026-11-05 · PR #77 · C0 = 31 re-measured; run 3 uses r = C0 = 31, b = 4r = 124, n = 3,720 (DESIGN.md sec 10, 15)
- Decision: run 3's r = C0 = 31 and b = 4r = 124 (n = round(124 x 30) = 3,720), set only where per-run values live: the loadgen-up.sh argument and the orchestrator's -rate (the renderer derives b = 4r). c0 still prints r by the default 0.5 x C0 rule (15.5); run 3 overrides it.
- Measured: c0 on 68403f8 (repo_dirty: docs/STATUS.md only), image ek8s-wrapper:ecb783ac3370, pinned clocks, default ladder, confirm 2; run dir $HOME/workspace/scratch/ek8s-runs/c0-20261105T000346Z (c0.json, stderr.txt). Every step scored. Steps (rate: shed+failed %, p95 ms, verdict): 8: 0.00, 37.5, pass; 16: 0.00, 35.6, pass; 24: 0.00, 34.8, pass; 28: 0.00, 33.5, pass; 32: 0.29, 552.2, fail (p95 over 500); 30: 0.00, 34.3, pass; 31: 0.04, 402.9, pass; 31: 0.00, 324.3, pass; 31: 0.00, 398.5, pass. Same C0 as PR #65's 31.
- Reason: owner decision D3 (r = C0, b = 4r); C0 is re-measured on the tree that run 3 will use, with pinned clocks and the HPA pinned.
- Rejected: r = 0.5 x C0 = 15.5 (the default rule, run 2's basis) and b decoupled from r: owner decision D3.
- Reverse: run 3 with -rate 15.5 and loadgen-up.sh 15.5; revert the DESIGN.md sec 10 and sec 15 run-3 sentences.
- Origin: owner (r = C0, b = 4r), Code (the measurement)

## 2026-11-05 · Run 3 recorded: GO, gen 0 degrades under stress and variants spread more than drift
- Decision: run 3 (r = 31, b = 124, n = 3,720, -generations 2; run3-20261105T021240Z) is a GO. Exit 0, 13 turns, 3 windows, 6 halves, 0 excluded, 0 stalls or OHT, CPUUnmeasured none. Gen 0 stress success_rate 0.9954 / 0.9837 / 0.9552 (below 1, so it sheds or fails), p95 568-598 ms.
- Rule: spread (max - min of variant scores in a window) 0.0331 (w2) and 0.0511 (w3) vs gen 0 drift 0.0198 (w1 to w2) and 0.0431 (w2 to w3): spread > drift each time. Caveat: drift and spread are confounded, live count grew 1/2/3 and gen 0 fell with it; shed and failed counts are not in the run dir (success_rate only).
- Origin: owner

## 2026-11-05 · Run 4 recorded: 8 generations
- Decision: run 4 (r = 31, b = 124, -generations 8; run4-20261105T033013Z) ran to its generation limit: exit 0, 8 of 8 mutations passed the gate, 36 turns, 9 windows, 18 halves, 0 excluded, 0 stall runs or OHT, CPUUnmeasured none, no gate fault.
- Result: best-ever fitness 0.9066 (gen 1, window 2), never beaten; gen 0 0.9033 / 0.8789 / 0.8430 / 0.8046 in windows 1-4, rank 1/1, 2/2, 3/3, 4/4, culled in window 4. Last window 0.8491-0.8684 (gen 7 best); live-count 1 to 4 and every variant fell as live count grew (confounded, as run 3).
- Reason: owner prompt EK-RUN4-20261105; numbers only, nothing read into them here. Tables in the PR body.
- Origin: owner

## 2026-11-05 · PR #80 · c0 on run 4 generation 7: C = 32
- Decision: record only. c0 on run 4's generation 7 wrapper gave C0 = 32 (last_pass 32, first_fail 33; c0 printed "C0 32, r = 16"); generation 0 (PR #77) gave C0 = 31. Run dir $HOME/workspace/scratch/ek8s-runs/c0-run4g7-20261105 (c0.json, c0.log); numbers below are from c0.json. Pod CPU request 250m, limit 1000m (deploy/variant/20-limitrange.yaml.tmpl:13-18). Concurrency slots are not a constant in either tunables.go: capacity() (internal/wrapper/server.go:51-59) is max(1, min(GOMAXPROCS, memLimit/2/20Mi)); the c0 source comment (orchestrator/cmd/c0/main.go:11-14) assumes 2 slots per pod: "Gen 0's wrapper has 2 slots per pod: under the 1000m CPU limit Go sets GOMAXPROCS to 2 (it rounds a cgroup limit up, with a minimum of 2; docs/decisions-archive.md, PR #26 variant-resources entry), and the slots are min(GOMAXPROCS, memory limit / 2 / 20 MiB), 2 here." The other constants in the two tunables.go are identical; the slot count was not read at run time.
- Image: ek8s-wrapper:d38833d16929, built by `go run ./gate/cmd/gate -repo . -candidate <run4 dir>/genomes/7/files` in worktree $HOME/workspace/scratch/ek8s-c-wt (stages 1-4 passed; host go version go1.27.3-X:nodwarf5 linux/amd64); genome digest recomputed, equal to the recorded f1e632d073fcd6f3de50b3851c8cb73e339532cb0a59058c4a9ec47dfe0b6c77. Run 4 records no image tags, so d38833d16929 cannot be tied to the image run 4 deployed. Gen 0 reference: PR #77, run dir $HOME/workspace/scratch/ek8s-runs/c0-20261105T000346Z, image ek8s-wrapper:ecb783ac3370.
- Diff gen 0 to gen 7 (internal/wrapper/tunables.go:21, one line): `acquireWait = classifier.HangTimeout / 4` becomes `acquireWait = classifier.HangTimeout / 30`.
- Labels: c0 labels the variant "generation 0" and prints r by the 0.5 x C rule; neither applies here (this is generation 7, and run 4 used r = 31). The c0 preflight passed with pinned clocks (c0.json: allow_unpinned false, governors performance on cpu0-7, intel_pstate no_turbo 1). Both runs: confirm 2, ladder 8,16,24,28,32,36,40,44,48, 1 pod, 0 restarts.
- Generation 7 (image d38833d16929, repo_head b9cf1b3, repo_dirty true; ladder order as run):
| r | requests | shed | failed | successes | p95 ms | cpu/success s | verdict |
|---|---|---|---|---|---|---|---|
| 8 | 2342 | 0 | 0 | 2342 | 33.7 | 0.0306 | pass |
| 16 | 4720 | 0 | 0 | 4720 | 33.1 | 0.0313 | pass |
| 24 | 7042 | 0 | 0 | 7042 | 33.3 | 0.0311 | pass |
| 28 | 8221 | 0 | 0 | 8221 | 33.0 | 0.0306 | pass |
| 32 | 9388 | 22 | 0 | 9366 | 124.2 | 0.0316 | pass |
| 36 | 10565 | 1209 | 0 | 9356 | 161.2 | 0.0318 | fail |
| 34 | 9993 | 623 | 0 | 9370 | 157.4 | 0.0315 | fail |
| 33 | 9713 | 341 | 0 | 9372 | 153.4 | 0.0317 | fail |
| 32 | 9392 | 7 | 0 | 9385 | 109.5 | 0.0313 | pass |
| 32 | 9392 | 23 | 0 | 9369 | 127.4 | 0.0312 | pass |
- Generation 0 (image ecb783ac3370, repo_head 68403f8, repo_dirty true; ladder order as run):
| r | requests | shed | failed | successes | p95 ms | cpu/success s | verdict |
|---|---|---|---|---|---|---|---|
| 8 | 2362 | 0 | 0 | 2362 | 37.5 | 0.0324 | pass |
| 16 | 4710 | 0 | 0 | 4710 | 35.6 | 0.0317 | pass |
| 24 | 7077 | 0 | 0 | 7077 | 34.8 | 0.0299 | pass |
| 28 | 8217 | 0 | 0 | 8217 | 33.5 | 0.0305 | pass |
| 32 | 9390 | 27 | 0 | 9363 | 552.2 | 0.0311 | fail |
| 30 | 8823 | 0 | 0 | 8823 | 34.3 | 0.0315 | pass |
| 31 | 9104 | 4 | 0 | 9100 | 402.9 | 0.0307 | pass |
| 31 | 9112 | 0 | 0 | 9112 | 324.3 | 0.0305 | pass |
| 31 | 9112 | 0 | 0 | 9112 | 398.5 | 0.0299 | pass |
- Origin: owner (the run), Code (the record)

## 2026-11-05 · PR #84 (inferred) · docs/workflow/ stays the PR #32 snapshot, plus the change table and a retrospective
- Decision: docs/workflow/ (WORKFLOW.md, chat-instructions.md, templates/) is kept as extracted at PR #32 and stays in this repo; WORKFLOW.md gains a snapshot note, section 12 (what changed after #32, and why) and section 13 (a retrospective); README.md links it; the two docs/AFTER-FIRST-RUN.md items about extraction and the freeze are marked resolved.
- Rejected: a full re-sync of the templates to the final method (multi-prompt work for a repo whose development has ended; the live CLAUDE.md, docs/handoff.md and .claude/agents/reviewer.md already show the final form).
- Reason: development has ended and the repo goes public; the snapshot plus an evidence-based record is enough for a reader to reuse the method.
- Reverse: delete WORKFLOW.md sections 12 and 13 and the snapshot notes; restore the two AFTER-FIRST-RUN.md items.
- Origin: owner
