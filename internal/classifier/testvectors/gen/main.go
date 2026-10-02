// Command gen writes vectors.json, the testvectors pool, into the
// current directory. Run it with `go generate` from the testvectors
// package directory. Every run draws fresh passwords and salts.
// gen imports testvectors, which embeds vectors.json, so to regenerate
// from scratch any placeholder vectors.json (e.g. "{}") must exist first.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/alexedwards/argon2id"

	"github.com/codeinthecreek/eK8s/internal/classifier/testvectors"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
)

const (
	numPasswords = 256
	minLen       = 1    // bytes
	maxLen       = 1024 // bytes; the HTTP contract's password limit
	numMultiByte = 4    // passwords that include multi-byte UTF-8
)

// ascii is printable ASCII; the multi-byte passwords add runes of two,
// three and four UTF-8 bytes. Passwords must be valid UTF-8 so they
// survive JSON unchanged.
const ascii = " !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"

var multiByte = []rune{'é', 'ß', 'ж', '€', '中', '😀', '𝄞'}

func main() {
	log.SetFlags(0)
	core := hashcore.New()

	passwords := make([]string, 0, numPasswords)
	seen := make(map[string]bool)
	for i := 0; len(passwords) < numPasswords; i++ {
		var n int
		switch i {
		case 0:
			n = minLen
		case 1:
			n = maxLen
		default:
			n = minLen + randInt(maxLen-minLen+1)
		}
		// The 1024-byte boundary is also among the multi-byte ones.
		multi := i == 1 || (i >= 2 && i < 1+numMultiByte)
		p := randPassword(n, multi)
		if seen[p] {
			continue // keep passwords distinct, so every mismatch pair is one
		}
		seen[p] = true
		passwords = append(passwords, p)
	}

	hashes := make([]string, len(passwords))
	for i, p := range passwords {
		h, err := core.Hash(p)
		if err != nil {
			log.Fatal(err)
		}
		hashes[i] = h
	}

	f := testvectors.File{Version: testvectors.Version, Params: testvectors.Params}
	for i, p := range passwords {
		other := hashes[(i+1)%len(hashes)]
		f.Pairs = append(f.Pairs,
			testvectors.Pair{Password: p, Hash: hashes[i], Match: true},
			testvectors.Pair{Password: p, Hash: other, Match: false})
	}
	f.Negatives = negatives()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("vectors.json", buf.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}

// negatives returns the gate-only set: computed with the reference
// library where that is cheap, hand-constructed for very large m
// (computing one would allocate that memory; DESIGN.md sec 5).
func negatives() []testvectors.Negative {
	computed := []struct {
		name string
		m, t uint32
		p    uint8
	}{
		{"t=1", 19456, 1, 1},
		{"t=3", 19456, 3, 1},
		{"m=19455", 19455, 2, 1},
		{"m=19457", 19457, 2, 1},
		{"p=2", 19456, 2, 2},
	}
	var out []testvectors.Negative
	for _, c := range computed {
		h, err := argon2id.CreateHash("negative "+c.name, &argon2id.Params{
			Memory: c.m, Iterations: c.t, Parallelism: c.p, SaltLength: 16, KeyLength: 32,
		})
		if err != nil {
			log.Fatal(err)
		}
		out = append(out, testvectors.Negative{Name: c.name, Hash: h})
	}
	for _, m := range []string{"4194304", "4294967295"} {
		// Canonical prefix, the other params exact, a 16-byte salt and a
		// 32-byte key in unpadded standard base64: only m is wrong.
		h := fmt.Sprintf("$argon2id$v=19$m=%s,t=2,p=1$%s$%s", m, randB64(16), randB64(32))
		out = append(out, testvectors.Negative{Name: "m=" + m, Hash: h, Constructed: true})
	}
	return out
}

// randPassword returns a password of exactly n bytes. With multi and
// n >= 4 (the longest multiByte rune), it starts with a multi-byte rune,
// so it always contains one, and the rest mixes in more at random.
func randPassword(n int, multi bool) string {
	var b strings.Builder
	if multi && n >= utf8.UTFMax {
		b.WriteRune(multiByte[randInt(len(multiByte))])
	}
	for b.Len() < n {
		left := n - b.Len()
		if multi && randInt(2) == 0 {
			r := multiByte[randInt(len(multiByte))]
			if utf8.RuneLen(r) <= left {
				b.WriteRune(r)
				continue
			}
		}
		b.WriteByte(ascii[randInt(len(ascii))])
	}
	return b.String()
}

func randInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		log.Fatal(err)
	}
	return int(v.Int64())
}

func randB64(n int) string {
	b := make([]byte, n)
	rand.Read(b) // never errors since Go 1.24; crashes instead
	return base64.RawStdEncoding.EncodeToString(b)
}
