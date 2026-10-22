#!/usr/bin/env bash
# Creates the ek8s kind cluster if it does not exist, loads the stress-ng
# image into it and applies the cluster-wide infrastructure: metrics-server,
# the stress-ng Deployment (left at 0 replicas, stress off) and namespace
# ek8s-system (loadgen's Service and NetworkPolicy; its Deployment comes from
# loadgen-up.sh). Safe to re-run; a re-run resets stress-ng to off. See
# deploy/README.md.
#
# Parameters (environment; the defaults are the full-size workstation setup,
# CI sets the smaller ones, see .github/workflows/deploy.yml):
#   EK8S_KIND_CONFIG       kind config, relative to deploy/ (kind/cluster.yaml)
#   EK8S_STRESS_KUSTOMIZE  stress-ng kustomization dir, relative to deploy/ (stress)
set -euo pipefail
cd "$(dirname "$0")"

cluster=ek8s
ctx="kind-$cluster"
kind_version=v0.33.0
stress_src=colinianking/stress-ng@sha256:f59a28ca71cf104db7c068d226ed77ad6b05afbbcd2560bfbf176f00b1808a7d
stress_tag=ek8s-stress-ng:f59a28ca71cf # must match deploy/stress/deployment.yaml
kind_config=${EK8S_KIND_CONFIG:-kind/cluster.yaml}
stress_kustomize=${EK8S_STRESS_KUSTOMIZE:-stress}

die() { echo "up.sh: $*" >&2; exit 1; }

for tool in docker kind kubectl; do
	command -v "$tool" >/dev/null || die "$tool not on PATH"
done
case "$(kind version)" in
"kind $kind_version "*) ;;
*) die "want kind $kind_version, have: $(kind version)" ;;
esac
[ "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs ] ||
	die "host is not on cgroup v2 (Kubernetes 1.35+ requires it)"

# Captured first: under pipefail, grep -q could exit early and fail the
# pipeline with SIGPIPE.
clusters=$(kind get clusters)
if ! grep -x "$cluster" >/dev/null <<<"$clusters"; then
	kind create cluster --config "$kind_config" --wait 180s
fi

# kind load takes a tagged local image; the pull is by digest, so the tag
# always names the same bytes.
docker pull "$stress_src"
docker tag "$stress_src" "$stress_tag"
kind load docker-image "$stress_tag" --name "$cluster"

kubectl --context "$ctx" apply -k metrics-server
kubectl --context "$ctx" apply -k "$stress_kustomize"
kubectl --context "$ctx" apply -k ek8s-system
kubectl --context "$ctx" -n kube-system rollout status deploy/metrics-server --timeout=180s
echo "up.sh: cluster $cluster ready (context $ctx)"
