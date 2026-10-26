package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// Paths inside the sandbox. They are fixed, so the session's cwd, HOME
// and binary path name no host directory. (/proc/self/mountinfo still
// shows each bind's host source: the call directory and the binary's
// install path, neither secret.)
const (
	sandboxWork   = "/sandbox/work"
	sandboxHome   = "/sandbox/home"
	sandboxClaude = "/sandbox/bin/claude"
	sandboxTmp    = "/tmp"
	tmpfsBytes    = 256 << 20 // the sandbox's /tmp, in memory
)

// libPaths are the host's library directories. Each is recreated as the
// same symlink when it is one on the host (/lib -> usr/lib on a
// merged-/usr system), bound read-only when it is a directory, and left
// out when missing.
var libPaths = []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64"}

// etcPaths are CA certificates and name resolution, bound read-only
// when present: the certificate directories only, never /etc/ssl or
// /etc/pki whole, whose private/ holds host keys. A symlink is followed
// on the host: /etc/resolv.conf often points into /run/systemd, and only
// that one file is bound, never /run. /usr/share/ca-certificates holds
// what /etc/ssl/certs links to.
var etcPaths = []string{
	"/etc/ssl/certs", "/etc/ca-certificates", "/etc/pki/tls/certs", "/etc/pki/ca-trust",
	"/usr/share/ca-certificates",
	"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf",
}

// sandboxOptions are bwrap's options for one call (DESIGN.md sec 12):
// every namespace unshared but the network, no capabilities, no further
// user namespaces, a new session (no terminal injection), death with
// the driver, and an environment built empty and then allowlisted
// (sandboxEnv). The tree is the fresh work and home directories (rw),
// the claude binary and the system libraries and certificates (ro),
// /proc, a minimal /dev and an in-memory /tmp: never /home, /run, /var,
// the repo or the module cache, so the fixed core's implementation is
// not in it. The options travel through bwrap's --args pipe, so the
// token never shows in bwrap's command line.
func sandboxOptions(work, home, claude string, env []string) ([]string, error) {
	opts := []string{
		"--unshare-all", "--unshare-user", "--share-net",
		"--disable-userns", "--die-with-parent", "--new-session",
		"--cap-drop", "ALL", "--clearenv",
	}
	for i := 0; i+1 < len(env); i += 2 {
		opts = append(opts, "--setenv", env[i], env[i+1])
	}
	for _, p := range libPaths {
		st, err := os.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return nil, fmt.Errorf("driver: %w", err)
		case st.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return nil, fmt.Errorf("driver: %w", err)
			}
			opts = append(opts, "--symlink", target, p)
		case st.IsDir():
			opts = append(opts, "--ro-bind", p, p)
		default:
			return nil, fmt.Errorf("driver: %s is neither a directory nor a symlink", p)
		}
	}
	for _, p := range etcPaths {
		opts = append(opts, "--ro-bind-try", p, p)
	}
	return append(opts,
		"--bind", work, sandboxWork,
		"--bind", home, sandboxHome,
		"--ro-bind", claude, sandboxClaude,
		"--proc", "/proc",
		"--dev", "/dev",
		"--size", strconv.Itoa(tmpfsBytes), "--tmpfs", sandboxTmp,
		"--chdir", sandboxWork,
	), nil
}

// sandboxEnv is the session's whole environment, as NAME, value pairs
// (DESIGN.md sec 12). ANTHROPIC_API_KEY (metered billing), the other
// ANTHROPIC_* variables, KUBECONFIG, DOCKER_HOST, SSH_AUTH_SOCK and the
// GitHub tokens are never in it: nothing outside this list is.
func sandboxEnv(token string) []string {
	return []string{
		"HOME", sandboxHome,
		"TMPDIR", sandboxTmp,
		"PATH", "/usr/bin:/bin",
		"LANG", "C.UTF-8",
		"CLAUDE_CODE_OAUTH_TOKEN", token,
		"CLAUDE_CODE_RESTRICTED", "1",
		"DISABLE_AUTOUPDATER", "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1",
	}
}

// claudeArgs is the mutation session's argv after the binary (DESIGN.md
// sec 12; CLAUDE.md non-negotiable 3): --tools, which decides which tools
// exist, with --restricted; acceptEdits, since --permission-prompts none
// would otherwise deny every Edit; no MCP servers and no saved session;
// stream-json, whose first event the driver checks (checkInit); and "--"
// before the prompt, so a prompt can never be read as a flag.
func claudeArgs(prompt string) []string {
	return []string{
		"-p", "--tools", "Read,Edit", "--restricted",
		"--model", PinnedModel,
		"--permission-mode", "acceptEdits", "--permission-prompts", "none",
		"--max-turns", strconv.Itoa(maxTurns),
		"--strict-mcp-config", "--no-session-persistence",
		"--output-format", "stream-json", "--verbose",
		"--", prompt,
	}
}

// waitDelay bounds Wait once the sandbox is killed or exits, should
// anything still hold its output pipes.
const waitDelay = 5 * time.Second

// runSandboxed runs the claude binary with args in the sandbox over the
// host directories work and home, and waits for it. bwrap runs in its
// own process group, which is SIGKILLed when ctx ends (the wall-clock
// backstop beyond --max-turns): bwrap's death kills the sandbox's pid 1
// (--die-with-parent), and with it everything in the sandbox's pid
// namespace. --die-with-parent also SIGKILLs bwrap itself when the
// driver's process dies (a parent-death signal, so it can fire early if
// the forking OS thread exits: only a goroutine that locked its thread
// and exited could cause that).
func (d *Driver) runSandboxed(ctx context.Context, work, home string, args []string, stdout, stderr io.Writer) error {
	opts, err := sandboxOptions(work, home, d.claude, sandboxEnv(d.token))
	if err != nil {
		return err
	}
	var data []byte
	for _, o := range opts {
		data = append(append(data, o...), 0)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("driver: %w", err)
	}
	// Exec, never a shell: the prompt holds model-written text.
	cmd := exec.CommandContext(ctx, d.bwrap, append([]string{"--args", "3", "--", sandboxClaude}, args...)...)
	cmd.Env = []string{} // bwrap's own; the session's is sandboxEnv
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = []*os.File{r} // fd 3
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	err = cmd.Start()
	r.Close()
	if err != nil {
		w.Close()
		return fmt.Errorf("driver: start bwrap: %w", err)
	}
	// The options can outgrow the pipe buffer, so they are written while
	// bwrap reads. A bwrap that dies first makes the write fail (EPIPE),
	// and Wait reports its exit.
	go func() {
		w.Write(data)
		w.Close()
	}()
	return cmd.Wait()
}
