package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/hostpairs"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// Record kinds and negative classes as the wire carries them
// (loadgen/request.go, loadgen/negative.go). An unknown kind or class
// fails closed: its record is never confirmed.
const (
	kindHash     = "hash"
	kindVerify   = "verify"
	kindNegative = "negative"

	classMalformedHash  = "malformed_hash"
	classNonExactParams = "non_exact_params"
	classEmptyPassword  = "empty_password"
	classTooLong        = "password_too_long"
	classInvalidJSON    = "invalid_json"
)

// maxPasswordBytes is the HTTP contract's password cap (DESIGN.md sec 8).
const maxPasswordBytes = 1024

// history is one variant's salt history on the host: a classifier.Run,
// with no Verifier, fed every /hash answer the host has read for the
// variant (its Stage 5 burst and every window), across loadgen restarts.
// Loadgen's own salt set empties on a restart; this one never does, so it
// holds every salt loadgen's could and more.
type history struct{ run *classifier.Run }

func newHistory() *history { return &history{run: classifier.NewRun(nil)} }

// add classifies hash as an on-time 200 /hash answer, which records its
// salt, and returns the reason: none for a canonical hash with a new
// salt; salt_reused if the host has seen its salt; non_canonical_phc or
// malformed_body for one loadgen should never have recorded.
func (h *history) add(hash string) classifier.Reason {
	_, reason := h.run.Classify(classifier.Observation{Kind: classifier.Hash, Status: http.StatusOK, Body: hashResponse(hash)})
	return reason
}

// hashResponse is the contract body {"hash": hash}.
func hashResponse(hash string) []byte {
	b, err := json.Marshal(struct {
		Hash string `json:"hash"`
	}{hash})
	if err != nil {
		panic(err) // a string field always marshals
	}
	return b
}

// verdict is the host's integrity finding for one variant in one
// window: the first confirmed integrity reason by record index, or none.
// Claims counts the records loadgen gave an integrity reason, and
// Unconfirmed those of them with evidence that the host did not confirm.
type verdict struct {
	Reason      classifier.Reason
	Index       int // the record whose failure the host found; -1 for none
	Claims      int
	Unconfirmed int
}

// judgeIntegrity is the host's integrity pass over one variant's
// records of one window (DESIGN.md sec 8, 9): it adds every recorded
// /hash pair to the variant's history, fully verifies every pair with the
// fixed core (results, from hostpairs.Verify, are in pair order), and
// re-checks each integrity record's evidence. A failure the host finds
// itself, a pair that does not verify or a salt it has seen, is
// confirmed by construction; a loadgen claim is confirmed only when the
// host's own classification of its evidence finds an integrity reason
// (confirm).
func judgeIntegrity(core *hashcore.Core, h *history, truth map[pairKey]bool, recs []wire.Record, results []hostpairs.Result) verdict {
	v := verdict{Index: -1}
	found := func(i int, reason classifier.Reason) {
		if v.Index < 0 || recs[i].Index < v.Index {
			v.Reason, v.Index = reason, recs[i].Index
		}
	}
	k := 0
	for i, r := range recs {
		if r.Pair == nil {
			continue
		}
		if reason := h.add(r.Pair.Hash); reason != classifier.ReasonNone {
			found(i, reason)
		}
		switch res := results[k]; {
		case res.Err != nil:
			found(i, classifier.ReasonVerifyError)
		case !res.OK:
			found(i, classifier.ReasonVerifyMismatch)
		}
		k++
	}
	// Claims after every pair, so a salt_reused claim is checked against
	// the whole window's pairs, whatever order loadgen saw them in.
	for i, r := range recs {
		if r.Bucket != classifier.Failed.String() || !classifier.Integrity(classifier.Reason(r.Reason)) {
			continue
		}
		v.Claims++
		if r.Evidence == nil {
			continue
		}
		if reason := confirm(core, h, truth, r); reason != classifier.ReasonNone {
			found(i, reason)
		} else {
			v.Unconfirmed++
		}
	}
	return v
}

// windowItems returns the pairs of recs to verify, in record order.
func windowItems(recs []wire.Record) []hostpairs.Item {
	var items []hostpairs.Item
	for _, r := range recs {
		if r.Pair != nil {
			items = append(items, hostpairs.Item{Password: r.Pair.Password, Hash: r.Pair.Hash})
		}
	}
	return items
}

// verifyAll fully verifies every pair of every target's records in one
// hostpairs.Verify call, so the workers stay busy across targets, and
// returns each target's results in its records' pair order.
func verifyAll(ctx context.Context, core *hashcore.Core, recs map[string][]wire.Record, order []string, workers int) (map[string][]hostpairs.Result, error) {
	var items []hostpairs.Item
	n := make(map[string]int, len(order))
	for _, id := range order {
		it := windowItems(recs[id])
		n[id] = len(it)
		items = append(items, it...)
	}
	res, err := hostpairs.Verify(ctx, core, items, workers)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: verifying hashes: %w", err)
	}
	out := make(map[string][]hostpairs.Result, len(order))
	at := 0
	for _, id := range order {
		out[id] = res[at : at+n[id]]
		at += n[id]
	}
	return out, nil
}

// confirm re-checks one integrity record on the host from its evidence
// (DESIGN.md sec 10): the bytes loadgen sent and the status and body it
// read. It returns the host's integrity reason, or ReasonNone when the
// host finds none or cannot build the check (an unparseable request, a
// /verify pair the host never made, an unknown kind or class), so a claim
// the host cannot account for is never confirmed. Latency is left out:
// integrity reasons apply at any latency (DESIGN.md sec 8).
//
//   - hash: the shared classifier on the response, with the fixed core's
//     Verify and the variant's history, so a reused salt is caught
//     against every hash the host has read for it.
//   - verify: the classifier on the response, with the host's own ground
//     truth for the pair in the request.
//   - negative: a 2xx to a request the HTTP contract rejects. The class
//     only picks which endpoints the request is checked against; the
//     request bytes must show the defect.
func confirm(core *hashcore.Core, h *history, truth map[pairKey]bool, r wire.Record) classifier.Reason {
	e := r.Evidence
	o := classifier.Observation{Status: e.Status, Header: http.Header{}, Body: e.Body}
	switch r.Kind {
	case kindHash:
		if !validHashRequest(e.RequestBody) {
			return classifier.ReasonNone
		}
		req, _ := decodeRequest(e.RequestBody)
		o.Kind, o.Password = classifier.Hash, req["password"]
		return classifyWithCore(core, h.run, o)
	case kindVerify:
		req, ok := decodeRequest(e.RequestBody)
		pw, okPW := req["password"]
		hash, okHash := req["hash"]
		if !ok || !okPW || !okHash || len(req) != 2 {
			return classifier.ReasonNone
		}
		match, known := truth[pairKey{pw, hash}]
		if !known {
			return classifier.ReasonNone
		}
		o.Kind, o.Password, o.WantMatch = classifier.Verify, pw, match
		_, reason := classifier.NewRun(nil).Classify(o)
		return integrityOnly(reason)
	case kindNegative:
		if !rejected(core, r.Class, e.RequestBody) {
			return classifier.ReasonNone
		}
		o.Kind = classifier.Negative
		_, reason := classifier.NewRun(nil).Classify(o)
		return integrityOnly(reason)
	}
	return classifier.ReasonNone
}

// classifyWithCore classifies hash observation o on run, the variant's
// history, which runs the body, canonical-form and salt checks and
// records the salt, then, if they pass, verifies the hash with core: the
// classifier's own order with FullVerify, split because the history is a
// Run with no Verifier.
func classifyWithCore(core *hashcore.Core, run *classifier.Run, o classifier.Observation) classifier.Reason {
	if _, reason := run.Classify(o); classifier.Integrity(reason) {
		return reason
	}
	if !is2xx(o.Status) {
		return classifier.ReasonNone
	}
	var body struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(o.Body, &body); err != nil {
		return classifier.ReasonNone // the classifier accepted it; cannot happen
	}
	ok, err := core.Verify(o.Password, body.Hash)
	switch {
	case err != nil:
		return classifier.ReasonVerifyError
	case !ok:
		return classifier.ReasonVerifyMismatch
	}
	return classifier.ReasonNone
}

func is2xx(status int) bool { return status >= 200 && status <= 299 }

// integrityOnly is reason if it is an integrity reason, else ReasonNone.
func integrityOnly(reason classifier.Reason) classifier.Reason {
	if classifier.Integrity(reason) {
		return reason
	}
	return classifier.ReasonNone
}

// decodeRequest decodes body as one JSON object of string members and
// nothing after it but white space. ok is false for anything else: not
// an object, a member that is not a string, a repeated member, trailing
// data, invalid UTF-8.
func decodeRequest(body []byte) (map[string]string, bool) {
	if !utf8.Valid(body) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	out := make(map[string]string)
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key := t.(string) // a key token in an object is always a string
		// A RawMessage first: null decodes into a string with no error.
		var raw json.RawMessage
		var v string
		if err := dec.Decode(&raw); err != nil || len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &v) != nil {
			return nil, false
		}
		if _, dup := out[key]; dup {
			return nil, false
		}
		out[key] = v
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return out, true
}

func validPassword(pw string) bool { return pw != "" && len(pw) <= maxPasswordBytes }

// validHashRequest reports whether body is a /hash request the HTTP
// contract accepts: exactly a password member, valid.
func validHashRequest(body []byte) bool {
	req, ok := decodeRequest(body)
	pw, okPW := req["password"]
	return ok && okPW && len(req) == 1 && validPassword(pw)
}

// validVerifyRequest reports whether body is a /verify request the HTTP
// contract accepts: exactly a valid password and a hash the fixed core
// takes (no Argon2 runs when it does not: Verify checks the parameters
// first).
func validVerifyRequest(core *hashcore.Core, body []byte) bool {
	req, ok := decodeRequest(body)
	pw, okPW := req["password"]
	hash, okHash := req["hash"]
	if !ok || !okPW || !okHash || len(req) != 2 || !validPassword(pw) {
		return false
	}
	_, err := core.Verify(pw, hash)
	return err == nil
}

// rejected reports whether the HTTP contract rejects a negative of class
// class with this body: the two hash classes go to /verify only
// (loadgen/negative.go), so the body must be invalid there; the other
// three may go to either endpoint, so it must be invalid on both. An
// unknown class is never rejected.
func rejected(core *hashcore.Core, class string, body []byte) bool {
	switch class {
	case classMalformedHash, classNonExactParams:
		return !validVerifyRequest(core, body)
	case classEmptyPassword, classTooLong, classInvalidJSON:
		return !validHashRequest(body) && !validVerifyRequest(core, body)
	}
	return false
}

// errUnconfirmed is a window's fault when loadgen claimed integrity
// failures for a variant, with evidence, and the host confirmed none of
// them: loadgen and the host disagree about the same bytes, which says
// nothing sure about the variant.
var errUnconfirmed = errors.New("integrity claims the host does not confirm")
