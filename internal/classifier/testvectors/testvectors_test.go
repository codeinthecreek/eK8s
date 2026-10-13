package testvectors

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
)

const (
	wantPasswords = 256
	minLen        = 1
	maxLen        = 1024
)

// TestPoolShape checks the pool's counts and structure without running
// Argon2.
func TestPoolShape(t *testing.T) {
	pairs := Pairs()
	if len(pairs) != 2*wantPasswords {
		t.Fatalf("got %d pairs, want %d", len(pairs), 2*wantPasswords)
	}

	type seen struct{ match, mismatch int }
	byPassword := make(map[string]*seen)
	ownHash := make(map[string]string) // password -> its Match-true hash
	for _, p := range pairs {
		s := byPassword[p.Password]
		if s == nil {
			s = &seen{}
			byPassword[p.Password] = s
		}
		if p.Match {
			s.match++
			ownHash[p.Password] = p.Hash
		} else {
			s.mismatch++
		}
	}
	if len(byPassword) != wantPasswords {
		t.Fatalf("got %d distinct passwords, want %d", len(byPassword), wantPasswords)
	}

	minSeen, maxSeen := maxLen+1, 0
	for pw, s := range byPassword {
		if s.match != 1 || s.mismatch != 1 {
			t.Errorf("password %.20q...: %d match and %d mismatch pairs, want 1 each", pw, s.match, s.mismatch)
		}
		if !utf8.ValidString(pw) {
			t.Errorf("password %.20q... is not valid UTF-8", pw)
		}
		n := len(pw) // bytes, not runes
		if n < minLen || n > maxLen {
			t.Errorf("password length %d bytes, want %d..%d", n, minLen, maxLen)
		}
		minSeen, maxSeen = min(minSeen, n), max(maxSeen, n)
	}
	if minSeen != minLen || maxSeen != maxLen {
		t.Errorf("password lengths span %d..%d bytes, want exactly %d..%d", minSeen, maxSeen, minLen, maxLen)
	}

	// A mismatch pair uses another pool password's hash, and every pool
	// hash has its own salt.
	hashes := make(map[string]bool)
	salts := make(map[string]bool)
	for _, h := range ownHash {
		hashes[h] = true
		salt := strings.Split(h, "$")[4]
		if salts[salt] {
			t.Errorf("salt %s appears in more than one pool hash", salt)
		}
		salts[salt] = true
	}
	for _, p := range pairs {
		if !p.Match && (!hashes[p.Hash] || ownHash[p.Password] == p.Hash) {
			t.Errorf("mismatch pair for %.20q...: hash is not another pool password's", p.Password)
		}
	}
}

// TestPoolMultiByte checks the pool exercises multi-byte UTF-8: gen
// guarantees numMultiByte passwords with a non-ASCII byte.
func TestPoolMultiByte(t *testing.T) {
	const wantMultiByte = 4 // gen's numMultiByte
	got := 0
	for _, p := range Pairs() {
		if p.Match && strings.IndexFunc(p.Password, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
			got++
		}
	}
	if got < wantMultiByte {
		t.Errorf("%d pool passwords contain a non-ASCII byte, want at least %d", got, wantMultiByte)
	}
}

// TestPoolVerify runs every pair through the fixed core. Not skipped
// under -short: the pool is the gate's ground truth.
// Skipped under -race only.
func TestPoolVerify(t *testing.T) {
	if raceEnabled {
		t.Skip("Argon2-heavy, no shared mutable state to race on; the plain go test step runs it on every CI run")
	}
	core := hashcore.New()
	for i, p := range Pairs() {
		t.Run(fmt.Sprintf("pair%03d", i), func(t *testing.T) {
			t.Parallel()
			match, err := core.Verify(p.Password, p.Hash)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if match != p.Match {
				t.Errorf("Verify = %v, want %v", match, p.Match)
			}
		})
	}
}

var wantNegatives = map[string]bool{ // name -> constructed
	"t=1": false, "t=3": false, "m=19455": false, "m=19457": false, "p=2": false,
	"m=4194304": true, "m=4294967295": true,
}

func TestNegatives(t *testing.T) {
	core := hashcore.New()
	negs := Negatives()
	if len(negs) != len(wantNegatives) {
		t.Errorf("got %d negatives, want %d", len(negs), len(wantNegatives))
	}
	for _, n := range negs {
		constructed, ok := wantNegatives[n.Name]
		if !ok {
			t.Errorf("unexpected negative %q", n.Name)
			continue
		}
		if n.Constructed != constructed {
			t.Errorf("%s: Constructed = %v, want %v", n.Name, n.Constructed, constructed)
		}
		segs := strings.Split(n.Hash, "$")
		if len(segs) != 6 || !strings.Contains(segs[3], n.Name) {
			t.Errorf("%s: params segment of %q does not carry %s", n.Name, n.Hash, n.Name)
		}
		if _, err := core.Verify("password", n.Hash); !errors.Is(err, hashcore.ErrUnsupportedParams) {
			t.Errorf("%s: Verify error = %v, want ErrUnsupportedParams", n.Name, err)
		}
		if n.Constructed {
			// With the params segment restored, the core must accept the
			// string as canonical: only the parameter under test differs.
			segs[3] = Params
			if _, err := core.Verify("password", strings.Join(segs, "$")); err != nil {
				t.Errorf("%s: canonical form rejected: %v", n.Name, err)
			}
		}
	}
}

// TestAgreement checks that the classifier's canonical-PHC check and
// hashcore's parameter check agree: hashcore Verify returns no error if
// and only if a hash response carrying the string classifies Success.
// Skipped under -race only.
func TestAgreement(t *testing.T) {
	if raceEnabled {
		t.Skip("Argon2-heavy, no shared mutable state to race on; the plain go test step runs it on every CI run")
	}
	type tc struct{ name, password, hash string }
	var cases []tc

	pairs := Pairs()
	for i, p := range pairs {
		if p.Match { // each pool hash once
			cases = append(cases, tc{fmt.Sprintf("pair%03d", i), p.Password, p.Hash})
		}
	}
	for _, n := range Negatives() {
		cases = append(cases, tc{"negative " + n.Name, "password", n.Hash})
	}

	base := pairs[0]
	segs := strings.Split(base.Hash, "$")
	with := func(i int, v string) string {
		s := append([]string(nil), segs...)
		s[i] = v
		return strings.Join(s, "$")
	}
	salt := segs[4]
	// 16 bytes leave 4 unused bits in the last base64 character; the
	// canonical encoding has them zero, so the next alphabet character
	// sets one.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	last := strings.IndexByte(alphabet, salt[len(salt)-1])
	trailing := salt[:len(salt)-1] + string(alphabet[last+1])
	if b, err := base64.RawStdEncoding.DecodeString(trailing); err != nil || len(b) != 16 {
		t.Fatalf("trailing-bits salt %q does not decode loosely to 16 bytes", trailing)
	}
	edges := []tc{
		{"m=019456", base.Password, with(3, "m=019456,t=2,p=1")},
		{"padded salt", base.Password, with(4, salt+"==")},
		{"non-zero trailing bits", base.Password, with(4, trailing)},
		{"embedded newline", base.Password, with(4, salt[:10]+"\n"+salt[10:])},
		{"argon2i", base.Password, with(1, "argon2i")},
		{"v=16", base.Password, with(2, "v=16")},
	}
	cases = append(cases, edges...)

	core := hashcore.New()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := core.Verify(c.password, c.hash)
			body, jerr := json.Marshal(map[string]string{"hash": c.hash})
			if jerr != nil {
				t.Fatal(jerr)
			}
			bucket, reason := classifier.NewRun(nil).Classify(classifier.Observation{
				Kind: classifier.Hash, Password: c.password,
				Status: http.StatusOK, Header: http.Header{}, Body: body,
			})
			if (err == nil) != (bucket == classifier.Success) {
				t.Errorf("hashcore Verify error = %v, but classifier = %v (%s)", err, bucket, reason)
			}
		})
	}
	// The edge cases exist to be rejected; a case both sides accept
	// would test nothing.
	for _, c := range edges {
		if _, err := core.Verify(c.password, c.hash); err == nil {
			t.Errorf("edge case %s: hashcore accepts it", c.name)
		}
	}
}

// TestParse checks the loader rejects files it must not accept.
func TestParse(t *testing.T) {
	ok := `{"version":1,"params":"m=19456,t=2,p=1","pairs":[{"password":"a","hash":"h","match":true}],"negatives":[{"name":"t=1","hash":"h","constructed":false}]}`
	if _, err := parse([]byte(ok)); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"version":       strings.Replace(ok, `"version":1`, `"version":2`, 1),
		"params":        strings.Replace(ok, `t=2`, `t=1`, 1),
		"unknown field": strings.Replace(ok, `"match":true`, `"match":true,"x":1`, 1),
		"trailing data": ok + "{}",
		"no pairs":      strings.Replace(ok, `[{"password":"a","hash":"h","match":true}]`, `[]`, 1),
		"not JSON":      "{",
	} {
		if _, err := parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
