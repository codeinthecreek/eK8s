package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/codeinthecreek/eK8s/lineage"
)

// wantTools is the session's whole tool set (CLAUDE.md non-negotiable 3),
// sorted.
var wantTools = []string{"Edit", "Read"}

// initEvent is the fields of stream-json's first event that the driver
// checks.
type initEvent struct {
	Type           string            `json:"type"`
	Subtype        string            `json:"subtype"`
	Cwd            string            `json:"cwd"`
	Tools          []string          `json:"tools"`
	MCPServers     []json.RawMessage `json:"mcp_servers"`
	Model          string            `json:"model"`
	PermissionMode string            `json:"permissionMode"`
	Version        string            `json:"claude_code_version"`
}

// checkInit errors unless line is the init event of a session with
// exactly the Edit and Read tools, no MCP server, the pinned model,
// acceptEdits, the sandbox's work directory and claude version version.
// It runs as soon as the first line arrives, which is before the
// session's first API round trip ends in practice (timing, not a
// guarantee), so a session the flags did not confine is killed at once.
func checkInit(line []byte, version string) error {
	var ev initEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return fmt.Errorf("first event is not JSON: %w", err)
	}
	tools := slices.Sorted(slices.Values(ev.Tools))
	switch {
	case ev.Type != "system" || ev.Subtype != "init":
		return fmt.Errorf("first event is %q/%q, want system/init", ev.Type, ev.Subtype)
	case !slices.Equal(tools, wantTools):
		return fmt.Errorf("session tools %q, want exactly %q", ev.Tools, wantTools)
	case len(ev.MCPServers) != 0:
		return fmt.Errorf("session has %d MCP servers, want none", len(ev.MCPServers))
	case ev.Model != PinnedModel:
		return fmt.Errorf("session model %q, want %q", ev.Model, PinnedModel)
	case ev.PermissionMode != "acceptEdits":
		return fmt.Errorf("session permission mode %q, want acceptEdits", ev.PermissionMode)
	case ev.Cwd != sandboxWork:
		return fmt.Errorf("session cwd %q, want %s", ev.Cwd, sandboxWork)
	case ev.Version != version:
		return fmt.Errorf("session claude version %q, want %q", ev.Version, version)
	}
	return nil
}

// streamSink is the session's stdout: it keeps up to max bytes, checks
// the first line with checkInit as soon as it is complete, and calls
// kill on an init failure or on output past max. os/exec writes to it
// from one goroutine; the mutex makes the reads after Wait safe too.
type streamSink struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	max      int
	version  string
	kill     func()
	over     bool
	initSeen bool
	initErr  error
}

func (s *streamSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.over || s.initErr != nil {
		return len(p), nil // keep draining: the kill is under way
	}
	if s.buf.Len()+len(p) > s.max {
		s.over = true
		s.kill()
		return len(p), nil
	}
	s.buf.Write(p)
	if !s.initSeen {
		if i := bytes.IndexByte(s.buf.Bytes(), '\n'); i >= 0 {
			s.initSeen = true
			if err := checkInit(s.buf.Bytes()[:i], s.version); err != nil {
				s.initErr = err
				s.kill()
			}
		}
	}
	return len(p), nil
}

// state returns a copy of what was kept and the sink's verdict: an error
// if the output went past max, its first line failed checkInit or it
// never completed a first line.
func (s *streamSink) state() (out []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.over:
		err = fmt.Errorf("output over %d bytes", s.max)
	case s.initErr != nil:
		err = fmt.Errorf("session refused: %w", s.initErr)
	case !s.initSeen:
		err = errors.New("no init event")
	}
	return bytes.Clone(s.buf.Bytes()), err
}

// resultEvent is the fields of stream-json's last event, the same object
// --output-format json prints, that the driver reads.
type resultEvent struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	SessionID  string  `json:"session_id"`
	NumTurns   int     `json:"num_turns"`
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"total_cost_usd"`
	Usage      struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

// parseStream reads a whole stream-json transcript: every line one JSON
// object with a type, and the last one, alone of its type, the result.
func parseStream(out []byte) (resultEvent, error) {
	lines := bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n"))
	if len(lines) == 0 || len(lines[0]) == 0 {
		return resultEvent{}, errors.New("no output")
	}
	results := 0
	for i, l := range lines {
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(l, &ev); err != nil || ev.Type == "" {
			return resultEvent{}, fmt.Errorf("output line %d is not a JSON event", i+1)
		}
		if ev.Type == "result" {
			results++
		}
	}
	var res resultEvent
	if err := json.Unmarshal(lines[len(lines)-1], &res); err != nil || res.Type != "result" || results != 1 {
		return resultEvent{}, fmt.Errorf("output does not end in its one result event (%d result events)", results)
	}
	return res, nil
}

// usage is the result's usage as a lineage record holds it.
func (r resultEvent) usage() (lineage.Usage, error) {
	u := lineage.Usage{
		NumTurns:            r.NumTurns,
		InputTokens:         r.Usage.InputTokens,
		OutputTokens:        r.Usage.OutputTokens,
		CacheReadTokens:     r.Usage.CacheReadTokens,
		CacheCreationTokens: r.Usage.CacheCreationTokens,
		DurationMS:          r.DurationMS,
		CostUSD:             r.CostUSD,
	}
	if u.NumTurns < 0 || u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 ||
		u.CacheCreationTokens < 0 || u.DurationMS < 0 || !(u.CostUSD >= 0) || math.IsInf(u.CostUSD, 1) {
		return lineage.Usage{}, errors.New("result has negative or non-finite usage")
	}
	return u, nil
}

// ranPinned reports whether the pinned model is among the models the
// session used. Others may appear too (the CLI's own small calls).
func (r resultEvent) ranPinned() bool {
	for m := range r.ModelUsage {
		if m == PinnedModel || strings.HasPrefix(m, PinnedModel+"-") || strings.HasPrefix(m, PinnedModel+"[") {
			return true
		}
	}
	return false
}
