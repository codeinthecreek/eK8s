package loadgen

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// Control is loadgen's control API (DESIGN.md sec 10): the targets it
// serves, by variant id, and its boot_id. It is made with NewControl and
// served by Handler, and is safe for concurrent use.
//
// mu guards targets, each target's busy flag and pending batch, runs,
// each run's target states, and active. A target's Ring and Checker have
// their own locks. Control never calls Ring.Read, Ring.ReadN or a Checker
// method while holding mu, so a long read never stalls the API; it calls
// Ring.Next under mu where a check must agree with what mu then changes.
type Control struct {
	cfg    Config
	ctx    context.Context // the process's: every run's context derives from it
	sender *Sender         // sends every run, so all records share its base
	bootID string          // 16 random bytes in hex, one per process

	// Fixed at NewControl, from cfg.
	windowSends, windowPairs int
	burstSends, burstPairs   int
	ringCap                  int
	pairsCap                 int64 // a pairs upload's body cap, in bytes

	mu      sync.Mutex
	targets map[string]*target
	runs    map[string]*run // every run started, by run_id, for the process's life
	active  *run            // the run still sending, nil for none
}

// target is one variant Control serves. busy is set while a run sends to
// it; pending is the batch its next run will send, nil for none. Both are
// guarded by Control.mu. Only a run adds to the Ring, and only while the
// target is busy, so a target that is not busy gets no Add.
type target struct {
	Target
	busy    bool
	pending *batch
}

// batch is one upload of /verify pairs, for run runID.
type batch struct {
	runID string
	pairs []Pair
}

// apiError is an error the control API answers with its status.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func apiErrorf(status int, format string, args ...any) error {
	return &apiError{status, fmt.Sprintf(format, args...)}
}

// NewControl returns a Control with no targets, which will send with s.
// ctx is the process's context: every run's context derives from it,
// never from a request's, so cancelling ctx cancels every run. It errors
// if cfg.Validate does.
func NewControl(ctx context.Context, cfg Config, s *Sender) (*Control, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &Control{
		cfg: cfg, ctx: ctx, sender: s, bootID: newBootID(),
		targets: make(map[string]*target), runs: make(map[string]*run),
	}
	var err error
	if c.windowSends, err = cfg.WindowSends(); err != nil {
		return nil, err
	}
	if c.windowPairs, err = cfg.WindowPairs(); err != nil {
		return nil, err
	}
	if c.burstSends, err = cfg.BurstSends(); err != nil {
		return nil, err
	}
	if c.burstPairs, err = cfg.BurstPairs(); err != nil {
		return nil, err
	}
	if c.ringCap, err = cfg.RingCapacity(); err != nil {
		return nil, err
	}
	c.pairsCap = int64(max(c.windowPairs, c.burstPairs))*maxPairJSON + pairsSlack
	return c, nil
}

// newBootID returns 16 crypto/rand bytes in hex.
func newBootID() string {
	b := make([]byte, 16)
	rand.Read(b) // never returns an error (Go 1.24+); it crashes the program instead
	return hex.EncodeToString(b)
}

// putTarget makes id a target, with a new Checker and Ring, and returns
// its Ring's Next and true. If id is already a target it changes nothing
// and returns false: the target keeps its Checker, so its salt set
// (DESIGN.md sec 8), and its Ring.
func (c *Control) putTarget(id string) (next uint64, created bool, err error) {
	url, err := c.cfg.targetURL(id)
	if err != nil {
		return 0, false, &apiError{http.StatusBadRequest, err.Error()}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.targets[id]; ok {
		return t.Ring.Next(), false, nil
	}
	ring, err := NewRing(c.ringCap)
	if err != nil {
		return 0, false, err
	}
	c.targets[id] = &target{Target: Target{ID: id, URL: url, Checker: NewChecker(), Ring: ring}}
	return 0, true, nil
}

// deleteTarget drops target id, its Checker and its Ring, once its
// records through Seq through have been read. It refuses while the
// target is busy or its Ring holds a record past through. The checks
// and the removal happen under one hold of mu: a target that is not busy
// gets no Add, so its Ring's Next cannot change between them.
func (c *Control) deleteTarget(id string, through uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.targets[id]
	if !ok {
		return apiErrorf(http.StatusNotFound, "loadgen: no target %q", id)
	}
	if t.busy {
		return apiErrorf(http.StatusConflict, "loadgen: target %s is in a run", id)
	}
	// Next() > through+1, written so that through = MaxUint64 cannot overflow.
	if next := t.Ring.Next(); next > 0 && next-1 > through {
		return apiErrorf(http.StatusConflict, "loadgen: target %s has unread records: its newest is %d, past through=%d", id, next-1, through)
	}
	delete(c.targets, id)
	return nil
}

// lookup returns target id.
func (c *Control) lookup(id string) (*target, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.targets[id]
	if !ok {
		return nil, apiErrorf(http.StatusNotFound, "loadgen: no target %q", id)
	}
	return t, nil
}

// records returns at most limit of target id's records from Seq since,
// with Ring.ReadN's gap flag, and the Seq to read from next: one past the
// last record returned, or the Ring's Next when none are.
func (c *Control) records(id string, since uint64, limit int) (recs []Record, gap bool, next uint64, err error) {
	t, err := c.lookup(id)
	if err != nil {
		return nil, false, 0, err
	}
	// Next is read before ReadN, so that when ReadN returns nothing every
	// record added since has a Seq >= next, and reading on from next
	// skips none.
	next = t.Ring.Next()
	recs, gap = t.Ring.ReadN(since, limit)
	if len(recs) > 0 {
		next = recs[len(recs)-1].Seq + 1
	}
	return recs, gap, next, nil
}

// targetNext is one target's id and its Ring's Next.
type targetNext struct {
	id   string
	next uint64
}

// targetList returns every target's id and Ring's Next, in numeric id
// order.
func (c *Control) targetList() []targetNext {
	c.mu.Lock()
	list := make([]targetNext, 0, len(c.targets))
	for id, t := range c.targets {
		list = append(list, targetNext{id, t.Ring.Next()})
	}
	c.mu.Unlock()
	slices.SortFunc(list, func(a, b targetNext) int { return compareIDs(a.id, b.id) })
	return list
}

// compareIDs orders variant ids by their numbers. An id has no leading
// zeros (validID), so a shorter id is a smaller number, and ids of one
// length compare as strings.
func compareIDs(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}
