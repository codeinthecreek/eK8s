package loadgen

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/codeinthecreek/eK8s"

// forbiddenDep reports whether loadgen's build must never depend on path
// (DESIGN.md sec 10). net/http/pprof and expvar register handlers on
// http.DefaultServeMux when imported, so they are refused too: by exact
// path, since the runtime itself imports internal/runtime/pprof/label.
func forbiddenDep(path string) bool {
	if path == "net/http/pprof" || path == "expvar" {
		return true
	}
	for _, p := range []string{
		modulePath + "/internal/hashcore",
		modulePath + "/internal/classifier/testvectors",
		modulePath + "/gate",
	} {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	// Any Argon2 package by name: github.com/alexedwards/argon2id,
	// golang.org/x/crypto/argon2 and any other.
	return strings.Contains(strings.ToLower(path), "argon2")
}

func TestForbiddenDep(t *testing.T) {
	for path, want := range map[string]bool{
		modulePath + "/internal/hashcore":                 true,
		modulePath + "/internal/classifier/testvectors":   true,
		modulePath + "/internal/classifier/testvectors/x": true,
		modulePath + "/gate":                              true,
		modulePath + "/gate/cmd/gate":                     true,
		"github.com/alexedwards/argon2id":                 true,
		"golang.org/x/crypto/argon2":                      true,
		"net/http/pprof":                                  true,
		"expvar":                                          true,
		"internal/runtime/pprof/label":                    false,
		"runtime/pprof":                                   false,
		"net/http":                                        false,
		modulePath + "/internal/classifier":               false,
		modulePath + "/gateway":                           false,
		modulePath + "/loadgen":                           false,
		"golang.org/x/crypto/blake2b":                     false,
		"crypto/rand":                                     false,
	} {
		if got := forbiddenDep(path); got != want {
			t.Errorf("forbiddenDep(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestBuildDeps checks that no package loadgen's build depends on,
// directly or transitively, is forbidden. go list -deps without -test
// lists only the build's packages, so test-only imports (hashcore in
// hashcore_test.go) are not counted, as DESIGN.md sec 10 allows.
//
// GOOS and cgo select which files, and so which imports, are built: the
// loadgen image build (loadgen series PR 5) must use these same settings,
// CGO_ENABLED=0 and GOOS=linux, or this test does not cover it.
func TestBuildDeps(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", modulePath+"/loadgen/...")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	listed := false
	var bad []string
	for _, path := range strings.Fields(string(out)) {
		listed = listed || path == modulePath+"/loadgen"
		if forbiddenDep(path) {
			bad = append(bad, path)
		}
	}
	if !listed {
		t.Fatalf("go list output does not list %s/loadgen:\n%s", modulePath, out)
	}
	if len(bad) > 0 {
		t.Errorf("loadgen's build depends on forbidden packages: %s", strings.Join(bad, ", "))
	}
}

// TestWireDeps checks that loadgen/wire's build depends, directly or
// transitively, on the standard library only, so that any client of the
// control API can import it. It lists with TestBuildDeps's settings.
func TestWireDeps(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", modulePath+"/loadgen/wire")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	listed := false
	var bad []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, std, _ := strings.Cut(line, " ")
		switch {
		case path == modulePath+"/loadgen/wire":
			listed = true
		case std != "true":
			bad = append(bad, path)
		}
	}
	if !listed {
		t.Fatalf("go list output does not list %s/loadgen/wire:\n%s", modulePath, out)
	}
	if len(bad) > 0 {
		t.Errorf("loadgen/wire's build depends on packages outside the standard library: %s", strings.Join(bad, ", "))
	}
}
