# Status: resume point for work after a gap

Maintained by Code sessions; every PR updates it (CLAUDE.md). Lines marked
*(inferred)* are guesses, not read from the repo; correct them.

## Now
- `claude/deploy` (PR #26): deploy/ PR (1) of 2, cluster infra: kind config (cluster ek8s, kind v0.33.0, node v1.37.0 by digest, 1 CPU / 2Gi reserved), metrics-server v0.9.0 vendored with --kubelet-insecure-tls, stress-ng (upstream image by digest, kind-loaded, 0 <-> 1 replicas, dose ConfigMap), up.sh, infra smoke.sh, deploy/README host setup (AppArmor bwrap userns profile, sysctl fallback); DESIGN.md sec 5, 7, 10, 11 edits from the deploy/ design packet. Plus a CI-size kind smoke workflow (.github/workflows/deploy.yml; cluster-ci.yaml, stress-ci overlay, up.sh/smoke.sh size parameters). Merge gate: the CI smoke run passing (owner away from the workstation; full-size checks are Parked, pending workstation).

## Next
- deploy/ PR (2), review packet: per-variant bundle (Namespace with PSA restricted, LimitRange 250m/1000m CPU and 256Mi, NetworkPolicy deny egress + ingress 8080 from ek8s-system app=loadgen, Deployment `hasher` to match newPodName's prefix, Service, HPA max 2) rendered by a stdlib text/template renderer that rejects a non-numeric id, a label not matching ek8s-<16 hex> and an image not matching ek8s-wrapper:[0-9a-f]{12} (both sides tested); go test golden files; egress smoke test that fails if egress is not denied; gate label prefix gate- -> ek8s- (DESIGN.md sec 5); a workflow_dispatch-only kind smoke workflow (design packet Q8 B). Then loadgen (sustained + burst, in-cluster in ek8s-system), then gate PR4: Stage 5.

## Parked
- Pending workstation (owner away): required before the first real orchestrator run, not before merge. (1) Full-size smoke run: `deploy/up.sh` then `deploy/smoke.sh` with no EK8S_* set (1 CPU reservation, 3-CPU stress). (2) Budget numbers from `kubectl --context kind-ek8s describe node` ("Allocated resources"): control-plane requests on top of the 1 CPU reservation; keep 1 CPU unless they are tight. (3) Gate host userns: deploy/README.md documents (a) the upstream bwrap-userns-restrict profile (preferred), (b) a hand-written unconfined bwrap profile only if (a) is unavailable, never both, (c) the sysctl; checked-by-hand pending: that Ubuntu 25.04+ ships bwrap-userns-restrict in the apparmor package and 24.04 has it only in apparmor-profiles' extra-profiles, and that its unpriv_bwrap child profile does not break the nsenter relay; record which option passed. (4) `EK8S_GATE_REQUIRE_TOOLS=1 go test -count=1 ./gate/` on the host (the pass criterion for (3)).
- Fitness (sec 9): treat integrity failures (verify_mismatch, verify_error, wrong_match, non_canonical_phc, salt_reused) as disqualifying, not one failed request: a mutant can relabel undelivered Argon2 results (timed-out clients) as hashes for later requests, caught only by sampled FullVerify.
- Verify-result caching: a finite pool lets a mutant memoize (password, hash) -> match and skip Argon2; loadgen should draw some verify pairs from the variant's own fresh /hash output.
- Pool-hash replay: a mutant can remember (password, hash) pairs from match=true pool /verify requests and answer /hash for that password with the remembered hash, skipping Argon2; the Run never sees that salt (it records salts from /hash responses only). loadgen must not send pool passwords to /hash, or the Run must be pre-seeded with the pool's salts.
- Bare boot has no memory/CPU bounds: a pathological mutant can put the gate host under pressure during Stages 1-2 and 4 (bwrap sets no rlimits or cgroup); under -race each in-flight hash costs ~60 MB (~3x plain; measured ~1.9 GB for 32 at once, so ~3.8 GB at N's cap of 64 *(inferred, linear)*), and that cap is the only bound; variant values are now set (256Mi, 1 CPU limit): set EK8S_*_LIMIT to them in the bare boot in a later gate PR, with a measured -race timing first. Real cgroup bounds stay parked, now also because the gate shares the host with the live population.
- Security gap, gate detectability: a candidate can tell it is in the gate (the "gate-<16 hex>" X-EK8s-Variant label, readable from w.Header() after WriteHeader; zero EK8S_*_LIMIT env; bwrap as pid 1 in /proc/1/cmdline via http.Dir; in Stage 4, runtime/debug.ReadBuildInfo shows -race=true, -tags=netgo,osusergo and CGO_ENABLED=1, and GORACE is readable from /proc/self/environ via http.Dir, so a mutant could take a lock only under -race; the hostname is now a random pod-like name, which removes only that tell; the label tell closes in deploy/ PR (2), DESIGN.md sec 5: ek8s-<16 hex> everywhere) and fail open on Verify errors only outside it. It then passes the Stage 2 contract, and Stage 5 and loadgen never send non-exact-param hashes, so the PR #6 fail-open bypass reopens in production. Candidate mitigation: send contract negatives (non-exact-param hashes expecting 400) in the Stage 5 burst and in sampled sustained loadgen traffic (classifier + DESIGN.md sec 7/8/10 change).
- Gate Details carry candidate-controlled output (sb.withOutput: Stage 1 boot, Stage 2, Stage 4 concurrency; the Stage 4 race-report excerpt is the output tail) and a mutant can log request bodies, so pool and fresh passwords can appear in Report Details. The driver (DESIGN.md sec 12 per-call context) must never pass gate Detail or candidate output into a prompt verbatim, or later mutants could learn pool pairs (table lookup, verify memoization).
- Orchestrator cull must `docker rmi` culled variants' images, and also remove them from the kind node, not only from the host: the gate keeps a passing candidate's image (Report.Image) for kind load and never removes it later.
- Fitness CPU-seconds source: metrics.k8s.io gives a usage rate, while DESIGN.md sec 9 wants cumulative pod CPU-seconds; settle in the fitness design (kubelet /metrics/resource or cgroup counters).
- loadgen PR: Guaranteed QoS with dedicated CPU, and record schedule lag (intended vs actual send time), so loadgen's own stalls are visible and those windows excluded.
- HPA at 70% of a 250m request pins variants at maxReplicas under load; revisit with the observation-window design (also the default 300s scale-down stabilisation).
- CI: unpin go-version 1.26.9 back to '1.26.x' once actions/go-versions' manifest lists >= 1.26.9 (docs/decisions.md, PR #18 entry).

## Recent
- #25 Gate PR3 of 4: Stage 4 concurrency safety (static race build in the same bwrap sandbox after a race probe preflight, N = 2 x NumCPU concurrent requests with bounded retries, race report by exit status plus output scan, cross-talk via the Stage 2 Run; fails a candidate that exits by the end of the load) (2026-10-12)
- #24 Reviewer checklist gains limit-boundary and child-process-lifetime items; handoff.md Feedback rule covers any owner reply carrying feedback, conditions or an approval with changes (2026-10-12)
- #23 Packets are the default end-of-task output (CLAUDE.md, docs/handoff.md); packet header gains origin/main and the last merged PR; new Sync packet type and a Chat side staleness check; the start-of-PR reconcile goes in its own commit (2026-10-12)
- #22 Newcomer overview (docs/overview.md) and a short front-door README.md; CLAUDE.md layout line and a bullet to keep the overview in step with DESIGN.md and the generation-0 wrapper (2026-10-12)
- #21 Gate PR2 of 4: Stage 1 build/boot (static build, bare boot under bwrap via an nsenter relay, FROM scratch image), Stage 2 interop + contract, Stage 3 cost params; minimum-toolchain gate fault; CI installs bubblewrap and lifts the AppArmor userns restriction; sandbox preflight (2026-10-09)
- #20 go.mod: `toolchain go1.26.9` for GOTOOLCHAIN=auto builds (2026-10-09)
- #19 reviewer: three checklist items from PR #18's [missed] feedback (2026-10-09)
- #18 Gate PR1 of 4: build-tree assembly and Stage 1 pre-build checks (candidate file set, hasher pin, build-constraint ban, go list allowlist under the deployed and race builds), gate/cmd/gate CLI; DESIGN.md sec 5, 7, 12 and CLAUDE.md edits for the approved gate design (2026-10-09)
- #17 Pre-packet self-review by a read-only reviewer subagent (2026-10-02)
- #16 internal/classifier PR B: test-vector pool (internal/classifier/testvectors), generator, pool-validity and agreement tests (2026-10-02)
- #15 internal/classifier PR A: three-bucket response classifier with injected Verifier (2026-10-02)
- #14 handoff: shorten review-packet full diffs (2026-10-02)
- #13 CLAUDE.md: reconcile STATUS.md and decisions.md with main at PR start (2026-10-01)
- #12 CI: run only on Go-relevant changes, plus weekly and on demand (2026-10-01)
- #11 Add docs/handoff.md packet formats for the planning/review chat (2026-10-01)
- #10 Add docs/STATUS.md as the resume point after long gaps (2026-10-01)
- #9 decisions: record dropping GIT_COMMITTER_* from the cloud environment (2026-10-01)
- #8 CLAUDE.md: don't act on the cloud Stop hook's commit/push prompts (2026-10-01)
- #7 CI: vet, test, race, govulncheck (2026-10-01)
- #6 Generation 0: hashcore, wrapper seed, cmd/service (2026-09-27)
- #5 Add working agreement and decision log (2026-09-27)
