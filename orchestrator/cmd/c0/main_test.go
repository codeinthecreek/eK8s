package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/internal/classifier"
)

func TestPassRule(t *testing.T) {
	quarter := classifier.HangTimeout / 4
	tests := []struct {
		name string
		st   fitness.Stats
		pass bool
		why  string
	}{
		{"clean", fitness.Stats{Requests: 1000, Successes: 1000, P95Latency: 100 * time.Millisecond}, true, ""},
		{"exactly 1% of sheds and failures", fitness.Stats{Requests: 1000, Successes: 990, Sheds: 6, Failures: 4, P95Latency: quarter}, true, ""},
		{"just over 1%", fitness.Stats{Requests: 1000, Successes: 989, Sheds: 6, Failures: 5, P95Latency: quarter}, false, "shed+failed 11 of 1000"},
		{"failures alone count", fitness.Stats{Requests: 100, Successes: 98, Failures: 2, P95Latency: quarter}, false, "shed+failed 2 of 100"},
		{"p95 over a quarter of the hang timeout", fitness.Stats{Requests: 1000, Successes: 1000, P95Latency: quarter + 1}, false, "p95"},
		{"both", fitness.Stats{Requests: 100, Successes: 90, Sheds: 10, P95Latency: classifier.HangTimeout}, false, "shed+failed 10 of 100 requests (10.00%) over 1%; p95"},
		{"no requests", fitness.Stats{}, false, "no requests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pass, why := passRule(tt.st)
			if pass != tt.pass || !strings.HasPrefix(why, tt.why) {
				t.Errorf("passRule = %v, %q; want %v, %q...", pass, why, tt.pass, tt.why)
			}
		})
	}
}

func TestParseLadder(t *testing.T) {
	for in, want := range map[string][]int{"6,9,12,15,18,19": {6, 9, 12, 15, 18, 19}, "7": {7}, " 6 , 9": {6, 9}} {
		if got, err := parseLadder(in); err != nil || !slices.Equal(got, want) {
			t.Errorf("parseLadder(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "6,,9", "6,6", "9,6", "0,6", "-1", "6.5", "a"} {
		if got, err := parseLadder(in); err == nil {
			t.Errorf("parseLadder(%q) = %v, want an error", in, got)
		}
	}
}

// TestCheckRates: loadgen's own validation refuses a rate over its
// maxRate (19), so the ladder default passes and 20 does not.
func TestCheckRates(t *testing.T) {
	if err := checkRates([]int{6, 9, 12, 15, 18, 19}); err != nil {
		t.Errorf("default ladder: %v", err)
	}
	err := checkRates([]int{6, 20})
	if err == nil || !strings.Contains(err.Error(), "20") {
		t.Errorf("ladder over maxRate: %v, want an error naming 20", err)
	}
}

func TestVerdictResult(t *testing.T) {
	tests := []struct {
		v    Verdict
		c0   string
		r    string
		halt bool
	}{
		{Verdict{Kind: Resolved, C0: 13, Pass: 13, Fail: 14}, "13", "6.5", false},
		{Verdict{Kind: Resolved, C0: 12, Pass: 12, Fail: 13}, "12", "6", false},
		{Verdict{Kind: AtLeast, C0: 19, Pass: 19}, ">= 19", "9.5", false},
		{Verdict{Kind: Unresolved, Rate: 13, Pass: 13, Fail: 14}, "unresolved", "", true},
		{Verdict{Kind: LadderTooHigh, Fail: 6}, "unresolved", "", true},
		{Verdict{Kind: InvalidTwice, Rate: 9}, "unresolved", "", true},
	}
	for _, tt := range tests {
		got := verdictResult(tt.v)
		if got.C0 != tt.c0 || got.R != tt.r || tt.v.Halted() != tt.halt {
			t.Errorf("%s: result %+v, halted %v; want c0 %q r %q halted %v", tt.v.Kind, got, tt.v.Halted(), tt.c0, tt.r, tt.halt)
		}
	}
}
