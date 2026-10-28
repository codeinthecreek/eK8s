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
ek8s-system/          namespace ek8s-system: loadgen's control Service and
                      its NetworkPolicy (not its Deployment)
loadgen/              loadgen's Deployment: template + Go renderer (cmd/render)
variant/              the per-variant bundle: templates + Go renderer
up.sh                 create the cluster (if absent) and apply the above
loadgen-up.sh <r>     build loadgen's image, load it and apply its Deployment
smoke.sh              infra smoke test (run after up.sh)
variant-smoke.sh      variant bundle smoke test (run after up.sh)
loadgen-smoke.sh      loadgen smoke test (run after up.sh; deploys loadgen)
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
deploy/loadgen-up.sh 2   # build and deploy loadgen at r = 2 (see loadgen below)
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
the API server, and the stand-in cannot use a DNS server other than
kube-dns (each first shown reachable from the `ek8s-system` pod without
`app=loadgen`); with the variant at 2 pods (HPA `minReplicas` raised in the
smoke only), a deleted pod's EndpointSlice entry is `ready=false`,
`terminating=true` within 2s while the other stays ready; and deleting the
namespace removes everything. It needs `ek8s-system` from up.sh and
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

With no ingress rule, the API server's proxy gets in only because kindnet
never applies policy to traffic from a root-owned socket on the node. That
covers every root process on the node (the API server, the kubelet, the
control plane, root hostNetwork pods), all of which can already exec into
loadgen (DESIGN.md sec 11).

## loadgen

Deployment `loadgen` in `ek8s-system`, rendered from
`loadgen/deployment.yaml.tmpl` by package `deploy/loadgen` and applied by
`loadgen-up.sh`, not by the ek8s-system kustomization. One replica,
strategy `Recreate`, 750m CPU / 256Mi Guaranteed, a `tcpSocket` readiness
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
namespace (the tool refuses otherwise, and refuses a ladder rate over
loadgen's maxRate):

```
deploy/up.sh
kubectl --context kind-ek8s -n ek8s-stress get deploy stress-ng -o jsonpath='{.spec.replicas}'
kubectl --context kind-ek8s get namespaces -l ek8s.io/role=variant
go run ./orchestrator/cmd/c0 -image ek8s-wrapper:<12 hex> -out c0.json
```

The second command must print `0` and the third no namespace. Optional
flags: `-id`, `-ladder`, `-confirm`, `-keep` (keep the namespace and the
loadgen target), `-repo`, `-workers`. It prints one line
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
the next scale-up reads the new values. The pod is Guaranteed QoS at 3 CPU /
2Gi; keep `VM_BYTES` under the 2Gi limit. All values are starting guesses
(DESIGN.md sec 15).
