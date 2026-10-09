package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// containerBootTimeout bounds docker run through the first healthy
// /healthz answer (DESIGN.md sec 7, Stage 1 sub-check 2).
const containerBootTimeout = 30 * time.Second

// dockerfile is the whole image: the host-built static binary, nothing
// else (docs/decisions.md, PR #18 image entry).
const dockerfile = `FROM scratch
COPY service /service
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/service"]
`

// imageTag names the image after the binary it holds, so a rebuild of
// identical bytes gets the same tag.
func imageTag(binary []byte) string {
	sum := sha256.Sum256(binary)
	return "ek8s-wrapper:" + hex.EncodeToString(sum[:])[:12]
}

// docker runs the docker CLI. A command that fails is a gate fault: only
// the health check below says anything about the candidate.
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("docker %s: %w", args[0], ctx.Err())
		}
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// dockerImage is an image built by checkDocker, and whether it already
// existed before this run (then the gate never removes it).
type dockerImage struct {
	tag     string
	existed bool
}

// checkDocker builds the image from binaryPath in a context dir under
// work holding only the Dockerfile and the binary, runs it and checks
// /healthz from the host, then removes the container. The image is
// returned whether or not the health check passed; the caller decides
// whether to keep it. problem is non-empty if the container did not come
// up healthy; err is a gate fault (no docker, no daemon, docker failed).
func checkDocker(ctx context.Context, binaryPath, work, label string) (img dockerImage, problem string, err error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return img, "", fmt.Errorf("docker sub-check needs docker on PATH: %w", err)
	}
	if _, err := docker(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return img, "", fmt.Errorf("docker sub-check needs a docker daemon: %w", err)
	}
	bin, err := os.ReadFile(binaryPath)
	if err != nil {
		return img, "", err
	}
	bctx := filepath.Join(work, "image")
	if err := os.Mkdir(bctx, 0o700); err != nil {
		return img, "", err
	}
	if err := os.WriteFile(filepath.Join(bctx, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return img, "", err
	}
	// 0755: the daemon may build as another user.
	if err := os.WriteFile(filepath.Join(bctx, "service"), bin, 0o755); err != nil {
		return img, "", err
	}

	img.tag = imageTag(bin)
	_, inspectErr := docker(ctx, "image", "inspect", "--format", "{{.Id}}", img.tag)
	var exit *exec.ExitError
	switch {
	case inspectErr == nil:
		img.existed = true
	case ctx.Err() != nil:
		return dockerImage{}, "", ctx.Err()
	case !errors.As(inspectErr, &exit):
		return dockerImage{}, "", inspectErr
	}
	if _, err := docker(ctx, "build", "-q", "-t", img.tag, bctx); err != nil {
		return img, "", err
	}

	start := time.Now()
	id, err := docker(ctx, "run", "-d", "--rm",
		"-p", "127.0.0.1::8080",
		"--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "256",
		"--memory", "256m", "--memory-swap", "256m", "--cpus", "1",
		"-e", "EK8S_VARIANT="+label,
		img.tag)
	if err != nil {
		return img, "", err
	}
	defer func() {
		// --rm removes it once stopped; a fresh context, so a cancelled
		// run still cleans up.
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		docker(rctx, "rm", "-f", id)
	}()

	ports, err := docker(ctx, "port", id, "8080/tcp")
	if err != nil {
		if ctx.Err() != nil {
			return img, "", ctx.Err()
		}
		// A container that exits at once (--rm then removes it) has no
		// port: the candidate's failure, as long as the daemon still
		// answers.
		running, inspectErr := docker(ctx, "inspect", "--format", "{{.State.Running}}", id)
		if inspectErr == nil && running == "true" {
			return img, "", err
		}
		if _, verr := docker(ctx, "version", "--format", "{{.Server.Version}}"); verr != nil {
			return img, "", err
		}
		problem = "container exited before /healthz answered"
	} else {
		hostPort, _, _ := strings.Cut(ports, "\n") // e.g. 127.0.0.1:32768
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		problem = waitHealthy(ctx, client, "http://"+hostPort+"/healthz", label, containerBootTimeout-time.Since(start), nil)
	}
	if ctx.Err() != nil {
		return img, "", ctx.Err()
	}
	if problem != "" {
		// Best effort: with --rm, an exited container is already gone.
		logs := new(cappedBuffer)
		cmd := exec.CommandContext(ctx, "docker", "logs", id)
		cmd.Stdout, cmd.Stderr = logs, logs
		if cmd.Run() == nil {
			if out := strings.TrimSpace(logs.String()); out != "" {
				problem += "\ncontainer output:\n" + out
			}
		}
		return img, "container: " + problem, nil
	}
	return img, "", nil
}

// removeImage removes an image this run built, unless it existed before
// the run (an identical binary's image may be in use).
func removeImage(img dockerImage) {
	if img.tag == "" || img.existed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	docker(ctx, "rmi", img.tag)
}
