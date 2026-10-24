package hostpairs

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/codeinthecreek/eK8s"

// TestDeps checks that this package's build depends, directly or
// transitively, on the standard library and internal/hashcore only:
// every other package it lists must be one hashcore's own build lists
// (its Argon2 implementation). It lists with loadgen's TestBuildDeps
// settings.
func TestDeps(t *testing.T) {
	core := modulePath + "/internal/hashcore"
	self := modulePath + "/internal/hostpairs"
	allowed := make(map[string]bool)
	for path, std := range listDeps(t, core) {
		if !std {
			allowed[path] = true
		}
	}
	if !allowed[core] {
		t.Fatalf("go list -deps %s does not list it", core)
	}
	deps := listDeps(t, self)
	if _, ok := deps[self]; !ok {
		t.Fatalf("go list -deps %s does not list it", self)
	}
	var bad []string
	for path, std := range deps {
		if !std && path != self && !allowed[path] {
			bad = append(bad, path)
		}
	}
	if len(bad) > 0 {
		t.Errorf("hostpairs's build depends on packages outside the standard library and internal/hashcore's build: %s", strings.Join(bad, ", "))
	}
}

// listDeps returns go list -deps of pkg: each package's import path and
// whether it is in the standard library.
func listDeps(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	deps := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, std, _ := strings.Cut(line, " ")
		deps[path] = std == "true"
	}
	return deps
}
