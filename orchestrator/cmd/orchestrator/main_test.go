package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestReadWrapper: generation 0 is the wrapper's non-test .go files,
// exactly the files the gate takes.
func TestReadWrapper(t *testing.T) {
	files, err := readWrapper("../../../internal/wrapper")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"doc.go", "hasher.go", "server.go", "tunables.go"}) {
		t.Errorf("generation 0 = %q", names)
	}
	if _, err := readWrapper(t.TempDir()); err == nil {
		t.Error("a directory with no hasher.go was read")
	}
}

// TestRunRefusesUnpinnedFirst: run reads the host clocks before it
// touches the run directory or the cluster, so an unpinned host fails
// here with no kubectl and leaves no run directory.
func TestRunRefusesUnpinnedFirst(t *testing.T) {
	sysfs := t.TempDir()
	gov := filepath.Join(sysfs, "devices", "system", "cpu", "cpu0", "cpufreq", "scaling_governor")
	if err := os.MkdirAll(filepath.Dir(gov), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gov, []byte("powersave\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := config{runDir: filepath.Join(t.TempDir(), "run"), repo: "../../..", kubeCtx: "kind-ek8s", sysfs: sysfs}
	_, err := run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "host clocks are not pinned") {
		t.Errorf("run = %v, want the unpinned refusal", err)
	}
	if _, serr := os.Lstat(c.runDir); serr == nil {
		t.Error("a run directory was created before the clock check")
	}
}
