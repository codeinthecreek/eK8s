package loadgen

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	for _, tc := range wireCases() {
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

// TestWireGoldenFull checks that every _full case sets every field, so
// that a field added to a wire type, omitempty or not, shows in that
// case's golden file and fails TestWireGolden until the file is
// updated. A nil pointer is unset; a non-nil pointer to a non-struct is
// set, and one to a struct is checked as that struct. A slice, array or
// map must be non-empty, and each field of its element type set in at
// least one element. time.Time is set when not IsZero; any other struct
// is checked field by field; any other value is set when not zero.
func TestWireGoldenFull(t *testing.T) {
	for _, tc := range wireCases() {
		if !strings.HasSuffix(tc.name, "_full") {
			continue
		}
		if u := unsetFields(reflect.ValueOf(tc.v), tc.name); len(u) > 0 {
			t.Errorf("%s is unset", u[0])
		}
	}
}

// unsetFields returns the paths of v's unset fields, in field order, as
// TestWireGoldenFull defines unset. A collection's element fields are
// under path[], and unset only when unset in every element.
func unsetFields(v reflect.Value, path string) []string {
	if v.Type() == reflect.TypeFor[time.Time]() {
		if v.Interface().(time.Time).IsZero() {
			return []string{path}
		}
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return []string{path}
		}
		if e := v.Elem(); e.Kind() == reflect.Struct {
			return unsetFields(e, path)
		}
		return nil
	case reflect.Struct:
		var u []string
		for i := range v.NumField() {
			u = append(u, unsetFields(v.Field(i), path+"."+v.Type().Field(i).Name)...)
		}
		return u
	case reflect.Slice, reflect.Array, reflect.Map:
		if v.Len() == 0 {
			return []string{path}
		}
		var elems []reflect.Value
		if v.Kind() == reflect.Map {
			for it := v.MapRange(); it.Next(); {
				elems = append(elems, it.Value())
			}
		} else {
			for i := range v.Len() {
				elems = append(elems, v.Index(i))
			}
		}
		// Unset in every element: keep the first element's paths that
		// each later element also leaves unset.
		u := unsetFields(elems[0], path+"[]")
		for _, e := range elems[1:] {
			also := make(map[string]bool)
			for _, p := range unsetFields(e, path+"[]") {
				also[p] = true
			}
			var both []string
			for _, p := range u {
				if also[p] {
					both = append(both, p)
				}
			}
			u = both
		}
		return u
	}
	if v.IsZero() {
		return []string{path}
	}
	return nil
}

// wireCases returns TestWireGolden's cases: each is a value and the
// name of its golden file, testdata/wire/<name>.json.
func wireCases() []struct {
	name string
	v    any
} {
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

	return []struct {
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
