package loadgen

import (
	"encoding/base64"
	mrand "math/rand/v2"
	"strings"
)

// NegativeClass is one of the five contract-negative classes (DESIGN.md
// sec 8). Each must get a 400.
type NegativeClass string

const (
	MalformedHash   NegativeClass = "malformed_hash"    // /verify, hash not a valid PHC string
	NonExactParams  NegativeClass = "non_exact_params"  // /verify, well-formed hash at other parameters
	EmptyPassword   NegativeClass = "empty_password"    // /hash or /verify, password empty or missing
	PasswordTooLong NegativeClass = "password_too_long" // /hash or /verify, password over 1024 bytes
	InvalidJSON     NegativeClass = "invalid_json"      // /hash or /verify, body not one valid request object
)

// String returns c as the control API's records carry it.
func (c NegativeClass) String() string { return string(c) }

// negativeClasses is the rotation order.
var negativeClasses = [...]NegativeClass{MalformedHash, NonExactParams, EmptyPassword, PasswordTooLong, InvalidJSON}

// maxTooLongLen caps the over-1024 passwords so that a /verify body
// (password, hash and about 120 bytes of JSON) stays well inside the
// 4 KiB body limit cmd/service applies: the 400 must come from the
// password check, not from the body-size path.
const maxTooLongLen = 2048

// A negForm builds one form of a negative class, with fresh random parts
// on every call.
type negForm struct {
	name  string
	build func() Request
}

// negativeForms holds every form of each class; newNegative draws one
// uniformly.
var negativeForms = map[NegativeClass][]negForm{
	MalformedHash: {
		{"not_phc", func() Request { return malformed(b64(randomBytes(48))) }},
		{"truncated", func() Request {
			s := randomExactPHC()
			return malformed(s[:1+mrand.IntN(lastCut(s))])
		}},
		{"noncanonical_bits", func() Request { return malformed(nonCanonicalPHC(setUnusedBits)) }},
		{"noncanonical_newline", func() Request { return malformed(nonCanonicalPHC(insertNewline)) }},
		{"noncanonical_padded", func() Request { return malformed(nonCanonicalPHC(base64.StdEncoding.EncodeToString)) }},
	},
	NonExactParams: nonExactForms(),
	EmptyPassword: {
		{"hash_empty", func() Request { return negativeRequest(EmptyPassword, HashPath, "", hashBody("")) }},
		{"hash_missing", func() Request { return negativeRequest(EmptyPassword, HashPath, "", []byte("{}")) }},
		{"verify_empty", func() Request {
			return negativeRequest(EmptyPassword, VerifyPath, "", verifyBody("", randomExactPHC()))
		}},
		{"verify_missing", func() Request {
			return negativeRequest(EmptyPassword, VerifyPath, "", mustJSON(map[string]string{"hash": randomExactPHC()}))
		}},
	},
	PasswordTooLong: {
		{"hash", func() Request {
			pw := tooLongPassword()
			return negativeRequest(PasswordTooLong, HashPath, pw, hashBody(pw))
		}},
		{"verify", func() Request {
			pw := tooLongPassword()
			return negativeRequest(PasswordTooLong, VerifyPath, pw, verifyBody(pw, randomExactPHC()))
		}},
	},
	InvalidJSON: {
		{"hash_unterminated", func() Request {
			pw := newPassword()
			return invalidJSON(HashPath, pw, unterminated(hashBody(pw)))
		}},
		{"verify_unterminated", func() Request {
			pw := newPassword()
			return invalidJSON(VerifyPath, pw, unterminated(verifyBody(pw, randomExactPHC())))
		}},
		{"hash_wrong_type", func() Request {
			pw := newPassword()
			return invalidJSON(HashPath, pw, mustJSON(map[string]any{"password": wrongType(pw)}))
		}},
		{"verify_wrong_type_password", func() Request {
			pw := newPassword()
			return invalidJSON(VerifyPath, pw, mustJSON(map[string]any{"password": wrongType(pw), "hash": randomExactPHC()}))
		}},
		{"verify_wrong_type_hash", func() Request {
			pw := newPassword()
			return invalidJSON(VerifyPath, pw, mustJSON(map[string]any{"password": pw, "hash": wrongType(randomExactPHC())}))
		}},
		{"hash_trailing", func() Request {
			pw := newPassword()
			return invalidJSON(HashPath, pw, trailing(hashBody(pw)))
		}},
		{"verify_trailing", func() Request {
			pw := newPassword()
			return invalidJSON(VerifyPath, pw, trailing(verifyBody(pw, randomExactPHC())))
		}},
	},
}

// newNegative returns a negative of class c, in a form drawn uniformly
// from the class's forms.
func newNegative(c NegativeClass) Request {
	forms := negativeForms[c]
	return forms[mrand.IntN(len(forms))].build()
}

func negativeRequest(c NegativeClass, path, password string, body []byte) Request {
	return Request{Path: path, Body: body, Kind: KindNegative, Class: c, Password: password}
}

// malformed returns a /verify negative carrying hash, which hashcore
// rejects as ErrMalformedHash, with a fresh valid password.
func malformed(hash string) Request {
	pw := newPassword()
	return negativeRequest(MalformedHash, VerifyPath, pw, verifyBody(pw, hash))
}

func invalidJSON(path, password string, body []byte) Request {
	return negativeRequest(InvalidJSON, path, password, body)
}

// nonExactHeaders are the non-exact parameter sets: each differs from
// the fixed ones in one segment or one value.
var nonExactHeaders = []struct{ name, variant, version, params string }{
	{"t1", exactVariant, exactVersion, "m=19456,t=1,p=1"},
	{"t3", exactVariant, exactVersion, "m=19456,t=3,p=1"},
	{"m19455", exactVariant, exactVersion, "m=19455,t=2,p=1"},
	{"m19457", exactVariant, exactVersion, "m=19457,t=2,p=1"},
	{"p2", exactVariant, exactVersion, "m=19456,t=2,p=2"},
	{"m_max_uint32", exactVariant, exactVersion, "m=4294967295,t=2,p=1"},
	{"argon2i", "argon2i", exactVersion, exactParams},
	{"v16", exactVariant, "v=16", exactParams},
}

// nonExactForms returns one form per non-exact header, each with a random
// canonical salt and key and a fresh valid password.
func nonExactForms() []negForm {
	forms := make([]negForm, 0, len(nonExactHeaders))
	for _, h := range nonExactHeaders {
		forms = append(forms, negForm{h.name, func() Request {
			pw := newPassword()
			hash := phc(h.variant, h.version, h.params, b64(randomBytes(saltLen)), b64(randomBytes(keyLen)))
			return negativeRequest(NonExactParams, VerifyPath, pw, verifyBody(pw, hash))
		}})
	}
	return forms
}

// lastCut is the longest cut of PHC string s the truncated form makes: s
// up to, not including, its last '$'. Every cut from 1 to lastCut(s)
// leaves at most five segments, so none can parse as a PHC string;
// hashcore_test.go tries them all. Longer cuts can (an empty or short
// key) and are not malformed.
func lastCut(s string) int { return strings.LastIndexByte(s, '$') }

// nonCanonicalPHC returns a PHC string at the fixed parameters whose salt
// or key (drawn evenly) is encoded by enc instead of canonically.
func nonCanonicalPHC(enc func([]byte) string) string {
	salt, key := b64(randomBytes(saltLen)), b64(randomBytes(keyLen))
	if mrand.IntN(2) == 0 {
		salt = enc(randomBytes(saltLen))
	} else {
		key = enc(randomBytes(keyLen))
	}
	return exactPHC(salt, key)
}

const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// setUnusedBits encodes b, then sets the unused low bits of the last
// character (4 bits for 16 bytes, 2 for 32) to a value drawn from the
// non-zero ones. A lenient decoder returns the same bytes; the canonical
// encoding has those bits zero.
func setUnusedBits(b []byte) string {
	return setUnusedBitsTo(b, 1+mrand.IntN(unusedBitValues(b)))
}

// unusedBitValues returns how many non-zero values the unused low bits
// of b's encoding can take.
func unusedBitValues(b []byte) int {
	return 1<<(6*len(b64(b))-8*len(b)) - 1
}

// setUnusedBitsTo encodes b with its unused low bits set to v, from 1 to
// unusedBitValues(b).
func setUnusedBitsTo(b []byte, v int) string {
	s := b64(b)
	last := strings.IndexByte(base64Alphabet, s[len(s)-1]) | v
	return s[:len(s)-1] + base64Alphabet[last:last+1]
}

// insertNewline encodes b with a newline inside, which Go's base64
// decoders skip, so it decodes but is not the canonical encoding.
func insertNewline(b []byte) string {
	s := b64(b)
	return insertNewlineAt(s, 1+mrand.IntN(len(s)-1))
}

// insertNewlineAt returns s with a newline before s[i].
func insertNewlineAt(s string, i int) string { return s[:i] + "\n" + s[i:] }

// tooLongPassword returns a fresh password of a length drawn uniformly
// from maxPasswordLen+1 to maxTooLongLen inclusive.
func tooLongPassword() string { return passwordOfLen(tooLongLen()) }

func tooLongLen() int {
	return maxPasswordLen + 1 + mrand.IntN(maxTooLongLen-maxPasswordLen)
}

// unterminated returns a proper, non-empty prefix of body, a compact JSON
// object: the object only closes at its last byte, so no prefix parses.
func unterminated(body []byte) []byte {
	return body[:1+mrand.IntN(len(body)-1)]
}

// wrongType returns a JSON value that is not a string, built around s.
func wrongType(s string) any {
	switch mrand.IntN(4) {
	case 0:
		return len(s)
	case 1:
		return true
	case 2:
		return []string{s}
	default:
		return map[string]string{"value": s}
	}
}

// trailing appends non-whitespace data after body's object.
func trailing(body []byte) []byte {
	trailers := [...]string{"{}", "x", "0"}
	return append(body, trailers[mrand.IntN(len(trailers))]...)
}
