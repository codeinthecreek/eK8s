// Command loadgen serves loadgen's control API (DESIGN.md sec 10) on
// :9090. Its flags are its start-up configuration (loadgen.Config); all
// are required, and none has a production default. Its one environment
// variable, EK8S_MEM_LIMIT_BYTES, is the pod's memory limit from the
// downward API; absent means no limit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/codeinthecreek/eK8s/loadgen"
)

// addr is the control API's address. No flag changes it.
const addr = ":9090"

// The control API server's timeouts. An upload is up to about 20 MiB and
// a records page several MB, both through the API server's proxy:
// readTimeout bounds a request's headers and body together, and
// writeTimeout, which runs from the end of the headers, bounds reading
// the body, the handler and writing the response. Both are generous for
// those sizes but still end a stalled connection.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 2 * time.Minute
	writeTimeout      = 2 * time.Minute
	idleTimeout       = time.Minute
)

// shutdownTimeout bounds shutdown: the control API's requests in flight
// and then every cancelled run's sends in flight. A send ends within
// loadgen's client timeout, cmd/service's WriteTimeout + 1 s (7 s), so
// 10 s lets each be recorded, and stays under the 15 s grace period
// deploy/loadgen sets. An upload or records page still in flight when it
// ends is cut off: the process is exiting, and its records die with it.
const shutdownTimeout = 10 * time.Second

// listen opens the control API's listener. Tests replace it.
var listen = func() (net.Listener, error) { return net.Listen("tcp", addr) }

// lookupEnv reads the environment. Tests replace it.
var lookupEnv = os.LookupEnv

// memLimitEnv is the pod's memory limit in bytes, set from the downward
// API (limits.memory).
const memLimitEnv = "EK8S_MEM_LIMIT_BYTES"

// parseLimit reads the positive integer limit name through lookup (e.g.
// os.LookupEnv). An absent limit is 0 (unconstrained); a present one
// must be a positive integer. Restated from cmd/service, which loadgen
// does not import.
func parseLimit(lookup func(string) (string, bool), name string) (int64, error) {
	v, ok := lookup(name)
	if !ok {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q: must be a positive integer", name, v)
	}
	return n, nil
}

// goMemLimit is 90% of the pod memory limit, written so it cannot
// overflow for any positive int64. Restated from cmd/service: the GC
// works harder near it, before the kernel's OOM kill at the pod limit
// would end every run and lose every record.
func goMemLimit(limit int64) int64 { return limit - limit/10 }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	context.AfterFunc(ctx, stop) // restore default signal handling: a second signal kills at once
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		os.Exit(1)
	}
}

// run parses args and the environment, sets the Go memory limit, serves
// the control API until ctx ends, then shuts down: ctx is the Control's,
// so its end cancels every run. It returns nil after a clean shutdown. A
// bad flag or environment variable is printed to stderr and returned
// before anything listens.
func run(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	memLimit, err := parseLimit(lookupEnv, memLimitEnv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}
	if memLimit > 0 {
		debug.SetMemoryLimit(goMemLimit(memLimit))
	}
	c, err := loadgen.NewControl(ctx, cfg, loadgen.NewSender())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return err
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ln, err := listen()
	if err != nil {
		log.Error("loadgen: listen failed", "addr", addr, "err", err)
		return err
	}
	// Only the config and boot_id: never a request body, a pair, a
	// password, a record or Evidence.
	log.Info("loadgen starting", "addr", ln.Addr().String(), "boot_id", c.BootID(),
		"rate", cfg.Rate, "window", cfg.Window, "burst_rate", cfg.BurstRate,
		"target_url", cfg.TargetURL, "canary_addr", cfg.CanaryAddr,
		"mem_limit_bytes", memLimit,
		"go_mem_limit", debug.SetMemoryLimit(-1)) // -1 reads without changing

	hs := &http.Server{
		Handler:           c.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	select {
	case err := <-serveErr:
		log.Error("loadgen stopped: serving failed", "err", err)
		return err
	case <-ctx.Done():
	}

	t0 := time.Now()
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Shutdown first, so that no start can come in once Wait returns.
	err = hs.Shutdown(sctx)
	if err == nil {
		err = c.Wait(sctx)
	}
	if err != nil {
		hs.Close()
		log.Error("loadgen stopped: shutdown timed out", "timeout", shutdownTimeout, "err", err)
		return err
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		log.Error("loadgen stopped: serving failed during shutdown", "err", err)
		return err
	}
	log.Info("loadgen stopped", "took", time.Since(t0))
	return nil
}

// requiredFlags is every flag: each must be set.
var requiredFlags = []string{"rate", "window", "burst-rate", "target-url", "canary-addr"}

// parseFlags returns the Config args set, which Config.Validate accepts.
// It prints any error, with the usage, to stderr.
func parseFlags(args []string, stderr io.Writer) (loadgen.Config, error) {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg loadgen.Config
	fs.Float64Var(&cfg.Rate, "rate", 0, "r: each window stream's sends per second")
	fs.DurationVar(&cfg.Window, "window", 0, "W: a window's length, e.g. 60s")
	fs.Float64Var(&cfg.BurstRate, "burst-rate", 0, "b: a Stage 5 burst's sends per second")
	fs.StringVar(&cfg.TargetURL, "target-url", "", "a variant's base URL, with {id} where its id goes")
	fs.StringVar(&cfg.CanaryAddr, "canary-addr", "", "the policy canary's target, an IP literal host:port")
	if err := fs.Parse(args); err != nil {
		return loadgen.Config{}, err // fs has printed it, with the usage
	}
	err := missingFlag(fs)
	if err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("loadgen: unexpected argument %q", fs.Arg(0))
	}
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		fs.Usage()
		return loadgen.Config{}, err
	}
	return cfg, nil
}

// missingFlag returns an error naming the first of requiredFlags that fs
// did not set, nil if it set them all.
func missingFlag(fs *flag.FlagSet) error {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, name := range requiredFlags {
		if !set[name] {
			return fmt.Errorf("loadgen: flag -%s is required", name)
		}
	}
	return nil
}
