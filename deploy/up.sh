#!/usr/bin/env bash
# Creates the ek8s kind cluster if it does not exist (kind's default CNI,
# kindnet, disabled), applies Calico as its CNI and NetworkPolicy engine and
# waits for the node to turn Ready, loads the stress-ng image into it and
# applies the cluster-wide infrastructure: metrics-server, the stress-ng
# Deployment (left at 0 replicas, stress off) and namespace ek8s-system
# (loadgen's Service and NetworkPolicy; its Deployment comes from
# loadgen-up.sh). Safe to re-run; a re-run resets stress-ng to off. Refuses a
# cluster built before Calico (it still runs kindnet): delete and recreate it.
# See deploy/README.md.
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
	# No --wait: with no CNI the node is NotReady until calico-node runs,
	# and kind's wait only warns on its timeout (kind v0.33.0
	# waitforready.go), so it would add its full length to every create.
	# The Ready wait follows the Calico apply below.
	kind create cluster --config "$kind_config"
else
	# up.sh never recreates an existing cluster, and one created before the
	# kind configs disabled the default CNI runs kindnet, whose policy path
	# stalls new connections (docs/decisions.md, 2026-11-03). Calico cannot
	# be layered on top of it. A failed lookup refuses too.
	kindnet=$(kubectl --context "$ctx" -n kube-system get daemonset kindnet \
		--ignore-not-found -o name) ||
		die "cannot check cluster $cluster for kindnet (is it running?)"
	[ -z "$kindnet" ] ||
		die "cluster $cluster runs kindnet (built before Calico); delete and recreate it: kind delete cluster --name $cluster, then deploy/up.sh"
fi

# Calico: server-side, since its CRD bundle is too large for a client-side
# apply's last-applied annotation. The node turns Ready once calico-node has
# installed the CNI config; coredns, Pending until then, follows.
kubectl --context "$ctx" apply --server-side -k calico
kubectl --context "$ctx" -n kube-system rollout status ds/calico-node --timeout=300s
kubectl --context "$ctx" -n kube-system rollout status deploy/calico-kube-controllers --timeout=300s
kubectl --context "$ctx" wait --for=condition=Ready node --all --timeout=180s
kubectl --context "$ctx" -n kube-system rollout status deploy/coredns --timeout=180s

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
