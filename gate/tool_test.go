package gate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeToolPath puts a fake tool named name, running script under
// /bin/sh, on a PATH holding only it and the coreutils the scripts use.
func fakeToolPath(t *testing.T, name, script string) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"cat", "sleep"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("%s not on PATH: %v", tool, err)
		}
		must(t, os.Symlink(p, filepath.Join(bin, tool)))
	}
	must(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o755))
	t.Setenv("PATH", bin)
}

// runToolBounded runs runTool in a goroutine so a hang fails the test at
// its own 10 s bound.
func runToolBounded(t *testing.T, ctx context.Context, stdin *strings.Reader, name string, args ...string) (string, error) {
	t.Helper()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		var out string
		var err error
		if stdin == nil {
			out, err = runTool(ctx, nil, name, args...)
		} else {
			out, err = runTool(ctx, stdin, name, args...)
		}
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("runTool did not return within 10s")
		return "", nil
	}
}

func TestRunToolSuccess(t *testing.T) {
	fakeToolPath(t, "faketool", `echo "$@"`)
	out, err := runToolBounded(t, context.Background(), nil, "faketool", "a", "b c")
	if err != nil {
		t.Fatal(err)
	}
	if out != "a b c" {
		t.Fatalf("stdout = %q, want %q", out, "a b c")
	}
}

func TestRunToolStdin(t *testing.T) {
	fakeToolPath(t, "faketool", `cat`)
	out, err := runToolBounded(t, context.Background(), strings.NewReader("abc"), "faketool")
	if err != nil {
		t.Fatal(err)
	}
	if out != "abc" {
		t.Fatalf("stdout = %q, want %q", out, "abc")
	}
}

func TestRunToolFailureCarriesStderr(t *testing.T) {
	fakeToolPath(t, "faketool", "echo boom-marker >&2\nexit 3")
	_, err := runToolBounded(t, context.Background(), nil, "faketool", "sub")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "boom-marker") {
		t.Fatalf("error %q lacks stderr", err)
	}
	if !strings.HasPrefix(err.Error(), "faketool sub: ") {
		t.Fatalf("error %q lacks the name and first arg prefix", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is a ctx error", err)
	}
}

func TestRunToolContextEndedOmitsStderr(t *testing.T) {
	fakeToolPath(t, "faketool", "echo stderr-marker >&2\nexec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := runToolBounded(t, ctx, nil, "faketool", "sub")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "stderr-marker") {
		t.Fatalf("error %q carries stderr", err)
	}
}

// A grandchild holding stdout open outlives the killed child; WaitDelay
// must still release Wait.
func TestRunToolGrandchildHoldingStdout(t *testing.T) {
	old := toolWaitDelay
	toolWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { toolWaitDelay = old })
	fakeToolPath(t, "faketool", "sleep 30 &\nexec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := runToolBounded(t, ctx, nil, "faketool", "sub")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestRunToolMissingTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := runToolBounded(t, context.Background(), nil, "no-such-tool", "x")
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is a ctx error", err)
	}
}

// A tool that exits 0 while a process it started still holds stdout open
// is an error after toolWaitDelay, not a hang and not a success.
func TestRunToolExitWithHeldStdout(t *testing.T) {
	old := toolWaitDelay
	toolWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { toolWaitDelay = old })
	fakeToolPath(t, "faketool", "sleep 30 &\necho out\nexit 0")
	_, err := runToolBounded(t, context.Background(), nil, "faketool", "sub")
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("err = %v, want exec.ErrWaitDelay", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is a ctx error", err)
	}
}

// docker keeps the error text it had before runTool: "docker <subcommand>:
// <run error>: <stderr>".
func TestDockerErrorText(t *testing.T) {
	fakeToolPath(t, "docker", "echo daemon-down >&2\nexit 3")
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := docker(context.Background(), "build", "x")
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("want an error")
		}
		if got, want := r.err.Error(), "docker build: exit status 3: daemon-down"; got != want {
			t.Fatalf("error = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("docker did not return within 10s")
	}
}
