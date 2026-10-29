package lineage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
)

// Limits, so a runaway writer cannot fill the disk through lineage.
const (
	// MaxGenomeFile is the largest wrapper file the archive takes.
	MaxGenomeFile = 1 << 20
	// MaxGenomeFiles is the most files one genome holds (the wrapper has
	// four).
	MaxGenomeFiles = 16
	// MaxTranscript is the largest transcript the run takes.
	MaxTranscript = 16 << 20
)

const (
	version      = 1
	runFile      = "run.json"
	attemptsFile = "attempts.jsonl"
	genomesDir   = "genomes"
	transcripts  = "transcripts"
	digestFile   = "sha256"
	filesDir     = "files"
)

var (
	// idRE is the variant id format, as deploy/variant's: a decimal
	// number without leading zeros, at most 18 digits.
	idRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})$`)
	// nameRE is a genome file name: a plain base name, never a path and
	// never a dot file.
	nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
)

// ErrExists is the error, wrapped, for a write under an id that already
// has an entry.
var ErrExists = errors.New("lineage: entry already exists")

// Run is one run's directory. It is safe for concurrent use by the
// goroutines of one process; two processes must not share a directory.
type Run struct {
	dir string
	mu  sync.Mutex // serialises attempt appends
}

// runMeta is run.json. The host clock fields are absent from a file
// written before they were added; Open reads those as the zero values.
type runMeta struct {
	Version int `json:"version"`
	// HostClock is what hostclock.Read found at the run's start. It is
	// held as any so lineage, and the driver that builds on it, do not
	// depend on internal/hostclock (the driver's TestDeps).
	HostClock any `json:"host_clock"`
	// AllowUnpinned is whether the run was started with the unpinned-clock
	// override.
	AllowUnpinned bool `json:"allow_unpinned"`
}

// Create makes a new run directory at dir, which must not exist (its
// parent must), and records in run.json hostClock (a hostclock.State;
// any value that marshals to JSON) and whether the run overrode the
// pinned-clock check. Directories are 0700 and files 0600: transcripts
// hold the mutation model's raw output.
func Create(dir string, hostClock any, allowUnpinned bool) (*Run, error) {
	// Marshal first: a value that cannot be recorded must not leave a
	// run directory behind.
	meta, err := json.Marshal(runMeta{Version: version, HostClock: hostClock, AllowUnpinned: allowUnpinned})
	if err != nil {
		return nil, fmt.Errorf("lineage: run.json: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lineage: %w", err)
	}
	for _, d := range []string{genomesDir, transcripts} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			return nil, fmt.Errorf("lineage: %w", err)
		}
	}
	if err := writeNew(filepath.Join(dir, runFile), append(meta, '\n')); err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(dir, attemptsFile), nil); err != nil {
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	return &Run{dir: dir}, nil
}

// Open opens the run directory at dir, made by Create. It refuses a
// directory of another version and an attempts file whose last line is
// torn (a crash mid-append): fix or truncate it by hand, since appending
// after it would corrupt the next record too.
func Open(dir string) (*Run, error) {
	b, err := os.ReadFile(filepath.Join(dir, runFile))
	if err != nil {
		return nil, fmt.Errorf("lineage: %w", err)
	}
	var m runMeta
	if err := strictDecode(b, &m); err != nil || m.Version != version {
		return nil, fmt.Errorf("lineage: %s is not a version %d run directory", runFile, version)
	}
	r := &Run{dir: dir}
	if err := r.checkTail(); err != nil {
		return nil, err
	}
	return r, nil
}

// Dir returns the run directory.
func (r *Run) Dir() string { return r.dir }

// Digest is the sha256, in lowercase hex, of a genome's files: for each
// file in name order, the name, a zero byte, the content's length in
// decimal, a zero byte and the content. Two genomes have one digest iff
// they hold the same names and bytes.
func Digest(files map[string][]byte) string {
	h := sha256.New()
	for _, name := range slices.Sorted(mapKeys(files)) {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(len(files[name]))))
		h.Write([]byte{0})
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func checkFiles(files map[string][]byte) error {
	if len(files) == 0 || len(files) > MaxGenomeFiles {
		return fmt.Errorf("lineage: genome of %d files, want 1 to %d", len(files), MaxGenomeFiles)
	}
	for name, b := range files {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("lineage: genome file name %q is not a plain name", name)
		}
		if len(b) > MaxGenomeFile {
			return fmt.Errorf("lineage: genome file %s is %d bytes, over %d", name, len(b), MaxGenomeFile)
		}
	}
	return nil
}

// ArchiveGenome stores the wrapper files under id, once, and returns
// their Digest. An id that is archived already is ErrExists, so an
// archived genome never changes.
func (r *Run) ArchiveGenome(id string, files map[string][]byte) (string, error) {
	if !idRE.MatchString(id) {
		return "", fmt.Errorf("lineage: genome id %q is not a decimal number", id)
	}
	if err := checkFiles(files); err != nil {
		return "", err
	}
	final := filepath.Join(r.dir, genomesDir, id)
	if _, err := os.Lstat(final); err == nil {
		return "", fmt.Errorf("genome %s: %w", id, ErrExists)
	}
	tmp, err := os.MkdirTemp(filepath.Join(r.dir, genomesDir), ".tmp-")
	if err != nil {
		return "", fmt.Errorf("lineage: %w", err)
	}
	defer os.RemoveAll(tmp) // a no-op once renamed
	if err := os.Mkdir(filepath.Join(tmp, filesDir), 0o700); err != nil {
		return "", fmt.Errorf("lineage: %w", err)
	}
	for name, b := range files {
		if err := writeNew(filepath.Join(tmp, filesDir, name), b); err != nil {
			return "", err
		}
	}
	sum := Digest(files)
	if err := writeNew(filepath.Join(tmp, digestFile), []byte(sum+"\n")); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		if _, serr := os.Lstat(final); serr == nil {
			return "", fmt.Errorf("genome %s: %w", id, ErrExists)
		}
		return "", fmt.Errorf("lineage: %w", err)
	}
	if err := syncDir(filepath.Join(r.dir, genomesDir)); err != nil {
		return "", err
	}
	return sum, nil
}

// Genome returns the files archived under id and their digest, after
// recomputing the digest and comparing it with the one stored: a changed
// or missing file, an extra one or a changed digest is an error.
func (r *Run) Genome(id string) (map[string][]byte, string, error) {
	if !idRE.MatchString(id) {
		return nil, "", fmt.Errorf("lineage: genome id %q is not a decimal number", id)
	}
	dir := filepath.Join(r.dir, genomesDir, id)
	for _, d := range []string{dir, filepath.Join(dir, filesDir)} {
		if st, err := os.Lstat(d); err != nil || !st.IsDir() {
			return nil, "", fmt.Errorf("lineage: genome %s: %s is not a directory", id, filepath.Base(d))
		}
	}
	want, err := os.ReadFile(filepath.Join(dir, digestFile))
	if err != nil {
		return nil, "", fmt.Errorf("lineage: genome %s: %w", id, err)
	}
	ents, err := os.ReadDir(filepath.Join(dir, filesDir))
	if err != nil {
		return nil, "", fmt.Errorf("lineage: genome %s: %w", id, err)
	}
	files := make(map[string][]byte, len(ents))
	for _, e := range ents {
		if !e.Type().IsRegular() || !nameRE.MatchString(e.Name()) {
			return nil, "", fmt.Errorf("lineage: genome %s: unexpected entry %q", id, e.Name())
		}
		if info, err := e.Info(); err != nil || info.Size() > MaxGenomeFile {
			return nil, "", fmt.Errorf("lineage: genome %s: file %s unreadable or over %d bytes", id, e.Name(), MaxGenomeFile)
		}
		b, err := os.ReadFile(filepath.Join(dir, filesDir, e.Name()))
		if err != nil {
			return nil, "", fmt.Errorf("lineage: genome %s: %w", id, err)
		}
		files[e.Name()] = b
	}
	if err := checkFiles(files); err != nil {
		return nil, "", fmt.Errorf("genome %s: %w", id, err)
	}
	got := Digest(files)
	if string(want) != got+"\n" {
		return nil, "", fmt.Errorf("lineage: genome %s: digest %s, stored %s", id, got, trimNL(want))
	}
	return files, got, nil
}

// HasGenome reports whether id is archived.
func (r *Run) HasGenome(id string) bool {
	if !idRE.MatchString(id) {
		return false
	}
	_, err := os.Lstat(filepath.Join(r.dir, genomesDir, id))
	return err == nil
}

// WriteTranscript stores the raw claude -p output of attempt id, once.
func (r *Run) WriteTranscript(id string, raw []byte) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("lineage: transcript id %q is not a decimal number", id)
	}
	if len(raw) > MaxTranscript {
		return fmt.Errorf("lineage: transcript %s is %d bytes, over %d", id, len(raw), MaxTranscript)
	}
	final := filepath.Join(r.dir, transcripts, id+".json")
	f, err := os.CreateTemp(filepath.Join(r.dir, transcripts), ".tmp-")
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	f.Close()
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	// Link, not rename: link fails if final exists, so two writers of one
	// id cannot overwrite each other.
	if err := os.Link(tmp, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("transcript %s: %w", id, ErrExists)
		}
		return fmt.Errorf("lineage: %w", err)
	}
	return syncDir(filepath.Join(r.dir, transcripts))
}

// Transcript returns the raw output stored for attempt id.
func (r *Run) Transcript(id string) ([]byte, error) {
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("lineage: transcript id %q is not a decimal number", id)
	}
	b, err := os.ReadFile(filepath.Join(r.dir, transcripts, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("lineage: %w", err)
	}
	return b, nil
}

// syncDir syncs directory dir, so a rename or link in it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	return nil
}

// writeNew writes b to a new file path, mode 0600, and syncs it. It
// fails if path exists.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", filepath.Base(path), ErrExists)
		}
		return fmt.Errorf("lineage: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("lineage: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("lineage: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	return nil
}

func trimNL(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return string(b)
}
