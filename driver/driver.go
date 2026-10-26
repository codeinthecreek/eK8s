package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/codeinthecreek/eK8s/lineage"
)

// PinnedModel is the mutation model for the whole run (docs/decisions.md,
// PR #52 S12): never a configuration value, so one run is one model.
const PinnedModel = "claude-sonnet-5-5"

// Limits of one mutation call.
const (
	maxTurns         = 30
	callTimeout      = 15 * time.Minute // wall clock, the backstop beyond maxTurns
	preflightTimeout = time.Minute
	maxPromptBytes   = 64 << 10 // well under Linux's 128 KiB per argv string
	maxStderrBytes   = 64 << 10
	maxNoteBytes     = 300 // of a failed session's result text, in its error
)

// genomeNameRE is a wrapper file the driver copies in: a plain .go base
// name, never a path or a dot file. Test files are refused separately.
var genomeNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,60}\.go$`)

// Config configures a Driver.
type Config struct {
	// Claude is the claude binary. A symlink is resolved once, by New, and
	// that file serves the whole run: an installer's update moves the
	// symlink, not the file (DESIGN.md sec 12, the versioned binary).
	Claude string
	// Version is the version `claude --version` must report, such as
	// "2.1.310"; every session's init event must report it too.
	Version string
	// Token is CLAUDE_CODE_OAUTH_TOKEN for the sessions: a dedicated
	// token from `claude setup-token`, revocable on its own.
	Token string
	// ScratchDir is an existing directory, outside the repo, under which
	// each call makes and then removes its own directory.
	ScratchDir string
}

// Driver runs mutation sessions. Its methods may be called from one
// goroutine at a time or several; each call has its own directories.
type Driver struct {
	bwrap, claude, version, token, scratch string
	claudeInfo                             os.FileInfo
	timeout                                time.Duration // callTimeout; tests shorten it
	maxOutput                              int           // lineage.MaxTranscript; tests shorten it
}

// New checks cfg and the host, resolves the claude binary and runs it
// once in the sandbox with --version (a preflight that also proves the
// sandbox comes up), refusing any version but cfg.Version.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	switch {
	case cfg.Version == "" || strings.ContainsAny(cfg.Version, " \t\n\x00"):
		return nil, fmt.Errorf("driver: version %q", cfg.Version)
	case cfg.Token == "" || strings.ContainsFunc(cfg.Token, func(r rune) bool { return r <= ' ' || r >= 0x7f }):
		return nil, errors.New("driver: the token is empty or holds a space or control character")
	case !filepath.IsAbs(cfg.ScratchDir):
		return nil, fmt.Errorf("driver: scratch dir %q is not absolute", cfg.ScratchDir)
	}
	if st, err := os.Stat(cfg.ScratchDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("driver: scratch dir %q is not a directory", cfg.ScratchDir)
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("driver: the sandbox needs bwrap on PATH: %w", err)
	}
	if bwrap, err = filepath.Abs(bwrap); err != nil {
		return nil, fmt.Errorf("driver: %w", err)
	}
	claude, err := filepath.EvalSymlinks(cfg.Claude)
	if err == nil {
		claude, err = filepath.Abs(claude)
	}
	if err != nil {
		return nil, fmt.Errorf("driver: claude binary: %w", err)
	}
	info, err := os.Stat(claude)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("driver: claude binary %s is not an executable file", claude)
	}
	d := &Driver{
		bwrap: bwrap, claude: claude, version: cfg.Version, token: cfg.Token, scratch: cfg.ScratchDir,
		claudeInfo: info, timeout: callTimeout, maxOutput: lineage.MaxTranscript,
	}
	got, err := d.sandboxedVersion(ctx)
	if err != nil {
		return nil, err
	}
	if want := cfg.Version + " (Claude Code)"; got != want {
		return nil, fmt.Errorf("driver: claude --version in the sandbox reads %q, want %q", got, want)
	}
	return d, nil
}

// Claude returns the resolved claude binary the driver runs.
func (d *Driver) Claude() string { return d.claude }

// sandboxedVersion runs claude --version in a fresh sandbox, exactly as
// a session runs but for the arguments.
func (d *Driver) sandboxedVersion(ctx context.Context) (string, error) {
	work, home, cleanup, err := d.callDirs()
	if err != nil {
		return "", err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	out, errOut := &capped{max: 4 << 10}, &capped{max: maxStderrBytes}
	if err := d.runSandboxed(ctx, work, home, []string{"--version"}, out, errOut); err != nil {
		return "", fmt.Errorf("driver: sandbox preflight (claude --version): %v: %s", err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Mutation is one session's outcome. Transcript is set whenever the
// session started, and Usage whenever its output holds a well-formed
// result event, even if Mutate also returns an error (a session killed
// after printing its result included), so the run can count every
// call's usage it can see (DESIGN.md sec 13). A session killed before
// its result has no usage to count.
type Mutation struct {
	// Files are the parent's file names with their content after the
	// session: only those names are read back (DESIGN.md sec 12).
	Files map[string][]byte
	// Changed lists, sorted, the files whose bytes differ from the
	// parent's. It is empty when the session edited nothing, which the
	// caller may want to discard without a gate run.
	Changed []string
	// Extra counts entries the session left in its directory beyond the
	// parent's files; they are never read.
	Extra int
	// Rationale is the session's final reply, cut to
	// lineage.MaxRationale bytes of valid UTF-8.
	Rationale string
	SessionID string
	Usage     lineage.Usage
	// Transcript is the session's whole stream-json output, for
	// lineage.Run.WriteTranscript.
	Transcript []byte
}

// Mutate runs one mutation session on a copy of parent, the parent
// genome's files, with prompt (BuildPrompt). It returns the edited files
// only if the session passed checkInit, ended in a success result on the
// pinned model and left every parent file a regular file within
// lineage.MaxGenomeFile; otherwise it returns an error, with the
// Mutation's Usage and Transcript when the session produced them.
func (d *Driver) Mutate(ctx context.Context, parent map[string][]byte, prompt string) (*Mutation, error) {
	if err := checkParent(parent); err != nil {
		return nil, err
	}
	switch {
	case prompt == "" || len(prompt) > maxPromptBytes:
		return nil, fmt.Errorf("driver: prompt is %d bytes, want 1 to %d", len(prompt), maxPromptBytes)
	case !utf8.ValidString(prompt) || strings.ContainsRune(prompt, 0):
		return nil, errors.New("driver: prompt is not valid UTF-8 without NUL")
	}
	if info, err := os.Stat(d.claude); err != nil || !os.SameFile(info, d.claudeInfo) ||
		info.Size() != d.claudeInfo.Size() || !info.ModTime().Equal(d.claudeInfo.ModTime()) {
		return nil, fmt.Errorf("driver: claude binary %s changed since the driver started", d.claude)
	}
	work, home, cleanup, err := d.callDirs()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	for name, b := range parent {
		if err := writeNew(filepath.Join(work, name), b); err != nil {
			return nil, err
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	sink := &streamSink{max: d.maxOutput, version: d.version, kill: cancel}
	errOut := &capped{max: maxStderrBytes}
	runErr := d.runSandboxed(callCtx, work, home, claudeArgs(prompt), sink, errOut)
	out, sinkErr := sink.state()
	diag := func() string {
		return fmt.Sprintf(" (exit: %v; stderr: %q)", runErr, cut(strings.TrimSpace(errOut.String()), maxNoteBytes))
	}
	m := &Mutation{Transcript: out}
	res, parseErr := parseStream(out)
	if parseErr == nil {
		m.SessionID, m.Rationale = res.SessionID, cut(strings.TrimSpace(res.Result), lineage.MaxRationale)
		if m.Usage, err = res.usage(); err != nil {
			return m, fmt.Errorf("driver: %w", err)
		}
	}
	switch {
	case ctx.Err() != nil: // the caller's: a shutdown, not the session's fault
		return m, fmt.Errorf("driver: %w", ctx.Err())
	case sinkErr != nil:
		return m, fmt.Errorf("driver: %w%s", sinkErr, diag())
	case callCtx.Err() != nil:
		return m, fmt.Errorf("driver: session killed after %v", d.timeout)
	case parseErr != nil:
		return m, fmt.Errorf("driver: %w%s", parseErr, diag())
	}
	switch {
	case res.IsError || res.Subtype != "success":
		return m, fmt.Errorf("driver: session ended %s (is_error %v): %q", res.Subtype, res.IsError, cut(res.Result, maxNoteBytes))
	case runErr != nil:
		return m, fmt.Errorf("driver: session reported success but%s", diag())
	case !res.ranPinned():
		return m, fmt.Errorf("driver: session did not run on %s (models: %q)", PinnedModel, slices.Sorted(maps.Keys(res.ModelUsage)))
	}
	if m.Files, m.Extra, err = readBack(work, parent); err != nil {
		return m, err
	}
	for name, b := range m.Files {
		if !bytes.Equal(b, parent[name]) {
			m.Changed = append(m.Changed, name)
		}
	}
	slices.Sort(m.Changed)
	return m, nil
}

// checkParent errors unless parent is a wrapper genome the driver may
// copy in: 1 to lineage.MaxGenomeFiles plain non-test .go files, each at
// most lineage.MaxGenomeFile bytes, hasher.go among them (the fixed
// core's calling interface, DESIGN.md sec 12).
func checkParent(parent map[string][]byte) error {
	if len(parent) == 0 || len(parent) > lineage.MaxGenomeFiles {
		return fmt.Errorf("driver: parent of %d files, want 1 to %d", len(parent), lineage.MaxGenomeFiles)
	}
	if _, ok := parent["hasher.go"]; !ok {
		return errors.New("driver: parent has no hasher.go")
	}
	for name, b := range parent {
		if !genomeNameRE.MatchString(name) || strings.HasSuffix(name, "_test.go") {
			return fmt.Errorf("driver: parent file %q is not a plain non-test .go name", name)
		}
		if len(b) > lineage.MaxGenomeFile {
			return fmt.Errorf("driver: parent file %s is %d bytes, over %d", name, len(b), lineage.MaxGenomeFile)
		}
	}
	return nil
}

// callDirs makes a call's directory under the scratch dir, with fresh,
// empty work and home directories in it, and returns a cleanup that
// removes it all.
func (d *Driver) callDirs() (work, home string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(d.scratch, "mutate-")
	if err != nil {
		return "", "", nil, fmt.Errorf("driver: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }
	work, home = filepath.Join(dir, "work"), filepath.Join(dir, "home")
	for _, p := range []string{work, home} {
		if err := os.Mkdir(p, 0o700); err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("driver: %w", err)
		}
	}
	return work, home, cleanup, nil
}

// readBack reads the parent's file names back from work: each must be a
// regular file, not a symlink, of at most lineage.MaxGenomeFile bytes.
// Anything else in work is counted, never read (DESIGN.md sec 12).
func readBack(work string, parent map[string][]byte) (map[string][]byte, int, error) {
	files := make(map[string][]byte, len(parent))
	for name := range parent {
		b, err := readRegular(filepath.Join(work, name))
		if err != nil {
			return nil, 0, fmt.Errorf("driver: copy-back of %s: %w", name, err)
		}
		files[name] = b
	}
	ents, err := os.ReadDir(work)
	if err != nil {
		return nil, 0, fmt.Errorf("driver: %w", err)
	}
	extra := 0
	for _, e := range ents {
		if _, ok := parent[e.Name()]; !ok {
			extra++
		}
	}
	return files, extra, nil
}

// readRegular reads path, refusing a symlink (O_NOFOLLOW), anything but
// a regular file, and more than lineage.MaxGenomeFile bytes.
func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%v)", st.Mode().Type())
	}
	b, err := io.ReadAll(io.LimitReader(f, lineage.MaxGenomeFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > lineage.MaxGenomeFile {
		return nil, fmt.Errorf("over %d bytes", lineage.MaxGenomeFile)
	}
	return b, nil
}

// writeNew writes b to a new file path, mode 0600.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("driver: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("driver: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("driver: %w", err)
	}
	return nil
}

// cut returns s as valid UTF-8, cut to at most n bytes on a rune
// boundary.
func cut(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// capped is an io.Writer that keeps the first max bytes and drops the
// rest, always reporting a full write so os/exec keeps draining the pipe.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	c.buf.Write(p[:min(n, max(c.max-c.buf.Len(), 0))])
	return n, nil
}

func (c *capped) String() string { return c.buf.String() }
