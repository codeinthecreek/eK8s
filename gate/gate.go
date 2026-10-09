package gate

import (
	"context"
	"fmt"
	"os"
)

// Config says what to gate.
type Config struct {
	// RepoRoot is a checkout of this repo; the fixed tree (go.mod, go.sum,
	// cmd/service, internal/hashcore, internal/classifier) is copied from
	// it into every build tree.
	RepoRoot string

	// CandidateDir holds the candidate wrapper: exactly doc.go, hasher.go,
	// server.go and tunables.go (the driver's copy-back output).
	CandidateDir string
}

// Run gates the candidate. So far it runs Stage 1's pre-build checks only
// (DESIGN.md sec 7); later stages come in later PRs (docs/STATUS.md).
// A check that fails is a Result in the Report and stops the run; the
// error is only for the gate itself failing (no go command, I/O), which
// says nothing about the candidate.
func Run(ctx context.Context, cfg Config) (Report, error) {
	var r Report
	files, detail, err := readCandidate(cfg.CandidateDir)
	if err != nil {
		return r, err
	}
	if !r.add(1, "candidate-files", detail) ||
		!r.add(1, "hasher-pin", checkHasherPin(files)) ||
		!r.add(1, "build-constraints", checkBuildConstraints(files)) {
		return r, nil
	}

	tc, err := resolveToolchain(ctx, cfg.RepoRoot)
	if err != nil {
		return r, err
	}
	tree, err := assemble(cfg.RepoRoot, files)
	if err != nil {
		return r, fmt.Errorf("assemble build tree: %w", err)
	}
	defer os.RemoveAll(tree)

	pkgs := make([]goPackage, len(listings))
	for i, l := range listings {
		pkg, problem, err := goList(ctx, tc, tree, l)
		if err != nil {
			return r, err
		}
		if problem != "" {
			r.add(1, "go-list", problem)
			return r, nil
		}
		pkgs[i] = pkg
	}
	r.add(1, "go-list", "")

	if !r.add(1, "imports", checkImports(pkgs)) {
		return r, nil
	}
	r.add(1, "package-files", checkPackageFiles(pkgs))
	return r, nil
}
