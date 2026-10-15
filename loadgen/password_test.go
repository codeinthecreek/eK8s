package loadgen

import (
	"strings"
	"testing"
)

const base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

func checkAlphabet(t *testing.T, pw string) {
	t.Helper()
	if i := strings.IndexFunc(pw, func(r rune) bool { return !strings.ContainsRune(base32Alphabet, r) }); i >= 0 {
		t.Fatalf("password has %q at %d, outside A-Z and 2-7", pw[i], i)
	}
}

// TestPasswordLen checks the /hash length bounds, both ends reachable.
func TestPasswordLen(t *testing.T) {
	seenMin, seenMax := false, false
	for range 100000 {
		n := passwordLen()
		if n < minPasswordLen || n > maxPasswordLen {
			t.Fatalf("password length %d, want %d to %d", n, minPasswordLen, maxPasswordLen)
		}
		seenMin = seenMin || n == minPasswordLen
		seenMax = seenMax || n == maxPasswordLen
	}
	if !seenMin || !seenMax {
		t.Errorf("length %d seen: %v, %d seen: %v", minPasswordLen, seenMin, maxPasswordLen, seenMax)
	}
}

// TestPasswordLengthsAsSent checks newPassword and tooLongPassword
// themselves, not only the length draws: a fixed length would move the
// mean. Each tolerance is about 6 standard deviations of the mean.
func TestPasswordLengthsAsSent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gen      func() string
		draws    int
		min, max int
		tol      float64
	}{
		{"newPassword", newPassword, 2000, minPasswordLen, maxPasswordLen, 40},
		{"tooLongPassword", tooLongPassword, 500, maxPasswordLen + 1, maxTooLongLen, 80},
	} {
		sum := 0
		for range tc.draws {
			pw := tc.gen()
			if len(pw) < tc.min || len(pw) > tc.max {
				t.Fatalf("%s: length %d, want %d to %d", tc.name, len(pw), tc.min, tc.max)
			}
			checkAlphabet(t, pw)
			sum += len(pw)
		}
		mean, want := float64(sum)/float64(tc.draws), float64(tc.min+tc.max)/2
		if mean < want-tc.tol || mean > want+tc.tol {
			t.Errorf("%s: mean length %.1f, want %.1f +- %.0f", tc.name, mean, want, tc.tol)
		}
	}
}

// TestPasswordOfLen checks exact lengths, including ones that are not a
// multiple of rand.Text's current 26 characters, and the alphabet.
func TestPasswordOfLen(t *testing.T) {
	for _, n := range []int{1, minPasswordLen, 25, 26, 27, 52, 53, 1023, maxPasswordLen, maxTooLongLen} {
		pw := passwordOfLen(n)
		if len(pw) != n {
			t.Fatalf("passwordOfLen(%d) has length %d", n, len(pw))
		}
		checkAlphabet(t, pw)
	}
}

// TestHashBodyEscapesNothing checks that encoding/json sends a password
// byte for byte, so its wire size is its length.
func TestHashBodyEscapesNothing(t *testing.T) {
	for range 50 {
		pw := newPassword()
		checkAlphabet(t, pw)
		if got, want := string(hashBody(pw)), `{"password":"`+pw+`"}`; got != want {
			t.Fatalf("hashBody = %q, want %q", got, want)
		}
	}
}
