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
