package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/driver"
)

// cniClock is the fixed host clock these tests give the loop.
var cniClock = time.Date(2026, 11, 4, 5, 6, 7, 800, time.UTC)

func fixedClock() time.Time { return cniClock }

// cniFiles lists the run's cni/ directory, sorted; nil when it is absent.
func cniFiles(t *testing.T, fx *loopFixture) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(fx.run.Dir(), "cni"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func cniFile(t *testing.T, fx *loopFixture, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fx.run.Dir(), "cni", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// after reports whether an event b comes after the first event a.
func after(ev []string, a, b string) bool {
	i := slices.Index(ev, a)
	return i >= 0 && slices.Contains(ev[i+1:], b)
}

var cniAtClock = "cni " + cniClock.Format(time.RFC3339Nano)

// TestLoopCNIBurst: a Stage 5 burst that was read, on a pass, a
// failure or a fault after it, is archived as cni/<run_id>.log from the
// host time just before Stage 5, and before a fault halts the run; a
// Stage 5 fault before the burst was read archives nothing.
func TestLoopCNIBurst(t *testing.T) {
	const burst1 = "gate-1-ek8s-0123456789abcdef"
	for _, tc := range []struct {
		name  string
		set   map[string]bool
		stop  string
		files []string
	}{
		{"pass", nil, StopGenerations, []string{"gate-0-ek8s-0123456789abcdef.log", burst1 + ".log"}},
		{"fail", failStage5, StopGenerations, []string{"gate-0-ek8s-0123456789abcdef.log", burst1 + ".log"}},
		{"fault-after-burst", faultStage5, StopFault, []string{"gate-0-ek8s-0123456789abcdef.log", burst1 + ".log"}},
		{"fault-before-burst", faultBeforeBurst, StopFault, []string{"gate-0-ek8s-0123456789abcdef.log"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLoopFixture(t, 1, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
				return edit(parent, "gen1", 3), nil
			})
			fx.win.score = map[string]float64{"0": 0.5, "1": 0.6}
			if tc.set != nil {
				tc.set["1"] = true
			}
			reason, err := fx.startAt(t, context.Background(), fixedClock)
			if reason != tc.stop || (err != nil) != (tc.stop == StopFault) {
				t.Fatalf("Run = %q, %v; want %q", reason, err, tc.stop)
			}
			if got := cniFiles(t, fx); !slices.Equal(got, tc.files) {
				t.Fatalf("cni/ = %q, want %q", got, tc.files)
			}
			if got := cniFile(t, fx, "gate-0-ek8s-0123456789abcdef.log"); got != string(fx.cni.out) {
				t.Errorf("burst log = %q, want the read's bytes", got)
			}
			ev := fx.ev.all()
			reads := 0
			for i, e := range ev {
				if strings.HasPrefix(e, "cni ") {
					reads++
					if e != cniAtClock || !strings.HasPrefix(ev[i-1], "stage5 ") {
						t.Errorf("event %d %q after %q, want %q right after Stage 5", i, e, ev[i-1], cniAtClock)
					}
				}
			}
			if reads != len(tc.files) {
				t.Errorf("%d reads, want %d: %q", reads, len(tc.files), ev)
			}
		})
	}
}

// TestLoopCNIWindow: a window whose baseline half ran is archived as
// cni/<RunID>.log from the baseline half's start, after its stalls
// lines and before the restart check halts the run; a window with no
// baseline span is not read.
func TestLoopCNIWindow(t *testing.T) {
	t.Run("scored", func(t *testing.T) {
		fx := newLoopFixture(t, 0, nil)
		fx.win.score = map[string]float64{"0": 0.5}
		fx.win.window = func(n int, live []string) WindowResult { return stallWindow(n, live, fx.win.score) }
		if reason, err := fx.start(t); err != nil || reason != StopGenerations {
			t.Fatalf("Run = %q, %v", reason, err)
		}
		if got, want := cniFiles(t, fx), []string{"gate-0-ek8s-0123456789abcdef.log", "w-1.log"}; !slices.Equal(got, want) {
			t.Fatalf("cni/ = %q, want %q", got, want)
		}
		if !after(fx.ev.all(), "window 0", "cni "+spanT0.UTC().Format(time.RFC3339Nano)) {
			t.Errorf("no read from the baseline half's start after the window: %q", fx.ev.all())
		}
	})
	t.Run("restarted", func(t *testing.T) {
		fx := newLoopFixture(t, 0, nil)
		fx.win.score = map[string]float64{"0": 0.5}
		fx.win.window = func(n int, live []string) WindowResult {
			res := stallWindow(n, live, fx.win.score)
			fx.win.restarted = true
			res.Excluded, res.StressRunID, res.StressSpan = []string{ExcludedRestart}, "", Span{}
			return res
		}
		if reason, err := fx.start(t); err != nil || reason != StopRestart {
			t.Fatalf("Run = %q, %v; want %q", reason, err, StopRestart)
		}
		if got := cniFiles(t, fx); !slices.Contains(got, "w-1.log") {
			t.Errorf("cni/ = %q, want w-1.log before the restart halt", got)
		}
	})
	t.Run("no-baseline-span", func(t *testing.T) {
		fx := newLoopFixture(t, 0, nil)
		fx.win.score = map[string]float64{"0": 0.5} // scoredWindow: no spans
		if reason, err := fx.start(t); err != nil || reason != StopGenerations {
			t.Fatalf("Run = %q, %v", reason, err)
		}
		if got, want := cniFiles(t, fx), []string{"gate-0-ek8s-0123456789abcdef.log"}; !slices.Equal(got, want) {
			t.Errorf("cni/ = %q, want only the burst's %q", got, want)
		}
	})
}

// TestLoopCNIWindowFault: a window fault is archived as
// cni/fault-<unix seconds>.log from the host time just before the
// window, before the fault returns.
func TestLoopCNIWindowFault(t *testing.T) {
	fx := newLoopFixture(t, 0, nil)
	fx.win.score = map[string]float64{"0": 0.5}
	fx.win.fault = func(int) error { return errors.New("loadgen gone") }
	reason, err := fx.startAt(t, context.Background(), fixedClock)
	if reason != StopFault || err == nil || !strings.Contains(err.Error(), "loadgen gone") {
		t.Fatalf("Run = %q, %v; want the window fault", reason, err)
	}
	name := fmt.Sprintf("fault-%d.log", cniClock.Unix())
	if got, want := cniFiles(t, fx), []string{name, "gate-0-ek8s-0123456789abcdef.log"}; !slices.Equal(got, want) {
		t.Fatalf("cni/ = %q, want %q", got, want)
	}
	if got := cniFile(t, fx, name); got != string(fx.cni.out) {
		t.Errorf("%s = %q", name, got)
	}
	if ev := fx.ev.all(); !after(ev, "window 0", cniAtClock) {
		t.Errorf("no read from the window's start after its fault: %q", ev)
	}
}

// TestLoopCNIRecordingOnly: a failed read writes cni/<name>.failed and a
// failed write is one log line; either way the run decides exactly as
// it does when every read and write succeeds.
func TestLoopCNIRecordingOnly(t *testing.T) {
	run := func(t *testing.T, readErr error, breakDir bool) (*loopFixture, string, string) {
		fx := newLoopFixture(t, 2, func(n int, parent map[string][]byte) (*driver.Mutation, error) {
			return edit(parent, fmt.Sprintf("gen%d", n), 3), nil
		})
		fx.win.score = map[string]float64{"0": 0.5, "1": 0.7}
		fx.win.window = func(n int, live []string) WindowResult { return stallWindow(n, live, fx.win.score) }
		failStage5["2"] = true
		fx.cni.err = readErr
		if breakDir {
			// A file where cni/ goes: every write fails.
			if err := os.WriteFile(filepath.Join(fx.run.Dir(), "cni"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var log bytes.Buffer
		fx.cfg.Log = &log
		reason, err := fx.start(t)
		if err != nil || reason != StopGenerations {
			t.Fatalf("Run = %q, %v\n%s", reason, err, log.String())
		}
		return fx, decisions(t, fx, reason), log.String()
	}
	// 3 bursts (gen 0, 1, 2) and 2 windows.
	names := []string{"gate-0-ek8s-0123456789abcdef", "gate-1-ek8s-0123456789abcdef", "gate-2-ek8s-0123456789abcdef", "w-1", "w-2"}

	fx, ok, log := run(t, nil, false)
	var want []string
	for _, n := range names {
		want = append(want, n+".log")
	}
	slices.Sort(want)
	if got := cniFiles(t, fx); !slices.Equal(got, want) {
		t.Errorf("cni/ = %q, want %q", got, want)
	}
	if strings.Contains(log, " cni: ") {
		t.Errorf("log reports a cni error:\n%s", log)
	}

	fx, failed, flog := run(t, errors.New("kubectl: exit status 1: pods not found"), false)
	if failed != ok {
		t.Errorf("decisions with failing reads\n%s\ndiffer from\n%s", failed, ok)
	}
	want = nil
	for _, n := range names {
		want = append(want, n+".failed")
	}
	slices.Sort(want)
	if got := cniFiles(t, fx); !slices.Equal(got, want) {
		t.Errorf("cni/ = %q, want %q", got, want)
	}
	if got := cniFile(t, fx, "w-1.failed"); got != "kubectl: exit status 1: pods not found\n" {
		t.Errorf("w-1.failed = %q", got)
	}
	if n := strings.Count(flog, " cni: "); n != len(names) || strings.Count(flog, "log not read") != len(names) {
		t.Errorf("%d cni lines, want %d 'log not read':\n%s", n, len(names), flog)
	}

	_, broken, blog := run(t, nil, true)
	if broken != ok {
		t.Errorf("decisions with failing writes\n%s\ndiffer from\n%s", broken, ok)
	}
	if n := strings.Count(blog, " cni: "); n != len(names) || strings.Count(blog, "log not recorded") != len(names) {
		t.Errorf("%d cni lines, want %d 'log not recorded':\n%s", n, len(names), blog)
	}
}

// TestLoopCNIInterrupted: once the caller's context has ended, a window
// fault reads no log.
func TestLoopCNIInterrupted(t *testing.T) {
	fx := newLoopFixture(t, 0, nil)
	fx.win.score = map[string]float64{"0": 0.5}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.win.fault = func(int) error { cancel(); return context.Canceled }
	reason, err := fx.startAt(t, ctx, fixedClock)
	if reason != StopInterrupted || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %q, %v; want interrupted", reason, err)
	}
	if got, want := cniFiles(t, fx), []string{"gate-0-ek8s-0123456789abcdef.log"}; !slices.Equal(got, want) {
		t.Errorf("cni/ = %q, want only the burst's %q", got, want)
	}
	if ev := fx.ev.all(); after(ev, "window 0", cniAtClock) {
		t.Errorf("a read after the interrupt: %q", ev)
	}
}
