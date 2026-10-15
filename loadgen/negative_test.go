package loadgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// maxBodyBytes is cmd/service's request-body limit (cmd/service/main.go,
// maxBodyBytes = 4 << 10); every negative must fit inside it.
const maxBodyBytes = 4 << 10

// why400 decodes body as the HTTP contract does (DESIGN.md sec 8, and
// internal/wrapper's decode: one JSON object, nothing after it but
// whitespace) and returns the first reason it must get a 400, or
// "accepted" if the body is a valid request.
func why400(path string, body []byte) string {
	var req struct {
		Password *string `json:"password"`
		Hash     *string `json:"hash"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return "wrong_type"
		}
		return "syntax"
	}
	if _, err := dec.Token(); err != io.EOF {
		return "trailing"
	}
	switch {
	case req.Password == nil:
		return "password_missing"
	case *req.Password == "":
		return "password_empty"
	case len(*req.Password) > maxPasswordLen:
		return "password_too_long"
	case path == VerifyPath && (req.Hash == nil || *req.Hash == ""):
		return "hash_missing"
	}
	return "accepted"
}

// TestNegativeBodies decodes every form of every class. The three
// 400-by-body classes must fail decoding for the reason their form names;
// the two hash classes must decode as valid /verify requests, so that
// only the hash is wrong (hashcore_test.go checks the hash).
func TestNegativeBodies(t *testing.T) {
	want := map[NegativeClass]map[string]string{
		EmptyPassword: {
			"hash_empty":     "password_empty",
			"hash_missing":   "password_missing",
			"verify_empty":   "password_empty",
			"verify_missing": "password_missing",
		},
		PasswordTooLong: {
			"hash":   "password_too_long",
			"verify": "password_too_long",
		},
		InvalidJSON: {
			"hash_unterminated":          "syntax",
			"verify_unterminated":        "syntax",
			"hash_wrong_type":            "wrong_type",
			"verify_wrong_type_password": "wrong_type",
			"verify_wrong_type_hash":     "wrong_type",
			"hash_trailing":              "trailing",
			"verify_trailing":            "trailing",
		},
		MalformedHash: {
			"not_phc":              "accepted",
			"truncated":            "accepted",
			"noncanonical_bits":    "accepted",
			"noncanonical_newline": "accepted",
			"noncanonical_padded":  "accepted",
		},
		NonExactParams: {
			"t1":           "accepted",
			"t3":           "accepted",
			"m19455":       "accepted",
			"m19457":       "accepted",
			"p2":           "accepted",
			"m_max_uint32": "accepted",
			"argon2i":      "accepted",
			"v16":          "accepted",
		},
	}
	for _, c := range negativeClasses {
		var names []string
		for _, f := range negativeForms[c] {
			names = append(names, f.name)
			reason, ok := want[c][f.name]
			if !ok {
				t.Errorf("%s/%s: form not in this test's table", c, f.name)
				continue
			}
			wantPath := VerifyPath
			if strings.HasPrefix(f.name, "hash") && c != MalformedHash {
				wantPath = HashPath
			}
			passwords, hashes := map[string]bool{}, map[string]bool{}
			for range 50 {
				r := f.build()
				// Fresh parts: a repeated password, salt or key would make
				// negatives recognisable in live traffic. Truncated hashes
				// are exempt: short cuts of different hashes coincide.
				if r.Password != "" {
					if passwords[r.Password] {
						t.Fatalf("%s/%s: password %.40q repeated", c, f.name, r.Password)
					}
					passwords[r.Password] = true
				}
				if (c == MalformedHash || c == NonExactParams) && f.name != "truncated" {
					var body struct{ Hash string }
					if err := json.Unmarshal(r.Body, &body); err != nil || hashes[body.Hash] {
						t.Fatalf("%s/%s: hash %q repeated (decode error %v)", c, f.name, body.Hash, err)
					}
					hashes[body.Hash] = true
				}
				if r.Path != wantPath || r.Kind != KindNegative || r.Class != c {
					t.Fatalf("%s/%s: path %s, kind %v, class %s; want %s, negative, %s", c, f.name, r.Path, r.Kind, r.Class, wantPath, c)
				}
				if len(r.Body) > maxBodyBytes {
					t.Fatalf("%s/%s: body is %d bytes, over the %d-byte limit", c, f.name, len(r.Body), maxBodyBytes)
				}
				if got := why400(r.Path, r.Body); got != reason {
					t.Fatalf("%s/%s: body %.200q decodes as %s, want %s", c, f.name, r.Body, got, reason)
				}
			}
		}
		for name := range want[c] {
			if !slices.Contains(names, name) {
				t.Errorf("%s: no form %s", c, name)
			}
		}
	}
}

// TestLongestNegativeBody checks the largest body any negative can have,
// a /verify with a maxTooLongLen password, against the body limit.
func TestLongestNegativeBody(t *testing.T) {
	if n := len(verifyBody(passwordOfLen(maxTooLongLen), randomExactPHC())); n > maxBodyBytes {
		t.Errorf("longest negative body is %d bytes, over the %d-byte limit", n, maxBodyBytes)
	}
}

// TestNewNegative checks that each class draws a form of that class.
func TestNewNegative(t *testing.T) {
	for _, c := range negativeClasses {
		for range 20 {
			if r := newNegative(c); r.Class != c || r.Kind != KindNegative {
				t.Fatalf("newNegative(%s) = class %s, kind %v", c, r.Class, r.Kind)
			}
		}
	}
}

// TestTooLongLen checks the over-1024 length bounds, both ends reachable.
func TestTooLongLen(t *testing.T) {
	seenMin, seenMax := false, false
	for range 100000 {
		n := tooLongLen()
		if n <= maxPasswordLen || n > maxTooLongLen {
			t.Fatalf("too-long length %d, want %d to %d", n, maxPasswordLen+1, maxTooLongLen)
		}
		seenMin = seenMin || n == maxPasswordLen+1
		seenMax = seenMax || n == maxTooLongLen
	}
	if !seenMin || !seenMax {
		t.Errorf("lengths %d seen: %v, %d seen: %v", maxPasswordLen+1, seenMin, maxTooLongLen, seenMax)
	}
}
