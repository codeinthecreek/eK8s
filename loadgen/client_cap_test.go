package loadgen

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// TestClientRecordsCap checks that loadgen/client reads a maximal
// records page, as writeJSON encodes it: maxRecordsPage records, each
// with every field at once and each field at least as long as loadgen
// makes it. Both evidence byte fields hold classifier.MaxBodyBytes+1
// bytes, the response body read's cap; no request body loadgen builds
// is longer, which the first check pins.
func TestClientRecordsCap(t *testing.T) {
	// The longest request body: a /verify with the longest too-long
	// password and a hash longer than any form's.
	if n := len(verifyBody(strings.Repeat("A", maxTooLongLen), strings.Repeat("A", 128))); n > classifier.MaxBodyBytes+1 {
		t.Fatalf("a /verify body can be %d bytes, over the %d the client's cap assumes", n, classifier.MaxBodyBytes+1)
	}
	const boot = "00112233445566778899aabbccddeeff"
	evidence := make([]byte, classifier.MaxBodyBytes+1)
	for i := range evidence {
		evidence[i] = 0xff
	}
	rec := wire.Record{
		Seq:            math.MaxUint64,
		Target:         strings.Repeat("9", 18),
		RunID:          strings.Repeat("a", 64),
		Index:          maxRecords,
		Kind:           KindNegative.String(),
		Class:          strings.Repeat("c", 64), // longer than any class
		IntendedWall:   time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		IntendedMonoNS: math.MinInt64,
		ActualMonoNS:   math.MinInt64,
		LatencyNS:      math.MinInt64,
		Bucket:         strings.Repeat("b", 64), // longer than any bucket
		Reason:         strings.Repeat("r", 64), // longer than any reason
		Skipped:        true,
		Fault:          strings.Repeat("f", 4096), // longer than any fault: one error's message
		Pair:           &wire.Pair{Password: strings.Repeat("A", maxPasswordLen), Hash: randomExactPHC()},
		Evidence:       &wire.Evidence{RequestBody: evidence, Status: 599, Body: evidence},
	}
	recs := make([]wire.Record, maxRecordsPage)
	for i := range recs {
		recs[i] = rec
	}
	page := wire.Records{BootID: boot, Records: recs, Gap: true, Next: math.MaxUint64}
	n := len(wireBody(t, page))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.BootIDHeader, boot)
		(&Control{bootID: boot}).writeJSON(w, http.StatusOK, page)
	}))
	t.Cleanup(srv.Close)
	got, err := client.New(srv.URL, srv.Client()).Records(t.Context(), "1", 0, maxRecordsPage)
	if err != nil {
		t.Fatalf("Records on a maximal page of %d bytes: %v", n, err)
	}
	if len(got.Records) != maxRecordsPage {
		t.Fatalf("Records returned %d records, want %d", len(got.Records), maxRecordsPage)
	}
	t.Logf("a maximal page is %d bytes", n)
}
