package lineage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func readCNI(t *testing.T, r *Run, file string) []byte {
	t.Helper()
	p := filepath.Join(r.dir, cniDir, file)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if mode(t, p) != 0o600 {
		t.Errorf("%s is not 0600", file)
	}
	return b
}

func TestCNICutMarker(t *testing.T) {
	if want := "ek8s: cut at " + strconv.Itoa(MaxCNILog) + " bytes\n"; cniCutMarker != want {
		t.Fatalf("cniCutMarker = %q, want %q", cniCutMarker, want)
	}
	if MaxCNILog != 1048576 {
		t.Fatalf("MaxCNILog = %d, want 1048576", MaxCNILog)
	}
}

// TestCNILogCap: under and exactly at the cap the file holds the bytes as
// read; one byte over keeps the first MaxCNILog bytes and then the marker
// line, after a newline only when the kept bytes lack one.
func TestCNILogCap(t *testing.T) {
	line := func(n int) []byte { // n bytes, newline-terminated lines
		b := bytes.Repeat([]byte("2026-11-04T10:00:00Z x\n"), n/23+1)
		return b[:n]
	}
	atCapNL := line(MaxCNILog)
	atCapNL[MaxCNILog-1] = '\n'
	for _, c := range []struct {
		name string
		in   []byte
		want []byte
	}{
		{"under", line(100), line(100)},
		{"empty", nil, nil},
		{"at-cap", line(MaxCNILog), line(MaxCNILog)},
		{"one-over", line(MaxCNILog + 1), append(append(line(MaxCNILog), '\n'), cniCutMarker...)},
		{"one-over-nl", append(bytes.Clone(atCapNL), 'z'), append(bytes.Clone(atCapNL), cniCutMarker...)},
		{"far-over", line(3 * MaxCNILog), append(append(line(MaxCNILog), '\n'), cniCutMarker...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t)
			in := bytes.Clone(c.in)
			if err := r.WriteCNILog("w-0000000000000001", in); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(in, c.in) {
				t.Error("WriteCNILog changed its input")
			}
			got := readCNI(t, r, "w-0000000000000001.log")
			if !bytes.Equal(got, c.want) {
				t.Fatalf("file is %d bytes (tail %q), want %d (tail %q)", len(got), tail(got), len(c.want), tail(c.want))
			}
			cut := bytes.HasSuffix(got, []byte(cniCutMarker))
			if cut != (len(c.in) > MaxCNILog) {
				t.Errorf("marker present = %v for %d bytes", cut, len(c.in))
			}
		})
	}
}

func tail(b []byte) []byte { return b[max(0, len(b)-40):] }

func TestCNIFailedCut(t *testing.T) {
	r := newRun(t)
	short := "kubectl logs: exit status 1: error: container not found"
	if err := r.WriteCNIFailed("gate-3-ek8s-0123456789abcdef", short); err != nil {
		t.Fatal(err)
	}
	if got := string(readCNI(t, r, "gate-3-ek8s-0123456789abcdef.failed")); got != short+"\n" {
		t.Errorf(".failed = %q", got)
	}
	long := strings.Repeat("e", MaxCNIFailed) + "TAIL"
	if err := r.WriteCNIFailed("w-0000000000000002", long); err != nil {
		t.Fatal(err)
	}
	if got := string(readCNI(t, r, "w-0000000000000002.failed")); got != long[:MaxCNIFailed]+"\n" {
		t.Errorf(".failed is %d bytes, want %d", len(got), MaxCNIFailed+1)
	}
}

func TestCNINoOverwrite(t *testing.T) {
	r := newRun(t)
	const name = "w-0000000000000003"
	if err := r.WriteCNILog(name, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteCNILog(name, []byte("second\n")); !errors.Is(err, ErrExists) {
		t.Errorf("second log = %v, want ErrExists", err)
	}
	if err := r.WriteCNIFailed(name, "a"); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteCNIFailed(name, "b"); !errors.Is(err, ErrExists) {
		t.Errorf("second .failed = %v, want ErrExists", err)
	}
	if got := string(readCNI(t, r, name+".log")); got != "first\n" {
		t.Errorf("log = %q, want the first write", got)
	}
	if got := string(readCNI(t, r, name+".failed")); got != "a\n" {
		t.Errorf(".failed = %q, want the first write", got)
	}
	if mode(t, filepath.Join(r.dir, cniDir)) != 0o700 {
		t.Error("cni/ is not 0700")
	}
	ents, _ := os.ReadDir(filepath.Join(r.dir, cniDir))
	if len(ents) != 2 {
		t.Errorf("cni/ has %d entries, want 2 (no temp files)", len(ents))
	}
}

func TestCNIBadName(t *testing.T) {
	r := newRun(t)
	for _, name := range []string{"", ".hidden", "../x", "a/b", "a b", strings.Repeat("a", 65)} {
		if err := r.WriteCNILog(name, []byte("x\n")); err == nil {
			t.Errorf("WriteCNILog(%q) accepted", name)
		}
		if err := r.WriteCNIFailed(name, "x"); err == nil {
			t.Errorf("WriteCNIFailed(%q) accepted", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(r.dir, cniDir)); err == nil {
		t.Error("a refused name made cni/")
	}
}

// TestCNIDirNotDirectory: cni/ that is a symlink or a file is refused,
// never written through.
func TestCNIDirNotDirectory(t *testing.T) {
	r := newRun(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(r.dir, cniDir)); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteCNILog("w-0000000000000004", []byte("x\n")); err == nil {
		t.Error("log written through a symlinked cni/")
	}
}
