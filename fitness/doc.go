// Package fitness scores one variant's observation window (DESIGN.md sec
// 9) from loadgen's records and the pod CPU counters the kubelet reports.
// It imports the standard library, internal/classifier and loadgen/wire
// only, and does no Argon2: integrity failures are counted here but
// confirmed on the host by the orchestrator, which culls before scoring.
//
// The first run scores one baseline half and no stress half (docs/
// decisions.md, PR #52 S4 and the PR #56 entry), so a variant's fitness
// is its baseline score; Combine keeps DESIGN.md's min rule for a run
// that has both halves. Every bound and weight is a fixed absolute value
// (DESIGN.md sec 9): nothing here is relative to the live population.
package fitness
