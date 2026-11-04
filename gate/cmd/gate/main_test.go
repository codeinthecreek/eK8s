package main

import (
	"regexp"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/stallrun"
)

func TestStallLine(t *testing.T) {
	tests := []struct {
		name string
		s    stallrun.Stats
		want string
	}{
		{"no OHT record: zero start wall", stallrun.Stats{Records: 240, LongestStartIndex: -1, Threshold: 16},
			"stall: records 240 oht 0 stall-runs 0 tail 0 longest 0 start-index -1 start-wall - threshold 16"},
		{"a stall run, start wall in UTC", stallrun.Stats{
			Records: 130, OHT: 124, StallRuns: 1, TailRun: 0, Longest: 124, LongestStartIndex: 0,
			LongestStartWall: time.Date(2026, 11, 4, 13, 0, 0, 500000000, time.FixedZone("UTC+1", 3600)), Threshold: 124},
			"stall: records 130 oht 124 stall-runs 1 tail 0 longest 124 start-index 0 start-wall 2026-11-04T12:00:00.5Z threshold 124"},
		{"a tail run", stallrun.Stats{
			Records: 30, OHT: 3, TailRun: 3, Longest: 3, LongestStartIndex: 27,
			LongestStartWall: time.Date(2026, 11, 4, 12, 0, 27, 0, time.UTC), Threshold: 60},
			"stall: records 30 oht 3 stall-runs 0 tail 3 longest 3 start-index 27 start-wall 2026-11-04T12:00:27Z threshold 60"},
	}
	// deploy/stage5-smoke.sh's PASS check, which the line must not match.
	pass := regexp.MustCompile(`^stage 5 [a-z-]+ +PASS$`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stallLine(tt.s)
			if got != tt.want {
				t.Errorf("stallLine\n got  %q\n want %q", got, tt.want)
			}
			if pass.MatchString(got) {
				t.Errorf("%q matches the smoke's PASS pattern", got)
			}
		})
	}
}
