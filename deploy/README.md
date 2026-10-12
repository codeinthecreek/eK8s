# deploy/

The kind cluster config, Kubernetes manifests and the stress-ng noisy
neighbour (DESIGN.md sec 5). Everything here runs on the one Linux
workstation (DESIGN.md sec 2).

```
kind/cluster.yaml     the cluster: name ek8s, one node, Kubernetes 1.37.0
kind/cluster-ci.yaml  the same at CI size (100m / 256Mi reserved)
metrics-server/       metrics-server v0.9.0, vendored, + --kubelet-insecure-tls
stress/               namespace ek8s-stress: stress-ng Deployment (0 replicas),
                      its dose ConfigMap, a deny-all NetworkPolicy
stress-ci/            overlay of stress/ at CI size (500m / 512Mi, 1 CPU worker)
up.sh                 create the cluster (if absent) and apply the above
smoke.sh              infra smoke test (run after up.sh)
```

The per-variant bundle (namespace `ek8s-v<id>`, LimitRange, Deployment
`hasher`, Service, HPA, NetworkPolicy) comes in a later PR, rendered by
Go from templates and applied by gate Stage 5.

## Cluster

- Name: **ek8s**, kubectl context **kind-ek8s**. The scripts pass
  `--context kind-ek8s` to every kubectl call; `kind create cluster`
  itself also makes kind-ek8s your current context.
- One control-plane node, which also runs every workload.
- The API server listens on 127.0.0.1 only. No host ports are mapped:
  loadgen runs inside the cluster, in namespace `ek8s-system`, and reaches
  each variant by Service DNS (DESIGN.md sec 10, sec 11).
- The kubelet reserves 1 CPU and 2Gi (kubeReserved + systemReserved), so
  pod requests are scheduled against the ~7 cores / ~13-14 GB budget of
  DESIGN.md sec 2, not the whole host.

## Host setup

Needed on the workstation, in addition to the gate's tools (CLAUDE.md,
Build / test):

| Tool | Version | Check |
|------|---------|-------|
| docker | a running daemon | `docker info` |
| kind | v0.33.0 | `kind version`; linux-amd64 binary sha256 `aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d` (the release's `kind-linux-amd64.sha256sum`) |
| kubectl | v1.37.x (within one minor of the cluster) | `kubectl version --client`; verify the download against its `.sha256` file from dl.k8s.io |
| cgroup v2 | Kubernetes 1.35+ needs it | `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs` |

The node image (`kindest/node:v1.37.0@sha256:...`, kind/cluster.yaml) and
metrics-server (`registry.k8s.io/...@sha256:...`) are pulled by digest from
public registries. stress-ng is pulled by digest on the host and loaded into
the node with `kind load`, so the node never pulls it.

### Unprivileged user namespaces for the gate's bare boot

The gate runs on this same host and boots candidates under bubblewrap,
which needs unprivileged user namespaces with capabilities. Ubuntu >= 23.10
restricts them through AppArmor
(`kernel.apparmor_restrict_unprivileged_userns=1`).

Pick exactly one of these, in this order:

**(a) Preferred: the upstream `bwrap-userns-restrict` profile.** Its
profile is named `bwrap` (attached to /usr/bin/bwrap) and has a child
profile `unpriv_bwrap` for what bwrap runs. Check whether it is loaded:
`sudo aa-status | grep bwrap`. Ubuntu 25.04 and later ship it in the
apparmor package as `/etc/apparmor.d/bwrap-userns-restrict`. Ubuntu 24.04
does not; there it comes with the apparmor-profiles package:

```
sudo apt install apparmor-profiles
sudo cp /usr/share/apparmor/extra-profiles/bwrap-userns-restrict /etc/apparmor.d/
sudo apparmor_parser -r /etc/apparmor.d/bwrap-userns-restrict
```

**(b) Only if (a) is unavailable: a hand-written unconfined profile.**
Never alongside (a): both declare `profile bwrap /usr/bin/bwrap`, so they
clash and the last one loaded wins. Create `/etc/apparmor.d/bwrap`:

```
abi <abi/4.0>,
include <tunables/global>

profile bwrap /usr/bin/bwrap flags=(unconfined) {
  userns,

  include if exists <local/bwrap>
}
```

and load it with `sudo apparmor_parser -r /etc/apparmor.d/bwrap`. This is
the shape of upstream AppArmor's unconfined profiles for other programs
that need user namespaces (e.g. busybox): bwrap stays unconfined and gets
`userns`; the restriction stays on for everything else. `abi <abi/4.0>`
matches AppArmor 4.0 (Ubuntu 24.04).

**(c) Last fallback: lift the restriction host-wide**, as CI does:
`sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0` (add it to
`/etc/sysctl.d/` to keep it across reboots). This affects every program on
the host.

**Pass criterion** (by hand, on the workstation), whichever you chose:

```
EK8S_GATE_REQUIRE_TOOLS=1 go test -count=1 ./gate/
```

A loaded profile is not enough: the gate tests also exercise the nsenter
relay that joins the sandbox's namespaces, which the `unpriv_bwrap` child
profile in (a) might affect (unverified). For a quick first check,
`bwrap --unshare-all --unshare-user --ro-bind / / true && echo userns ok`
fails under the restriction with "loopback: Failed RTM_NEWADDR".

## Bring-up and smoke test

```
deploy/up.sh      # create cluster ek8s if absent; load stress-ng; apply infra
deploy/smoke.sh   # node reservation, metrics-server, stress-ng on and off
kind delete cluster --name ek8s   # tear down
```

`up.sh` is safe to re-run; each run resets stress-ng to off.
`up.sh` is setup only: because it resets stress-ng to off, never run it
during an orchestrator run.

### Parameters and CI size

Both scripts take their sizes from the environment; the defaults are the
full-size workstation setup, so running them with nothing set is the real
configuration.

| Variable | Script | Default (full size) | CI size |
|----------|--------|---------------------|---------|
| `EK8S_KIND_CONFIG` | up.sh | `kind/cluster.yaml` | `kind/cluster-ci.yaml` |
| `EK8S_STRESS_KUSTOMIZE` | up.sh | `stress` | `stress-ci` |
| `EK8S_RESERVED_CPU_MILLIS` | smoke.sh | `1000` | `100` |
| `EK8S_STRESS_MIN_CPU_MILLIS` | smoke.sh | `1000` | `250` |

Paths are relative to deploy/. The reserved value must equal the kind
config's kubeReserved + systemReserved CPU; the stress minimum must sit
below the stress-ng CPU limit (3 CPU full size, 500m CI size).

CI (`.github/workflows/deploy.yml`) runs up.sh then smoke.sh at CI size on
every pull request touching deploy/ or the workflow; the repo is private, so
its ubuntu-latest runner has 2 CPU / 8 GB, too small for the full-size
reservation and stress dose. On failure it dumps `kubectl get pods -A`,
`kubectl describe node` and the failing pods' events. It proves the
mechanics (pinned images, kustomizations, PSA, metrics-server, stress-ng on
and off), not the full-size numbers; those need a workstation run.
`workflow_dispatch` (manual runs) works only once the workflow is on the
default branch.

## stress-ng

Deployment `stress-ng` in namespace `ek8s-stress`, 0 replicas (off) by
default. **The orchestrator owns stress state** (DESIGN.md sec 7, sec 11):
it scales the Deployment to 1 for each stress half and back to 0 for each
baseline half, and turns it on, marking the window excluded from scoring,
before it calls gate Stage 5. Stage 5 only checks that stress-ng is
Running; anything else is a gate fault.

By hand:

```
kubectl --context kind-ek8s -n ek8s-stress scale deploy/stress-ng --replicas=1   # on
kubectl --context kind-ek8s -n ek8s-stress scale deploy/stress-ng --replicas=0   # off
```

The dose lives in ConfigMap `stress-dose` (stress/dose.yaml): CPU workers,
per-worker CPU load, VM workers and VM bytes. Change it while stress is off;
the next scale-up reads the new values. The pod is Guaranteed QoS at 3 CPU /
2Gi; keep `VM_BYTES` under the 2Gi limit. All values are starting guesses
(DESIGN.md sec 15).
