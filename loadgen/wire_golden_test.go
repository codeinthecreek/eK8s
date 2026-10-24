package loadgen

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// TestWireGolden pins every control API wire type's JSON byte for byte:
// the wire shape is the orchestrator's contract (docs/decisions.md, PR
// #44). Each case's file, testdata/wire/<name>.json, holds writeJSON's
// body for the value: json.Marshal's output (HTML escaping on, no
// indent) and a newline. The server never encodes the request bodies
// (wire.Upload, wire.UploadPair, wire.Start); their files pin what a client
// marshalling the type sends. Each file is also decoded the way
// decodeBody decodes a request body, unknown fields refused, and must
// encode back to the same bytes. The files are written by hand from the
// encoder's output; there is no -update flag.
func TestWireGolden(t *testing.T) {
	const boot = "00112233445566778899aabbccddeeff"
	wall := time.Date(2026, 10, 18, 2, 4, 5, 123456789, time.UTC)
	pair := wire.Pair{Password: "PW", Hash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$a2V5"}
	evidence := wire.Evidence{RequestBody: []byte(`{"password":"PW"}`), Status: 200, Body: []byte("x\x00")}
	record := wire.Record{
		Seq: 4, Target: "12", RunID: "b1", Index: 3, Kind: "negative", Class: "invalid_json",
		IntendedWall: wall, IntendedMonoNS: 1500000000, ActualMonoNS: 1501000000, LatencyNS: 250000000,
		Bucket: "failed", Reason: "malformed_body", Skipped: true, Fault: "loadgen: f",
		Pair: &pair, Evidence: &evidence,
	}
	p0, h0, m0 := "p0", "h0", true
	p1, h1, m1 := "p1", "h1", false
	runTarget := wire.RunTarget{State: wire.StateError, Error: "loadgen: boom"}

	for _, tc := range []struct {
		name string
		v    any
	}{
		{"error_full", wire.Error{BootID: boot, Error: `loadgen: <x> & "y"`}},
		{"boot_id_full", wire.BootID{BootID: boot}},
		{"config_full", wire.Config{
			BootID: boot, Rate: 2.5, WindowS: 300, BurstRate: 10, BurstSends: 300,
			WindowSends: 750, WindowPairs: 360, BurstPairs: 140, RingCapacity: 1050,
		}},
		{"target_full", wire.Target{ID: "12", Next: 34}},
		{"targets_full", wire.Targets{BootID: boot, Targets: []wire.Target{{ID: "9", Next: 1}, {ID: "10", Next: 18446744073709551615}}}},
		{"put_target_full", wire.PutTarget{BootID: boot, ID: "7", Next: 2}},
		{"records_full", wire.Records{BootID: boot, Records: []wire.Record{record}, Gap: true, Next: 5}},
		{"record_full", record},
		{"record_zero", wire.Record{}},
		{"pair_full", pair},
		{"evidence_full", evidence},
		{"canary_full", wire.Canary{BootID: boot, Result: wire.CanaryRefused}},
		{"upload_full", wire.Upload{Pairs: []wire.UploadPair{
			{Password: &p0, Hash: &h0, Match: &m0},
			{Password: &p1, Hash: &h1, Match: &m1},
		}}},
		{"upload_pair_full", wire.UploadPair{Password: &p0, Hash: &h0, Match: &m0}},
		{"upload_pair_zero", wire.UploadPair{}},
		{"uploaded_full", wire.Uploaded{BootID: boot, Count: 140}},
		{"start_full", wire.Start{Kind: "window", Targets: []string{"1", "12"}}},
		{"started_full", wire.Started{BootID: boot, RunID: "b1", StartWall: wall}},
		{"run_full", wire.Run{
			BootID: boot, RunID: "w1", Kind: "window", StartWall: wall,
			Targets: map[string]wire.RunTarget{"1": {State: wire.StateDone}, "12": runTarget},
		}},
		{"run_target_full", runTarget},
		{"run_target_zero", wire.RunTarget{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", "wire", tc.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if got := wireBody(t, tc.v); !bytes.Equal(got, want) {
				t.Errorf("encoded:\n got %s\nwant %s", got, want)
			}
			p := reflect.New(reflect.TypeOf(tc.v))
			req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(want))
			if err := decodeBody(httptest.NewRecorder(), req, maxStartBody, p.Interface()); err != nil {
				t.Fatalf("decoding the golden file: %v", err)
			}
			if got := wireBody(t, p.Elem().Interface()); !bytes.Equal(got, want) {
				t.Errorf("decoded and encoded again:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// wireBody returns the body writeJSON answers with for v.
func wireBody(t *testing.T, v any) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	(&Control{}).writeJSON(rec, http.StatusOK, v)
	if rec.Code != http.StatusOK {
		t.Fatalf("writeJSON: status %d, body %s", rec.Code, rec.Body)
	}
	return rec.Body.Bytes()
}
