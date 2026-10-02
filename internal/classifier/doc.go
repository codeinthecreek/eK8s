// Package classifier implements the shared three-bucket response
// classifier and hang-timeout/latency ceiling constant used by gate
// Stage 5, the fitness function, and wrapper's own timeout enforcement
// (DESIGN.md sec 5, sec 8). Never in mutation scope.
//
// internal/wrapper imports this package, so it stays stdlib-only: it
// must never import internal/hashcore, argon2id, golang.org/x/crypto or
// any crypto package, and must never hold test vectors. Full hash
// verification is injected by the caller as a Verifier.
package classifier
