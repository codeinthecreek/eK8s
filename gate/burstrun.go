package gate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/hostpairs"
	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// recordsPage is the records page size the burst run asks for: loadgen's
// cap (DESIGN.md sec 10).
const recordsPage = 1000

// loadgenShape is the loadgen configuration the caller deployed it with,
// compared exactly with GET /config before a burst (docs/decisions.md,
// the PR #53 burst run entry): a loadgen deployed with other numbers
// would score a burst nobody planned.
type loadgenShape struct {
	Rate      float64 // r
	WindowS   float64 // W in seconds
	BurstRate float64 // b
}

// burstParams is one Stage 5 burst: its target, its run and what it
// needs on the host. The unexported timings default (timing) and are
// tests' to shrink.
type burstParams struct {
	Target  string // the candidate's variant id
	RunID   string // a run_id loadgen has not seen this boot
	Want    loadgenShape
	Core    *hashcore.Core
	Workers int // host pair generation; GOMAXPROCS when <= 0

	canaryEvery   time.Duration // between canary polls
	canaryBound   time.Duration // how long a canary may take to read refused
	pollEvery     time.Duration // between GET /runs polls
	clientTimeout time.Duration // loadgen's per-request timeout, 3 x HangTimeout + 1 s
	runSlack      time.Duration // allowed past the burst's length and one timeout
	drainBound    time.Duration // how long a cancelled run may take to settle
}

func (p burstParams) timing() burstParams {
	p.canaryEvery = cmp.Or(p.canaryEvery, time.Second)
	p.canaryBound = cmp.Or(p.canaryBound, 30*time.Second)
	p.pollEvery = cmp.Or(p.pollEvery, 500*time.Millisecond)
	p.clientTimeout = cmp.Or(p.clientTimeout, 3*classifier.HangTimeout+time.Second)
	p.runSlack = cmp.Or(p.runSlack, 15*time.Second)
	p.drainBound = cmp.Or(p.drainBound, 20*time.Second)
	return p
}

// burstResult is a finished burst: its records in Index order, and the
// Seq of the newest of them, which DELETE /targets/{id}?through= takes
// once the records have been judged.
type burstResult struct {
	Config  wire.Config
	Records []wire.Record
	Through uint64
}

// runBurst runs one Stage 5 burst against p.Target over loadgen's
// control API: the pre-run steps (config check, a fresh target, host
// pairs, the canary), then start, the bounded poll and the records read.
// Every error is a gate fault: nothing here can show a candidate failed
// (its responses are burstVerdicts' to judge), so an error carries no
// Result and no text a candidate controls. On an error the target may
// remain in loadgen, since its records may be unread; the caller halts
// (docs/STATUS.md, S10). On success the caller judges the records and
// DELETEs the target through Through when the candidate failed or is
// culled. cl must be new: runBurst calls Expect on it.
func runBurst(ctx context.Context, cl *client.Client, p burstParams) (burstResult, error) {
	p = p.timing()
	cfg, since, err := prepareBurst(ctx, cl, p)
	if err != nil {
		return burstResult{}, fmt.Errorf("stage 5 pre-run: %w", err)
	}
	recs, err := burstRecords(ctx, cl, p, cfg, since)
	if err != nil {
		return burstResult{}, fmt.Errorf("stage 5 burst: %w", err)
	}
	through := uint64(0)
	for _, r := range recs {
		through = max(through, r.Seq)
	}
	return burstResult{Config: cfg, Records: recs, Through: through}, nil
}

// prepareBurst is the pre-run: it reads and checks GET /config and pins
// its boot_id, requires p.Target to be new to loadgen, makes the host's
// pairs, registers the target, uploads the pairs and waits for the
// canary to read refused. It returns the config and the Seq the target's
// records start at.
func prepareBurst(ctx context.Context, cl *client.Client, p burstParams) (wire.Config, uint64, error) {
	cfg, err := cl.Config(ctx)
	if err != nil {
		return wire.Config{}, 0, err
	}
	if err := checkShape(cfg, p.Want); err != nil {
		return wire.Config{}, 0, err
	}
	cl.Expect(cfg.BootID)

	ts, err := cl.Targets(ctx)
	if err != nil {
		return wire.Config{}, 0, err
	}
	for _, t := range ts.Targets {
		if t.ID == p.Target {
			return wire.Config{}, 0, fmt.Errorf("target %s is already in loadgen", quoteCut(p.Target))
		}
	}

	// The host's pairs first: generation takes time that should not
	// stand between registering the target and starting its run.
	matches := (cfg.BurstPairs + 1) / 2
	pairs, err := hostpairs.Generate(ctx, p.Core, matches, cfg.BurstPairs-matches, p.Workers)
	if err != nil {
		return wire.Config{}, 0, fmt.Errorf("generating pairs: %w", err)
	}
	up := wire.Upload{Pairs: make([]wire.UploadPair, len(pairs))}
	for i := range pairs {
		up.Pairs[i] = wire.UploadPair{Password: &pairs[i].Password, Hash: &pairs[i].Hash, Match: &pairs[i].Match}
	}

	put, created, err := cl.PutTarget(ctx, p.Target)
	if err != nil {
		return wire.Config{}, 0, err
	}
	if !created {
		return wire.Config{}, 0, fmt.Errorf("target %s existed when put", quoteCut(p.Target))
	}
	got, err := cl.PutPairs(ctx, p.Target, p.RunID, up)
	if err != nil {
		return wire.Config{}, 0, err
	}
	if got.Count != len(up.Pairs) {
		return wire.Config{}, 0, fmt.Errorf("loadgen holds %d pairs, uploaded %d", got.Count, len(up.Pairs))
	}
	if err := awaitCanary(ctx, cl, p); err != nil {
		return wire.Config{}, 0, err
	}
	return cfg, put.Next, nil
}

// checkShape checks that cfg is what the caller deployed loadgen with,
// exactly, and that its burst is usable.
func checkShape(cfg wire.Config, want loadgenShape) error {
	if cfg.Rate != want.Rate || cfg.WindowS != want.WindowS || cfg.BurstRate != want.BurstRate {
		return fmt.Errorf("loadgen config r %v, window_s %v, burst_rate %v; want %v, %v, %v",
			cfg.Rate, cfg.WindowS, cfg.BurstRate, want.Rate, want.WindowS, want.BurstRate)
	}
	if cfg.BurstSends < 1 || cfg.BurstPairs < 0 || cfg.BurstPairs > cfg.BurstSends ||
		!(cfg.BurstRate > 0) || math.IsInf(cfg.BurstRate, 1) {
		return fmt.Errorf("loadgen burst of %d sends and %d pairs at %v/s", cfg.BurstSends, cfg.BurstPairs, cfg.BurstRate)
	}
	return nil
}

// awaitCanary polls the policy canary until it reads refused, at most
// p.canaryBound: kindnet enforces a new pod's policy only after a short
// lag (DESIGN.md sec 11), and loadgen sends nothing to a candidate
// without it. Any other result when the bound ends is a fault.
func awaitCanary(ctx context.Context, cl *client.Client, p burstParams) error {
	deadline := time.Now().Add(p.canaryBound)
	for {
		c, err := cl.Canary(ctx)
		if err != nil {
			return err
		}
		if c.Result == wire.CanaryRefused {
			return nil
		}
		if !time.Now().Add(p.canaryEvery).Before(deadline) {
			return fmt.Errorf("canary still reads %s after %v", quoteCut(c.Result), p.canaryBound)
		}
		if err := sleep(ctx, p.canaryEvery); err != nil {
			return err
		}
	}
}

// burstRecords starts the burst, waits for it, and reads and checks its
// records.
func burstRecords(ctx context.Context, cl *client.Client, p burstParams, cfg wire.Config, since uint64) ([]wire.Record, error) {
	if _, err := cl.Start(ctx, p.RunID, wire.Start{Kind: wire.KindBurst, Targets: []string{p.Target}}); err != nil {
		// A status loadgen answered means it started nothing (a start is
		// all or nothing); any other failure (a timeout, a lost or
		// unreadable response) may have started the run.
		var ae *client.APIError
		if !errors.As(err, &ae) {
			err = abandon(ctx, cl, p, err)
		}
		return nil, err
	}
	length := time.Duration(float64(cfg.BurstSends) / cfg.BurstRate * float64(time.Second))
	if err := awaitRun(ctx, cl, p, length+p.clientTimeout+p.runSlack); err != nil {
		return nil, err
	}
	return readRecords(ctx, cl, p, cfg.BurstSends, since)
}

// awaitRun polls the run until its target leaves running, at most
// bound. A target that ends cancelled or error is a fault (the run is
// excluded, DESIGN.md sec 10). If ctx ends or the bound passes, it
// cancels the run and drains it: waits, bounded, for the target to leave
// running, since a target leaves running only after every send it
// launched is recorded.
func awaitRun(ctx context.Context, cl *client.Client, p burstParams, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		run, err := cl.Run(ctx, p.RunID)
		if err != nil {
			return abandon(ctx, cl, p, err)
		}
		rt, err := runTarget(run, p.Target)
		if err != nil {
			return err
		}
		switch rt.State {
		case wire.StateDone:
			return nil
		case wire.StateRunning:
		default:
			return fmt.Errorf("run ended %s: %s", quoteCut(rt.State), quoteCut(rt.Error))
		}
		if !time.Now().Add(p.pollEvery).Before(deadline) {
			return abandon(ctx, cl, p, fmt.Errorf("run still running after %v", bound))
		}
		if err := sleep(ctx, p.pollEvery); err != nil {
			return abandon(ctx, cl, p, err)
		}
	}
}

// abandon cancels the run and drains it after cause, returning cause
// (joined with any drain error). It runs on a context detached from
// ctx's cancellation, so that a cancelled gate still stops its run; a
// cause that is a loadgen restart or a bad response skips the drain,
// which would read the same.
func abandon(ctx context.Context, cl *client.Client, p burstParams, cause error) error {
	if errors.Is(cause, client.ErrBootChanged) {
		return cause
	}
	dctx, stop := context.WithTimeout(context.WithoutCancel(ctx), p.drainBound)
	defer stop()
	run, err := cl.Cancel(dctx, p.RunID)
	for err == nil {
		var rt wire.RunTarget
		if rt, err = runTarget(run, p.Target); err != nil || rt.State != wire.StateRunning {
			break
		}
		if err = sleep(dctx, p.pollEvery); err != nil {
			break
		}
		run, err = cl.Run(dctx, p.RunID)
	}
	if err != nil {
		return errors.Join(cause, fmt.Errorf("draining the cancelled run: %w", err))
	}
	return cause
}

// runTarget returns run's one target's part: a run of anything else is a
// fault.
func runTarget(run wire.Run, target string) (wire.RunTarget, error) {
	rt, ok := run.Targets[target]
	if !ok || len(run.Targets) != 1 || run.Kind != wire.KindBurst {
		return wire.RunTarget{}, fmt.Errorf("run is a %s of %d targets, none named %s", quoteCut(run.Kind), len(run.Targets), quoteCut(target))
	}
	return rt, nil
}

// readRecords reads the target's records from since and returns them in
// Index order, requiring exactly one per index 0..n-1 of this run and
// target, no gap, no more than n in all (burstVerdicts does not check
// completeness, DESIGN.md sec 7).
func readRecords(ctx context.Context, cl *client.Client, p burstParams, n int, since uint64) ([]wire.Record, error) {
	seen := make([]bool, n)
	var all []wire.Record
	for pages := 0; ; pages++ {
		if pages > n/recordsPage+1 {
			return nil, errors.New("records past the burst's last page")
		}
		page, err := cl.Records(ctx, p.Target, since, recordsPage)
		if err != nil {
			return nil, err
		}
		if page.Gap {
			return nil, errors.New("records have a gap")
		}
		if len(page.Records) == 0 {
			break
		}
		if page.Next <= since {
			return nil, fmt.Errorf("records page since %d has next %d", since, page.Next)
		}
		all = append(all, page.Records...)
		if len(all) > n {
			return nil, fmt.Errorf("more than the %d records of the burst", n)
		}
		since = page.Next
	}
	for i := range all {
		r := &all[i]
		switch {
		case r.Target != p.Target || r.RunID != p.RunID:
			return nil, fmt.Errorf("record %d is of target %s, run %s", r.Seq, quoteCut(r.Target), quoteCut(r.RunID))
		case r.Index < 0 || r.Index >= n:
			return nil, fmt.Errorf("record %d has index %d, of %d", r.Seq, r.Index, n)
		case seen[r.Index]:
			return nil, fmt.Errorf("index %d is recorded twice", r.Index)
		}
		seen[r.Index] = true
	}
	if len(all) != n {
		return nil, fmt.Errorf("%d records, want %d", len(all), n)
	}
	slices.SortFunc(all, func(a, b wire.Record) int { return cmp.Compare(a.Index, b.Index) })
	return all, nil
}

// sleep waits d or until ctx ends, returning ctx's error then.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
