#!/usr/bin/env bash
# Deletes the ek8s kind cluster (the counterpart to deploy/up.sh), with
# everything in it: a fresh up.sh follows. Exits 0 with a message when no
# such cluster exists. Takes no arguments. See deploy/README.md.
set -euo pipefail
cd "$(dirname "$0")"

cluster=ek8s # the name up.sh uses

die() { echo "down.sh: $*" >&2; exit 1; }

[ "$#" -eq 0 ] || die "takes no arguments"
command -v kind >/dev/null || die "kind not on PATH"

# Captured first: under pipefail, grep -q could exit early and fail the
# pipeline with SIGPIPE.
clusters=$(kind get clusters)
if ! grep -x "$cluster" >/dev/null <<<"$clusters"; then
	echo "down.sh: no kind cluster $cluster; nothing to delete"
	exit 0
fi
kind delete cluster --name "$cluster"
echo "down.sh: deleted kind cluster $cluster"
