package loadgen

import (
	"math"
	"testing"
	"time"
)

func TestWindowSendsAndRingCapacity(t *testing.T) {
	for _, tc := range []struct {
		r        float64
		w        time.Duration
		maxBurst int
		want     int // WindowSends; RingCapacity is want + maxBurst
	}{
		{2, 5 * time.Minute, 100, 600},
		{2.5, time.Second, 0, 3},  // a half rounds away from zero
		{1.5, time.Second, 7, 2},  // likewise
		{1.49, time.Second, 1, 1}, // rounds down
		{0.5, time.Second, 0, 1},  // the smallest that rounds to one send
		{0.1, 30 * time.Second, 5, 3},
		{3, 1500 * time.Millisecond, 2, 5}, // 4.5 rounds up
		{maxRecords, time.Second, 0, maxRecords},
		{maxRecords - 1, time.Second, 1, maxRecords - 1},
	} {
		n, err := WindowSends(tc.r, tc.w)
		if err != nil || n != tc.want {
			t.Errorf("WindowSends(%v, %v) = %d, %v; want %d, nil", tc.r, tc.w, n, err, tc.want)
		}
		c, err := RingCapacity(tc.r, tc.w, tc.maxBurst)
		if err != nil || c != tc.want+tc.maxBurst {
			t.Errorf("RingCapacity(%v, %v, %d) = %d, %v; want %d, nil", tc.r, tc.w, tc.maxBurst, c, err, tc.want+tc.maxBurst)
		}
	}
}

func TestWindowSendsAndRingCapacityErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		r        float64
		w        time.Duration
		maxBurst int
	}{
		{"r zero", 0, time.Minute, 1},
		{"r negative", -1, time.Minute, 1},
		{"w zero", 2, 0, 1},
		{"w negative", 2, -time.Minute, 1},
		{"rounds to zero", 0.4, time.Second, 1},
		{"rounds to zero, tiny", 1e-9, time.Minute, 100},
		{"r NaN", math.NaN(), time.Minute, 1},
		{"r +Inf", math.Inf(1), time.Minute, 1},
		{"over maxRecords", maxRecords + 1, time.Second, 0},
		{"huge r", 1e300, time.Hour, 0},
	} {
		if n, err := WindowSends(tc.r, tc.w); err == nil {
			t.Errorf("%s: WindowSends(%v, %v) = %d, nil; want an error", tc.name, tc.r, tc.w, n)
		}
		if c, err := RingCapacity(tc.r, tc.w, tc.maxBurst); err == nil {
			t.Errorf("%s: RingCapacity(%v, %v, %d) = %d, nil; want an error", tc.name, tc.r, tc.w, tc.maxBurst, c)
		}
	}
	for _, maxBurst := range []int{-1, maxRecords - 119, math.MaxInt} {
		if c, err := RingCapacity(2, time.Minute, maxBurst); err == nil {
			t.Errorf("RingCapacity(2, 1m, %d) = %d, nil; want an error", maxBurst, c)
		}
	}
}
