// Package loadgen is the open-loop load generator (DESIGN.md sec 10).
//
// Its request generator builds what loadgen sends: a Stream produces one
// variant's sustained-mode requests for one window, and Burst produces a
// Stage 5 burst. Both build the same three kinds of request, each in its
// own proportions: /hash with fresh passwords (about 1% repeated),
// /verify from host-generated pairs, and the five classes of contract
// negatives (DESIGN.md sec 8).
//
// Loadgen does no Argon2. Its build never depends, directly or
// transitively, on internal/hashcore, internal/classifier/testvectors,
// gate/ or any Argon2 package; deps_test.go checks this. Its _test.go
// files may import hashcore, to check each negative against it.
package loadgen
