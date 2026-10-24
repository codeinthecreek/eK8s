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

// TestImports checks that this package's non-test files import the
// standard library and internal/hashcore only. TestDeps alone would pass
// a direct import of hashcore's Argon2 package, which its build lists.
func TestImports(t *testing.T) {
	core := modulePath + "/internal/hashcore"
	self := modulePath + "/internal/hostpairs"
	deps := listDeps(t, self)
	var bad []string
	for _, path := range strings.Split(strings.TrimSpace(goList(t, "-f", `{{join .Imports "\n"}}`, self)), "\n") {
		if path != core && !deps[path] {
			bad = append(bad, path)
		}
	}
	if len(bad) > 0 {
		t.Errorf("hostpairs imports packages outside the standard library and internal/hashcore: %s", strings.Join(bad, ", "))
	}
}

// listDeps returns go list -deps of pkg: each package's import path and
// whether it is in the standard library.
func listDeps(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	deps := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(goList(t, "-deps", "-f", "{{.ImportPath}} {{.Standard}}", pkg)), "\n") {
		path, std, _ := strings.Cut(line, " ")
		deps[path] = std == "true"
	}
	return deps
}

// goList returns go list's output for args, with loadgen's TestBuildDeps
// settings.
func goList(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	return string(out)
}
