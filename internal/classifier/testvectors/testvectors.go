package testvectors

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"
)

// Version and Params are what vectors.json must declare; the loader
// panics on any other value.
const (
	Version = 1
	Params  = "m=19456,t=2,p=1"
)

// Pair is a password and a hash; Match is whether Verify must accept
// them. Each pool password appears in exactly two pairs: once with its
// own hash (Match true) and once with another password's (Match false).
type Pair struct {
	Password string `json:"password"`
	Hash     string `json:"hash"`
	Match    bool   `json:"match"`
}

// Negative is a well-formed PHC hash at non-exact parameters, for the
// gate's Stage 2 contract checks. Name is the parameter under
// test, e.g. "t=1". Constructed entries were built by hand (a very
// large m cannot be computed) and differ from a canonical hash only in
// the params segment.
type Negative struct {
	Name        string `json:"name"`
	Hash        string `json:"hash"`
	Constructed bool   `json:"constructed"`
}

// File is the vectors.json format.
type File struct {
	Version   int        `json:"version"`
	Params    string     `json:"params"`
	Pairs     []Pair     `json:"pairs"`
	Negatives []Negative `json:"negatives"`
}

//go:embed vectors.json
var raw []byte

var load = sync.OnceValue(func() File {
	f, err := parse(raw)
	if err != nil {
		panic("testvectors: " + err.Error())
	}
	return f
})

// Pairs returns the pool's password/hash pairs. Each call returns a
// fresh slice, so callers cannot alter each other's view.
func Pairs() []Pair { return slices.Clone(load().Pairs) }

// Negatives returns the gate-only negative set; see Negative.
func Negatives() []Negative { return slices.Clone(load().Negatives) }

// parse decodes b strictly: no unknown fields, no trailing data, and
// the declared version and params must be exactly Version and Params.
func parse(b []byte) (File, error) {
	var f File
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return File{}, fmt.Errorf("trailing data after the JSON object")
	}
	if f.Version != Version {
		return File{}, fmt.Errorf("version %d, want %d", f.Version, Version)
	}
	if f.Params != Params {
		return File{}, fmt.Errorf("params %q, want %q", f.Params, Params)
	}
	if len(f.Pairs) == 0 || len(f.Negatives) == 0 {
		return File{}, fmt.Errorf("empty pairs or negatives")
	}
	return f, nil
}
