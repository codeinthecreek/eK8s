# Run records

The records behind [docs/RESULTS.md](../RESULTS.md), copied from the
workstation that made the runs. Each directory is named by run id.

## Which directory is which run

| Directory | RESULTS.md run |
|---|---|
| `run1` | run 1 |
| `run2` | the failed first attempt at run 2: generation 0 failed its Stage 5 gate, 0 generations, 0 AI turns |
| `run2b` | run 2 (the second attempt) |
| `run3-20261105T021240Z` | run 3 |
| `run4-20261105T033013Z` | run 4 |
| `c0-20261105T000346Z` | c0 on generation 0 (C = 31) |
| `c0-run4g7-20261105` | c0 on run 4 generation 7 (C = 32) |

## Files

- `run.json`: the host clock state at the start (governors, scaling driver,
  turbo setting). Run 1's holds only `{"version":1}`.
- `summary.json`: why the run stopped, the generation count, and the AI usage
  totals (turns, tokens, cost in USD as reported by `claude -p`).
- `attempts.jsonl`: one JSON record per attempt event: the generation id and
  parent id, the genome sha256, the gate result, the window score
  (baseline and stress halves), the action, and the AI's rationale for the
  mutation. Rows with action `archived` repeat a culled variant's last score;
  count only the other rows when counting scores. The `usage` object in each
  row is cumulative, so use `summary.json` for turns and cost.
- `stalls.jsonl` (runs 3 and 4 only): connection-stall observations for the Stage 5 burst and for each baseline and stress half.
- `run.log`: the orchestrator's log of the run. Its first line gives r, the
  window length and the generation count.
- `c0.json` (the c0 directories): the capacity measurement, with the ladder,
  every step's requests, sheds, failures, successes, p95 and CPU per success,
  and the result.

Genome archives and `claude -p` transcripts are not published.

## Changes from the originals

The only change is in `run.log`: the workstation user's home directory path is replaced by `$HOME`.
All other files are byte-for-byte copies.
