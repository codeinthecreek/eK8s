package orchestrator

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/hostpairs"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustHash(t *testing.T, core *hashcore.Core, pw string) string {
	t.Helper()
	h, err := core.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// fakePHC is a canonical PHC string at the fixed parameters with the
// given salt and key bytes repeated: it parses, but is no hash of
// anything.
func fakePHC(salt, key byte) string {
	b64 := base64.RawStdEncoding.EncodeToString
	return "$argon2id$v=19$m=19456,t=2,p=1$" + b64([]byte(strings.Repeat(string(salt), 16))) + "$" + b64([]byte(strings.Repeat(string(key), 32)))
}

func TestReadRequest(t *testing.T) {
	for _, tc := range []struct {
		body string
		pw   string
		ok   bool
	}{
		{`{"password":"p"}`, "p", true},
		{` {"password":"p"} ` + "\n", "p", true},
		{`{"password":"\u0070"}`, "p", true},
		// Read as the gen-0 wrapper reads it: unknown members ignored,
		// the last of a repeated member wins, names matched as
		// encoding/json matches them.
		{`{"password":"p","hash":7}`, "p", true},
		{`{"password":"q","password":"p"}`, "p", true},
		{`{"PASSWORD":"p"}`, "p", true},
		{`{}`, "", false},
		{`{"password":null}`, "", false},
		{`{"password":""}`, "", false},
		{`{"password":"p"`, "", false},
		{`{"password":"p"}x`, "", false},
		{`{"password":"p"}{}`, "", false},
		{`{"password":5}`, "", false},
		{`["p"]`, "", false},
		{``, "", false},
	} {
		if pw, ok := hashRequest([]byte(tc.body)); pw != tc.pw || ok != tc.ok {
			t.Errorf("hashRequest(%q) = %q, %v; want %q, %v", tc.body, pw, ok, tc.pw, tc.ok)
		}
	}
}

// TestRejected checks the host's contract check on each negative form
// loadgen builds (loadgen/negative.go), and that a request the contract
// accepts is never taken for a negative, whatever its class.
func TestRejected(t *testing.T) {
	core := hashcore.New()
	good := mustHash(t, core, "pw")
	long := strings.Repeat("a", 1025)
	verify := func(pw, hash string) []byte { return mustJSON(t, map[string]string{"password": pw, "hash": hash}) }
	hash := func(pw string) []byte { return mustJSON(t, map[string]string{"password": pw}) }
	for _, tc := range []struct {
		class string
		body  []byte
		want  bool
	}{
		{classMalformedHash, verify("pw", "not a phc"), true},
		{classMalformedHash, verify("pw", good[:20]), true},
		{classMalformedHash, verify("pw", good+"\n"), true},
		{classNonExactParams, verify("pw", strings.Replace(good, "t=2", "t=1", 1)), true},
		{classNonExactParams, verify("pw", strings.Replace(good, "argon2id", "argon2i", 1)), true},
		{classEmptyPassword, hash(""), true},
		{classEmptyPassword, []byte(`{}`), true},
		{classEmptyPassword, verify("", good), true},
		{classEmptyPassword, mustJSON(t, map[string]string{"hash": good}), true},
		{classTooLong, hash(long), true},
		{classTooLong, verify(long, good), true},
		{classInvalidJSON, []byte(`{"password":"pw"`), true},
		{classInvalidJSON, []byte(`{"password":"pw"}x`), true},
		{classInvalidJSON, []byte(`{"password":7}`), true},
		{classInvalidJSON, mustJSON(t, map[string]any{"password": 7, "hash": good}), true},

		// The contract accepts these, so a 2xx to one is no failure.
		{classMalformedHash, verify("pw", good), false},
		{classNonExactParams, verify("pw", good), false},
		{classEmptyPassword, hash("pw"), false},
		{classTooLong, hash(strings.Repeat("a", 1024)), false},
		{classInvalidJSON, verify("pw", good), false},
		{classInvalidJSON, hash("pw"), false},
		// verify_wrong_type_hash is read as a valid /hash request (the
		// hash member is ignored), so it never confirms on its own.
		{classInvalidJSON, mustJSON(t, map[string]any{"password": "pw", "hash": true}), false},
		{"unknown_class", []byte(`{`), false},
	} {
		if got := rejected(core, tc.class, tc.body); got != tc.want {
			t.Errorf("rejected(%s, %.60q) = %v, want %v", tc.class, tc.body, got, tc.want)
		}
	}
}

// claim is an integrity record of kind with evidence.
func claim(kind, class, reason string, req []byte, status int, body []byte) wire.Record {
	return wire.Record{Kind: kind, Class: class, Bucket: "failed", Reason: reason,
		Evidence: &wire.Evidence{RequestBody: req, Status: status, Body: body}}
}

func TestConfirm(t *testing.T) {
	core := hashcore.New()
	hashOf := mustHash(t, core, "pw")
	other := mustHash(t, core, "other")
	hashReq := mustJSON(t, map[string]string{"password": "pw"})
	resp := func(k string, v any) []byte { return mustJSON(t, map[string]any{k: v}) }
	truth := map[pairKey]bool{{"pw", hashOf}: true, {"pw", other}: false}
	verifyReq := func(h string) []byte { return mustJSON(t, map[string]string{"password": "pw", "hash": h}) }

	for _, tc := range []struct {
		name string
		rec  wire.Record
		seed []string // hashes already in the variant's history
		want classifier.Reason
	}{
		{"hash non-canonical", claim(kindHash, "", "non_canonical_phc", hashReq, 200, resp("hash", hashOf+"\n")), nil, classifier.ReasonNonCanonicalPHC},
		{"hash malformed body", claim(kindHash, "", "malformed_body", hashReq, 200, []byte(`{"hash":"x","extra":1}`)), nil, classifier.ReasonMalformedBody},
		{"hash salt seen", claim(kindHash, "", "salt_reused", hashReq, 200, resp("hash", hashOf)), []string{hashOf}, classifier.ReasonSaltReused},
		{"hash salt seen, late 201", claim(kindHash, "", "salt_reused", hashReq, 201, resp("hash", hashOf)), []string{hashOf}, classifier.ReasonSaltReused},
		{"hash salt new, verifies", claim(kindHash, "", "salt_reused", hashReq, 200, resp("hash", hashOf)), nil, classifier.ReasonNone},
		{"hash salt new, wrong key", claim(kindHash, "", "salt_reused", hashReq, 200, resp("hash", fakePHC(1, 2))), nil, classifier.ReasonVerifyMismatch},
		{"hash of another password", claim(kindHash, "", "salt_reused", hashReq, 200, resp("hash", other)), nil, classifier.ReasonVerifyMismatch},
		{"hash request unreadable", claim(kindHash, "", "non_canonical_phc", []byte(`{"password":`), 200, resp("hash", hashOf+"\n")), nil, classifier.ReasonNone},
		{"hash request no password", claim(kindHash, "", "non_canonical_phc", []byte(`{}`), 200, resp("hash", hashOf+"\n")), nil, classifier.ReasonNone},

		{"verify wrong match", claim(kindVerify, "", "wrong_match", verifyReq(other), 200, resp("match", true)), nil, classifier.ReasonWrongMatch},
		{"verify wrong reject", claim(kindVerify, "", "wrong_match", verifyReq(hashOf), 200, resp("match", false)), nil, classifier.ReasonWrongMatch},
		{"verify malformed", claim(kindVerify, "", "malformed_body", verifyReq(hashOf), 200, []byte(`{"match":"yes"}`)), nil, classifier.ReasonMalformedBody},
		{"verify right", claim(kindVerify, "", "wrong_match", verifyReq(hashOf), 200, resp("match", true)), nil, classifier.ReasonNone},
		{"verify pair not the host's", claim(kindVerify, "", "wrong_match", verifyReq(fakePHC(3, 4)), 200, resp("match", true)), nil, classifier.ReasonNone},

		{"negative accepted", claim(kindNegative, classEmptyPassword, "negative_accepted", []byte(`{}`), 200, nil), nil, classifier.ReasonNegativeAccepted},
		{"negative accepted late 204", claim(kindNegative, classMalformedHash, "negative_accepted", verifyReq("bad"), 204, nil), nil, classifier.ReasonNegativeAccepted},
		{"negative not a negative", claim(kindNegative, classEmptyPassword, "negative_accepted", hashReq, 200, nil), nil, classifier.ReasonNone},
		{"negative 400", claim(kindNegative, classEmptyPassword, "negative_accepted", []byte(`{}`), 400, nil), nil, classifier.ReasonNone},

		{"unknown kind", claim("other", "", "malformed_body", hashReq, 200, []byte(`x`)), nil, classifier.ReasonNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHistory()
			for _, s := range tc.seed {
				if r := h.add(s); r != classifier.ReasonNone {
					t.Fatalf("seeding: %s", r)
				}
			}
			if got := confirm(core, h, truth, tc.rec); got != tc.want {
				t.Errorf("confirm = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJudgeIntegrity checks the host's pass over one variant's records:
// pairs that fail full verification or repeat a salt are found by the
// host itself, the earliest index wins, and claims are counted with or
// without evidence.
func TestJudgeIntegrity(t *testing.T) {
	core := hashcore.New()
	good := mustHash(t, core, "pw")
	pairRec := func(index int, pw, hash string) wire.Record {
		return wire.Record{Index: index, Kind: kindHash, Bucket: "success", Pair: &wire.Pair{Password: pw, Hash: hash}}
	}
	run := func(h *history, recs []wire.Record) verdict {
		t.Helper()
		res, err := hostpairs.Verify(t.Context(), core, windowItems(recs), 0)
		if err != nil {
			t.Fatal(err)
		}
		return judgeIntegrity(core, h, nil, recs, res)
	}

	if v := run(newHistory(), []wire.Record{pairRec(0, "pw", good), {Index: 1, Kind: kindVerify, Bucket: "success"}}); v.Reason != "" || v.Index != -1 || v.Claims != 0 {
		t.Errorf("clean records: %+v", v)
	}
	// A wrong key at index 5 and a replayed salt at index 3: index 3 wins.
	recs := []wire.Record{pairRec(5, "pw", fakePHC(9, 9)), pairRec(1, "pw", good), pairRec(3, "pw", good)}
	if v := run(newHistory(), recs); v.Reason != classifier.ReasonSaltReused || v.Index != 3 {
		t.Errorf("reused salt and a wrong key: %+v, want salt_reused at 3", v)
	}
	if v := run(newHistory(), recs[:1]); v.Reason != classifier.ReasonVerifyMismatch || v.Index != 5 {
		t.Errorf("wrong key: %+v, want verify_mismatch at 5", v)
	}
	// A salt seen in an earlier window or the burst is caught too.
	h := newHistory()
	h.add(good)
	if v := run(h, recs[1:2]); v.Reason != classifier.ReasonSaltReused || v.Index != 1 {
		t.Errorf("salt from history: %+v, want salt_reused at 1", v)
	}
	// Claims the host does not confirm are counted, and so are claims
	// without evidence.
	unconfirmed := claim(kindHash, "", "salt_reused", mustJSON(t, map[string]string{"password": "pw"}), 200, mustJSON(t, map[string]string{"hash": mustHash(t, core, "pw")}))
	unconfirmed.Index = 2
	bare := wire.Record{Index: 4, Kind: kindVerify, Bucket: "failed", Reason: "wrong_match"}
	if v := run(newHistory(), []wire.Record{unconfirmed, bare}); v.Reason != "" || v.Claims != 2 || v.Unconfirmed != 1 {
		t.Errorf("unconfirmed claims: %+v, want none confirmed, 2 claims, 1 unconfirmed", v)
	}
	// A non-integrity failure is no claim.
	if v := run(newHistory(), []wire.Record{{Kind: kindHash, Bucket: "failed", Reason: "over_hang_timeout"}}); v.Claims != 0 {
		t.Errorf("an over_hang_timeout counted as a claim: %+v", v)
	}
}
