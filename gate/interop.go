package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/classifier/testvectors"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
)

const (
	// pairsPerAssertion is how many pairs each interop assertion sends.
	pairsPerAssertion = 3

	// clientTimeout is a little over the hang-timeout, so a hang shows
	// up as the classifier's over_hang_timeout, not a client error.
	clientTimeout = classifier.HangTimeout + 500*time.Millisecond

	// maxPasswordBytes is the HTTP contract's password limit (DESIGN.md
	// sec 8).
	maxPasswordBytes = 1024
)

// realVerify is the "real reference library" of DESIGN.md sec 7 Stage 2:
// the fixed core's Verify, never the candidate's.
var realVerify classifier.Verifier = hashcore.New().Verify

// interop talks to the booted candidate one request at a time (DESIGN.md
// sec 7, Stage 2). Every 2xx goes through one classifier.Run, hash
// responses fully verified.
type interop struct {
	client *http.Client
	label  string
	run    *classifier.Run
	hashes []string // every hash the candidate returned, for Stage 3
}

func newInterop(client *http.Client, label string) *interop {
	c := *client
	c.Timeout = clientTimeout
	return &interop{client: &c, label: label, run: classifier.NewRun(realVerify)}
}

// freshPassword returns a random password from rand.Text, which the Go
// docs say holds at least 128 bits of randomness, enough to make the
// likelihood of collisions vanishingly small. Nothing checks the result
// against the pool; a candidate has no pool pair to replay for /hash
// because a collision is that unlikely.
func freshPassword() string { return rand.Text() }

// boundaryPasswords returns fresh passwords at the contract's limits, 1
// and maxPasswordBytes bytes. A short random password can collide with a
// pool password (the pool has a 1-byte one), so pool passwords are
// redrawn.
func boundaryPasswords() []string {
	pool := make(map[string]bool)
	for _, p := range testvectors.Pairs() {
		pool[p.Password] = true
	}
	var out []string
	for _, n := range []int{1, maxPasswordBytes} {
		for {
			var b strings.Builder
			for b.Len() < n {
				b.WriteString(rand.Text())
			}
			if p := b.String()[:n]; !pool[p] {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// post sends body to path and returns the response as an Observation
// (Kind and ground truth left to the caller), or a problem if the
// response lacks the run's X-EK8s-Variant. It reads at most
// classifier.MaxBodyBytes+1 bytes of the body and passes them with Err
// nil whether or not it hit that cap, so the classifier sees an over-cap
// 2xx body (malformed_body); Err is set only for a failed read.
func (s *interop) post(ctx context.Context, path string, body []byte) (classifier.Observation, string) {
	var o classifier.Observation
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://candidate"+path, bytes.NewReader(body))
	if err != nil {
		o.Err = err
		return o, ""
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		o.Err = err
		o.Latency = time.Since(start)
		return o, ""
	}
	defer resp.Body.Close()
	o.Status, o.Header = resp.StatusCode, resp.Header
	o.Body, o.Err = io.ReadAll(io.LimitReader(resp.Body, classifier.MaxBodyBytes+1))
	o.Latency = time.Since(start)
	return o, checkVariant(resp.Header, s.label)
}

// classify fails unless o classifies as success.
func (s *interop) classify(o classifier.Observation) string {
	if b, reason := s.run.Classify(o); b != classifier.Success {
		detail := fmt.Sprintf("%s %v (%s, status %d", o.Kind, b, reason, o.Status)
		if o.Err != nil {
			detail += ", " + o.Err.Error()
		}
		return detail + ")"
	}
	return ""
}

func jsonBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only maps of strings are passed
	}
	return b
}

// mutantHash hashes password via the candidate. The classifier has
// already checked the hash against password with realVerify (FullVerify).
func (s *interop) mutantHash(ctx context.Context, password string) (hash, problem string) {
	o, problem := s.post(ctx, "/hash", jsonBody(map[string]string{"password": password}))
	if problem != "" {
		return "", problem
	}
	o.Kind, o.Password, o.FullVerify = classifier.Hash, password, true
	if problem := s.classify(o); problem != "" {
		return "", problem
	}
	var resp struct{ Hash string }
	if err := json.Unmarshal(o.Body, &resp); err != nil {
		return "", "hash body: " + err.Error() // unreachable after a success
	}
	s.hashes = append(s.hashes, resp.Hash)
	return resp.Hash, ""
}

// mutantVerify verifies a pool pair via the candidate, against the pool's
// ground truth.
func (s *interop) mutantVerify(ctx context.Context, p testvectors.Pair) string {
	o, problem := s.post(ctx, "/verify", jsonBody(map[string]string{"password": p.Password, "hash": p.Hash}))
	if problem != "" {
		return problem
	}
	o.Kind, o.Password, o.WantMatch = classifier.Verify, p.Password, p.Match
	return s.classify(o)
}

// assertion is one of Stage 2's four interop assertions, run on
// pairsPerAssertion inputs; it returns the first failure, or "".
type assertion struct {
	check string
	run   func(s *interop, ctx context.Context, i int) string
}

// assertions returns the four interop assertions in DESIGN.md sec 7
// order, the RealHash ones over random pool pairs of each kind.
func assertions() []assertion {
	matches, mismatches := poolPairs()
	return []assertion{
		{"mutanthash-realverify-match", func(s *interop, ctx context.Context, _ int) string {
			_, problem := s.mutantHash(ctx, freshPassword()) // FullVerify is the real verify
			return problem
		}},
		{"realhash-mutantverify-match", func(s *interop, ctx context.Context, i int) string {
			return s.mutantVerify(ctx, matches[i])
		}},
		{"mutanthash-realverify-nomatch", func(s *interop, ctx context.Context, _ int) string {
			h, problem := s.mutantHash(ctx, freshPassword())
			if problem != "" {
				return problem
			}
			ok, err := realVerify(freshPassword(), h)
			if ok || err != nil {
				return fmt.Sprintf("real Verify of a different password against the candidate's hash: match=%v err=%v", ok, err)
			}
			return ""
		}},
		{"realhash-mutantverify-nomatch", func(s *interop, ctx context.Context, i int) string {
			return s.mutantVerify(ctx, mismatches[i])
		}},
	}
}

// poolPairs returns pairsPerAssertion random pool pairs with Match true
// and as many with Match false.
func poolPairs() (matches, mismatches []testvectors.Pair) {
	pairs := testvectors.Pairs()
	mrand.Shuffle(len(pairs), func(i, j int) { pairs[i], pairs[j] = pairs[j], pairs[i] })
	for _, p := range pairs {
		if p.Match && len(matches) < pairsPerAssertion {
			matches = append(matches, p)
		} else if !p.Match && len(mismatches) < pairsPerAssertion {
			mismatches = append(mismatches, p)
		}
	}
	if len(matches) < pairsPerAssertion || len(mismatches) < pairsPerAssertion {
		panic("gate: test-vector pool too small") // TestPoolSize guards this
	}
	return matches, mismatches
}

// contractCase is one Stage 2 contract-conformance request; the
// candidate must answer exactly 400 (DESIGN.md sec 7, sec 8).
type contractCase struct {
	name, path string
	body       []byte
}

// contractCases returns every contract case, numbered as in DESIGN.md
// sec 7 Stage 2.
func contractCases() []contractCase {
	pool := testvectors.Pairs()[0]
	long := strings.Repeat("a", maxPasswordBytes+1)
	verify := func(password, hash string) []byte {
		return jsonBody(map[string]string{"password": password, "hash": hash})
	}
	cases := []contractCase{
		{"1 verify malformed hash", "/verify", verify(pool.Password, "not-a-phc-string")},
		{"1 verify truncated hash", "/verify", verify(pool.Password, pool.Hash[:len(pool.Hash)-10])},
	}
	for _, n := range testvectors.Negatives() {
		cases = append(cases, contractCase{"2 verify non-exact params " + n.Name, "/verify", verify(pool.Password, n.Hash)})
	}
	cases = append(cases,
		contractCase{"3 verify empty hash", "/verify", verify(pool.Password, "")},
		contractCase{"3 verify missing hash", "/verify", jsonBody(map[string]string{"password": pool.Password})},
		contractCase{"3 hash empty password", "/hash", jsonBody(map[string]string{"password": ""})},
		contractCase{"3 hash missing password", "/hash", []byte(`{}`)},
		contractCase{"3 verify empty password", "/verify", verify("", pool.Hash)},
		contractCase{"3 verify missing password", "/verify", jsonBody(map[string]string{"hash": pool.Hash})},
		contractCase{"4 hash password over 1024 bytes", "/hash", jsonBody(map[string]string{"password": long})},
		contractCase{"4 verify password over 1024 bytes", "/verify", verify(long, pool.Hash)},
		contractCase{"5 hash invalid JSON", "/hash", []byte(`{"password": "x"`)},
		contractCase{"5 verify invalid JSON", "/verify", []byte(`{"password": "x", "hash": }`)},
		contractCase{"5 hash empty body", "/hash", nil},
		contractCase{"5 hash password not a string", "/hash", []byte(`{"password": 123}`)},
		contractCase{"5 verify hash not a string", "/verify", []byte(`{"password": "x", "hash": true}`)},
		contractCase{"5 verify body not an object", "/verify", []byte(`["x", "y"]`)},
		contractCase{"5 hash trailing object", "/hash", []byte(`{"password": "x"} {}`)},
		contractCase{"5 verify trailing garbage", "/verify", append(verify(pool.Password, pool.Hash), "x"...)},
	)
	return cases
}

// checkContract sends every contract case and fails on any answer but
// exactly 400 with the run's label.
func (s *interop) checkContract(ctx context.Context) string {
	var problems []string
	for _, c := range contractCases() {
		o, problem := s.post(ctx, c.path, c.body)
		switch {
		case o.Err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", c.name, o.Err))
		case problem != "":
			problems = append(problems, c.name+": "+problem)
		case o.Status != http.StatusBadRequest:
			problems = append(problems, fmt.Sprintf("%s: status %d, want 400", c.name, o.Status))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return strings.Join(problems, "; ")
}

// runInterop runs Stage 2 against a booted candidate, adding one Result
// per assertion, then "password-bounds" (the contract's 1- and 1024-byte
// passwords must hash, fully verified) and "contract"; ok reports whether
// all passed. Stage 3 then reads s.hashes, boundary hashes included.
func runInterop(ctx context.Context, r *Report, s *interop) (ok bool, err error) {
	for _, a := range assertions() {
		problem := ""
		for i := 0; i < pairsPerAssertion && problem == ""; i++ {
			if problem = a.run(s, ctx, i); problem != "" {
				problem = fmt.Sprintf("input %d: %s", i+1, problem)
			}
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if !r.add(2, a.check, problem) {
			return false, nil
		}
	}
	problem := ""
	for _, p := range boundaryPasswords() {
		if _, problem = s.mutantHash(ctx, p); problem != "" {
			problem = fmt.Sprintf("%d-byte password: %s", len(p), problem)
			break
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !r.add(2, "password-bounds", problem) {
		return false, nil
	}
	problem = s.checkContract(ctx)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return r.add(2, "contract", problem), nil
}

// Exact cost parameters (DESIGN.md sec 6), restated here rather than
// taken from hashcore or the classifier, so Stage 3 does not share a
// bug with either.
const (
	wantMemory      = 19456
	wantIterations  = 2
	wantParallelism = 1
)

// checkCostParams is Stage 3: every hash the candidate returned in
// Stage 2 encodes exactly m=19456, t=2, p=1, parsed as numbers. The
// classifier already demands the exact params segment, so this is
// deliberate defense in depth against a classifier bug.
func checkCostParams(hashes []string) string {
	if len(hashes) == 0 {
		return "no hashes from Stage 2"
	}
	var problems []string
	for i, h := range hashes {
		if p := costParams(h); p != "" {
			problems = append(problems, fmt.Sprintf("hash %d: %s", i+1, p))
		}
	}
	return strings.Join(problems, "; ")
}

// costParams checks one PHC string, $argon2id$v=19$m=..,t=..,p=..$salt$key.
func costParams(h string) string {
	seg := strings.Split(h, "$")
	if len(seg) != 6 || seg[0] != "" || seg[1] != "argon2id" {
		return "not an argon2id PHC string"
	}
	got := map[string]uint64{}
	for _, kv := range strings.Split(seg[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		n, err := strconv.ParseUint(v, 10, 32)
		if _, dup := got[k]; !ok || err != nil || dup {
			return fmt.Sprintf("params segment %q unparseable", seg[3])
		}
		got[k] = n
	}
	want := map[string]uint64{"m": wantMemory, "t": wantIterations, "p": wantParallelism}
	if len(got) != len(want) {
		return fmt.Sprintf("params segment %q: want exactly m, t, p", seg[3])
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			return fmt.Sprintf("params segment %q: %s must be exactly %d", seg[3], k, w)
		}
	}
	return ""
}
