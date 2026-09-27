package wrapper

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"time"
)

// maxPasswordBytes is fixed by the HTTP contract (DESIGN.md sec 8), not
// a tunable.
const maxPasswordBytes = 1024

// Env carries environmental facts about the pod, never tunables.
type Env struct {
	// CPULimitMillis is the pod CPU limit in millicores; 0 means
	// unconstrained or unknown (e.g. a bare local run with no downward
	// API). Unused in generation 0: Go >= 1.25 already derives
	// GOMAXPROCS from the cgroup CPU limit. Kept for mutants.
	CPULimitMillis int64

	// MemoryLimitBytes is the pod memory limit in bytes; 0 means
	// unconstrained or unknown (e.g. a bare local run with no downward
	// API), and the memory term is dropped from concurrency sizing.
	MemoryLimitBytes int64
}

// Server serves the hash, verify and readiness endpoints.
//
// Env, New and the HashHandler, VerifyHandler and ReadyHandler methods
// are called by the fixed cmd/service: changing their signatures fails
// the build.
type Server struct {
	h     Hasher
	slots chan struct{} // semaphore: one element per in-flight Hasher call
}

// New returns a Server that calls h, with its concurrency limit sized
// from env.
func New(h Hasher, env Env) *Server {
	return &Server{
		h:     h,
		slots: make(chan struct{}, capacity(runtime.GOMAXPROCS(0), env.MemoryLimitBytes)),
	}
}

// capacity is the number of concurrent Hasher calls allowed:
// max(1, min(gomaxprocs, memoryBudget/perHashMemory)), with the memory
// term dropped when memLimit is 0 (unconstrained).
func capacity(gomaxprocs int, memLimit int64) int {
	n := int64(gomaxprocs)
	if memLimit > 0 {
		n = min(n, memLimit*memoryFractionNum/memoryFractionDen/perHashMemory)
	}
	return int(max(1, n))
}

// HashHandler serves POST /hash: {"password": string} -> {"hash": string}.
func (s *Server) HashHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password string `json:"password"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}
		if !validPassword(req.Password) {
			writeError(w, http.StatusBadRequest, "password must be 1-1024 bytes")
			return
		}
		if !s.acquire(w, r) {
			return
		}
		defer s.release()
		hash, err := s.h.Hash(req.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "hashing failed")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Hash string `json:"hash"`
		}{hash})
	})
}

// VerifyHandler serves POST /verify:
// {"password": string, "hash": string} -> {"match": bool}.
func (s *Server) VerifyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password string `json:"password"`
			Hash     string `json:"hash"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body")
			return
		}
		if !validPassword(req.Password) {
			writeError(w, http.StatusBadRequest, "password must be 1-1024 bytes")
			return
		}
		if req.Hash == "" {
			writeError(w, http.StatusBadRequest, "hash is required")
			return
		}
		if !s.acquire(w, r) {
			return
		}
		defer s.release()
		match, err := s.h.Verify(req.Password, req.Hash)
		if err != nil {
			// Per the Hasher contract, an error means only a malformed or
			// unsupported hash: the client's fault, never a match.
			writeError(w, http.StatusBadRequest, "invalid hash")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Match bool `json:"match"`
		}{match})
	})
}

// ReadyHandler serves GET /readyz. Generation 0 is always ready;
// cmd/service returns 503 itself once shutdown begins.
func (s *Server) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok")
	})
}

// acquire waits up to acquireWait for a hashing slot. On success the
// caller must call release exactly once. On timeout it writes a 503
// with Retry-After; if the client goes away first it writes nothing.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request) bool {
	timer := time.NewTimer(acquireWait)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		return true
	case <-timer.C:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		writeError(w, http.StatusServiceUnavailable, "overloaded, retry later")
		return false
	case <-r.Context().Done():
		return false
	}
}

// release frees the slot taken by a successful acquire.
func (s *Server) release() { <-s.slots }

var errTrailingData = errors.New("trailing data after JSON object")

// decode reads exactly one JSON value from the body into v. Unknown
// fields are ignored; trailing non-whitespace data is an error, and so
// is any read error (including *http.MaxBytesError from the body limit
// cmd/service applies).
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errTrailingData
	}
	return nil
}

func validPassword(p string) bool {
	return len(p) > 0 && len(p) <= maxPasswordBytes
}

// writeError writes {"error": msg}. msg is a fixed string: never echo
// passwords or hashes.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
