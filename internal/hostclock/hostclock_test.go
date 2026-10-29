package hostclock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake writes a sysfs tree: files maps a path under the root to its text.
func fake(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, text := range files {
		full := filepath.Join(root, "devices", "system", "cpu", p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func pinned() map[string]string {
	return map[string]string{
		"cpu0/cpufreq/scaling_governor":              "performance\n",
		"cpu1/cpufreq/scaling_governor":              "performance\n",
		"cpu10/cpufreq/scaling_governor":             "performance\n",
		"cpu0/cpufreq/scaling_driver":                "intel_pstate\n",
		"cpu0/cpufreq/energy_performance_preference": "performance\n",
		"intel_pstate/no_turbo":                      "1\n",
		"cpufreq/policy0/scaling_governor":           "powersave\n", // not a cpuN directory
		"cpu0/cpufreq/scaling_available_governors":   "performance powersave\n",
		"cpuidle/current_governor":                   "menu\n",
		"cpu1/cpufreq/energy_performance_preference": "balance_performance\n", // only cpu0's is read
	}
}

func TestReadPinned(t *testing.T) {
	st, err := Read(fake(t, pinned()))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Governors) != 3 || st.Governors["cpu10"] != "performance" {
		t.Errorf("governors %v, want cpu0, cpu1 and cpu10 as performance", st.Governors)
	}
	if st.Driver == nil || *st.Driver != "intel_pstate" || st.EPP == nil || *st.EPP != "performance" {
		t.Errorf("driver %v, epp %v", st.Driver, st.EPP)
	}
	if st.NoTurbo == nil || *st.NoTurbo != "1" || st.Boost != nil {
		t.Errorf("no_turbo %v, boost %v; want 1 and absent", st.NoTurbo, st.Boost)
	}
	if why := st.Unpinned(); len(why) != 0 {
		t.Errorf("Unpinned() = %v, want none", why)
	}
}

func TestReadAbsent(t *testing.T) {
	st, err := Read(fake(t, map[string]string{"cpu0/cpufreq/scaling_governor": "performance"}))
	if err != nil {
		t.Fatal(err)
	}
	if st.Driver != nil || st.EPP != nil || st.NoTurbo != nil || st.Boost != nil {
		t.Errorf("absent files read as present: %+v", st)
	}
	if why := st.Unpinned(); len(why) != 0 {
		t.Errorf("a missing turbo switch is not a reason: %v", why)
	}
	// An empty root has no governor at all: nothing was checked.
	st, err = Read(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if why := st.Unpinned(); len(why) != 1 || !strings.Contains(why[0], "no cpufreq") {
		t.Errorf("empty root: %v, want the no-governor reason", why)
	}
}

func TestUnpinned(t *testing.T) {
	tests := []struct {
		name   string
		change map[string]string
		want   []string // substrings, one reason each, in order
	}{
		{"one governor", map[string]string{"cpu1/cpufreq/scaling_governor": "powersave\n"}, []string{`cpu1 governor "powersave"`}},
		{"governors in cpu order", map[string]string{"cpu10/cpufreq/scaling_governor": "schedutil", "cpu1/cpufreq/scaling_governor": "schedutil"},
			[]string{"cpu1 governor", "cpu10 governor"}},
		{"turbo on", map[string]string{"intel_pstate/no_turbo": "0\n"}, []string{`no_turbo "0"`}},
		{"boost on", map[string]string{"cpufreq/boost": "1\n"}, []string{`boost "1"`}},
		{"boost off", map[string]string{"cpufreq/boost": "0\n"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := pinned()
			for k, v := range tt.change {
				files[k] = v
			}
			st, err := Read(fake(t, files))
			if err != nil {
				t.Fatal(err)
			}
			why := st.Unpinned()
			if len(why) != len(tt.want) {
				t.Fatalf("Unpinned() = %q, want %d reasons", why, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(why[i], w) {
					t.Errorf("reason %d = %q, want it to contain %q", i, why[i], w)
				}
			}
		})
	}
}

// TestReadError: a file that exists but cannot be read is an error, not
// an absent value.
func TestReadError(t *testing.T) {
	root := fake(t, map[string]string{"cpu0/cpufreq/scaling_governor": "performance"})
	// A directory where scaling_driver should be a file.
	if err := os.MkdirAll(filepath.Join(root, "devices", "system", "cpu", "cpu0", "cpufreq", "scaling_driver"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st, err := Read(root); err == nil {
		t.Errorf("Read = %+v, want an error", st)
	}
}

func TestPreflight(t *testing.T) {
	st, err := Preflight(fake(t, pinned()), false)
	if err != nil || len(st.Governors) != 3 {
		t.Fatalf("pinned: %+v, %v; want accepted with 3 governors", st, err)
	}
	files := pinned()
	files["cpu1/cpufreq/scaling_governor"] = "powersave\n"
	root := fake(t, files)
	_, err = Preflight(root, false)
	if err == nil || !strings.Contains(err.Error(), "-allow-unpinned") || !strings.Contains(err.Error(), "cpu1") {
		t.Errorf("unpinned without allow: %v, want a refusal naming cpu1 and -allow-unpinned", err)
	}
	st, err = Preflight(root, true)
	if err != nil || st.Governors["cpu1"] != "powersave" {
		t.Errorf("unpinned with allow: %+v, %v; want the state read, accepted", st, err)
	}
}
