package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/wrapper"
)

// Fixed at generation 0: none of these are tunables (DESIGN.md sec 5).
const (
	addr            = ":8080"
	maxBodyBytes    = 4 << 10
	drainDelay      = 5 * time.Second  // readiness 503 before Shutdown, for endpoint removal
	shutdownTimeout = 20 * time.Second // drain + this stays under the 30s grace period
	variantHeader   = "X-EK8s-Variant"
)

// The fixed core must satisfy the wrapper's pinned calling interface.
var _ wrapper.Hasher = (*hashcore.Core)(nil)

type config struct {
	variant string
	env     wrapper.Env
}

// parseConfig reads the downward-API environment through lookup (e.g.
// os.LookupEnv). An absent limit is 0 (unconstrained); a present one
// must be a positive integer.
func parseConfig(lookup func(string) (string, bool)) (config, error) {
	cfg := config{variant: "local"}
	if v, ok := lookup("EK8S_VARIANT"); ok && v != "" {
		cfg.variant = v
	}
	var err error
	if cfg.env.CPULimitMillis, err = parseLimit(lookup, "EK8S_CPU_LIMIT_MILLIS"); err != nil {
		return config{}, err
	}
	if cfg.env.MemoryLimitBytes, err = parseLimit(lookup, "EK8S_MEM_LIMIT_BYTES"); err != nil {
		return config{}, err
	}
	return cfg, nil
}

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
// overflow for any positive int64.
func goMemLimit(limit int64) int64 { return limit - limit/10 }

// newHandler builds the fixed route table around the wrapper.
func newHandler(srv *wrapper.Server, variant string, draining *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /hash", srv.HashHandler())
	mux.Handle("POST /verify", srv.VerifyHandler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok")
	})
	ready := srv.ReadyHandler()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		ready.ServeHTTP(w, r)
	})
	// Outside withVariant so MaxBytesReader gets net/http's own writer and can close the connection after an oversized body.
	return limitBody(withVariant(variant, mux))
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// withVariant sets X-EK8s-Variant on every response, overruling any
// value a handler set or deleted. DESIGN.md sec 5, sec 7 make it
// load-bearing in the gate: required on every response.
func withVariant(variant string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vw := &variantWriter{ResponseWriter: w, variant: variant}
		next.ServeHTTP(vw, r)
		if !vw.committed {
			// Nothing written: net/http sends an implicit 200 with the
			// header map as it is now.
			w.Header().Set(variantHeader, variant)
		}
	})
}

// variantWriter forces the variant header just before the response
// headers are committed.
type variantWriter struct {
	http.ResponseWriter
	variant   string
	committed bool
}

func (w *variantWriter) WriteHeader(code int) {
	w.Header().Set(variantHeader, w.variant)
	w.committed = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *variantWriter) Write(b []byte) (int, error) {
	if !w.committed {
		// Write without WriteHeader commits an implicit 200.
		w.Header().Set(variantHeader, w.variant)
		w.committed = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer
// (Flush, deadlines). Writing through it bypasses the variant header,
// which by DESIGN.md sec 5 fails the gate.
func (w *variantWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func main() { os.Exit(run()) }

func run() int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := parseConfig(os.LookupEnv)
	if err != nil {
		log.Error("invalid environment", "err", err)
		return 2
	}
	if cfg.env.MemoryLimitBytes > 0 {
		debug.SetMemoryLimit(goMemLimit(cfg.env.MemoryLimitBytes))
	}
	log.Info("starting", "variant", cfg.variant, "addr", addr,
		"cpu_limit_millis", cfg.env.CPULimitMillis,
		"mem_limit_bytes", cfg.env.MemoryLimitBytes,
		"go_mem_limit", debug.SetMemoryLimit(-1)) // -1 reads without changing

	var draining atomic.Bool
	hs := &http.Server{
		Addr:              addr,
		Handler:           newHandler(wrapper.New(hashcore.New(), cfg.env), cfg.variant, &draining),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      3 * classifier.HangTimeout,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.ListenAndServe() }()

	select {
	case err := <-serveErr:
		log.Error("server failed", "err", err)
		return 1
	case <-ctx.Done():
	}
	stop() // restore default signal handling: a second signal kills at once

	log.Info("shutdown: readiness now 503, draining", "delay", drainDelay)
	draining.Store(true)
	time.Sleep(drainDelay)

	log.Info("shutdown: closing listener, waiting for in-flight requests", "timeout", shutdownTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		log.Error("shutdown timed out, closing connections", "err", err)
		hs.Close()
		return 1
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed during shutdown", "err", err)
		return 1
	}
	log.Info("shutdown complete")
	return 0
}
