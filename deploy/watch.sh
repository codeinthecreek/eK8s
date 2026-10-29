#!/usr/bin/env bash
# Watches an orchestrator run on the workstation: one tmux session, one
# window, a tiled pane per view:
#   - pods on every namespace, with their node and IP (every 5 s);
#   - pod CPU and memory from metrics-server (every 10 s);
#   - the variants' HPAs (every 10 s);
#   - cluster events as they happen;
#   - docker stats of the kind node container;
#   - the run's attempts.jsonl as it grows, and the orchestrator's log
#     (its stderr, saved by the caller, e.g. `2>&1 | tee run.log`) when a
#     log path is given.
# Every pane only reads state: nothing here scales, patches, deletes or
# restarts anything. Change nothing by hand during a run either: the
# orchestrator owns stress-ng, the variant namespaces and loadgen, and a
# hand-made change can exclude windows, halt the run or skew its scores
# (DESIGN.md sec 11).
#
# Usage: deploy/watch.sh [-context CTX] [-cluster NAME] RUN_DIR [LOG]
#   RUN_DIR  the orchestrator's -run directory (it need not exist yet)
#   LOG      optional: a file the orchestrator's output is written to
#   -context the kubectl context (default kind-ek8s)
#   -cluster the kind cluster name, for its node container (default ek8s)
# Needs tmux, watch, kubectl, docker and tail on PATH; it installs
# nothing. Refuses if its tmux session (ek8s-watch) exists: attach to it
# with `tmux attach -t ek8s-watch`, or end it with
# `tmux kill-session -t ek8s-watch`. Detach with C-b d; the panes keep
# running until the session is killed.
set -euo pipefail

session=ek8s-watch
ctx=kind-ek8s
cluster=ek8s

die() { echo "watch.sh: $*" >&2; exit 1; }
usage() { die "usage: deploy/watch.sh [-context CTX] [-cluster NAME] RUN_DIR [LOG]"; }

while [ $# -gt 0 ]; do
	case "$1" in
	-context) [ $# -ge 2 ] || usage; ctx=$2; shift 2 ;;
	-cluster) [ $# -ge 2 ] || usage; cluster=$2; shift 2 ;;
	--) shift; break ;;
	-*) usage ;;
	*) break ;;
	esac
done
[ $# -ge 1 ] && [ $# -le 2 ] || usage
run=$1
log=${2:-}
[ -n "$run" ] && [ -n "$ctx" ] && [ -n "$cluster" ] || usage

for tool in tmux watch kubectl docker tail; do
	command -v "$tool" >/dev/null || die "$tool is not on PATH; install it first (this script installs nothing)"
done
! tmux has-session -t "=$session" 2>/dev/null ||
	die "tmux session $session exists: attach with 'tmux attach -t $session' or end it with 'tmux kill-session -t $session'"

# Each pane's command, its arguments quoted for the shell tmux starts it in.
q() { printf '%q ' "$@"; }
panes=(
	"$(q watch -n 5 kubectl --context "$ctx" get pods -A -o wide)"
	"$(q watch -n 10 kubectl --context "$ctx" top pods -A)"
	"$(q watch -n 10 kubectl --context "$ctx" get hpa -A)"
	"$(q kubectl --context "$ctx" get events -A --watch)"
	"$(q docker stats "$cluster-control-plane")"
	"$(q tail -F "$run/attempts.jsonl")"
)
if [ -n "$log" ]; then
	panes+=("$(q tail -F "$log")")
fi

tmux new-session -d -s "$session" -n run "${panes[0]}"
# A pane whose command ends (a dropped watch, a stopped container) stays
# on screen with its last output instead of vanishing.
tmux set-option -t "$session" remain-on-exit on >/dev/null
for cmd in "${panes[@]:1}"; do
	tmux split-window -t "$session:run" "$cmd"
	tmux select-layout -t "$session:run" tiled >/dev/null
done
tmux select-pane -t "$session:run.0"

if [ -n "${TMUX:-}" ]; then
	exec tmux switch-client -t "$session"
fi
exec tmux attach-session -t "$session"
