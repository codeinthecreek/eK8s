package loadgen

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"time"
)

// startLead is how far after a start request a run's first send is
// intended, so every target's goroutine is waiting before it.
const startLead = 200 * time.Millisecond

// maxPairJSON is the most bytes one pair takes in an upload body: a
// 1024-byte password (maxPasswordLen; base32, so nothing to escape), a
// 97-byte exact-params hash, and 40 bytes of names, quotes, false and a
// comma. pairsSlack covers the rest of the body.
const (
	maxPairJSON = 1161
	pairsSlack  = 4096
)

// runIDRE is the run_id format. A run_id cannot start with a dot, so
// none is a dot segment, which the ServeMux would clean away.
var runIDRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)

// validRunID reports whether id is a run_id.
func validRunID(id string) bool { return runIDRE.MatchString(id) }

// runKind is what a run sends: a sustained-mode window to each of its
// targets, or a Stage 5 burst to its one target.
type runKind string

const (
	windowRun runKind = "window"
	burstRun  runKind = "burst"
)

// A run's per-target states.
const (
	stateRunning   = "running"   // its RunWindow or RunBurst has not returned
	stateDone      = "done"      // it returned nil: every send was made and recorded
	stateCancelled = "cancelled" // the run's context ended first
	stateError     = "error"     // it returned another error, before sending anything
)

// run is one window or burst. id, kind, start and cancel are fixed when
// it starts; targets, each target's state by id, is guarded by
// Control.mu.
type run struct {
	id      string
	kind    runKind
	start   time.Time // carries its monotonic reading, for the schedule
	cancel  context.CancelFunc
	targets map[string]targetState
}

// targetState is one target's part in a run: a state, and the error's
// message when the state is stateError.
type targetState struct {
	state string
	err   string
}

// runView is a copy of a run's fields, for the API to answer with.
type runView struct {
	id      string
	kind    runKind
	start   time.Time
	targets map[string]targetState
}

func (r *run) view() runView {
	return runView{r.id, r.kind, r.start, maps.Clone(r.targets)}
}

// putPairs makes pairs target id's pending batch, for run runID, and
// returns how many it holds. A batch for another run_id is replaced; one
// for runID is refused, as is any upload while the target is busy or
// for a run_id a run already used.
func (c *Control) putPairs(id, runID string, pairs []Pair) (int, error) {
	if len(pairs) == 0 {
		return 0, apiErrorf(http.StatusBadRequest, "loadgen: no pairs")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.targets[id]
	switch {
	case !ok:
		return 0, apiErrorf(http.StatusNotFound, "loadgen: no target %q", id)
	case t.busy:
		return 0, apiErrorf(http.StatusConflict, "loadgen: target %s is in a run", id)
	case c.runs[runID] != nil:
		return 0, apiErrorf(http.StatusConflict, "loadgen: run_id %s is already used", runID)
	case t.pending != nil && t.pending.runID == runID:
		return 0, apiErrorf(http.StatusConflict, "loadgen: target %s already has a batch for run_id %s", id, runID)
	}
	t.pending = &batch{runID, pairs}
	return len(pairs), nil
}

// startRun starts run runID of kind on targets ids, each sending its
// pending batch for runID, and returns the run. It is all or nothing: it
// checks every target before it takes any batch, so a refusal consumes
// no batch and changes nothing. The run's context derives from c.ctx,
// never from the request's.
func (c *Control) startRun(runID string, kind runKind, ids []string) (runView, error) {
	need := c.windowPairs
	switch {
	case kind == burstRun:
		need = c.burstPairs
		if len(ids) != 1 {
			return runView{}, apiErrorf(http.StatusBadRequest, "loadgen: a burst names %d targets, want exactly one", len(ids))
		}
	case kind != windowRun:
		return runView{}, apiErrorf(http.StatusBadRequest, "loadgen: kind %q, want window or burst", kind)
	case len(ids) == 0:
		return runView{}, apiErrorf(http.StatusBadRequest, "loadgen: no targets")
	}
	if sorted := slices.Sorted(slices.Values(ids)); len(slices.Compact(sorted)) != len(ids) {
		return runView{}, apiErrorf(http.StatusBadRequest, "loadgen: duplicate targets")
	}

	c.mu.Lock()
	ts, err := c.startable(runID, need, ids)
	if err != nil {
		c.mu.Unlock()
		return runView{}, err
	}
	ctx, cancel := context.WithCancel(c.ctx)
	r := &run{id: runID, kind: kind, start: time.Now().Add(startLead), cancel: cancel, targets: make(map[string]targetState, len(ts))}
	batches := make([][]Pair, len(ts))
	for i, t := range ts {
		batches[i], t.pending, t.busy = t.pending.pairs, nil, true
		r.targets[t.ID] = targetState{state: stateRunning}
	}
	c.runs[runID], c.active = r, r
	v := r.view()
	c.mu.Unlock()

	for i, t := range ts {
		go c.runTarget(ctx, r, t, batches[i])
	}
	return v, nil
}

// startable returns targets ids, if run runID can start on them, each
// needing a pending batch for runID of at least need pairs. It changes
// nothing. c.mu must be held.
func (c *Control) startable(runID string, need int, ids []string) ([]*target, error) {
	if c.active != nil {
		return nil, apiErrorf(http.StatusConflict, "loadgen: run %s is active", c.active.id)
	}
	if c.runs[runID] != nil {
		return nil, apiErrorf(http.StatusConflict, "loadgen: run_id %s is already used", runID)
	}
	ts := make([]*target, len(ids))
	for i, id := range ids {
		t, ok := c.targets[id]
		switch {
		case !ok:
			return nil, apiErrorf(http.StatusConflict, "loadgen: no target %q", id)
		case t.busy:
			return nil, apiErrorf(http.StatusConflict, "loadgen: target %s is in a run", id)
		case t.pending == nil || t.pending.runID != runID:
			return nil, apiErrorf(http.StatusConflict, "loadgen: target %s has no batch for run_id %s", id, runID)
		case len(t.pending.pairs) < need:
			return nil, apiErrorf(http.StatusConflict, "loadgen: target %s's batch holds %d pairs, want at least %d", id, len(t.pending.pairs), need)
		}
		ts[i] = t
	}
	return ts, nil
}

// runTarget sends run r to t, then records how it ended (finish).
func (c *Control) runTarget(ctx context.Context, r *run, t *target, pairs []Pair) {
	var err error
	switch r.kind {
	case windowRun:
		err = c.sender.RunWindow(ctx, t.Target, r.id, r.start, c.cfg.Rate, c.cfg.Window, pairs)
	case burstRun:
		err = c.sender.RunBurst(ctx, t.Target, r.id, r.start, c.cfg.BurstRate, c.burstSends, pairs)
	}
	c.finish(r, t, err)
}

// finish records that t's part in run r has ended with err, which
// RunWindow or RunBurst returned, and frees t. RunX returns only once
// every send it launched is recorded, so when no target of r is still
// running every record of r is in its Ring; r is then no longer active,
// and its context is released.
func (c *Control) finish(r *run, t *target, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case err == nil:
		r.targets[t.ID] = targetState{state: stateDone}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		r.targets[t.ID] = targetState{state: stateCancelled}
	default:
		r.targets[t.ID] = targetState{stateError, err.Error()}
	}
	t.busy = false
	for _, ts := range r.targets {
		if ts.state == stateRunning {
			return
		}
	}
	c.active = nil
	r.cancel()
}

// runStatus returns run id.
func (c *Control) runStatus(id string) (runView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return runView{}, apiErrorf(http.StatusNotFound, "loadgen: no run %q", id)
	}
	return r.view(), nil
}

// cancelRun cancels run id's context and returns the run. Its targets
// stay running until each one's sends in flight are recorded. Cancelling
// a run that has ended changes nothing.
func (c *Control) cancelRun(id string) (runView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return runView{}, apiErrorf(http.StatusNotFound, "loadgen: no run %q", id)
	}
	r.cancel()
	return r.view(), nil
}
