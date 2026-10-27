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
//
// Loop is the generational process over them: tournament selection
// from the lineage archive, a mutation (driver) while the host makes the
// next window's pairs, the gate (GateRunner: Stages 1-4, stress on,
// Stage 5), the newcomer's probation window, integrity culls, then the
// lowest score back to LiveCap, every attempt recorded in lineage, and
// the run's stopping rules. cmd/orchestrator runs it.
package orchestrator
