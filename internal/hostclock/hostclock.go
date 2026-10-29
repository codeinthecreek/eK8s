// Package hostclock reads the host's CPU clock policy from sysfs: the
// cpufreq governor of every CPU, the scaling driver, the energy
// performance preference and the turbo switches. Unpinned clocks moved a
// variant's CPU per success 2x between runs (docs/decisions.md, 2026-10-29),
// so c0 records the state and refuses to measure on an unpinned host.
// Stdlib-only, so the orchestrator can reuse it.
package hostclock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultRoot is where sysfs is mounted.
const DefaultRoot = "/sys"

// State is what Read found. A file that does not exist leaves its field
// empty (nil, or no map entries) and omitted from JSON, so a host without
// intel_pstate or cpufreq reads as absent, not as a value.
type State struct {
	// Governors maps "cpu0", "cpu1", ... to cpufreq/scaling_governor.
	Governors map[string]string `json:"governors,omitempty"`
	// Driver is cpu0's cpufreq/scaling_driver.
	Driver *string `json:"scaling_driver,omitempty"`
	// EPP is cpu0's cpufreq/energy_performance_preference.
	EPP *string `json:"energy_performance_preference,omitempty"`
	// NoTurbo is intel_pstate/no_turbo, Boost is cpufreq/boost, both as
	// the file's trimmed text ("1", "0").
	NoTurbo *string `json:"intel_pstate_no_turbo,omitempty"`
	Boost   *string `json:"cpufreq_boost,omitempty"`
}

// Read reads the state under root (DefaultRoot on a real host). A file
// that is missing is recorded as absent; any other read error is returned.
func Read(root string) (State, error) {
	cpu := filepath.Join(root, "devices", "system", "cpu")
	var s State
	dirs, err := filepath.Glob(filepath.Join(cpu, "cpu[0-9]*"))
	if err != nil {
		return State{}, err
	}
	for _, d := range dirs {
		name := filepath.Base(d)
		if !cpuName.MatchString(name) {
			continue
		}
		v, err := readOpt(filepath.Join(d, "cpufreq", "scaling_governor"))
		if err != nil {
			return State{}, err
		}
		if v != nil {
			if s.Governors == nil {
				s.Governors = make(map[string]string)
			}
			s.Governors[name] = *v
		}
	}
	for _, f := range []struct {
		dst  **string
		path string
	}{
		{&s.Driver, filepath.Join(cpu, "cpu0", "cpufreq", "scaling_driver")},
		{&s.EPP, filepath.Join(cpu, "cpu0", "cpufreq", "energy_performance_preference")},
		{&s.NoTurbo, filepath.Join(cpu, "intel_pstate", "no_turbo")},
		{&s.Boost, filepath.Join(cpu, "cpufreq", "boost")},
	} {
		if *f.dst, err = readOpt(f.path); err != nil {
			return State{}, err
		}
	}
	return s, nil
}

var cpuName = regexp.MustCompile(`^cpu[0-9]+$`)

// readOpt returns the file's text with surrounding space trimmed, or nil
// if the file does not exist.
func readOpt(path string) (*string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := strings.TrimSpace(string(b))
	return &v, nil
}

// Unpinned lists why the clocks are not pinned, empty when they are: every
// governor must be "performance" (and at least one must be readable, or
// nothing was checked), and turbo must be off where readable (no_turbo =
// 1, boost = 0). A turbo switch that is absent is not a reason.
func (s State) Unpinned() []string {
	var why []string
	if len(s.Governors) == 0 {
		why = append(why, "no cpufreq scaling_governor readable")
	}
	names := make([]string, 0, len(s.Governors))
	for n := range s.Governors {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return cpuNum(names[i]) < cpuNum(names[j]) })
	for _, n := range names {
		if g := s.Governors[n]; g != "performance" {
			why = append(why, fmt.Sprintf("%s governor %q, want \"performance\"", n, g))
		}
	}
	if s.NoTurbo != nil && *s.NoTurbo != "1" {
		why = append(why, fmt.Sprintf("intel_pstate no_turbo %q, want \"1\"", *s.NoTurbo))
	}
	if s.Boost != nil && *s.Boost != "0" {
		why = append(why, fmt.Sprintf("cpufreq boost %q, want \"0\"", *s.Boost))
	}
	return why
}

// cpuNum is the number in "cpu12".
func cpuNum(name string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(name, "cpu"))
	return n
}
