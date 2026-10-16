package loadgen

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

// Response is what the sender saw for one request.
type Response struct {
	Status int
	Header http.Header

	// Body is the response body. The sender reads at most
	// classifier.MaxBodyBytes+1 bytes and passes what it read with Err
	// nil, whether or not it hit that cap: a 2xx /hash or /verify body
	// longer than classifier.MaxBodyBytes is malformed_body (an
	// integrity failure, see classifier.Integrity); a negative's 2xx is
	// negative_accepted whatever its body. A sender whose read failed
	// sets Err instead: a body cut short by a failed read is a transport
	// error, never malformed_body.
	Body []byte

	// Latency is from send to full body read.
	Latency time.Duration

	// Err is any transport error (connection refused or reset, timeout,
	// body read failure). Reaching the classifier.MaxBodyBytes+1 read
	// cap is not one.
	Err error
}

// Verdict is one response's classification (DESIGN.md sec 8). Whether
// its Reason is an integrity failure is classifier.Integrity(Reason).
type Verdict struct {
	Bucket classifier.Bucket
	Reason classifier.Reason
}

// HashPair is a /hash request's password and the hash the variant
// answered with. Loadgen checks the hash's format, parameters and salt
// but cannot verify it; the host does (DESIGN.md sec 8).
type HashPair struct {
	Password string
	Hash     string
}

// Checker checks one variant's responses and records its /hash pairs.
// Its salt set spans every response it checks, so one Checker serves a
// variant for as long as loadgen serves it (DESIGN.md sec 8). It is safe
// for concurrent use.
type Checker struct {
	run *classifier.Run // no Verifier: loadgen does no Argon2

	mu    sync.Mutex
	pairs []HashPair
}

// NewChecker returns a Checker with an empty salt set and no pairs.
func NewChecker() *Checker {
	return &Checker{run: classifier.NewRun(nil)}
}

// Check returns resp's verdict as a response to req. It records req's
// password and the response's hash when req is a /hash request, resp is
// 2xx, and the hash passed every check loadgen makes, whatever the 2xx
// status or the latency: the host verifies those hashes too. A /verify
// request or a contract negative never records a pair.
//
// It errors, with a zero Verdict, if req.Kind is not one of loadgen's
// kinds, before anything is classified. It also errors if it cannot
// read the hash back from a body the classifier accepted, which should
// not happen; the Verdict is then still valid, but no pair is recorded.
func (c *Checker) Check(req Request, resp Response) (Verdict, error) {
	kind, err := classifierKind(req.Kind)
	if err != nil {
		return Verdict{}, err
	}
	bucket, reason := c.run.Classify(classifier.Observation{
		Kind:      kind,
		Password:  req.Password,
		WantMatch: req.Match,
		Status:    resp.Status,
		Header:    resp.Header,
		Body:      resp.Body,
		Latency:   resp.Latency,
		Err:       resp.Err,
	})
	v := Verdict{bucket, reason}
	if !records(req.Kind, resp.Status, reason) {
		return v, nil
	}
	hash, err := hashField(resp.Body)
	if err != nil {
		return v, fmt.Errorf("loadgen: /hash body passed the checks but %w", err)
	}
	c.mu.Lock()
	c.pairs = append(c.pairs, HashPair{req.Password, hash})
	c.mu.Unlock()
	return v, nil
}

// records reports whether a response with this status and reason, to a
// request of this kind, carries a hash that passed every check. On a
// 2xx /hash response, over_hang_timeout and bad_status (a 2xx other than
// 200) come only after the integrity checks pass
// (classifier.Run.Classify), so every 2xx hash that passes loadgen's
// checks is recorded for the host to verify, whatever its status or
// latency.
func records(kind Kind, status int, reason classifier.Reason) bool {
	return kind == KindHash && status >= 200 && status <= 299 &&
		(reason == classifier.ReasonNone || reason == classifier.ReasonOverHangTimeout ||
			reason == classifier.ReasonBadStatus)
}

// Pairs returns a copy of the pairs recorded so far, in the order
// recorded.
func (c *Checker) Pairs() []HashPair {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.pairs)
}

// classifierKind maps a loadgen Kind onto the classifier's. A contract
// negative is classifier.Negative whatever its path: the classifier
// never reads a negative's body.
func classifierKind(k Kind) (classifier.Kind, error) {
	switch k {
	case KindHash:
		return classifier.Hash, nil
	case KindVerify:
		return classifier.Verify, nil
	case KindNegative:
		return classifier.Negative, nil
	}
	return 0, fmt.Errorf("loadgen: unknown request kind %q", k)
}

// hashField returns the "hash" string of a /hash response body that the
// classifier has accepted. The classifier accepts only an object whose
// one member is "hash" with a string value (a second member, a duplicate
// "hash" included, is malformed_body), so hashField reads back that one
// member: the hash whose salt the classifier recorded.
func hashField(body []byte) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", fmt.Errorf("decoding it failed: %w", err)
	}
	raw, ok := obj["hash"]
	if !ok {
		return "", errors.New("it has no hash field")
	}
	var hash string
	if err := json.Unmarshal(raw, &hash); err != nil {
		return "", fmt.Errorf("decoding its hash failed: %w", err)
	}
	return hash, nil
}
