package main

import (
	"slices"
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
