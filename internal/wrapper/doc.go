// Package wrapper is the evolvable concurrency, backpressure, and timeout
// layer around the fixed hashing core, which it reaches only through the
// Hasher interface in hasher.go (DESIGN.md sec 3, sec 5). This is the
// generation-0 seed genome: simple and correct, not tuned.
package wrapper
