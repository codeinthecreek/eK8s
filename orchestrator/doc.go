// Package orchestrator manages the live variant population, selection,
// and scoring loop (DESIGN.md sec 11).
//
// Its window mechanics are a Session over loadgen's control API and a
// Cluster (Kube, through kubectl): GenerateBatches makes the host's
// /verify pairs in a gap; RunWindow runs one observation window on the
// live variants and judges it on the host, with every exclusion
// decision, a full verification of every recorded /hash pair, a
// re-check of every integrity claim from its evidence, and a baseline
// score per variant; Adopt, Recover and Drop follow a variant's and
// loadgen's lifecycle. The host runs Argon2 here, so no call may overlap
// a window or a gate run (internal/hostpairs).
package orchestrator
