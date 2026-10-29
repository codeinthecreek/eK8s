package fitness

import (
	"math"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

func rec(kind, bucket, reason string, lat time.Duration) wire.Record {
	return wire.Record{Kind: kind, Bucket: bucket, Reason: reason, LatencyNS: int64(lat)}
}

func succ(lat time.Duration) wire.Record { return rec("hash", "success", "", lat) }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParams(t *testing.T) {
	if err := DefaultParams().Validate(); err != nil {
		t.Fatalf("DefaultParams: %v", err)
	}
	if DefaultParams().LatencyCeiling != classifier.HangTimeout {
		t.Error("the latency ceiling is not the hang timeout")
	}
	mut := func(f func(p *Params)) Params { p := DefaultParams(); f(&p); return p }
	for name, p := range map[string]Params{
		"credit over 1":      mut(func(p *Params) { p.ShedCredit = 1.1 }),
		"negative credit":    mut(func(p *Params) { p.ShedCredit = -0.1 }),
		"nan credit":         mut(func(p *Params) { p.ShedCredit = math.NaN() }),
		"latency flipped":    mut(func(p *Params) { p.LatencyFloor = p.LatencyCeiling }),
		"negative floor":     mut(func(p *Params) { p.LatencyFloor = -1 }),
		"cpu flipped":        mut(func(p *Params) { p.CPUFloor = p.CPUCeiling }),
		"weights not 1":      mut(func(p *Params) { p.WeightCPU = 0.4 }),
		"negative weight":    mut(func(p *Params) { p.WeightSuccess, p.WeightLatency = 1.2, -0.2 }),
		"infinite cpu bound": mut(func(p *Params) { p.CPUCeiling = math.Inf(1) }),
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	if _, err := Score(Params{}, []wire.Record{succ(time.Second)}, 1); err == nil {
		t.Error("Score accepted zero Params")
	}
}

func TestScoreBasics(t *testing.T) {
	p := DefaultParams()
	recs := []wire.Record{
		succ(100 * time.Millisecond), succ(200 * time.Millisecond), succ(300 * time.Millisecond),
		rec("verify", "success", "", 400*time.Millisecond),
		rec("verify", "graceful_shed", "", 5*time.Millisecond),
		rec("hash", "failed", "transport_error", 0),
		rec("negative", "success", "", 10*time.Second), // outside every metric
		rec("negative", "failed", "negative_accepted", 0),
	}
	res, err := Score(p, recs, 2.0)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Stats
	if s.Requests != 6 || s.Successes != 4 || s.Sheds != 1 || s.Failures != 1 || s.Negatives != 2 {
		t.Fatalf("counts = %+v", s)
	}
	if !near(s.SuccessRate, (4+0.5)/6) {
		t.Errorf("success rate %v, want %v", s.SuccessRate, 4.5/6)
	}
	if s.P95Latency != 400*time.Millisecond { // rank ceil(0.95 x 4) = 4
		t.Errorf("p95 %v, want 400ms", s.P95Latency)
	}
	if !near(s.CPUPerSuccess, 0.5) {
		t.Errorf("cpu per success %v, want 0.5", s.CPUPerSuccess)
	}
	if len(s.Integrity) != 0 {
		t.Errorf("integrity %v: a negative's negative_accepted is not a scored failure", s.Integrity)
	}
	wantLat := float64(2*time.Second-400*time.Millisecond) / float64(2*time.Second-25*time.Millisecond)
	if !near(res.SuccessScore, 4.5/6) || !near(res.LatencyScore, wantLat) || res.CPUScore != 0 {
		t.Errorf("component scores = %v, %v, %v", res.SuccessScore, res.LatencyScore, res.CPUScore)
	}
	if want := 0.4*4.5/6 + (4.0/6)*0.3*wantLat; !near(res.Score, want) || !near(res.Served, 4.0/6) {
		t.Errorf("score %v, want %v", res.Score, want)
	}
}

func TestP95NearestRank(t *testing.T) {
	var recs []wire.Record
	for i := 1; i <= 20; i++ {
		recs = append(recs, succ(time.Duration(i)*time.Millisecond))
	}
	res, _ := Score(DefaultParams(), recs, 1)
	if res.P95Latency != 19*time.Millisecond { // rank 19 of 20
		t.Errorf("p95 of 20 = %v, want 19ms", res.P95Latency)
	}
	recs = append(recs, succ(21*time.Millisecond))
	res, _ = Score(DefaultParams(), recs, 1)
	if res.P95Latency != 20*time.Millisecond { // rank ceil(19.95) = 20 of 21
		t.Errorf("p95 of 21 = %v, want 20ms", res.P95Latency)
	}
	// Shuffled input gives the same answer.
	recs[0], recs[20] = recs[20], recs[0]
	if r2, _ := Score(DefaultParams(), recs, 1); r2.P95Latency != 20*time.Millisecond {
		t.Errorf("p95 of shuffled 21 = %v", r2.P95Latency)
	}
	if r1, _ := Score(DefaultParams(), []wire.Record{succ(7 * time.Millisecond)}, 1); r1.P95Latency != 7*time.Millisecond {
		t.Errorf("p95 of one = %v", r1.P95Latency)
	}
}

func TestScoreBounds(t *testing.T) {
	p := DefaultParams()
	best, _ := Score(p, []wire.Record{succ(10 * time.Millisecond)}, 0.01) // under both floors
	if !near(best.Score, 1) {
		t.Errorf("best score %v, want 1", best.Score)
	}
	worst, _ := Score(p, []wire.Record{succ(classifier.HangTimeout)}, 100) // at the ceiling, far over the cpu one
	if !near(worst.Score, 0.4) || worst.LatencyScore != 0 || worst.CPUScore != 0 {
		t.Errorf("worst-component result %+v", worst)
	}
	mid, _ := Score(p, []wire.Record{succ(p.LatencyFloor + (p.LatencyCeiling-p.LatencyFloor)/2)}, (p.CPUFloor+p.CPUCeiling)/2)
	if !near(mid.LatencyScore, 0.5) || !near(mid.CPUScore, 0.5) {
		t.Errorf("midpoint scores %v, %v", mid.LatencyScore, mid.CPUScore)
	}
}

func TestScoreNoSuccess(t *testing.T) {
	p := DefaultParams()
	res, err := Score(p, []wire.Record{
		rec("hash", "graceful_shed", "", time.Millisecond), rec("hash", "failed", "bare_503", 0),
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Successes != 0 || res.P95Latency != p.LatencyCeiling || res.CPUPerSuccess != p.CPUCeiling ||
		res.LatencyScore != 0 || res.CPUScore != 0 || res.Served != 0 || !near(res.Score, 0.4*0.25) {
		t.Errorf("no-success result %+v", res)
	}
}

func TestIntegrityCounted(t *testing.T) {
	res, err := Score(DefaultParams(), []wire.Record{
		succ(time.Millisecond),
		rec("verify", "failed", "wrong_match", 0), rec("verify", "failed", "wrong_match", 0),
		rec("hash", "failed", "salt_reused", 0), rec("hash", "failed", "over_hang_timeout", 0),
		rec("hash", "failed", "malformed_body", 0),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Integrity) != 3 || res.Integrity["wrong_match"] != 2 || res.Integrity["salt_reused"] != 1 || res.Integrity["malformed_body"] != 1 {
		t.Errorf("integrity = %v", res.Integrity)
	}
	if res.Failures != 5 {
		t.Errorf("failures %d, want 5", res.Failures)
	}
}

func TestScoreRefusals(t *testing.T) {
	ok := succ(time.Millisecond)
	for name, tc := range map[string]struct {
		recs []wire.Record
		cpu  float64
	}{
		"no records":       {nil, 1},
		"only negatives":   {[]wire.Record{rec("negative", "success", "", 0)}, 1},
		"skipped":          {[]wire.Record{ok, {Kind: "hash", Skipped: true}}, 1},
		"fault":            {[]wire.Record{ok, {Kind: "hash", Fault: "build"}}, 1},
		"unknown bucket":   {[]wire.Record{ok, rec("hash", "great", "", 0)}, 1},
		"empty bucket":     {[]wire.Record{ok, rec("hash", "", "", 0)}, 1},
		"negative latency": {[]wire.Record{ok, rec("hash", "success", "", -1)}, 1},
		"nan cpu":          {[]wire.Record{ok}, math.NaN()},
		"negative cpu":     {[]wire.Record{ok}, -1},
		"infinite cpu":     {[]wire.Record{ok}, math.Inf(1)},
		"skipped negative": {[]wire.Record{ok, {Kind: "negative", Skipped: true}}, 1},
	} {
		if _, err := Score(DefaultParams(), tc.recs, tc.cpu); err == nil {
			t.Errorf("%s: Score accepted it", name)
		}
	}
}

func TestCombine(t *testing.T) {
	lo, hi := 0.3, 0.9
	if Combine(0.7, nil) != 0.7 || Combine(0.7, &lo) != 0.3 || Combine(0.2, &hi) != 0.2 {
		t.Error("Combine is not the baseline alone, or the minimum of the two halves")
	}
}

// kubeletText is the shape of the kubelet's /metrics/resource.
const kubeletText = `# HELP container_cpu_usage_seconds_total [STABLE] Cumulative cpu time consumed by the container in core-seconds
# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{container="hasher",namespace="ek8s-v7",pod="hasher-aaa"} 3.5 1730000000000
# HELP pod_cpu_usage_seconds_total [STABLE] Cumulative cpu time consumed by the pod in core-seconds
# TYPE pod_cpu_usage_seconds_total counter
pod_cpu_usage_seconds_total{namespace="ek8s-v7",pod="hasher-aaa"} 4.25 1730000000000
pod_cpu_usage_seconds_total{namespace="ek8s-v7",pod="hasher-bbb"} 1.5 1730000000000
pod_cpu_usage_seconds_total{namespace="ek8s-v8",pod="hasher-ccc"} 9
pod_cpu_usage_seconds_total{namespace="kube-system",pod="we\\ird\"na\nme"} 1e1

pod_memory_working_set_bytes{namespace="ek8s-v7",pod="hasher-aaa"} 100 1730000000000
`

func TestParseResource(t *testing.T) {
	s, err := ParseResource(strings.NewReader(kubeletText))
	if err != nil {
		t.Fatal(err)
	}
	want := Sample{
		{"ek8s-v7", "hasher-aaa"}: 4.25, {"ek8s-v7", "hasher-bbb"}: 1.5, {"ek8s-v8", "hasher-ccc"}: 9,
		{"kube-system", "we\\ird\"na\nme"}: 10,
	}
	if len(s) != len(want) {
		t.Fatalf("parsed %v", s)
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("%v = %v, want %v", k, s[k], v)
		}
	}
	if s, err := ParseResource(strings.NewReader("")); err != nil || len(s) != 0 {
		t.Errorf("empty input = %v, %v", s, err)
	}
}

func TestParseResourceRefusals(t *testing.T) {
	line := func(s string) string { return "pod_cpu_usage_seconds_total" + s + "\n" }
	for name, in := range map[string]string{
		"nan":             line(`{namespace="a",pod="b"} NaN`),
		"inf":             line(`{namespace="a",pod="b"} +Inf`),
		"negative":        line(`{namespace="a",pod="b"} -1`),
		"not a number":    line(`{namespace="a",pod="b"} lots`),
		"no value":        line(`{namespace="a",pod="b"}`),
		"extra field":     line(`{namespace="a",pod="b"} 1 2 3`),
		"no pod":          line(`{namespace="a"} 1`),
		"no namespace":    line(`{pod="b"} 1`),
		"empty pod":       line(`{namespace="a",pod=""} 1`),
		"duplicate pod":   line(`{namespace="a",pod="b"} 1`) + line(`{namespace="a",pod="b"} 2`),
		"repeated label":  line(`{namespace="a",namespace="c",pod="b"} 1`),
		"unterminated":    line(`{namespace="a",pod="b} 1`),
		"unquoted":        line(`{namespace=a,pod="b"} 1`),
		"unknown escape":  line(`{namespace="a",pod="b\t"} 1`),
		"unclosed braces": line(`{namespace="a",pod="b" 1`),
	} {
		if _, err := ParseResource(strings.NewReader(in)); err == nil {
			t.Errorf("%s: ParseResource accepted %q", name, in)
		}
	}
}

func TestCPUSeconds(t *testing.T) {
	a, b, c := Pod{"ns", "a"}, Pod{"ns", "b"}, Pod{"other", "x"}
	for _, tc := range []struct {
		name       string
		start, end Sample
		want       float64
		fail       string
	}{
		{"one pod", Sample{a: 1, c: 5}, Sample{a: 3.5, c: 9}, 2.5, ""},
		{"a pod created in the window counts from 0", Sample{a: 1}, Sample{a: 2, b: 0.75}, 1.75, ""},
		{"idle", Sample{a: 2}, Sample{a: 2}, 0, ""},
		{"no pod at the end", Sample{a: 1}, Sample{c: 1}, 0, "no pod in namespace"},
		{"a pod is gone", Sample{a: 1, b: 1}, Sample{a: 2}, 0, "gone"},
		{"a counter reset", Sample{a: 5}, Sample{a: 1}, 0, "went from"},
		{"another namespace's pod gone is not ours", Sample{a: 1, c: 1}, Sample{a: 2}, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CPUSeconds(tc.start, tc.end, "ns")
			if tc.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fail) {
					t.Fatalf("CPUSeconds error = %v, want %q", err, tc.fail)
				}
				return
			}
			if err != nil || !near(got, tc.want) {
				t.Fatalf("CPUSeconds = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// TestDeps checks that the package's build depends on the standard
// library, internal/classifier and loadgen/wire only: no Argon2, no
// hashcore, no test vectors.
func TestDeps(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const mod = "github.com/codeinthecreek/eK8s"
	ok := map[string]bool{mod + "/fitness": true, mod + "/internal/classifier": true, mod + "/loadgen/wire": true}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, std, _ := strings.Cut(l, " ")
		if std != "true" && !ok[path] {
			t.Errorf("fitness's build depends on %s", path)
		}
	}
}

// TestShedGaming: a half that sheds all but one request does not keep
// full latency and CPU marks for the one it served.
func TestShedGaming(t *testing.T) {
	p := DefaultParams()
	recs := []wire.Record{succ(10 * time.Millisecond)}
	for range 99 {
		recs = append(recs, rec("hash", "graceful_shed", "", time.Millisecond))
	}
	gamer, err := Score(p, recs, 0.001)
	if err != nil {
		t.Fatal(err)
	}
	honest, _ := Score(p, []wire.Record{succ(100 * time.Millisecond), succ(120 * time.Millisecond)}, 0.2)
	if gamer.Score >= 0.25 || honest.Score <= 0.8 || gamer.Score >= honest.Score {
		t.Errorf("shed-everything scores %v, an honest half %v", gamer.Score, honest.Score)
	}
}

func TestParseResourceLimits(t *testing.T) {
	line := "pod_cpu_usage_seconds_total{namespace=\"a\",pod=\"b\"} 1\n"
	if _, err := parseResource(strings.NewReader(line), int64(len(line))); err != nil {
		t.Errorf("input of exactly the cap: %v", err)
	}
	if _, err := parseResource(strings.NewReader(line), int64(len(line))-1); err == nil {
		t.Error("input one byte over the cap was accepted")
	}
	long := "# " + strings.Repeat("x", 2<<20) + "\n" + line
	if _, err := ParseResource(strings.NewReader(long)); err == nil {
		t.Error("a line over the scanner's cap was accepted")
	}
}
