package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// maxRecordsPage is the most records one records request returns, and
// its default limit.
const maxRecordsPage = 1000

// maxStartBody is a start request's body cap, in bytes.
const maxStartBody = 64 << 10

// Handler returns the control API's HTTP handler (DESIGN.md sec 10).
// Every response, the ServeMux's own 404 and 405 included, carries
// boot_id in the Ek8s-Boot-Id header, and every JSON body carries it as
// "boot_id". An error's body is {boot_id, error}.
func (c *Control) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", c.handleConfig)
	mux.HandleFunc("GET /targets", c.handleTargets)
	mux.HandleFunc("PUT /targets/{id}", c.handlePutTarget)
	mux.HandleFunc("DELETE /targets/{id}", c.handleDeleteTarget)
	mux.HandleFunc("GET /targets/{id}/records", c.handleRecords)
	mux.HandleFunc("PUT /targets/{id}/pairs/{run_id}", c.handlePutPairs)
	mux.HandleFunc("POST /runs/{run_id}/start", c.handleStart)
	mux.HandleFunc("GET /runs/{run_id}", c.handleRun)
	mux.HandleFunc("POST /runs/{run_id}/cancel", c.handleCancel)
	mux.HandleFunc("POST /canary", c.handleCanary)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.BootIDHeader, c.bootID)
		mux.ServeHTTP(w, r)
	})
}

// missing returns the name of the first field p lacks, "" for none.
func missing(p wire.UploadPair) string {
	switch {
	case p.Password == nil:
		return "password"
	case p.Hash == nil:
		return "hash"
	case p.Match == nil:
		return "match"
	}
	return ""
}

// startWall is a run's start as the wire carries it: UTC, without the
// monotonic reading, so it equals its records' first IntendedWall.
func startWall(start time.Time) time.Time { return start.Round(0).UTC() }

func (c *Control) toWireRun(v runView) wire.Run {
	ts := make(map[string]wire.RunTarget, len(v.targets))
	for id, s := range v.targets {
		ts[id] = wire.RunTarget{State: s.state, Error: s.err}
	}
	return wire.Run{BootID: c.bootID, RunID: v.id, Kind: string(v.kind), StartWall: startWall(v.start), Targets: ts}
}

func toWireRecord(r Record) wire.Record {
	w := wire.Record{
		Seq:            r.Seq,
		Target:         r.Target,
		RunID:          r.RunID,
		Index:          r.Index,
		Kind:           r.Kind.String(),
		IntendedWall:   r.IntendedWall.UTC(),
		IntendedMonoNS: int64(r.IntendedMono),
		ActualMonoNS:   int64(r.ActualMono),
		LatencyNS:      int64(r.Latency),
		Skipped:        r.Skipped,
		Fault:          r.Fault,
	}
	if !r.Skipped && r.Fault == "" {
		w.Bucket, w.Reason = r.Verdict.Bucket.String(), string(r.Verdict.Reason)
	}
	if r.Kind == KindNegative {
		w.Class = r.Class.String()
	}
	if r.Pair != nil {
		w.Pair = &wire.Pair{Password: r.Pair.Password, Hash: r.Pair.Hash}
	}
	if r.Evidence != nil {
		w.Evidence = &wire.Evidence{Path: r.Evidence.Path, RequestBody: r.Evidence.RequestBody, Status: r.Evidence.Status, Body: r.Evidence.Body}
	}
	return w
}

// GET /config
func (c *Control) handleConfig(w http.ResponseWriter, r *http.Request) {
	c.writeJSON(w, http.StatusOK, wire.Config{
		BootID:       c.bootID,
		Rate:         c.cfg.Rate,
		WindowS:      c.cfg.Window.Seconds(),
		BurstRate:    c.cfg.BurstRate,
		BurstSends:   c.burstSends,
		WindowSends:  c.windowSends,
		WindowPairs:  c.windowPairs,
		BurstPairs:   c.burstPairs,
		RingCapacity: c.ringCap,
	})
}

// GET /targets
func (c *Control) handleTargets(w http.ResponseWriter, r *http.Request) {
	list := c.targetList()
	ts := make([]wire.Target, len(list))
	for i, t := range list {
		ts[i] = wire.Target{ID: t.id, Next: t.next}
	}
	c.writeJSON(w, http.StatusOK, wire.Targets{BootID: c.bootID, Targets: ts})
}

// PUT /targets/{id}; the body is ignored.
func (c *Control) handlePutTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	next, created, err := c.putTarget(id)
	if err != nil {
		c.writeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.writeJSON(w, status, wire.PutTarget{BootID: c.bootID, ID: id, Next: next})
}

// DELETE /targets/{id}?through=S
func (c *Control) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	through, err := uintParam(r.URL.Query(), "through")
	if err != nil {
		c.writeError(w, err)
		return
	}
	if err := c.deleteTarget(r.PathValue("id"), through); err != nil {
		c.writeError(w, err)
		return
	}
	c.writeJSON(w, http.StatusOK, wire.BootID{BootID: c.bootID})
}

// GET /targets/{id}/records?since=S[&limit=L]
func (c *Control) handleRecords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := uintParam(q, "since")
	if err != nil {
		c.writeError(w, err)
		return
	}
	limit := maxRecordsPage
	if q.Has("limit") {
		l, err := strconv.Atoi(q.Get("limit"))
		if err != nil || l < 1 || l > maxRecordsPage {
			c.writeError(w, apiErrorf(http.StatusBadRequest, "loadgen: limit %q, want 1 to %d", q.Get("limit"), maxRecordsPage))
			return
		}
		limit = l
	}
	recs, gap, next, err := c.records(r.PathValue("id"), since, limit)
	if err != nil {
		c.writeError(w, err)
		return
	}
	wrs := make([]wire.Record, len(recs))
	for i, rec := range recs {
		wrs[i] = toWireRecord(rec)
	}
	c.writeJSON(w, http.StatusOK, wire.Records{BootID: c.bootID, Records: wrs, Gap: gap, Next: next})
}

// PUT /targets/{id}/pairs/{run_id}, body {pairs: [{password, hash, match}]}.
// The run_id is checked before the body is read. Every pair must carry
// all three fields; their values are not checked.
func (c *Control) handlePutPairs(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validRunID(runID) {
		c.writeError(w, badRunID(runID))
		return
	}
	var body wire.Upload
	if err := decodeBody(w, r, c.pairsCap, &body); err != nil {
		c.writeError(w, err)
		return
	}
	pairs := make([]Pair, len(body.Pairs))
	for i, p := range body.Pairs {
		if f := missing(p); f != "" {
			c.writeError(w, apiErrorf(http.StatusBadRequest, "loadgen: pair %d has no %s", i, f))
			return
		}
		pairs[i] = Pair{*p.Password, *p.Hash, *p.Match}
	}
	n, err := c.putPairs(r.PathValue("id"), runID, pairs)
	if err != nil {
		c.writeError(w, err)
		return
	}
	c.writeJSON(w, http.StatusCreated, wire.Uploaded{BootID: c.bootID, Count: n})
}

// POST /runs/{run_id}/start, body {kind, targets}.
func (c *Control) handleStart(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validRunID(runID) {
		c.writeError(w, badRunID(runID))
		return
	}
	var body wire.Start
	if err := decodeBody(w, r, maxStartBody, &body); err != nil {
		c.writeError(w, err)
		return
	}
	v, err := c.startRun(runID, runKind(body.Kind), body.Targets)
	if err != nil {
		c.writeError(w, err)
		return
	}
	c.writeJSON(w, http.StatusOK, wire.Started{BootID: c.bootID, RunID: runID, StartWall: startWall(v.start)})
}

// GET /runs/{run_id}
func (c *Control) handleRun(w http.ResponseWriter, r *http.Request) {
	v, err := c.runStatus(r.PathValue("run_id"))
	if err != nil {
		c.writeError(w, err)
		return
	}
	c.writeJSON(w, http.StatusOK, c.toWireRun(v))
}

// POST /runs/{run_id}/cancel; the body is ignored.
func (c *Control) handleCancel(w http.ResponseWriter, r *http.Request) {
	v, err := c.cancelRun(r.PathValue("run_id"))
	if err != nil {
		c.writeError(w, err)
		return
	}
	c.writeJSON(w, http.StatusOK, c.toWireRun(v))
}

func badRunID(id string) error {
	return apiErrorf(http.StatusBadRequest, "loadgen: run_id %q, want 1 to 64 of A-Z a-z 0-9 . _ -, not starting with .", id)
}

// decodeBody decodes r's body, one JSON value, into v. It refuses with
// 400 a body that is not valid JSON, has fields v lacks, or has anything
// but white space after the value, and with 413 one over limit bytes.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, err = dec.Token(); err == io.EOF {
			return nil
		}
		if err == nil {
			err = errors.New("data after the JSON value")
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return apiErrorf(http.StatusRequestEntityTooLarge, "loadgen: body over %d bytes", limit)
	}
	return apiErrorf(http.StatusBadRequest, "loadgen: decoding the body: %v", err)
}

// POST /canary. The dial is detached from the request's context, so only
// the dial timeout and the policy decide the result: a caller's deadline
// cannot read as refused.
func (c *Control) handleCanary(w http.ResponseWriter, r *http.Request) {
	c.writeJSON(w, http.StatusOK, wire.Canary{BootID: c.bootID, Result: canary(context.WithoutCancel(r.Context()), c.cfg.CanaryAddr)})
}

// uintParam returns query parameter name, which must be a decimal uint64.
func uintParam(q url.Values, name string) (uint64, error) {
	if !q.Has(name) {
		return 0, apiErrorf(http.StatusBadRequest, "loadgen: %s missing", name)
	}
	v, err := strconv.ParseUint(q.Get(name), 10, 64)
	if err != nil {
		return 0, apiErrorf(http.StatusBadRequest, "loadgen: %s %q, want a decimal uint64", name, q.Get(name))
	}
	return v, nil
}

// writeError answers with err as {boot_id, error}: an apiError's status,
// or 500 for any other error.
func (c *Control) writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var ae *apiError
	if errors.As(err, &ae) {
		status = ae.status
	}
	c.writeJSON(w, status, wire.Error{BootID: c.bootID, Error: err.Error()})
}

// writeJSON answers with status and v as JSON. It encodes v whole before
// writing, so a failed encoding is still a 500.
func (c *Control) writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b, _ = json.Marshal(wire.Error{BootID: c.bootID, Error: "loadgen: encoding the response: " + err.Error()})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}
