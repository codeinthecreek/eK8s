package stallrun

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestDeps holds the package to the standard library, loadgen/wire and
// internal/classifier (doc comment), over the whole build: both are
// stdlib-only themselves, so anything else in the list came in here.
// go list -deps without -test leaves out test-only imports, so this
// file's os/exec is not counted.
func TestDeps(t *testing.T) {
	const mod = "github.com/codeinthecreek/eK8s"
	const self = mod + "/internal/stallrun"
	allowed := map[string]bool{
		self:                         true,
		mod + "/loadgen/wire":        true,
		mod + "/internal/classifier": true,
	}
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", self)
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
		case path == self:
			listed = true
		case std != "true" && !allowed[path]:
			bad = append(bad, path)
		}
	}
	if !listed {
		t.Fatalf("go list output does not list %s:\n%s", self, out)
	}
	if len(bad) > 0 {
		t.Errorf("%s depends on packages it must not: %s", self, strings.Join(bad, ", "))
	}
}
