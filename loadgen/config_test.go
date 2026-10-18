package loadgen

import (
	"math"
	mrand "math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/deploy/variant"
)

// validConfig returns a Config that Validate accepts, in production's
// shape: CI smoke's r = 2, a 5-minute window and a burst at b = 4r, so
// n = 240.
func validConfig() Config {
	return Config{
		Rate:       2,
		Window:     5 * time.Minute,
		BurstRate:  8,
		TargetURL:  "http://hasher.ek8s-v{id}.svc.cluster.local:8080",
		CanaryAddr: "10.96.0.1:443",
	}
}

func TestConfigValidateAccepts(t *testing.T) {
	for name, mod := range map[string]func(*Config){
		"production":            func(*Config) {},
		"httptest path":         func(c *Config) { c.TargetURL = "http://127.0.0.1:43210/v{id}" },
		"deeper path":           func(c *Config) { c.TargetURL = "http://127.0.0.1:43210/a/v{id}/b" },
		"https":                 func(c *Config) { c.TargetURL = "https://hasher.ek8s-v{id}.svc.cluster.local:8443" },
		"no port":               func(c *Config) { c.TargetURL = "http://hasher.ek8s-v{id}.svc.cluster.local" },
		"IPv6 target":           func(c *Config) { c.TargetURL = "http://[fd00::1]:8080/v{id}" },
		"IPv6 canary":           func(c *Config) { c.CanaryAddr = "[fd00::1]:443" },
		"smallest burst":        func(c *Config) { c.BurstRate = 0.15 }, // n = round(4.5) = 5
		"largest burst":         func(c *Config) { c.BurstRate = 100 },  // n = 3,000
		"one-send window":       func(c *Config) { c.Rate, c.Window = 1, time.Second },
		"fractional burst rate": func(c *Config) { c.BurstRate = 0.5 }, // n = 15
	} {
		c := validConfig()
		mod(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("%s: Validate(%+v) = %v, want nil", name, c, err)
		}
	}
}

func TestConfigValidateRefuses(t *testing.T) {
	for name, mod := range map[string]func(*Config){
		"rate 0":             func(c *Config) { c.Rate = 0 },
		"rate negative":      func(c *Config) { c.Rate = -1 },
		"rate NaN":           func(c *Config) { c.Rate = math.NaN() },
		"rate Inf":           func(c *Config) { c.Rate = math.Inf(1) },
		"window 0":           func(c *Config) { c.Window = 0 },
		"window negative":    func(c *Config) { c.Window = -time.Second },
		"zero sends":         func(c *Config) { c.Rate, c.Window = 0.4, time.Second },
		"burst rate 0":       func(c *Config) { c.BurstRate = 0 },
		"burst rate neg":     func(c *Config) { c.BurstRate = -8 },
		"burst rate NaN":     func(c *Config) { c.BurstRate = math.NaN() },
		"burst rate Inf":     func(c *Config) { c.BurstRate = math.Inf(1) },
		"burst n = 4":        func(c *Config) { c.BurstRate = 0.149 },  // round(4.47)
		"burst n = 0":        func(c *Config) { c.BurstRate = 0.01 },   // round(0.3)
		"burst n = 3,001":    func(c *Config) { c.BurstRate = 100.02 }, // round(3000.6)
		"burst rate huge":    func(c *Config) { c.BurstRate = 1e300 },
		"ring over max":      func(c *Config) { c.Rate, c.Window = maxRecords, time.Second },
		"canary empty":       func(c *Config) { c.CanaryAddr = "" },
		"canary name":        func(c *Config) { c.CanaryAddr = "kubernetes.default:443" },
		"canary localhost":   func(c *Config) { c.CanaryAddr = "localhost:443" },
		"canary no port":     func(c *Config) { c.CanaryAddr = "10.96.0.1" },
		"canary port 0":      func(c *Config) { c.CanaryAddr = "10.96.0.1:0" },
		"canary port range":  func(c *Config) { c.CanaryAddr = "10.96.0.1:65536" },
		"canary v6 no port":  func(c *Config) { c.CanaryAddr = "[fd00::1]" },
		"canary v6 bare":     func(c *Config) { c.CanaryAddr = "fd00::1:443" },
		"canary leading 0":   func(c *Config) { c.CanaryAddr = "010.96.0.1:443" },
		"canary with scheme": func(c *Config) { c.CanaryAddr = "https://10.96.0.1:443" },
	} {
		c := validConfig()
		mod(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate(%+v) = nil, want an error", name, c)
		}
	}
}

// TestConfigValidateTargetURL checks the TargetURL rules one by one.
func TestConfigValidateTargetURL(t *testing.T) {
	for name, tmpl := range map[string]string{
		"empty":               "",
		"no {id}":             "http://hasher.ek8s-v7.svc.cluster.local:8080",
		"two {id}":            "http://hasher.ek8s-v{id}.svc.cluster.local:8080/v{id}",
		"two {id} adjacent":   "http://h{id}{id}:8080",
		"userinfo":            "http://u@hasher.ek8s-v{id}:8080",
		"userinfo password":   "http://u:p@hasher.ek8s-v{id}:8080",
		"empty userinfo":      "http://@hasher.ek8s-v{id}:8080",
		"query":               "http://hasher.ek8s-v{id}:8080?a=1",
		"empty query":         "http://hasher.ek8s-v{id}:8080/x?",
		"fragment":            "http://hasher.ek8s-v{id}:8080#f",
		"empty fragment":      "http://hasher.ek8s-v{id}:8080/x#",
		"trailing slash":      "http://hasher.ek8s-v{id}:8080/",
		"trailing slash path": "http://127.0.0.1:8080/v{id}/",
		"ftp":                 "ftp://hasher.ek8s-v{id}:8080",
		"file":                "file:///v{id}",
		"unix":                "unix://hasher.ek8s-v{id}",
		"ws":                  "ws://hasher.ek8s-v{id}:8080",
		"no scheme":           "hasher.ek8s-v{id}:8080",
		"scheme-relative":     "//hasher.ek8s-v{id}:8080",
		"opaque":              "http:hasher.ek8s-v{id}",
		"upper-case scheme":   "HTTP://hasher.ek8s-v{id}:8080",
		"no host":             "http:///v{id}",
		"port only":           "http://:8080/v{id}",
		"id is the port":      "http://hasher:{id}",
		"id in the port":      "http://hasher:80{id}",
		"space in path":       "http://127.0.0.1:8080/a b/v{id}",
		"bad port":            "http://hasher.ek8s-v{id}:http",
		"newline":             "http://hasher.ek8s-v{id}:8080\n",
	} {
		c := validConfig()
		c.TargetURL = tmpl
		if err := c.Validate(); err == nil {
			t.Errorf("%s: TargetURL %q accepted, want an error", name, tmpl)
		}
	}
}

// badIDs are strings that are not variant ids, among them attempts to
// add path, query, userinfo, port or host characters to a target URL.
var badIDs = []string{
	"", "01", "00", "-1", "+1", "1.5", "1e3", "0x1", " 1", "1 ", "1\n",
	"1/x", "1?a", "1@h", "1#f", "1:80", "1.evil.example", "1/../2", "1%2f", "a", "v1",
	"1234567890123456789", // 19 digits
	"\uff11",              // a fullwidth digit one
}

// goodIDs are variant ids, the longest included.
var goodIDs = []string{"0", "1", "7", "10", "1234", "999999999999999999"}

func TestTargetURL(t *testing.T) {
	c := validConfig()
	for _, id := range goodIDs {
		got, err := c.targetURL(id)
		if want := "http://hasher.ek8s-v" + id + ".svc.cluster.local:8080"; err != nil || got != want {
			t.Errorf("targetURL(%q) = %q, %v; want %q, nil", id, got, err, want)
		}
	}
	for _, id := range badIDs {
		if got, err := c.targetURL(id); err == nil {
			t.Errorf("targetURL(%q) = %q, nil; want an error", id, got)
		}
	}
	c.TargetURL = "http://127.0.0.1:43210/v{id}"
	if got, err := c.targetURL("12"); err != nil || got != "http://127.0.0.1:43210/v12" {
		t.Errorf("path template: targetURL(12) = %q, %v; want http://127.0.0.1:43210/v12, nil", got, err)
	}
}

// variantAccepts reports whether deploy/variant takes id as a variant id.
func variantAccepts(id string) bool {
	return variant.Params{ID: id, Label: "ek8s-0123456789abcdef", Image: "ek8s-wrapper:0123456789ab"}.Validate() == nil
}

// TestValidIDAgreesWithVariant checks loadgen's restated id format
// against deploy/variant's, on the listed ids and on random strings over
// digits and the characters an injection would need.
func TestValidIDAgreesWithVariant(t *testing.T) {
	if !variantAccepts("1") || variantAccepts("01") {
		t.Fatal("variantAccepts does not tell ids apart; its Label or Image is no longer valid")
	}
	ids := append(append([]string{}, goodIDs...), badIDs...)
	const alphabet = "0123456789019a/?@:#. %\n"
	for range 20000 {
		var b strings.Builder
		for range mrand.IntN(21) {
			b.WriteByte(alphabet[mrand.IntN(len(alphabet))])
		}
		ids = append(ids, b.String())
	}
	for _, id := range ids {
		if got, want := validID(id), variantAccepts(id); got != want {
			t.Errorf("validID(%q) = %v, deploy/variant accepts it: %v", id, got, want)
		}
	}
}

func TestWindowPairs(t *testing.T) {
	for _, tc := range []struct{ n, want int }{
		{120, 93},
		{1200, 688},
		{3000, 1617},
		{6000, 3130},
		{12000, 6113},
		{36000, 17891},
		{1, 1},   // capped at n: the formula gives 4
		{10, 10}, // likewise, 15
	} {
		if got := windowPairs(tc.n); got != tc.want {
			t.Errorf("windowPairs(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
	for n := 1; n <= 50000; n++ {
		p := windowPairs(n)
		bound := 0.48*float64(n) + math.Sqrt(float64(n)*math.Log(1e9)/2)
		if p > n || (p < n && float64(p) < bound) {
			t.Fatalf("windowPairs(%d) = %d, want min(n, ceil(%.3f))", n, p, bound)
		}
	}
}

// binomialTail returns P(V > k) for V ~ Binomial(n, p), summed in log
// space.
func binomialTail(n, k int, p float64) float64 {
	lg := func(x int) float64 { v, _ := math.Lgamma(float64(x) + 1); return v }
	var sum float64
	for v := k + 1; v <= n; v++ {
		sum += math.Exp(lg(n) - lg(v) - lg(n-v) + float64(v)*math.Log(p) + float64(n-v)*math.Log(1-p))
	}
	return sum
}

// TestWindowPairsTail checks the claim windowPairs exists for: the exact
// chance that a window of n sends draws more /verify requests than its
// batch holds is at most pairTailEpsilon.
func TestWindowPairsTail(t *testing.T) {
	share := float64(verifyWeight) / float64(hashWeight+verifyWeight+negativeWeight)
	for _, n := range []int{20, 120, 1200, 6000, 36000} {
		if tail := binomialTail(n, windowPairs(n), share); tail > pairTailEpsilon {
			t.Errorf("n = %d: P(V > %d) = %.3g, want <= %g", n, windowPairs(n), tail, pairTailEpsilon)
		}
	}
}

func TestConfigCounts(t *testing.T) {
	c := validConfig()
	if n, err := c.WindowSends(); err != nil || n != 600 {
		t.Errorf("WindowSends = %d, %v; want 600, nil", n, err)
	}
	if p, err := c.WindowPairs(); err != nil || p != windowPairs(600) {
		t.Errorf("WindowPairs = %d, %v; want %d, nil", p, err, windowPairs(600))
	}
	if n, err := c.BurstSends(); err != nil || n != 240 {
		t.Errorf("BurstSends = %d, %v; want 240, nil", n, err)
	}
	if p, err := c.BurstPairs(); err != nil || p != 112 {
		t.Errorf("BurstPairs = %d, %v; want 112, nil", p, err)
	}
	if n, err := c.RingCapacity(); err != nil || n != 600+240 {
		t.Errorf("RingCapacity = %d, %v; want 840, nil", n, err)
	}
	c.BurstRate = 100
	if n, err := c.BurstSends(); err != nil || n != maxBurstSends {
		t.Errorf("BurstSends at b = 100 = %d, %v; want %d, nil", n, err, maxBurstSends)
	}
	if p, err := c.BurstPairs(); err != nil || p != 1396 {
		t.Errorf("BurstPairs at b = 100 = %d, %v; want 1396, nil", p, err)
	}

	bad := validConfig()
	bad.Rate = 0
	if _, err := bad.WindowSends(); err == nil {
		t.Error("WindowSends at rate 0: nil error")
	}
	if _, err := bad.WindowPairs(); err == nil {
		t.Error("WindowPairs at rate 0: nil error")
	}
	if _, err := bad.RingCapacity(); err == nil {
		t.Error("RingCapacity at rate 0: nil error")
	}
	for _, b := range []float64{0.149, 100.02, 0, math.NaN()} {
		bad = validConfig()
		bad.BurstRate = b
		if _, err := bad.BurstSends(); err == nil {
			t.Errorf("BurstSends at b = %v: nil error", b)
		}
		if _, err := bad.BurstPairs(); err == nil {
			t.Errorf("BurstPairs at b = %v: nil error", b)
		}
		if _, err := bad.RingCapacity(); err == nil {
			t.Errorf("RingCapacity at b = %v: nil error", b)
		}
	}
}
