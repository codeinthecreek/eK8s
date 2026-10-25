package gate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// toolWaitDelay bounds Wait after a tool's context ends or the tool
// exits. A cancelled context kills only the direct child; a grandchild
// holding stdout or stderr open would otherwise block Wait without bound.
// Such a process keeps running after runTool returns.
var toolWaitDelay = 5 * time.Second

// runTool runs the named tool with args, feeding it stdin (nil means no
// stdin, as exec.Cmd treats it), and returns its trimmed stdout. If ctx
// has ended, the error wraps ctx.Err() (errors.Is works) and does not
// carry stderr. Otherwise a failed run's error wraps the run error and
// carries the trimmed stderr. A tool that exits but leaves its stdout or
// stderr held open (by a process it started) past toolWaitDelay is an
// error wrapping exec.ErrWaitDelay, since its output may be incomplete.
func runTool(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.WaitDelay = toolWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		label := name
		if len(args) > 0 {
			label += " " + args[0]
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s: %w", label, ctx.Err())
		}
		return "", fmt.Errorf("%s: %w: %s", label, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
