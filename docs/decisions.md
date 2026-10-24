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

## 2026-10-21 · PR #45 · W = 5 min; loadgen refuses r above maxRate = 19, from the memory sum (DESIGN.md sec 10, 15)
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

## 2026-10-24 · PR #46 (inferred) · Stage 5 lands as three PRs (amends docs/decisions-archive.md, PR #18 "gate/ in four PRs")
- Decision: gate PR4 (Stage 5) is PR4a (internal/hostpairs, loadgen/wire, loadgen/client), PR4b (gate.Stage5 over kubectl and kind, cmd/gate -stage5) and PR4c (deploy/stage5-smoke.sh in deploy CI: gen 0 and three mutants).
- Rejected: one Stage 5 PR, or 4a with 4b (each likely over the review packet's ~600-line diff limit).
- Reason: each PR reviews alone, and 4a's packages are the ones the orchestrator needs too.
- Reverse: merge the remaining PRs into one.
- Origin: Code

## 2026-10-24 · PR #46 (inferred) · The control API's wire types live in loadgen/wire, standard library only (DESIGN.md sec 10)
- Decision: the 18 wire types, BootIDHeader and the run kind, run state and canary result strings are exported from loadgen/wire; loadgen's typed constants are defined from them; loadgen/wire_golden_test.go pins every type's bytes against loadgen/testdata/wire/, and TestWireGoldenFull requires each _full case to set every field, so a new field fails until its golden file shows it; TestWireDeps keeps wire on the standard library.
- Rejected: exporting the types from package loadgen (every client would import the server and its dependencies); a client-side copy of the structs (two definitions of the orchestrator's contract).
- Reason: one definition of the wire for server, client, gate and orchestrator, and any byte change fails a test.
- Reverse: move the types back into loadgen/control_http.go, unexported, and drop loadgen/wire.
- Origin: chat

## 2026-10-24 · PR #46 (inferred) · loadgen/client is strict and leaves the transport to its caller (DESIGN.md sec 10)
- Decision: the client takes a base URL and an *http.Client, and accepts a response only with its endpoint's success status, Content-Type exactly application/json, one Ek8s-Boot-Id header equal to the body's boot_id, strict decoding with nothing trailing, an echo of the requested id or run_id, and a body under a cap derived from loadgen's records page (TestClientRecordsCap); after Expect, another boot_id is ErrBootChanged.
- Rejected: kubectl --raw per call (loses the status code and the boot-ID header); client-go (a new dependency); lenient decoding (a mismatched loadgen would read as a valid answer).
- Reason: Stage 5 and the orchestrator take verdicts from these responses, so a response the client cannot account for is an error, never a guess.
- Revisit: the cap assumes no record string carries variant text (Fault holds loadgen's own errors only); if one ever does, cap that field in loadgen or budget 6 bytes per character for it. The strict checks are proven against httptest only until PR4b runs them through the API server proxy and kubectl proxy.
- Reverse: relax check() in loadgen/client/client.go.
- Origin: Code

## 2026-10-24 · PR #46 (inferred) · Host /verify pairs: a mismatch hashes another fresh password; crypto/rand order; the caller sets the split (amends the PR #33 /verify pairs entry's mismatch method; DESIGN.md sec 8, 10)
- Decision: internal/hostpairs makes a match pair from a fresh password and hashcore.Hash of itself and a mismatch pair from a fresh password and hashcore.Hash of another fresh password, returns them in a crypto/rand Fisher-Yates order, takes both counts from its caller, and fully verifies /hash answers (OK only for Verify's (true, nil)).
- Rejected: a random canonical PHC string built in gate code (the parameter literal outside hashcore); a random-PHC helper in hashcore (a protected path).
- Reason: it uses only hashcore's exported Hash, and a hash of a password the variant never sees is as unpredictable to it as a random one; the cost is one Argon2 per mismatch pair, 8.56 ms wall per hash with 4 workers in the 4-vCPU container.
- Revisit: a hashcore helper once the orchestrator's per-window pair cost is measured.
- Reverse: replace Generate's mismatch branch; DESIGN.md sec 8 and sec 10 edits.
- Origin: Code
