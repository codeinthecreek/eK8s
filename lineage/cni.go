package lineage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// MaxCNILog is the most log bytes one cni/<name>.log holds; a read
// longer than this is cut here and marked (docs/decisions.md,
// 2026-11-04, Q7).
const MaxCNILog = 1 << 20

// MaxCNIFailed is the most error-text bytes one cni/<name>.failed holds.
const MaxCNIFailed = 1 << 10

const cniDir = "cni"

// cniCutMarker is the line that ends a log cut at MaxCNILog; its number
// is MaxCNILog's (a test checks).
const cniCutMarker = "ek8s: cut at 1048576 bytes\n"

// WriteCNILog stores log, the CNI's log bytes as read, as cni/<name>.log,
// once. A log longer than MaxCNILog keeps its first MaxCNILog bytes and
// then ends with the line "ek8s: cut at 1048576 bytes" (after a newline
// when the kept bytes do not end in one), so a reader that asks for more
// than MaxCNILog bytes tells the writer whether anything was cut. name
// is a run id (the Stage 5 burst's or the window's loadgen run_id).
func (r *Run) WriteCNILog(name string, log []byte) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("lineage: cni log name %q is not a run id", name)
	}
	b := log
	if len(b) > MaxCNILog {
		b = append([]byte(nil), log[:MaxCNILog]...)
		if b[len(b)-1] != '\n' {
			b = append(b, '\n')
		}
		b = append(b, cniCutMarker...)
	}
	return r.writeCNI(name+".log", b)
}

// WriteCNIFailed stores msg, the error of a failed read of the CNI's
// log, cut to MaxCNIFailed bytes and ended with a newline, as
// cni/<name>.failed, once. name is as WriteCNILog's.
func (r *Run) WriteCNIFailed(name, msg string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("lineage: cni log name %q is not a run id", name)
	}
	if len(msg) > MaxCNIFailed {
		msg = msg[:MaxCNIFailed]
	}
	return r.writeCNI(name+".failed", []byte(msg+"\n"))
}

// writeCNI writes b to cni/<file>, mode 0600, as WriteTranscript writes
// a transcript: a synced temporary file linked into place, so a crash
// leaves the whole file or none and an existing file is never
// overwritten (ErrExists). cni/ is made by the first write, so a run
// directory from before it needs no migration.
func (r *Run) writeCNI(file string, b []byte) error {
	dir := filepath.Join(r.dir, cniDir)
	if err := os.Mkdir(dir, 0o700); err == nil {
		if err := syncDir(r.dir); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("lineage: %w", err)
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("lineage: %s is not a directory", cniDir)
	}
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("lineage: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
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
	// Link, not rename: link fails if the file exists.
	if err := os.Link(tmp, filepath.Join(dir, file)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("cni %s: %w", file, ErrExists)
		}
		return fmt.Errorf("lineage: %w", err)
	}
	return syncDir(dir)
}
