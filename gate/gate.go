package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// Run gates the candidate through Stages 1-4 (DESIGN.md sec 7); Stage 5
// is Stage5, which needs the cluster. A check that fails is a
// Result in the Report and stops the run; the error is only for the gate
// itself failing (no go, bwrap, nsenter, docker daemon, or gcc and a
// static libc for the race build; I/O), which says nothing about the
// candidate. On a pass, Report.Image names the
// candidate's image, kept for kind load; otherwise the gate removes it,
// unless that image already existed before the run.
func Run(ctx context.Context, cfg Config) (r Report, err error) {
	tc, tree, ok, err := preBuild(ctx, cfg, &r)
	if tree != "" {
		defer os.RemoveAll(tree)
	}
	if err != nil || !ok {
		return r, err
	}

	work, err := os.MkdirTemp("", "ek8s-gate-run-*")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(work)
	service := filepath.Join(work, "service")
	relay := filepath.Join(work, "relay")
	probe := filepath.Join(work, "probe")

	// Stage 1 build/boot sub-check 1: static build, bare boot.
	built, out, err := goBuild(ctx, tc, tree, "cmd/service", service, false)
	if err != nil {
		return r, err
	}
	if !r.add(1, "build", buildDetail(built, out)) {
		return r, nil
	}
	for _, b := range []struct{ pkg, out string }{{relayPkg, relay}, {probePkg, probe}} {
		if ok, out, err := goBuild(ctx, tc, cfg.RepoRoot, b.pkg, b.out, false); err != nil {
			return r, fmt.Errorf("build %s: %w", b.pkg, err)
		} else if !ok {
			return r, fmt.Errorf("build %s: %s", b.pkg, out)
		}
	}
	if err := preflight(ctx, probe, relay, work); err != nil {
		return r, err
	}
	label := newLabel()
	sb, problem, err := bootBare(ctx, "candidate", service, relay, work, label)
	if err != nil {
		return r, err
	}
	if !r.add(1, "boot", problem) {
		return r, nil
	}
	defer sb.stop()

	// Sub-check 2: the same binary as a FROM scratch image.
	img, problem, err := dockerSubCheck(ctx, service, work, label)
	defer func() {
		if r.Passed() && err == nil {
			r.Image = img.tag
		} else {
			removeImage(img)
		}
	}()
	if err != nil {
		return r, err
	}
	if !r.add(1, "docker", problem) {
		return r, nil
	}

	// Stages 2 and 3 against the bare process.
	s := newInterop(sb.client, label)
	if ok, err := runInterop(ctx, &r, s); err != nil || !ok {
		if !ok && err == nil {
			r.Results[len(r.Results)-1].Detail = sb.withOutput(r.Results[len(r.Results)-1].Detail)
		}
		return r, err
	}
	if !r.add(3, "cost-params", checkCostParams(s.hashes)) {
		return r, nil
	}

	// Stage 4 against a separate race build, never deployed; the Stage 2
	// process goes first, so the two never compete for the host.
	sb.stop()
	// Stage 4 shares Stage 2's Run on purpose: a race build whose salts
	// repeat Stage 2's (say, a fixed-seed salt RNG) is salt_reused.
	_, err = runStage4(ctx, &r, tc, tree, cfg.RepoRoot, work, relay, s.run)
	return r, err
}

// dockerSubCheck is checkDocker; tests replace it on hosts with no
// docker daemon, so Stages 2-3 still run there.
var dockerSubCheck = checkDocker

// relayPkg and probePkg are the relay's and the preflight probe's
// packages, built from the repo per run (bootBare, preflight).
const (
	relayPkg = "gate/internal/relay"
	probePkg = "gate/internal/probe"
)

// preBuild runs Stage 1's pre-build checks, adding a Result per check;
// ok reports whether all passed. It returns the resolved toolchain and
// the assembled build tree, which the caller removes when non-empty.
func preBuild(ctx context.Context, cfg Config, r *Report) (tc toolchain, tree string, ok bool, err error) {
	files, detail, err := readCandidate(cfg.CandidateDir)
	if err != nil {
		return tc, "", false, err
	}
	if !r.add(1, "candidate-files", detail) ||
		!r.add(1, "hasher-pin", checkHasherPin(files)) ||
		!r.add(1, "build-constraints", checkBuildConstraints(files)) {
		return tc, "", false, nil
	}

	tc, err = resolveToolchain(ctx, cfg.RepoRoot)
	if err != nil {
		return tc, "", false, err
	}
	tree, err = assemble(cfg.RepoRoot, files)
	if err != nil {
		return tc, "", false, fmt.Errorf("assemble build tree: %w", err)
	}

	pkgs := make([]goPackage, len(listings))
	for i, l := range listings {
		pkg, problem, err := goList(ctx, tc, tree, l)
		if err != nil {
			return tc, tree, false, err
		}
		if problem != "" {
			r.add(1, "go-list", problem)
			return tc, tree, false, nil
		}
		pkgs[i] = pkg
	}
	r.add(1, "go-list", "")

	if !r.add(1, "imports", checkImports(pkgs)) {
		return tc, tree, false, nil
	}
	return tc, tree, r.add(1, "package-files", checkPackageFiles(pkgs)), nil
}
