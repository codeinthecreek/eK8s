package gate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// hasherSHA256 is the SHA-256 of the canonical internal/wrapper/hasher.go
// (DESIGN.md sec 5). TestRepoHasherMatchesPin keeps it equal to the
// repo's file, so changing hasher.go takes a deliberate gate edit; the
// gate never trusts a working-tree copy that could have been overwritten.
const hasherSHA256 = "4fd2eab67d385f557e91d7dd15871879822139aabc9459da6a4a4dd88d289d3f"

// modulePath is this repo's module path (go.mod).
const modulePath = "github.com/codeinthecreek/eK8s"

// allowedImports is every package internal/wrapper may import directly,
// matched exactly (DESIGN.md sec 7, Stage 1). The check is on direct
// imports only, which is sound because unsafe (needed for //go:linkname),
// cgo ("C"), os/exec, syscall and plugin are all absent.
var allowedImports = []string{
	"bytes", "container/heap", "container/list", "context",
	"encoding/json", "errors", "fmt", "io", "log/slog", "maps", "math",
	"math/rand/v2", "net/http", "runtime", "runtime/debug",
	"runtime/metrics", "slices", "sort", "strconv", "strings", "sync",
	"sync/atomic", "time", "unicode/utf8",
	modulePath + "/internal/classifier",
}

// denyReason labels a disallowed import for the failure detail. It
// never decides anything: allowedImports alone does.
func denyReason(path string) string {
	switch {
	case path == modulePath+"/internal/hashcore":
		return "fixed core implementation"
	case strings.HasPrefix(path, modulePath+"/internal/classifier/"):
		return "test-vector pool or other classifier subpackage"
	case path == "unsafe" || path == "C" || path == "os/exec" || path == "syscall" || path == "plugin":
		return "would void the direct-imports-only check"
	case strings.HasPrefix(path, "crypto/") || path == "crypto" || strings.HasPrefix(path, "hash/") || path == "hash":
		return "crypto/hash implementation"
	case strings.HasPrefix(path, "golang.org/x/") || strings.HasPrefix(path, "github.com/alexedwards/argon2id"):
		return "external module"
	case path == "os" || strings.HasPrefix(path, "os/"):
		return "filesystem/environment access"
	}
	return "not in allowlist"
}

// checkHasherPin fails unless hasher.go is byte-identical to the pin.
func checkHasherPin(files map[string][]byte) string {
	sum := sha256.Sum256(files["hasher.go"])
	if got := hex.EncodeToString(sum[:]); got != hasherSHA256 {
		return fmt.Sprintf("hasher.go sha256 %s, want %s (pinned, DESIGN.md sec 5)", got, hasherSHA256)
	}
	return ""
}

// buildLine matches a build-constraint line in either syntax, with any
// leading space or a leading UTF-8 BOM (which go/build strips). It is
// deliberately broader than what the compiler honours: a constraint can
// hide a file, and its imports, from one listing but not another. The
// GoFiles check in checkPackageFiles backstops anything it misses.
var buildLine = regexp.MustCompile(`^\x{FEFF}?\s*//(go:build|\s*\+build)`)

// checkBuildConstraints fails if any candidate file has a build
// constraint line, wherever it is in the file.
func checkBuildConstraints(files map[string][]byte) string {
	var problems []string
	for _, name := range candidateFiles {
		sc := bufio.NewScanner(bytes.NewReader(files[name]))
		sc.Buffer(nil, maxCandidateFileBytes+1)
		for n := 1; sc.Scan(); n++ {
			if buildLine.Match(sc.Bytes()) {
				problems = append(problems, fmt.Sprintf("%s:%d: build constraint", name, n))
			}
		}
		if err := sc.Err(); err != nil {
			// Fail closed: the unscanned rest could hold a constraint.
			problems = append(problems, fmt.Sprintf("%s: scan: %v", name, err))
		}
	}
	return strings.Join(problems, "; ")
}

// listing is one `go list` of the candidate package under one build
// configuration. Both are checked: the deployed build (CGO_ENABLED=0)
// and Stage 4's race build (CGO_ENABLED=1, raceFlags: the race tag plus
// raceTags), with the flags it is built with.
type listing struct {
	name string
	cgo  bool
	args []string
}

var listings = []listing{
	{name: "CGO_ENABLED=0", cgo: false},
	{name: "CGO_ENABLED=1 -race -tags " + raceTags, cgo: true, args: raceFlags},
}

// goPackage is the subset of `go list -json` output the checks read.
type goPackage struct {
	GoFiles, CgoFiles, IgnoredGoFiles, InvalidGoFiles []string
	TestGoFiles, XTestGoFiles                         []string
	Imports                                           []string
	Error                                             *struct{ Err string }
	DepsErrors                                        []*struct{ Err string }
}

// goList lists the candidate package in tree under l. A go list that
// runs but exits non-zero is a candidate failure (problem), not an error:
// the tree differs from a known-good one only in the candidate's files.
func goList(ctx context.Context, tc toolchain, tree string, l listing) (pkg goPackage, problem string, err error) {
	args := append([]string{"list", "-e", "-json"}, l.args...)
	args = append(args, "./"+wrapperPkg)
	cmd := exec.CommandContext(ctx, tc.goBin, args...)
	cmd.Dir = tree
	cmd.Env = tc.env(l.cgo)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		// A killed go list also yields an ExitError; a cancelled or timed-out
		// run is a gate fault, not a candidate failure.
		return goPackage{}, "", fmt.Errorf("go list: %w", ctx.Err())
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return goPackage{}, fmt.Sprintf("%s: go list failed: %s", l.name, strings.TrimSpace(stderr.String())), nil
		}
		return goPackage{}, "", fmt.Errorf("go list: %w", err)
	}
	if err := json.Unmarshal(out, &pkg); err != nil {
		return goPackage{}, "", fmt.Errorf("go list output: %w", err)
	}
	return pkg, "", nil
}

// checkImports fails if any listing has a direct import outside
// allowedImports. It runs before checkPackageFiles because go list -e
// still reports Imports when a forbidden import fails to resolve.
func checkImports(pkgs []goPackage) string {
	var problems []string
	for i, p := range pkgs {
		for _, imp := range p.Imports {
			if !slices.Contains(allowedImports, imp) {
				problems = append(problems, fmt.Sprintf("%s: import %q (%s)", listings[i].name, imp, denyReason(imp)))
			}
		}
	}
	return strings.Join(problems, "; ")
}

// checkPackageFiles fails unless every listing compiles exactly the
// candidate's files as plain Go, with no load errors. go list parses
// only package clauses and imports; errors past those surface in the
// build (Stage 1 build sub-check).
func checkPackageFiles(pkgs []goPackage) string {
	var problems []string
	for i, p := range pkgs {
		name := listings[i].name
		if got := slices.Sorted(slices.Values(p.GoFiles)); !slices.Equal(got, candidateFiles) {
			problems = append(problems, fmt.Sprintf("%s: GoFiles %v, want %v", name, got, candidateFiles))
		}
		for _, f := range []struct {
			kind  string
			files []string
		}{
			{"CgoFiles", p.CgoFiles}, {"IgnoredGoFiles", p.IgnoredGoFiles}, {"InvalidGoFiles", p.InvalidGoFiles},
			{"TestGoFiles", p.TestGoFiles}, {"XTestGoFiles", p.XTestGoFiles},
		} {
			if len(f.files) > 0 {
				problems = append(problems, fmt.Sprintf("%s: %s %v", name, f.kind, f.files))
			}
		}
		if p.Error != nil {
			problems = append(problems, fmt.Sprintf("%s: %s", name, p.Error.Err))
		}
		for _, e := range p.DepsErrors {
			problems = append(problems, fmt.Sprintf("%s: dependency: %s", name, e.Err))
		}
	}
	return strings.Join(problems, "; ")
}
