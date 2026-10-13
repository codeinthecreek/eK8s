#!/usr/bin/env bash
# Variant bundle smoke test, after deploy/up.sh: builds generation 0's
# image, kind-loads it, renders and applies one variant bundle
# (deploy/variant) and checks
#   (a) the Deployment is Ready with one pod named like a Deployment pod,
#       carrying the LimitRange's resources, and its env (as cmd/service
#       logs it) carries the limits and the label;
#   (b) a pod in ek8s-system labelled app=loadgen gets /healthz and a /hash
#       200 from the Service;
#   (c) the same pod, with that label removed, is refused;
#   (d) a pod in the variant namespace has no egress: no external address,
#       no API server (each target first shown reachable from ek8s-system,
#       where no policy applies);
#   (e) deleting the namespace removes everything in it.
# Exits non-zero on the first failed check. Run from anywhere; needs go,
# docker, kind and kubectl. On failure it leaves the namespaces as they
# are for inspection; on success it removes what it created.
set -euo pipefail
cd "$(dirname "$0")/.."

cluster=ek8s
ctx="kind-$cluster"
# An id no real variant gets, so a workstation run cannot touch one.
id=999999
ns="ek8s-v$id"
sys=ek8s-system
# The curl project's official image, 8.22.0 (published 2026-09-02),
# pulled by its linux/amd64 manifest digest (index sha256:58adaa4e...),
# so only one platform is ever pulled and kind-loaded; tagged by the
# digest's first 12 hex like stress-ng (deploy/up.sh). Runs as 65532 here,
# not its curl_user.
curl_src=curlimages/curl@sha256:43366cd60f226c7655181a0f7e85c468a41d182fdd2dc2c1c3b872a2b9d05d7a
curl_tag=ek8s-curl:43366cd60f22
# Kubernetes' generated-name alphabet (no vowels, no 0/1/3).
alnum=bcdfghjklmnpqrstvwxz2456789

k() { kubectl --context "$ctx" "$@"; }
die() { echo "variant-smoke.sh: FAIL: $*" >&2; exit 1; }
ok() { echo "variant-smoke.sh: ok: $*"; }

for tool in go docker kind kubectl; do
	command -v "$tool" >/dev/null || die "$tool not on PATH"
done
k get nodes >/dev/null || die "cluster unreachable (context $ctx); run deploy/up.sh"
! k get namespace "$ns" >/dev/null 2>&1 ||
	die "namespace $ns exists (a failed earlier run?); delete it first"

# Generation 0's image, as gate Stage 1 builds it: a static binary in a
# FROM scratch image tagged by the binary's sha256 (gate/docker.go).
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
CGO_ENABLED=0 go build -trimpath -o "$work/service" ./cmd/service
cat >"$work/Dockerfile" <<'EOF'
FROM scratch
COPY service /service
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/service"]
EOF
image="ek8s-wrapper:$(sha256sum "$work/service" | cut -c1-12)"
docker build -q -t "$image" "$work" >/dev/null
kind load docker-image "$image" --name "$cluster"
docker pull -q "$curl_src" >/dev/null
docker tag "$curl_src" "$curl_tag"
kind load docker-image "$curl_tag" --name "$cluster"
ok "built and loaded $image and $curl_tag"

label="ek8s-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
go run ./deploy/variant/cmd/render -id "$id" -label "$label" -image "$image" | k apply -f -

# loadgen's namespace; loadgen's own PR will create it for real.
created_sys=false
if ! k get namespace "$sys" >/dev/null 2>&1; then
	k create namespace "$sys"
	k label namespace "$sys" pod-security.kubernetes.io/enforce=restricted >/dev/null
	created_sys=true
fi

# curl_pod NS NAME [LABEL]: a PSA-restricted pod that idles, for kubectl
# exec. No resources: in the variant namespace the LimitRange sets them.
curl_pod() {
	k apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $2
  namespace: $1
  labels: {${3:-}}
spec:
  automountServiceAccountToken: false
  enableServiceLinks: false
  terminationGracePeriodSeconds: 1
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: curl
    image: $curl_tag
    imagePullPolicy: Never
    command: [sleep, "3600"]
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: {drop: [ALL]}
EOF
}
curl_pod "$sys" smoke-loadgen "app: loadgen"
curl_pod "$ns" smoke-egress "app: egress-probe"
for p in "$sys/smoke-loadgen" "$ns/smoke-egress"; do
	k -n "${p%/*}" wait --for=condition=Ready "pod/${p#*/}" --timeout=120s >/dev/null ||
		die "curl pod $p not Ready"
done

# --- (a) Deployment Ready, one pod, its resources and env.
k -n "$ns" rollout status deploy/hasher --timeout=180s || die "(a) Deployment hasher not Ready"
pods=$(k -n "$ns" get pods -l app=hasher -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
[ "$(grep -c . <<<"$pods")" -eq 1 ] || die "(a) want one hasher pod, have: $pods"
pod=$pods
# The ReplicaSet hash is a uint32's decimal digits through
# SafeEncodeString, which maps 0-9 to 456789bcdf: 10 characters for most
# templates, fewer for some (gate/boot.go newPodName draws the same).
[[ $pod =~ ^hasher-[456789bcdf]{1,10}-[$alnum]{5}$ ]] || die "(a) pod name $pod is not hasher-<hash>-<5>"
res=$(k -n "$ns" get pod "$pod" -o jsonpath='{.spec.containers[0].resources.requests.cpu} {.spec.containers[0].resources.requests.memory} {.spec.containers[0].resources.limits.cpu} {.spec.containers[0].resources.limits.memory}')
[ "$res" = "250m 256Mi 1 256Mi" ] || die "(a) pod resources '$res', want '250m 256Mi 1 256Mi' from the LimitRange"
# cmd/service logs the environment it parsed at start.
logs=$(k -n "$ns" logs "$pod") || die "(a) cannot read $pod's log"
start=$(grep -m1 'msg=starting' <<<"$logs") || die "(a) no start line in $pod's log"
for want in "variant=$label" cpu_limit_millis=1000 mem_limit_bytes=268435456; do
	[[ " $start " == *" $want "* ]] || die "(a) start line lacks $want: $start"
done
ok "(a) Deployment Ready; pod $pod, resources $res, env $label / 1000m / 268435456 bytes"

svc="http://hasher.$ns.svc.cluster.local:8080"
# kexec NS POD ARGS...: curl in a curl pod; kubectl exec exits with
# curl's status.
kexec() { k -n "$1" exec "$2" -- curl "${@:3}"; }

# --- (b) loadgen reaches /healthz and /hash through the Service.
hdr=$(kexec "$sys" smoke-loadgen -sS --max-time 10 -o /dev/null -D - "$svc/healthz") ||
	die "(b) loadgen pod: /healthz request failed"
[[ $(head -n1 <<<"$hdr") == HTTP/1.1\ 200* ]] || die "(b) /healthz: $(head -n1 <<<"$hdr")"
tr -d '\r' <<<"$hdr" | grep -ix "x-ek8s-variant: $label" >/dev/null || die "(b) /healthz lacks X-EK8s-Variant: $label"
out=$(kexec "$sys" smoke-loadgen -sS --max-time 30 -H 'Content-Type: application/json' \
	-d '{"password":"variant-smoke"}' -w '\n%{http_code}' "$svc/hash") ||
	die "(b) loadgen pod: /hash request failed"
[ "$(tail -n1 <<<"$out")" = 200 ] || die "(b) /hash status $(tail -n1 <<<"$out"): $out"
grep -qF '"hash":"$argon2id$v=19$m=19456,t=2,p=1$' <<<"$out" || die "(b) /hash body: $out"
ok "(b) app=loadgen in $sys: /healthz 200 with the label, /hash 200"

# refused NS POD URL ARGS...: curl fails to connect (7) or times out (28).
# Any other status, an HTTP answer included, is not a refusal.
refused() {
	local rc=0
	kexec "$1" "$2" -sS --max-time 5 -o /dev/null "${@:4}" "$3" 2>/dev/null || rc=$?
	[ "$rc" -eq 7 ] || [ "$rc" -eq 28 ]
}

# --- (c) the same pod, its app=loadgen label removed, is refused. The
# policy engine follows label changes asynchronously: allow it 30s.
k -n "$sys" label pod smoke-loadgen app- >/dev/null
deadline=$((SECONDS + 30))
until refused "$sys" smoke-loadgen "$svc/healthz"; do
	[ "$SECONDS" -lt "$deadline" ] || die "(c) without app=loadgen the pod still reached $svc after 30s"
	sleep 2
done
ok "(c) the same pod without app=loadgen is refused"

# --- (d) no egress from the variant namespace. Targets by IP: DNS is
# denied too, and a failed lookup would prove nothing about the address.
external=http://1.1.1.1/
api_ip=$(k -n default get service kubernetes -o jsonpath='{.spec.clusterIP}')
api_ep=$(k -n default get endpointslices -l kubernetes.io/service-name=kubernetes \
	-o jsonpath='{.items[0].endpoints[0].addresses[0]}:{.items[0].ports[0].port}')
for target in "$external" "https://$api_ip:443/version" "https://$api_ep/version"; do
	kexec "$sys" smoke-loadgen -sS -k --max-time 10 -o /dev/null "$target" ||
		die "(d) control: $target unreachable from $sys, so a refusal would prove nothing"
	refused "$ns" smoke-egress "$target" -k || die "(d) egress from $ns reached $target"
	ok "(d) $ns -> $target denied (reachable from $sys)"
done

# --- (e) deleting the namespace removes everything in it.
k delete namespace "$ns" --wait=true --timeout=180s >/dev/null || die "(e) namespace $ns not deleted in 180s"
! k get namespace "$ns" >/dev/null 2>&1 || die "(e) namespace $ns still exists"
left=$(k get all,networkpolicies,limitranges,horizontalpodautoscalers -A --no-headers \
	--field-selector "metadata.namespace=$ns") || die "(e) cannot list objects left in $ns"
[ -z "$left" ] || die "(e) left in $ns: $left"
ok "(e) namespace $ns and its objects are gone"

k -n "$sys" delete pod smoke-loadgen --wait=true >/dev/null
if $created_sys; then
	k delete namespace "$sys" --wait=true >/dev/null
fi
echo "variant-smoke.sh: all checks passed"
