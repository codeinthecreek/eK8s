// Package lineage is the run's record on disk (DESIGN.md sec 14): the
// genome archive, one attempt record per generation attempt, and the raw
// claude -p transcripts. It imports the standard library only.
//
// A run directory holds
//
//	run.json              {"version":1,"host_clock":{...},"allow_unpinned":false}
//	attempts.jsonl        one JSON Attempt per line, append-only
//	genomes/<id>/         the wrapper files and their digest (sha256), made once
//	transcripts/<id>.json the raw claude -p stream-json output (JSONL), made once
//	summary.json          why the run stopped, and its total usage, made once at the end
//
// For the first run the structured table of DESIGN.md sec 14 is this
// JSONL file, not SQLite (docs/decisions.md, PR #52 S2 and the PR #55
// entry); the rows load into SQLite later.
//
// Archive entries and transcripts are written to a temporary name and
// renamed or linked into place, so a crash leaves either the whole entry or none,
// (directories are synced after), and an entry is never overwritten: a second write under the same id is
// an error. Reading an archived genome recomputes its digest and
// refuses a mismatch. An attempt record carries no gate Detail, no
// candidate output and no evidence: those are candidate-controlled text,
// which must never reach a mutation prompt (DESIGN.md sec 12), and a
// record the prompt builder reads must not hold them.
package lineage
