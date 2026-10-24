// Package wire is the wire format of loadgen's control API (DESIGN.md
// sec 10): its request and response bodies, shared by loadgen's server
// and the clients that drive it. It is the orchestrator's contract
// (docs/decisions.md, PR #44): loadgen/wire_golden_test.go pins its
// bytes, so a change to a type, a field or a tag here changes the wire.
// It imports the standard library only.
//
// The wire format: snake_case names; buckets, reasons, kinds and
// negative classes as strings; durations as integer nanoseconds (_ns);
// times as RFC 3339 with nanoseconds, UTC; byte fields base64.
package wire

import "time"

// A run's kinds: Start.Kind and Run.Kind.
const (
	KindWindow = "window"
	KindBurst  = "burst"
)

// A run's per-target states: RunTarget.State.
const (
	StateRunning   = "running"   // its RunWindow or RunBurst has not returned
	StateDone      = "done"      // it returned nil: every send was made and recorded
	StateCancelled = "cancelled" // the run's context ended first
	StateError     = "error"     // it returned another error, before sending anything
)

// The policy canary's results (DESIGN.md sec 11): Canary.Result.
const (
	CanaryConnected = "connected" // policy enforcement is off: the half is excluded
	CanaryRefused   = "refused"   // the policy stopped the connection
	CanaryError     = "error"     // shows nothing about the policy: the half is excluded
)

// Error is the body of every error response.
type Error struct {
	BootID string `json:"boot_id"`
	Error  string `json:"error"`
}

// BootID is the body of DELETE /targets/{id}'s response.
type BootID struct {
	BootID string `json:"boot_id"`
}

// Config is the body of GET /config's response.
type Config struct {
	BootID       string  `json:"boot_id"`
	Rate         float64 `json:"r"`
	WindowS      float64 `json:"window_s"`
	BurstRate    float64 `json:"burst_rate"`
	BurstSends   int     `json:"burst_sends"`
	WindowSends  int     `json:"window_sends"`
	WindowPairs  int     `json:"window_pairs"`
	BurstPairs   int     `json:"burst_pairs"`
	RingCapacity int     `json:"ring_capacity"`
}

// Target is one target in GET /targets's response.
type Target struct {
	ID   string `json:"id"`
	Next uint64 `json:"next"`
}

// Targets is the body of GET /targets's response.
type Targets struct {
	BootID  string   `json:"boot_id"`
	Targets []Target `json:"targets"`
}

// PutTarget is the body of PUT /targets/{id}'s response.
type PutTarget struct {
	BootID string `json:"boot_id"`
	ID     string `json:"id"`
	Next   uint64 `json:"next"`
}

// Records is the body of GET /targets/{id}/records's response.
type Records struct {
	BootID  string   `json:"boot_id"`
	Records []Record `json:"records"`
	Gap     bool     `json:"gap"`
	Next    uint64   `json:"next"`
}

// Canary is the body of POST /canary's response.
type Canary struct {
	BootID string `json:"boot_id"`
	Result string `json:"result"`
}

// Upload is the request body of PUT /targets/{id}/pairs/{run_id}.
type Upload struct {
	Pairs []UploadPair `json:"pairs"`
}

// UploadPair is one pair in Upload, the request body of PUT
// /targets/{id}/pairs/{run_id}. Its fields are pointers so that an
// absent or null field is refused, not read as "" or false: a missing
// match would turn a mismatch pair into a match.
type UploadPair struct {
	Password *string `json:"password"`
	Hash     *string `json:"hash"`
	Match    *bool   `json:"match"`
}

// Uploaded is the body of PUT /targets/{id}/pairs/{run_id}'s response.
type Uploaded struct {
	BootID string `json:"boot_id"`
	Count  int    `json:"count"`
}

// Start is the request body of POST /runs/{run_id}/start.
type Start struct {
	Kind    string   `json:"kind"`
	Targets []string `json:"targets"`
}

// Started is the body of POST /runs/{run_id}/start's response.
type Started struct {
	BootID    string    `json:"boot_id"`
	RunID     string    `json:"run_id"`
	StartWall time.Time `json:"start_wall"`
}

// Run is the body of GET /runs/{run_id}'s and POST
// /runs/{run_id}/cancel's responses.
type Run struct {
	BootID    string               `json:"boot_id"`
	RunID     string               `json:"run_id"`
	Kind      string               `json:"kind"`
	StartWall time.Time            `json:"start_wall"`
	Targets   map[string]RunTarget `json:"targets"`
}

// RunTarget is one target's part in a Run.
type RunTarget struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// Record is a loadgen.Record on the wire, one record in GET
// /targets/{id}/records's response; its fields mean what
// loadgen.Record's do. A Skipped or Fault record has no Bucket or
// Reason, so that a loadgen problem cannot be counted as a variant
// failure.
type Record struct {
	Seq            uint64    `json:"seq"`
	Target         string    `json:"target"`
	RunID          string    `json:"run_id"`
	Index          int       `json:"index"`
	Kind           string    `json:"kind"`
	Class          string    `json:"class,omitempty"` // negatives only
	IntendedWall   time.Time `json:"intended_wall"`
	IntendedMonoNS int64     `json:"intended_mono_ns"`
	ActualMonoNS   int64     `json:"actual_mono_ns"`
	LatencyNS      int64     `json:"latency_ns"`
	Bucket         string    `json:"bucket,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Skipped        bool      `json:"skipped"`
	Fault          string    `json:"fault"`
	Pair           *Pair     `json:"pair,omitempty"`
	Evidence       *Evidence `json:"evidence,omitempty"`
}

// Pair is a Record's /hash pair, in GET /targets/{id}/records's response.
type Pair struct {
	Password string `json:"password"`
	Hash     string `json:"hash"`
}

// Evidence is a Record's evidence, in GET /targets/{id}/records's response.
type Evidence struct {
	RequestBody []byte `json:"request_body"`
	Status      int    `json:"status"`
	Body        []byte `json:"body"`
}
