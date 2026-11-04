# lineage/

A run's record on disk (DESIGN.md sec 5, sec 14): the genome archive (wrapper files plus a sha256), one JSONL attempt record per generation attempt, one JSONL stall-count line per (loadgen run, target) in `stalls.jsonl` (recording only; nothing in the run reads it), and the raw `claude -p` transcripts. For the first run the structured table is JSONL, not SQLite (docs/decisions.md, PR #52 S2 and PR #55); see `doc.go` for the layout and the rules.
