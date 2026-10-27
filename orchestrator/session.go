package orchestrator

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/codeinthecreek/eK8s/deploy/variant"
	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// Shape is the loadgen configuration the run deployed it with: r, W in
// seconds and b. It is frozen for the run (DESIGN.md sec 10), so a
// loadgen whose GET /config differs, exactly, is refused.
type Shape struct {
	Rate, WindowS, BurstRate float64
}

// SessionConfig is what a Session needs.
type SessionConfig struct {
	// Client reaches loadgen's control API; the Session alone calls
	// Expect and ExpectAny on it.
	Client  *client.Client
	Cluster Cluster
	Core    *hashcore.Core
	Want    Shape
	Params  fitness.Params
	// Workers is how many goroutines verify on the host (GOMAXPROCS
	// when <= 0).
	Workers int
}

// timing holds a Session's waits and its lag thresholds; zero fields
// default (withDefaults), and tests shrink or loosen them.
type timing struct {
	canaryEvery   time.Duration // between canary polls after a new boot_id
	canaryBound   time.Duration // how long the canary may take to read refused then
	pollEvery     time.Duration // between GET /runs polls
	clientTimeout time.Duration // loadgen's per-request timeout, 3 x HangTimeout + 1 s
	runSlack      time.Duration // allowed past the window's length and one timeout
	drainBound    time.Duration // how long a cancelled run may take to settle
	lagP99        time.Duration // a half's lag p99 over this excludes it
	lagMax        time.Duration // any lag over this excludes the half
}

func (t timing) withDefaults() timing {
	t.canaryEvery = cmp.Or(t.canaryEvery, time.Second)
	t.canaryBound = cmp.Or(t.canaryBound, 30*time.Second)
	t.pollEvery = cmp.Or(t.pollEvery, time.Second)
	t.clientTimeout = cmp.Or(t.clientTimeout, 3*classifier.HangTimeout+time.Second)
	t.runSlack = cmp.Or(t.runSlack, 15*time.Second)
	t.drainBound = cmp.Or(t.drainBound, 20*time.Second)
	// DESIGN.md sec 10's starting guesses.
	t.lagP99 = cmp.Or(t.lagP99, 10*time.Millisecond)
	t.lagMax = cmp.Or(t.lagMax, 100*time.Millisecond)
	return t
}

// Session is the orchestrator's side of one loadgen for one run: the
// config it checked, the boot_id it expects, a host salt history per
// variant, and a read cursor per target. Every error it returns is a
// fault of the harness (loadgen, the cluster, the host), never a
// variant's, and the first run halts on one (docs/decisions.md, PR #52
// S10). It is not safe for concurrent use: the orchestrator runs one
// step at a time.
type Session struct {
	c   SessionConfig
	t   timing
	cfg wire.Config

	hist map[string]*history // by variant id: every variant Adopted and not Dropped
	next map[string]uint64   // by variant id: the Seq after the last record read
	used map[string]bool     // Batch.key of every batch uploaded

	// restarted is set when loadgen's boot_id changed: Recover must run
	// before the next window. pollCanary is set at start and by Recover:
	// a new loadgen's policy may not be enforced yet (DESIGN.md sec 11).
	restarted, pollCanary bool
}

// NewSession reads GET /config, refuses a loadgen deployed with another
// shape, and expects its boot_id from then on.
func NewSession(ctx context.Context, c SessionConfig) (*Session, error) {
	if c.Client == nil || c.Cluster == nil || c.Core == nil {
		return nil, errors.New("orchestrator: session needs a client, a cluster and a hashcore")
	}
	if err := c.Params.Validate(); err != nil {
		return nil, err
	}
	s := &Session{c: c, t: timing{}.withDefaults(), hist: make(map[string]*history), next: make(map[string]uint64), used: make(map[string]bool), pollCanary: true}
	if err := s.readConfig(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// readConfig reads and checks GET /config and expects its boot_id.
func (s *Session) readConfig(ctx context.Context) error {
	cfg, err := s.c.Client.Config(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: loadgen config: %w", err)
	}
	w := s.c.Want
	if cfg.Rate != w.Rate || cfg.WindowS != w.WindowS || cfg.BurstRate != w.BurstRate {
		return fmt.Errorf("orchestrator: loadgen config r %v, window_s %v, burst_rate %v; want %v, %v, %v",
			cfg.Rate, cfg.WindowS, cfg.BurstRate, w.Rate, w.WindowS, w.BurstRate)
	}
	if cfg.WindowSends < 1 || cfg.WindowPairs < 0 || cfg.WindowPairs > cfg.WindowSends ||
		!(cfg.Rate > 0) || math.IsInf(cfg.Rate, 1) || !(cfg.WindowS > 0) || math.IsInf(cfg.WindowS, 1) {
		return fmt.Errorf("orchestrator: loadgen window of %d sends and %d pairs at %v/s over %v s",
			cfg.WindowSends, cfg.WindowPairs, cfg.Rate, cfg.WindowS)
	}
	s.c.Client.Expect(cfg.BootID)
	s.cfg = cfg
	return nil
}

// Config is loadgen's GET /config as the Session checked it; its
// WindowPairs is how many pairs each Batch needs.
func (s *Session) Config() wire.Config { return s.cfg }

// Restarted reports whether loadgen's boot_id changed since NewSession
// or the last Recover: Recover must run before the next window.
func (s *Session) Restarted() bool { return s.restarted }

// Adopt starts the host side of variant id once it has passed gate
// Stage 5 (its loadgen target exists from then on, DESIGN.md sec 11):
// a salt history seeded with burstHashes, the hashes of its burst's
// recorded /hash pairs (gate.Report.BurstHashes), so a window hash that
// replays a burst salt is confirmed on the host. A seeded hash whose
// salt repeats is an error: the gate verified the burst.
func (s *Session) Adopt(id string, burstHashes []string) error {
	if _, ok := s.hist[id]; ok {
		return fmt.Errorf("orchestrator: variant %s adopted twice", id)
	}
	h := newHistory()
	for _, hash := range burstHashes {
		if reason := h.add(hash); reason != classifier.ReasonNone {
			return fmt.Errorf("orchestrator: variant %s: a burst hash is %s on the host", id, reason)
		}
	}
	s.hist[id] = h
	return nil
}

// Recover follows a loadgen restart (DESIGN.md sec 10): it reads and
// checks the new GET /config, expects the new boot_id, and PUTs every
// live target again, each of which must be new to loadgen. The restart
// emptied loadgen's salt sets; the host's histories stay. The next
// window polls the canary until it reads refused before it starts.
func (s *Session) Recover(ctx context.Context, live []string) error {
	old := s.cfg.BootID
	s.c.Client.ExpectAny()
	if err := s.readConfig(ctx); err != nil {
		// Back to the old boot_id, so every call fails as a restart
		// until a Recover succeeds, rather than accepting any.
		s.c.Client.Expect(old)
		return err
	}
	for _, id := range live {
		if _, ok := s.hist[id]; !ok {
			return fmt.Errorf("orchestrator: variant %s is not adopted", id)
		}
		put, created, err := s.c.Client.PutTarget(ctx, id)
		if err != nil {
			return fmt.Errorf("orchestrator: re-putting target %s: %w", id, err)
		}
		if !created {
			return fmt.Errorf("orchestrator: target %s existed in the new loadgen", id)
		}
		s.next[id] = put.Next
	}
	s.restarted, s.pollCanary = false, true
	return nil
}

// Drop retires variant id from loadgen after its last records are read
// (DESIGN.md sec 11): it DELETEs the target through the last Seq the
// Session read, so loadgen refuses if any record is still unread, then
// deletes the variant's namespace and forgets its history. A variant no
// window has named is refused: its records are the gate's (a Stage 5
// failure, whose target gate.Stage5 drops itself).
func (s *Session) Drop(ctx context.Context, id string) error {
	next, ok := s.next[id]
	if !ok {
		return fmt.Errorf("orchestrator: dropping variant %s, whose records were never read", id)
	}
	through := uint64(0)
	if next > 0 {
		through = next - 1
	}
	if _, err := s.c.Client.DeleteTarget(ctx, id, through); err != nil {
		if errors.Is(err, client.ErrBootChanged) {
			s.restarted = true
		}
		return fmt.Errorf("orchestrator: deleting target %s: %w", id, err)
	}
	if err := s.c.Cluster.DeleteNamespace(ctx, variant.Params{ID: id}.Namespace()); err != nil {
		return err
	}
	delete(s.hist, id)
	delete(s.next, id)
	return nil
}

// newRunID returns w-<16 hex>, random, so a run_id is never reused
// within a boot or across them.
func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b) // never returns an error (Go 1.24+); it crashes the program instead
	return "w-" + hex.EncodeToString(b)
}
