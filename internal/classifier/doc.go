// Package classifier implements the shared three-bucket response
// classifier and hang-timeout/latency ceiling constant used by gate
// Stage 5, the fitness function, and wrapper's own timeout enforcement
// (DESIGN.md sec 5, sec 8). Never in mutation scope.
package classifier
