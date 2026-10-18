package loadgen

import (
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// burstDuration is D, a Stage 5 burst's length: n = round(b x D). A
// starting guess (DESIGN.md sec 15).
const burstDuration = 30 * time.Second

// maxBurstSends is the largest Stage 5 burst n loadgen accepts. The
// PR #43 no-cap memory sum (docs/decisions.md) holds for a burst of at
// most 3,000 records; a larger n needs that sum redone first.
const maxBurstSends = 3000

// pairTailEpsilon bounds the chance that one stream runs out of /verify
// pairs in one window (see WindowPairs). A starting guess (DESIGN.md
// sec 15).
const pairTailEpsilon = 1e-9

// idPlaceholder marks where a variant id goes in Config.TargetURL.
const idPlaceholder = "{id}"

// idRE is the variant id format, restated from deploy/variant (idRE in
// variant.go) because loadgen does not import it; config_test.go checks
// the two agree. Digits only, so an id cannot add path, host, port or
// query characters to a target URL.
var idRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})$`)

// validID reports whether id is a variant id.
func validID(id string) bool { return idRE.MatchString(id) }

// Config is loadgen's start-up configuration, fixed for its process.
type Config struct {
	// Rate is r, each window stream's sends per second.
	Rate float64
	// Window is W, a window's length.
	Window time.Duration
	// BurstRate is b, a Stage 5 burst's sends per second. It also sets
	// the burst's size (BurstSends).
	BurstRate float64
	// TargetURL is a variant's base URL with idPlaceholder where its id
	// goes, e.g. http://hasher.ek8s-v{id}.svc.cluster.local:8080. It
	// takes exactly one placeholder, in the host or the path.
	TargetURL string
	// CanaryAddr is the policy canary's target, an IP literal host:port
	// (the API server's ClusterIP, DESIGN.md sec 11). A name is refused:
	// a failed lookup would read as refused and hide a fail-open policy.
	CanaryAddr string
}

// Validate reports the first rule c breaks: the window must hold 1 to
// maxRecords sends (WindowSends); the burst must be valid (BurstSends);
// the ring must fit (RingCapacity); TargetURL and CanaryAddr must be as
// their fields describe.
func (c Config) Validate() error {
	if _, err := c.WindowSends(); err != nil {
		return err
	}
	if _, err := c.BurstSends(); err != nil {
		return err
	}
	if _, err := c.RingCapacity(); err != nil {
		return err
	}
	if err := validTargetURL(c.TargetURL); err != nil {
		return err
	}
	return validCanaryAddr(c.CanaryAddr)
}

// validTargetURL checks a TargetURL template: exactly one placeholder;
// with an id in it, an absolute http or https URL with a host, in
// canonical form (it formats back to itself), with no userinfo, query,
// fragment or trailing slash; and the id must not change the port, so
// the placeholder is in the host or the path.
func validTargetURL(tmpl string) error {
	if n := strings.Count(tmpl, idPlaceholder); n != 1 {
		return fmt.Errorf("loadgen: target URL %q holds %d %s, want exactly one", tmpl, n, idPlaceholder)
	}
	if strings.ContainsAny(tmpl, "?#") {
		return fmt.Errorf("loadgen: target URL %q has a query or fragment", tmpl)
	}
	var port string
	for i, id := range []string{"0", "1"} {
		s := strings.Replace(tmpl, idPlaceholder, id, 1)
		u, err := url.Parse(s)
		switch {
		case err != nil:
			return fmt.Errorf("loadgen: target URL %q: %w", tmpl, err)
		case u.Scheme != "http" && u.Scheme != "https":
			return fmt.Errorf("loadgen: target URL %q: scheme %q, want http or https", tmpl, u.Scheme)
		case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
			return fmt.Errorf("loadgen: target URL %q has no host", tmpl)
		case u.User != nil:
			return fmt.Errorf("loadgen: target URL %q has userinfo", tmpl)
		case strings.HasSuffix(s, "/"):
			return fmt.Errorf("loadgen: target URL %q ends in a slash", tmpl)
		case u.String() != s:
			return fmt.Errorf("loadgen: target URL %q is not in canonical form (%q)", tmpl, u.String())
		case i > 0 && u.Port() != port:
			return fmt.Errorf("loadgen: target URL %q puts the id in the port", tmpl)
		}
		port = u.Port()
	}
	return nil
}

// validCanaryAddr checks that addr is an IP literal host:port with a
// nonzero port.
func validCanaryAddr(addr string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return fmt.Errorf("loadgen: canary address %q, want an IP literal host:port: %w", addr, err)
	}
	if ap.Port() == 0 {
		return fmt.Errorf("loadgen: canary address %q has port 0", addr)
	}
	return nil
}

// targetURL returns the base URL of variant id's Service, from a TargetURL
// that Validate accepted. It errors if id is not a variant id.
func (c Config) targetURL(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("loadgen: variant id %q: want a decimal number without leading zeros, at most 18 digits", id)
	}
	return strings.Replace(c.TargetURL, idPlaceholder, id, 1), nil
}

// WindowSends returns N, the sends in one window: WindowSends(Rate, Window).
func (c Config) WindowSends() (int, error) { return WindowSends(c.Rate, c.Window) }

// WindowPairs returns how many /verify pairs one stream's window batch
// needs: windowPairs(N). It errors whenever WindowSends does.
func (c Config) WindowPairs() (int, error) {
	n, err := c.WindowSends()
	if err != nil {
		return 0, err
	}
	return windowPairs(n), nil
}

// windowPairs returns P = ceil(s n + sqrt(n ln(1/pairTailEpsilon) / 2)),
// at most n, where s is the request mix's /verify share. A Stream draws
// /verify on each send with probability s, independently (Stream.Next),
// so a window of n sends draws V ~ Binomial(n, s), and it runs out of
// pairs only when V > P. By Hoeffding's inequality,
// P(V >= s n + t) <= exp(-2 t^2 / n), which is pairTailEpsilon at the
// t above. With P = n no send can run out.
func windowPairs(n int) int {
	s := float64(verifyWeight) / float64(hashWeight+verifyWeight+negativeWeight)
	t := math.Sqrt(float64(n) * math.Log(1/pairTailEpsilon) / 2)
	return min(int(math.Ceil(s*float64(n)+t)), n)
}

// BurstSends returns n, the sends in a Stage 5 burst:
// round(BurstRate x burstDuration). It errors when BurstRate is not
// positive and finite, when n is over maxBurstSends (the PR #43 no-cap
// memory sum, docs/decisions.md, covers no more), or when BurstCounts
// refuses n (too small to hold its negatives).
func (c Config) BurstSends() (int, error) {
	if !(c.BurstRate > 0) || math.IsInf(c.BurstRate, 1) {
		return 0, fmt.Errorf("loadgen: burst rate %v, want a positive finite rate", c.BurstRate)
	}
	x := math.Round(c.BurstRate * burstDuration.Seconds())
	if x > maxBurstSends {
		return 0, fmt.Errorf("loadgen: burst rate %v over %v is %v sends, over the %d the memory sum covers", c.BurstRate, burstDuration, x, maxBurstSends)
	}
	n := int(x)
	if _, _, _, err := BurstCounts(n); err != nil {
		return 0, err
	}
	return n, nil
}

// BurstPairs returns how many /verify pairs a Stage 5 burst's batch
// needs: exactly BurstCounts(n)'s verifies, since a burst's counts are
// fixed. It errors whenever BurstSends does.
func (c Config) BurstPairs() (int, error) {
	n, err := c.BurstSends()
	if err != nil {
		return 0, err
	}
	_, verifies, _, err := BurstCounts(n)
	return verifies, err
}

// RingCapacity returns each target's ring capacity:
// RingCapacity(Rate, Window, n), n from BurstSends. It errors whenever
// BurstSends or RingCapacity does.
func (c Config) RingCapacity() (int, error) {
	n, err := c.BurstSends()
	if err != nil {
		return 0, err
	}
	return RingCapacity(c.Rate, c.Window, n)
}
