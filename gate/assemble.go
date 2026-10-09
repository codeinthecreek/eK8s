package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
)

// candidateFiles is the exact file set a candidate wrapper must consist
// of: gen 0's non-test .go files. The driver copies only these into the
// sandbox, and a mutant cannot create files (DESIGN.md sec 12).
var candidateFiles = []string{"doc.go", "hasher.go", "server.go", "tunables.go"}

// maxCandidateFileBytes caps each candidate file. Gen 0's largest is
// under 6 KiB; the cap only stops a runaway file, not a real mutation.
const maxCandidateFileBytes = 256 << 10

// fixedDirs are the repo packages copied into every build tree, non-test
// .go files only, top level only: internal/classifier/testvectors is left
// out, so the tree a candidate is built and booted from never holds the
// test-vector pool.
var fixedDirs = []string{"cmd/service", "internal/hashcore", "internal/classifier"}

// wrapperPkg is the candidate's package path inside the build tree.
const wrapperPkg = "internal/wrapper"

// readCandidate reads the candidate's files, failing (non-empty detail)
// unless dir holds exactly candidateFiles, each a regular file within
// maxCandidateFileBytes. Symlinks and anything else are rejected, never
// followed.
func readCandidate(dir string) (files map[string][]byte, detail string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("read candidate dir: %w", err)
	}
	var problems []string
	seen := make(map[string]bool)
	for _, e := range entries {
		name := e.Name()
		seen[name] = true
		switch {
		case !slices.Contains(candidateFiles, name):
			problems = append(problems, fmt.Sprintf("unexpected entry %q", name))
		case !e.Type().IsRegular(): // ReadDir types come from lstat
			problems = append(problems, fmt.Sprintf("%s: not a regular file (%v)", name, e.Type()))
		}
	}
	for _, name := range candidateFiles {
		if !seen[name] {
			problems = append(problems, "missing "+name)
		}
	}
	if len(problems) > 0 {
		return nil, strings.Join(problems, "; "), nil
	}

	files = make(map[string][]byte, len(candidateFiles))
	for _, name := range candidateFiles {
		b, problem, err := readRegular(filepath.Join(dir, name))
		if err != nil {
			return nil, "", err
		}
		if problem != "" {
			return nil, name + ": " + problem, nil
		}
		files[name] = b
	}
	return files, "", nil
}

// readRegular reads path, which must still be a regular file within the
// size cap when opened: O_NOFOLLOW refuses a symlink swapped in after the
// directory listing, O_NONBLOCK keeps a swapped-in FIFO from blocking the
// open, and the type and size are checked on the open descriptor.
func readRegular(path string) (b []byte, problem string, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, "not a regular file (symlink)", nil
		}
		return nil, "", fmt.Errorf("open candidate file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("stat candidate file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Sprintf("not a regular file (%v)", fi.Mode().Type()), nil
	}
	b, err = io.ReadAll(io.LimitReader(f, maxCandidateFileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read candidate file: %w", err)
	}
	if len(b) > maxCandidateFileBytes {
		return nil, fmt.Sprintf("larger than %d bytes", maxCandidateFileBytes), nil
	}
	return b, "", nil
}

// assemble builds a fresh tree under a new temp directory: the repo's
// go.mod, go.sum and fixedDirs, plus the candidate's files as
// internal/wrapper (DESIGN.md sec 4: fixed core + evolved wrapper). The
// caller removes the returned directory.
func assemble(repoRoot string, candidate map[string][]byte) (tree string, err error) {
	tree, err = os.MkdirTemp("", "ek8s-gate-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tree)
		}
	}()
	for _, name := range []string{"go.mod", "go.sum"} {
		if err := copyFile(filepath.Join(repoRoot, name), filepath.Join(tree, name)); err != nil {
			return "", err
		}
	}
	for _, dir := range fixedDirs {
		if err := copyGoFiles(filepath.Join(repoRoot, dir), filepath.Join(tree, dir)); err != nil {
			return "", err
		}
	}
	wdir := filepath.Join(tree, wrapperPkg)
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		return "", err
	}
	for name, b := range candidate {
		if err := os.WriteFile(filepath.Join(wdir, name), b, 0o644); err != nil {
			return "", err
		}
	}
	return tree, nil
}

// copyGoFiles copies src's top-level non-test .go files into dst.
func copyGoFiles(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if err := copyFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// toolchain is the go command the gate runs, with the facts it needs
// from the operator's environment resolved once.
type toolchain struct {
	goBin, modCache, buildCache, path string
}

// resolveToolchain asks the operator's go, run in repoRoot so any
// GOTOOLCHAIN switch to the go directive's version happens, for its
// GOROOT and caches. Every later go command runs that GOROOT's go
// directly, under toolchain.env. On a host whose go is older than the go
// directive, this go env may first download that toolchain
// (checksum-verified) under the operator's GOTOOLCHAIN and GOPROXY.
func resolveToolchain(ctx context.Context, repoRoot string) (toolchain, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOROOT", "GOMODCACHE", "GOCACHE")
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return toolchain{}, fmt.Errorf("go env: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var v struct{ GOROOT, GOMODCACHE, GOCACHE string }
	if err := json.Unmarshal(out, &v); err != nil {
		return toolchain{}, fmt.Errorf("go env: %w", err)
	}
	if v.GOROOT == "" || v.GOMODCACHE == "" || v.GOCACHE == "" {
		return toolchain{}, fmt.Errorf("go env: empty GOROOT, GOMODCACHE or GOCACHE: %s", out)
	}
	return toolchain{
		goBin:      filepath.Join(v.GOROOT, "bin", "go"),
		modCache:   v.GOMODCACHE,
		buildCache: v.GOCACHE,
		path:       os.Getenv("PATH"),
	}, nil
}

// env is the complete environment for a go command on a build tree,
// built from scratch, never inherited: inherited GOFLAGS could carry
// -toolexec, and the go env file could set CC, CGO_*, GOEXPERIMENT or
// GOCACHEPROG. With neither HOME nor XDG_CONFIG_HOME set, go cannot
// locate that file at all; GOENV=off ignores it regardless. GOPROXY=off
// means the module cache must already hold every module (run
// `go mod download` in the repo).
func (t toolchain) env(cgo bool) []string {
	cgoVal := "0"
	if cgo {
		cgoVal = "1"
	}
	return []string{
		"PATH=" + t.path,
		"GOENV=off",
		"GOFLAGS=-mod=readonly",
		"GOPROXY=off",
		"GOWORK=off",
		"GOTOOLCHAIN=local",
		"GOOS=linux",
		"GOARCH=" + runtime.GOARCH,
		"CGO_ENABLED=" + cgoVal,
		"GOMODCACHE=" + t.modCache,
		"GOCACHE=" + t.buildCache,
	}
}
