package classifier

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestStdlibOnly enforces doc.go's stdlib-only rule over the whole build,
// not just direct imports: the wrapper's Stage 1 allowlist checks only
// the wrapper's own direct imports, so a non-stdlib package reached
// through this one would be in every wrapper build with no check firing.
// go list -deps without -test leaves out test-only imports, so this
// file's os/exec is not counted. The settings are the deployed build's
// (gate/assemble.go).
func TestStdlibOnly(t *testing.T) {
	const self = "github.com/codeinthecreek/eK8s/internal/classifier"
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
		case std != "true":
			bad = append(bad, path)
		}
	}
	if !listed {
		t.Fatalf("go list output does not list %s:\n%s", self, out)
	}
	if len(bad) > 0 {
		t.Errorf("%s depends on packages outside the standard library: %s", self, strings.Join(bad, ", "))
	}
}
