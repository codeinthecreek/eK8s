# deploy/

The kind cluster config, Kubernetes manifests and the stress-ng noisy
neighbour (DESIGN.md sec 5). Everything here runs on the one Linux
workstation (DESIGN.md sec 2).

```
kind/cluster.yaml     the cluster: name ek8s, one node, Kubernetes 1.37.0
kind/cluster-ci.yaml  the same at CI size (10m / 256Mi reserved, default CPU manager)
calico/               Calico v3.33.0, vendored, the CNI and NetworkPolicy engine
                      (kind's kindnet disabled), + four patches
metrics-server/       metrics-server v0.9.0, vendored, + --kubelet-insecure-tls
stress/               namespace ek8s-stress: stress-ng Deployment (0 replicas),
                      its dose ConfigMap, a deny-all NetworkPolicy
stress-ci/            overlay of stress/ at CI size (500m / 512Mi, 1 CPU worker)
ek8s-system/          namespace ek8s-system: loadgen's control Service and
                      its NetworkPolicy (not its Deployment)
loadgen/              loadgen's Deployment: template + Go renderer (cmd/render)
variant/              the per-variant bundle: templates + Go renderer
up.sh                 create the cluster (if absent) and apply the above
down.sh               delete the cluster (exit 0 if there is none)
loadgen-up.sh <r>     build loadgen's image, load it and apply its Deployment
smoke.sh              infra smoke test (run after up.sh)
variant-smoke.sh      variant bundle smoke test (run after up.sh)
loadgen-smoke.sh      loadgen smoke test (run after up.sh; deploys loadgen)
watch.sh <run> [log]  read-only tmux panes for watching a run (pods, top, HPAs,
                      events, node docker stats, attempts.jsonl, the log)
diag/                 diagnostics, workstation only, never in a run or CI
                      (stage5-runs.sh: Stage 5 runs that validate D2, Calico)
```

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
- No kind default CNI (`disableDefaultCNI`): Calico is the CNI and the
  NetworkPolicy engine (Calico below). Pod subnet 10.244.0.0/16.

## Calico

`calico/` vendors Calico v3.33.0's release manifest (`calico.yaml`,
unmodified; its URL, tag commit and sha256 are in `kustomization.yaml`,
with a re-check command) and pins its three images from quay.io by
digest, the way `metrics-server/` does. No operator: one `calico-node`
DaemonSet (Felix, the iptables dataplane) and the
`calico-kube-controllers` Deployment. up.sh applies it server-side (the
CRD bundle is too large for a client-side apply). The patches:

- IP pool `default-ipv4-ippool`: 10.244.0.0/16 (both kind configs' pod
  subnet), no IPIP tunnel (one node).
- `FELIX_NFTABLESMODE=Disabled`: the iptables dataplane, pinned.
- CNI `policy_setup_timeout_seconds: 10`: a new pod starts only once Felix
  reports its policy programmed; past 10 s the CNI starts it anyway and
  logs `Unblocking pod creation` to `/var/log/calico/cni/cni.log` on the
  node. variant-smoke.sh and a final CI step fail on any such line.
- `calico-node` requests 100m CPU (the manifest has 250m), on CI and the
  workstation alike.

`FELIX_DEFAULTENDPOINTTOHOSTACTION` stays the manifest's `ACCEPT`: a pod's
traffic to the node itself passes only after the pod's own egress policy
did, and coredns, metrics-server and calico-kube-controllers need the
node. Calico fails closed: until Felix has programmed a pod, every packet
it sends, and every packet forwarded to it, is dropped (the "Unknown
interface" rule; smoke.sh checks the rule is in place). Traffic from the
node to a pod on it is never policed (Felix's rules, static.go), which is
how the API server's proxy reaches loadgen (ek8s-system below).

A cluster built before Calico still runs kindnet: up.sh refuses it, so
delete and recreate it (`kind delete cluster --name ek8s`, then
`deploy/up.sh`).

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
deploy/up.sh      # create cluster ek8s if absent; apply Calico; load stress-ng; apply infra
deploy/smoke.sh   # Calico, node reservation, metrics-server, stress-ng on and off
deploy/loadgen-up.sh 2   # build and deploy loadgen at r = 2 (see loadgen below)
deploy/down.sh    # delete cluster ek8s (exit 0 with a message if absent)
```

`up.sh` is safe to re-run; each run resets stress-ng to off.
`up.sh` is setup only: because it resets stress-ng to off, never run it
during an orchestrator run.

### Fresh-cluster check order

On a fresh cluster, in this order (the owner's workstation, pinned clocks
for the diag step):

```
deploy/down.sh
deploy/up.sh
deploy/smoke.sh
deploy/variant-smoke.sh      # refuses while a loadgen pod exists
deploy/loadgen-smoke.sh
deploy/stage5-smoke.sh
deploy/diag/stage5-runs.sh -rate R -try   # deploys loadgen itself; then without -try
```

Never run `loadgen-up.sh` before the smokes: variant-smoke.sh refuses
while loadgen pods exist, and each loadgen smoke deploys and removes its
own. `stage5-runs.sh` deploys loadgen at its `-rate` and leaves it. Each
smoke deletes what it created and waits for it to be gone, so a smoke run
after another finds nothing of the earlier one.

### Parameters and CI size

Both scripts take their sizes from the environment; the defaults are the
full-size workstation setup, so running them with nothing set is the real
configuration.

| Variable | Script | Default (full size) | CI size |
|----------|--------|---------------------|---------|
| `EK8S_KIND_CONFIG` | up.sh | `kind/cluster.yaml` | `kind/cluster-ci.yaml` |
| `EK8S_STRESS_KUSTOMIZE` | up.sh | `stress` | `stress-ci` |
| `EK8S_RESERVED_CPU_MILLIS` | smoke.sh | `1000` | `10` |
| `EK8S_STRESS_MIN_CPU_MILLIS` | smoke.sh | `1000` | `250` |

Paths are relative to deploy/. The reserved value must equal the kind
config's kubeReserved + systemReserved CPU; the stress minimum must sit
below the stress-ng CPU limit (3 CPU full size, 500m CI size).

CI (`.github/workflows/deploy.yml`) runs up.sh, smoke.sh,
variant-smoke.sh and loadgen-smoke.sh at CI size on
every pull request touching deploy/, cmd/service/, internal/wrapper/,
loadgen/, internal/classifier/ or the workflow; the repo is private, so
its ubuntu-latest runner has 2 CPU / 8 GB, too small for the full-size
reservation and stress dose. On failure it dumps `kubectl get pods -A`,
`kubectl describe node` and the failing pods' events. It proves the
mechanics (pinned images, kustomizations, PSA, metrics-server, stress-ng on
and off, the variant bundle), not the full-size numbers; those need a workstation run.
`workflow_dispatch` (manual runs) works only once the workflow is on the
default branch.

## Variant bundle

Each variant lives in its own namespace, `ek8s-v<id>` (DESIGN.md sec 11),
rendered from `variant/*.yaml.tmpl` by package `deploy/variant` and
applied by gate Stage 5 (not yet built):

| Object | What it holds |
|--------|---------------|
| Namespace `ek8s-v<id>` | Pod Security `restricted`; label `ek8s.io/role: variant` (loadgen's egress policy selects it) |
| LimitRange | every container: CPU 250m request / 1000m limit, memory 256Mi request = limit |
| NetworkPolicy | all pods: no egress; ingress TCP 8080 only from `ek8s-system` pods labelled `app=loadgen` |
| Deployment `hasher` | the candidate image (`imagePullPolicy: Never`), no `replicas` field (the HPA owns it), env from the downward API, readiness `/readyz` (every 2s, timeout 2s, 3 failures), liveness `/healthz`, 30s grace, restricted securityContext, no service account token |
| Service `hasher` | port 8080: `hasher.ek8s-v<id>.svc.cluster.local:8080` |
| HPA `hasher` | 1-2 pods at 70% of the CPU request |

The renderer takes only a decimal id, a label `ek8s-<16 hex>` (random per
deployment, DESIGN.md sec 5) and an image `ek8s-wrapper:<12 hex>` (the
gate's tag), and rejects anything else. By hand:

```
go run ./deploy/variant/cmd/render -id 1 -label ek8s-0123456789abcdef \
  -image ek8s-wrapper:0123456789ab | kubectl --context kind-ek8s apply -f -
kubectl --context kind-ek8s delete namespace ek8s-v1   # removes it all
```

After a template change, `go test ./deploy/variant -update` rewrites the
golden files; review their diff.

`variant-smoke.sh` builds generation 0's image, kind-loads it and a curl
image (`curlimages/curl` 8.22.0 by its linux/amd64 digest), applies one bundle (id
999999), starts a stand-in for loadgen in `ek8s-system` (labelled
`app=loadgen`, busybox `nc` answering HTTP on the control port) and checks:
the pod is Ready with the LimitRange's resources and its env; the stand-in
gets `/healthz` and a `/hash` 200 through the Service by DNS name; the API
server's service proxy reaches the stand-in's control port, and neither a
pod in the variant namespace nor an `ek8s-system` pod without
`app=loadgen` does; that pod is also refused by the variant; neither the
stand-in nor a pod in the variant namespace reaches an external address or
the API server or the kubelet's port on the node, and the stand-in cannot
use a DNS server other than kube-dns (each first shown reachable from the
`ek8s-system` pod without `app=loadgen`); a pod in `ek8s-stress`, under
its deny-all policy, reaches none of those targets, nor the variant or
DNS, and that `ek8s-system` pod cannot reach it, though the node can (each
refusal paired with a target shown reachable); with the variant at 2 pods
(HPA `minReplicas` raised in the smoke only), a deleted pod's EndpointSlice entry is `ready=false`,
`terminating=true` within 2s while the other stays ready; and deleting the
namespace removes everything. Last, it fails on any `Unblocking pod
creation` line its run added to the node's CNI log. It needs `ek8s-system` from up.sh and
removes what it created on success; on failure it leaves the variant
namespace and the smoke pods for inspection. CI runs it after smoke.sh.

## ek8s-system

Namespace `ek8s-system` (Pod Security `restricted`), where loadgen runs
(DESIGN.md sec 10). up.sh applies it; loadgen's Deployment is not part of
it (see loadgen below).

| Object | What it holds |
|--------|---------------|
| Service `loadgen` | ClusterIP, port `control` 9090 -> the pod port named `control`; reached from the host as `kubectl get --raw /api/v1/namespaces/ek8s-system/services/loadgen:control/proxy/...` |
| NetworkPolicy `loadgen` | pods `app=loadgen`: no ingress rule; egress TCP 8080 to namespaces labelled `ek8s.io/role: variant`, DNS (UDP/TCP 53) to kube-dns only |

With no ingress rule, the API server's proxy gets in only because Calico
never polices traffic from the node to a pod on it. That covers every
process in the node's network namespace, under any uid, and every
hostNetwork pod; in this cluster all of them run as root (the API server,
the kubelet, the control plane, kube-proxy, calico-node), and all can
already exec into loadgen. The ek8s namespaces are Pod Security
`restricted`, which forbids hostNetwork (DESIGN.md sec 11).

## loadgen

Deployment `loadgen` in `ek8s-system`, rendered from
`loadgen/deployment.yaml.tmpl` by package `deploy/loadgen` and applied by
`loadgen-up.sh`, not by the ek8s-system kustomization. One replica,
strategy `Recreate`, 1 CPU / 384Mi Guaranteed (an exclusive core under the workstation cluster's static CPU manager; `EK8S_LOADGEN_CPU_REQUEST` lowers only the CPU request, 100m in CI), a `tcpSocket` readiness
probe on the container port `control` (9090) and no liveness probe, a 15 s
grace period, `GOMEMLIMIT` from the downward API, no service-account
token (DESIGN.md sec 10). The renderer takes only the rate `r` and an
image `ek8s-loadgen:<12 hex>`; it fills `-rate`, `-window` and
`-burst-rate` from `r` alone (b = 4r, W = 5 min), refuses an r not in its
shortest decimal form, and refuses anything loadgen itself would refuse
(such as r above 19).

```
deploy/loadgen-up.sh 2   # r = 2, the smoke run's rate
go run ./deploy/loadgen/cmd/render -rate 2 -image ek8s-loadgen:0123456789ab
kubectl --context kind-ek8s -n ek8s-system delete deployment loadgen   # remove it
```

`loadgen-up.sh <r>` builds loadgen (`CGO_ENABLED=0`), tags the image with
the first 12 hex of the binary's sha256, kind-loads it, applies the
rendered Deployment and waits for the rollout. It refuses to run without
`ek8s-system` from up.sh.

`loadgen-smoke.sh` deploys loadgen at r = 2 and checks: the pod is Ready;
its start line shows the memory limit and `GOMEMLIMIT` (90%) it was sized
for; the policy canary reads refused from its pod; 9090 is its only
listening socket (read from an ephemeral container's `/proc/net`) and no UDP
socket is bound; every control endpoint works through the API server's
service proxy, including a refused `DELETE` during a run and a cancelled
run's records; and at r = 19 (the largest rate) the largest pair upload is
accepted. It targets id 999999 and refuses to start if `ek8s-v999999`
exists, so no variant is sent load; it refuses to start beside an
existing `app=loadgen` pod, and deletes its Deployment on exit
(describing the pod and printing loadgen's log first on failure). CI runs
it after variant-smoke.sh.

### Host clocks

Pin the CPU clocks before `orchestrator/cmd/c0`, fitness calibration and
every run: unpinned clocks moved gen 0's CPU per success 2x and its p95 from
27 to 67 ms (docs/decisions.md, 2026-10-29). On an intel_pstate host, as
root (they last until reboot or until a power daemon resets them):

```
echo performance | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor
echo performance | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/energy_performance_preference
echo 1 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo
```

On a host with `cpufreq/boost` instead, `echo 0 | sudo tee /sys/devices/system/cpu/cpufreq/boost`.
c0 reads these (and the scaling driver and energy performance
preference, which it records but does not enforce) under /sys, records them in the
result's `host_clock`, and refuses to start unless every governor is
`performance` and turbo is off where readable (`no_turbo` = 1 or `boost` =
0); a host with no readable governor counts as unpinned. `-allow-unpinned`
overrides the refusal and is recorded as `allow_unpinned`.

`orchestrator/cmd/orchestrator` does the same check (`hostclock.Preflight`),
once at start and before any cluster action, so a change to the clocks
during a run, by hand or by a power daemon, is not caught. It records the state as
`host_clock` and the override as `allow_unpinned` in run.json, and takes
`-allow-unpinned`. The CI smoke (orchestrator-smoke.sh) passes it, since a CI
runner cannot pin its clocks, and checks that run.json records both.

## Watching a run

`deploy/watch.sh RUN_DIR [LOG]` opens one tmux session, `ek8s-watch`, with
a tiled pane each for `kubectl get pods -A -o wide` (every 5 s), `kubectl
top pods -A` and `kubectl get hpa -A` (every 10 s), `kubectl get events -A
--watch`, `docker stats` of the kind node container, and `tail -F` of
`RUN_DIR/attempts.jsonl` and of LOG, a file the orchestrator's output is
saved to (`... 2>&1 | tee run.log`). `-context` (default `kind-ek8s`) and
`-cluster` (default `ek8s`) select the cluster. Every pane only reads; change
nothing by hand during a run, since the orchestrator owns stress-ng, the
variant namespaces and loadgen. It needs tmux, watch, kubectl and docker,
installs nothing, and refuses if the session exists (`tmux attach -t
ek8s-watch` to rejoin, `tmux kill-session -t ek8s-watch` to end it).

## C0

`orchestrator/cmd/c0` measures C0 as DESIGN.md sec 10 defines it: the
highest rate at which generation 0, on one pod (HPA `minReplicas` =
`maxReplicas` = 1) with no stress, has at most 1% of its scored requests
end as `graceful_shed` or `failed`, with the successes' p95 at most
`HangTimeout`/4, in a window that was scored. Gen 0 has 2 wrapper slots
per pod: under the 1000m limit Go sets GOMAXPROCS to 2 (a cgroup limit is
rounded up, with a minimum of 2; docs/decisions-archive.md, PR #26
variant-resources entry), and the slots are min(GOMAXPROCS, memory limit /
2 / 20 MiB). The tool creates a variant bundle (id 900001 by default),
patches its HPA to 1/1, and for each step redeploys loadgen at the step's
rate (`loadgen-up.sh`), runs one 5-minute window through
`orchestrator.Session` and applies the pass rule. It walks `-ladder`
(default 6,9,12,15,18,19) up to the first fail, bisects integer rates
between the last pass and that fail, then runs `-confirm` (default 2) more
windows at the last pass. r = 0.5 x C0; if every ladder rate passes, it
runs `-confirm` more windows at the top rate, and if they pass too the
result is C0 >= the top rate (loadgen's maxRate is 19, so the default
ladder ends there); a failed one leaves C0 unresolved. A step whose window was excluded, or whose pod, HPA or
restart checks failed, is not measured: it reruns once, and a second
invalid step halts. A disqualification of gen 0 halts at once. The JSON
result (`-out`, a new file) is rewritten after every step.

**Making `-image`.** The tool does not build it. Build it as the gate
does, from the files of `internal/wrapper` that `cmd/orchestrator` reads
as generation 0 (every non-test `.go` file; today these four), then
kind-load it. The gate needs the host tools of "Host setup" and a
`go mod download` first; it prints `image ek8s-wrapper:<12 hex>` on a
pass and leaves the image in the local docker daemon:

```
cand=$(mktemp -d)
cp internal/wrapper/doc.go internal/wrapper/hasher.go internal/wrapper/server.go internal/wrapper/tunables.go "$cand"
go run ./gate/cmd/gate -candidate "$cand"
kind load docker-image ek8s-wrapper:<12 hex> --name ek8s
```

This is the image `orchestrator.Loop` deploys for generation 0 *(inferred,
not compared here)*: the Loop writes the same files to a directory, runs
the same `gate.Run` on it and loads the image the report names; the tag is
the first 12 hex of the sha256 of the built binary (`go build -trimpath`),
so the same files and the same Go toolchain give the same tag. A
different toolchain gives another tag for the same source.

**Running it.** From the repo root, with the cluster up and loadgen's
prerequisites met (`up.sh`), stress-ng at 0 replicas and no variant
namespace (the tool refuses otherwise, and refuses unpinned host clocks, see Host clocks, and a ladder rate over
loadgen's maxRate):

```
deploy/up.sh
kubectl --context kind-ek8s -n ek8s-stress get deploy stress-ng -o jsonpath='{.spec.replicas}'
kubectl --context kind-ek8s get namespaces -l ek8s.io/role=variant
go run ./orchestrator/cmd/c0 -image ek8s-wrapper:<12 hex> -out c0.json
```

The second command must print `0` and the third no namespace. Optional
flags: `-id`, `-ladder`, `-confirm`, `-keep` (keep the namespace and the
loadgen target), `-repo`, `-workers`, `-allow-unpinned`. It prints one line
per step to stderr and `C0 13, r = 6.5` (or `C0 >= 19, r = 9.5`) to stdout.
Exit status: 0 for a C0 or a lower bound, 1 when the search halted without
one (ladder too high, a step invalid twice, a confirm window failed: C0
unresolved, everything recorded), 2 on a fault, a disqualification, an
interrupt or a refused start. On exit the namespace and the loadgen
target are deleted unless `-keep`; loadgen stays deployed at the last
stepped rate, so run 2 starts with `deploy/loadgen-up.sh <r>`. The pod, restart and HPA checks compare the state just before the window with the state just after it; a restart between steps is not seen. A run takes about an hour: at most 9 windows of 5 minutes
for the default ladder without reruns, plus a loadgen redeploy each
*(inferred)*.

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
the next scale-up reads the new values. The pod is Burstable (CPU request
2750m, limit 3: the dose stays 3 cores, as in run 2); memory request = limit =
2Gi, so keep `VM_BYTES` under it. All values are starting guesses
(DESIGN.md sec 15).

## Diagnostics

`diag/stage5-runs.sh` asks whether the Stage 5 stall on an HPA pod add,
which PR #70's experiment put on kindnet's NetworkPolicy path, is gone now
that Calico enforces the policies (D2; docs/decisions.md, 2026-11-03). It
runs real `gate -stage5` bursts on generation 0, stress-ng on, against the
cluster as deployed: it removes no policy and patches nothing in loadgen.
It refuses to start while a variant namespace or an orchestrator, c0 or
gate process exists, with local changes under `cmd/service`, `internal/`,
`gate/`, `loadgen/`, `deploy/`, `go.mod` or `go.sum`, or on a commit that
is not on origin/main or PR #71 (it fetches both first, and refuses if it
cannot). Before every gate start it requires calico-node Ready, loadgen's
policy and real canary in place, and stress-ng Running at origin/main's
`deploy/stress` requests, limits and dose; the experiment stops at the
first failed assertion or gate fault. Its stop rule is fixed in advance:
at least 15 runs, more only while fewer than 10 had a mid-burst add, at
most 25, and the first run that is not PASS ends it. Runs about 2 minutes each
(inferred), so 15 to 25 runs take 30 to 50 minutes. On exit, Ctrl-C
included, it stops its gate, deletes its namespace `ek8s-v999997` and
turns stress-ng off; after SIGKILL or a host crash, kill any leftover
gate, delete `ek8s-v999997` and scale stress-ng to 0 before any
orchestrator, c0 or gate run.

```
deploy/diag/stage5-runs.sh -rate 15.5        # the experiment
deploy/diag/stage5-runs.sh -rate 15.5 -try   # one run, no verdict: a dry try
```

`-rate` is loadgen's r (b = 4 x r), deployed by the script; it defaults to
the `-rate` argument of the loadgen already deployed (r = 15.5 for the
owner's Stage 5 runs: docs/decisions.md, PR #65 C0 entry, r = 0.5 x C0),
and is required when none is;
`-out` defaults to `~/workspace/scratch/ek8s-runs/diag-runs-<UTC stamp>/`
(a relative `-out` is taken from the directory you run it from).
A run is PASS only if the gate exits 0, Felix dropped no packet from an
unprogrammed pod ("Unknown interface") and denied none in the burst
(end of tier), and the CNI logged no "Unblocking pod creation" (a counter
that cannot be read is not 0); otherwise it is STALL, FAIL, FAULT or
DROPS (the gate passed, a counter is nonzero). The verdict is in
`verdict.txt`: STALLED on any stall; else INCONCLUSIVE if any run is not
PASS; else MET only with at least 15 runs, at least 10 of them with a
mid-burst add; else INCONCLUSIVE (docs/decisions.md, 2026-11-04). The
end-of-tier count over the whole run includes the canary's denied dial and
is only recorded. `summary.tsv` has
one row per run; each `run-NN/` keeps the gate's output, the state
assertions, records without pairs or evidence, loadgen's SYN and TCP
retransmit counters, the packets dropped by Felix's "Unknown interface"
and end-of-tier rules over the run and over the burst (their counters
read on every poll), calico-node's raw log lines for the run, and any CNI
"Unblocking pod creation" line. The script's header has the details;
`diag/stage5-runs_test.sh` tests its stop rule, verdict and counting
without a cluster.
PR #70's `stage5-nopolicy.sh`, which this replaces, is in git history.
