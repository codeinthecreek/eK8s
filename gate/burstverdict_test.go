package gate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// candidateMarker is planted in every test record's Class, Evidence and
// Pair, and is the unknown Reason: no Detail may contain it.
const candidateMarker = "CANDIDATE-TEXT"

var (
	bucketSuccess = classifier.Success.String()
	bucketShed    = classifier.GracefulShed.String()
	bucketFailed  = classifier.Failed.String()
)

// burstRec returns a record with Index i and the given kind, bucket and
// reason, the marker in its Class and Evidence, and on a hash record a
// placeholder Pair holding the marker.
func burstRec(i int, kind, bucket, reason string) wire.Record {
	r := wire.Record{
		Index: i, Kind: kind, Class: candidateMarker, Bucket: bucket, Reason: reason,
		Evidence: &wire.Evidence{RequestBody: []byte(candidateMarker), Status: 200, Body: []byte(candidateMarker)},
	}
	if kind == kindHash {
		r.Pair = &wire.Pair{Password: candidateMarker, Hash: candidateMarker}
	}
	return r
}

// hashRec returns a hash record with Index i, bucket and pair p.
func hashRec(i int, bucket string, p *wire.Pair) wire.Record {
	r := burstRec(i, kindHash, bucket, "")
	r.Pair = p
	return r
}

// burstPairs returns two pairs that verify and one whose hash is of
// another password, each password holding the marker: three Argon2
// hashes.
func burstPairs(t *testing.T, core *hashcore.Core) (good1, good2, mismatch *wire.Pair) {
	t.Helper()
	pair := func(password, of string) *wire.Pair {
		h, err := core.Hash(of)
		if err != nil {
			t.Fatal(err)
		}
		return &wire.Pair{Password: password, Hash: h}
	}
	return pair(candidateMarker+"-1", candidateMarker+"-1"),
		pair(candidateMarker+"-2", candidateMarker+"-2"),
		pair(candidateMarker+"-3", "another password")
}

// verifyCtx bounds a test's Argon2 work, so a hang fails the test.
func verifyCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func checkDetail(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: detail %q, want %q", name, got, want)
	}
	if strings.Contains(got, candidateMarker) {
		t.Errorf("%s: detail %q holds record text", name, got)
	}
}

func TestResponsesDetail(t *testing.T) {
	ok := []wire.Record{
		burstRec(0, kindHash, bucketSuccess, ""),
		burstRec(1, "verify", bucketSuccess, ""),
		burstRec(2, "negative", bucketSuccess, ""),
	}
	// Ten failed records in descending Index order: Detail sorts them
	// and lists the first 8.
	var ten []wire.Record
	for i := 9; i >= 0; i-- {
		reason := string(classifier.ReasonTransportError)
		if i >= 6 {
			reason = string(classifier.ReasonBadStatus)
		}
		ten = append(ten, burstRec(i, "verify", bucketFailed, reason))
	}
	tests := []struct {
		name string
		recs []wire.Record
		want string
	}{
		{"all success", ok, ""},
		{"success and graceful_shed", append(slices.Clone(ok),
			burstRec(3, kindHash, bucketShed, ""), burstRec(4, "negative", bucketShed, "")), ""},
		{"unknown reason", append(slices.Clone(ok), burstRec(3, "verify", bucketFailed, candidateMarker)),
			"burst: 1 of 4 responses failed (other 1); indices 3"},
		{"capped indices", ten,
			"burst: 10 of 10 responses failed (bad_status 4, transport_error 6); indices 0, 1, 2, 3, 4, 5, 6, 7, ..."},
		{"eight indices, no cut", ten[2:],
			"burst: 8 of 8 responses failed (bad_status 2, transport_error 6); indices 0, 1, 2, 3, 4, 5, 6, 7"},
	}
	// One failed record per classifier reason, on a kind that can fail
	// with it: negative_accepted and a negative's bad_status included.
	for _, c := range []struct {
		reason classifier.Reason
		kind   string
	}{
		{classifier.ReasonTransportError, "verify"},
		{classifier.ReasonOverHangTimeout, kindHash},
		{classifier.ReasonBadStatus, "negative"},
		{classifier.ReasonBare503, "verify"},
		{classifier.ReasonBadRetryAfter, kindHash},
		{classifier.ReasonMalformedBody, "verify"},
		{classifier.ReasonWrongMatch, "verify"},
		{classifier.ReasonNonCanonicalPHC, kindHash},
		{classifier.ReasonSaltReused, kindHash},
		{classifier.ReasonVerifyMismatch, kindHash},
		{classifier.ReasonVerifyError, kindHash},
		{classifier.ReasonNegativeAccepted, "negative"},
	} {
		tests = append(tests, struct {
			name string
			recs []wire.Record
			want string
		}{string(c.reason), append(slices.Clone(ok), burstRec(7, c.kind, bucketFailed, string(c.reason))),
			"burst: 1 of 4 responses failed (" + string(c.reason) + " 1); indices 7"})
	}
	for _, tc := range tests {
		checkDetail(t, tc.name, responsesDetail(tc.recs), tc.want)
	}
}

func TestCheckBurstRecords(t *testing.T) {
	ok := []wire.Record{
		burstRec(0, kindHash, bucketSuccess, ""),
		burstRec(1, "verify", bucketSuccess, ""),
		hashRec(2, bucketShed, nil),
		hashRec(3, bucketFailed, nil),
		burstRec(4, "negative", bucketFailed, string(classifier.ReasonNegativeAccepted)),
	}
	if err := checkBurstRecords(ok); err != nil {
		t.Errorf("valid records: %v", err)
	}

	skipped := burstRec(1, "verify", "", "")
	skipped.Skipped = true
	fault := burstRec(1, "verify", "", "")
	fault.Fault = "dial: refused"
	// With a valid Bucket, so only the Skipped and Fault checks can catch them.
	skippedOK := burstRec(1, "verify", bucketSuccess, "")
	skippedOK.Skipped = true
	faultOK := burstRec(1, "verify", bucketSuccess, "")
	faultOK.Fault = "dial: refused"
	pairOnVerify := burstRec(1, "verify", bucketSuccess, "")
	pairOnVerify.Pair = &wire.Pair{Password: candidateMarker, Hash: candidateMarker}
	for name, recs := range map[string][]wire.Record{
		"no records":                nil,
		"skipped":                   {ok[0], skipped},
		"fault":                     {ok[0], fault},
		"skipped with a bucket":     {ok[0], skippedOK},
		"fault with a bucket":       {ok[0], faultOK},
		"unknown bucket":            {ok[0], burstRec(1, "verify", candidateMarker, "")},
		"empty bucket":              {ok[0], burstRec(1, "verify", "", "")},
		"success hash with no pair": {ok[0], hashRec(1, bucketSuccess, nil)},
		"pair on a verify record":   {ok[0], pairOnVerify},
	} {
		if err := checkBurstRecords(recs); err == nil {
			t.Errorf("%s: no error, want a gate fault", name)
		}
	}

	// A 1 KiB Fault is quoted only up to maxQuoted bytes, the cut marked.
	long := burstRec(0, "verify", "", "")
	long.Fault = strings.Repeat("f", 1024)
	err := checkBurstRecords([]wire.Record{long})
	switch {
	case err == nil:
		t.Error("1 KiB fault: no error, want a gate fault")
	case len(err.Error()) > 160, strings.Contains(err.Error(), strings.Repeat("f", maxQuoted+1)),
		!strings.HasSuffix(err.Error(), `"...`):
		t.Errorf("1 KiB fault: error %q, want at most %d bytes of the fault and a marked cut", err, maxQuoted)
	}

	// Exactly 64 bytes are kept whole, 65 are cut: the literal pins maxQuoted.
	for n, wantCut := range map[int]bool{64: false, 65: true} {
		r := burstRec(0, "verify", "", "")
		r.Fault = strings.Repeat("g", n)
		err := checkBurstRecords([]wire.Record{r})
		switch {
		case err == nil:
			t.Errorf("%d-byte fault: no error, want a gate fault", n)
		case strings.HasSuffix(err.Error(), `"...`) != wantCut,
			!strings.Contains(err.Error(), `"`+strings.Repeat("g", 64)+`"`):
			t.Errorf("%d-byte fault: error %q, want 64 bytes quoted, cut %v", n, err, wantCut)
		}
	}
}

func TestHashVerifyDetail(t *testing.T) {
	core := hashcore.New()
	good1, good2, mismatch := burstPairs(t, core)
	truncated := &wire.Pair{Password: good1.Password, Hash: good1.Hash[:len(good1.Hash)/2]}

	tests := []struct {
		name string
		recs []wire.Record
		want string
	}{
		{"all verify", []wire.Record{
			hashRec(0, bucketSuccess, good1),
			burstRec(1, "verify", bucketSuccess, ""),
			hashRec(2, bucketShed, nil),
			hashRec(3, bucketSuccess, good2),
		}, ""},
		// Indices out of order, the mismatch first and the rejected
		// hash last: both are counted and listed ascending.
		{"mismatch and rejected", []wire.Record{
			hashRec(9, bucketSuccess, mismatch),
			hashRec(3, bucketSuccess, good1),
			hashRec(4, bucketSuccess, truncated),
		}, "burst: 2 of 3 hashes failed host verification (mismatch 1, rejected 1); indices 4, 9"},
	}
	for _, tc := range tests {
		got, err := hashVerifyDetail(verifyCtx(t), core, tc.recs, 2)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		checkDetail(t, tc.name, got, tc.want)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	recs := []wire.Record{hashRec(0, bucketSuccess, good1), hashRec(1, bucketSuccess, good2)}
	if got, err := hashVerifyDetail(ctx, core, recs, 2); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: detail %q, error %v, want context.Canceled", got, err)
	}
}

func TestBurstVerdicts(t *testing.T) {
	core := hashcore.New()
	good, _, mismatch := burstPairs(t, core)
	skipped := burstRec(0, "verify", "", "")
	skipped.Skipped = true
	failedRec := burstRec(0, "verify", bucketFailed, "wrong_match")
	pairOnVerify := burstRec(1, "verify", bucketSuccess, "")
	pairOnVerify.Pair = good
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		recs    []wire.Record
		wantOK  bool
		wantErr error // a gate fault: errors.Is target, or errAny
		want    []Result
	}{
		{"both pass", verifyCtx(t), []wire.Record{hashRec(0, bucketSuccess, good), burstRec(1, "verify", bucketSuccess, "")},
			true, nil, []Result{{5, "responses", true, ""}, {5, "hash-verify", true, ""}}},
		// The success hash would verify: hash-verify must not run at all.
		{"responses fails", verifyCtx(t), []wire.Record{hashRec(0, bucketSuccess, good), burstRec(1, "verify", bucketFailed, "wrong_match")},
			false, nil, []Result{{5, "responses", false, "burst: 1 of 2 responses failed (wrong_match 1); indices 1"}}},
		{"hash-verify fails", verifyCtx(t), []wire.Record{hashRec(0, bucketSuccess, mismatch)},
			false, nil, []Result{{5, "responses", true, ""}, {5, "hash-verify", false, "burst: 1 of 1 hashes failed host verification (mismatch 1); indices 0"}}},
		{"cancelled during hash-verify", cancelled, []wire.Record{hashRec(0, bucketSuccess, good)},
			false, context.Canceled, []Result{{5, "responses", true, ""}}},
		{"fault in responses", verifyCtx(t), []wire.Record{skipped},
			false, errAny, nil},
		// A failed "responses" must not mask a fault in a later record.
		{"failed bucket and a missing pair", verifyCtx(t), []wire.Record{failedRec, hashRec(1, bucketSuccess, nil)},
			false, errAny, nil},
		{"failed bucket and a pair on verify", verifyCtx(t), []wire.Record{failedRec, pairOnVerify},
			false, errAny, nil},
	}
	for _, tc := range tests {
		var r Report
		ok, err := burstVerdicts(tc.ctx, &r, core, tc.recs, 2)
		switch {
		case tc.wantErr == nil && err != nil:
			t.Errorf("%s: error %v", tc.name, err)
		case tc.wantErr == errAny && err == nil, tc.wantErr != nil && tc.wantErr != errAny && !errors.Is(err, tc.wantErr):
			t.Errorf("%s: error %v, want %v", tc.name, err, tc.wantErr)
		}
		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.wantOK)
		}
		if !slices.Equal(r.Results, tc.want) {
			t.Errorf("%s: Results %+v, want %+v", tc.name, r.Results, tc.want)
		}
		for _, res := range r.Results {
			if strings.Contains(res.Detail, candidateMarker) {
				t.Errorf("%s: %s detail %q holds record text", tc.name, res.Check, res.Detail)
			}
		}
	}
}

// errAny is a wantErr that accepts any non-nil error.
var errAny = errors.New("any error")
