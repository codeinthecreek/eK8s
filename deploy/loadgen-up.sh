#!/usr/bin/env bash
# Deploys loadgen in ek8s-system at rate r, after deploy/up.sh: builds
# loadgen's image, kind-loads it, renders loadgen's Deployment
# (deploy/loadgen) for r and applies it, then waits for the rollout. A
# re-run with another r replaces the pod (strategy Recreate). Run from
# anywhere; needs go, docker, kind and kubectl.
#   deploy/loadgen-up.sh <r>    e.g. deploy/loadgen-up.sh 2
# EK8S_LOADGEN_CPU_REQUEST lowers loadgen's CPU request (default 1, which is
# also its limit); .github/workflows/deploy.yml sets 100m for the 2-CPU CI
# runner, where a whole core cannot be requested. The limit stays 1.
set -euo pipefail
cd "$(dirname "$0")/.."

cluster=ek8s
ctx="kind-$cluster"
sys=ek8s-system

k() { kubectl --context "$ctx" "$@"; }
die() { echo "loadgen-up.sh: FAIL: $*" >&2; exit 1; }

[ "$#" -eq 1 ] || die "usage: deploy/loadgen-up.sh <r>"
rate=$1
for tool in go docker kind kubectl; do
	command -v "$tool" >/dev/null || die "$tool not on PATH"
done
k get namespace "$sys" >/dev/null 2>&1 || die "namespace $sys missing; run deploy/up.sh"

# A static binary in a FROM scratch image tagged by the binary's sha256,
# as variant-smoke.sh builds generation 0's (gate/docker.go).
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
GOOS=linux CGO_ENABLED=0 go build -trimpath -o "$work/loadgen" ./loadgen/cmd/loadgen
cat >"$work/Dockerfile" <<'EOF'
FROM scratch
COPY loadgen /loadgen
USER 65532:65532
EXPOSE 9090
ENTRYPOINT ["/loadgen"]
EOF
image="ek8s-loadgen:$(sha256sum "$work/loadgen" | cut -c1-12)"
docker build -q -t "$image" "$work" >/dev/null
kind load docker-image "$image" --name "$cluster"

# Rendered to a file first, so a rate the renderer refuses stops here
# with its message and nothing is applied.
go run ./deploy/loadgen/cmd/render -rate "$rate" -image "$image" -cpu-request "${EK8S_LOADGEN_CPU_REQUEST:-1}" >"$work/loadgen.yaml"
k apply -f "$work/loadgen.yaml"
k -n "$sys" rollout status deploy/loadgen --timeout=180s || die "Deployment loadgen not Ready"
echo "loadgen-up.sh: loadgen at r=$rate is Ready ($image)"
