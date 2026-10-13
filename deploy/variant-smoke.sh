#!/usr/bin/env bash
# Variant bundle and ek8s-system smoke test, after deploy/up.sh: builds
# generation 0's image, kind-loads it, renders and applies one variant
# bundle (deploy/variant), starts a stand-in for loadgen in ek8s-system
# (app=loadgen, a busybox nc answering HTTP on the control port) and checks
#   (a) the Deployment is Ready with one pod named like a Deployment pod,
#       carrying the LimitRange's resources, and its env (as cmd/service
#       logs it) carries the limits and the label;
#   (b) the stand-in gets /healthz and a /hash 200 from the variant's
#       Service by DNS (its egress policy admits DNS to kube-dns and TCP
#       8080 to ek8s.io/role=variant namespaces);
#   (c) the API server's service proxy reaches the stand-in's control
#       port (kindnet's root-socket bypass; deploy/ek8s-system/
#       networkpolicy.yaml);
#   (d) a pod in the variant namespace and a pod in ek8s-system without
#       app=loadgen are refused on the control port;
#   (e) an ek8s-system pod without app=loadgen is refused by the variant;
#   (f) the stand-in has no other egress: no external address (DNS to an
#       outside resolver included), no API server;
#   (g) a pod in the variant namespace has no egress: no external address,
#       no API server (each target in (f) and (g) first shown reachable
#       from the ek8s-system pod without app=loadgen, which no policy
#       selects);
#   (h) with the variant at 2 ready pods, deleting one marks its
#       EndpointSlice entry ready=false, terminating=true within 2s while
#       the other stays ready;
#   (i) deleting the namespace removes everything in it.
# Exits non-zero on the first failed check. Run from anywhere; needs go,
# docker, kind and kubectl. On failure it leaves the namespace and the
# smoke pods as they are for inspection; on success it removes them.
set -euo pipefail
cd "$(dirname "$0")/.."

cluster=ek8s
ctx="kind-$cluster"
# An id no real variant gets, so a workstation run cannot touch one.
id=999999
ns="ek8s-v$id"
sys=ek8s-system
# deploy/ek8s-system/service.yaml
ctl_port=9090
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
k get namespace "$sys" >/dev/null 2>&1 || die "namespace $sys missing; run deploy/up.sh"
[ -z "$(k -n "$sys" get pods smoke-loadgen smoke-other --ignore-not-found -o name)" ] ||
	die "smoke pods exist in $sys (a failed earlier run?); delete them first"
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

# pod_spec NAME NS LABELS: the head of a PSA-restricted pod with one
# container, for kubectl exec; the caller appends the container's command
# (and ports). No resources: in the variant namespace the LimitRange sets
# them.
pod_spec() {
	cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: $2
  labels: {$3}
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
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: {drop: [ALL]}
EOF
}
# curl_pod NS NAME LABELS: a pod that idles.
curl_pod() {
	{ pod_spec "$2" "$1" "$3"; echo '    command: [sleep, "3600"]'; } | k apply -f -
}
# The stand-in for loadgen: busybox nc on the control port, forking a
# shell per connection that reads the request head and answers 200
# "ek8s-ctl". Reading the head first matters: closing with request bytes
# unread would reset the connection.
{
	pod_spec smoke-loadgen "$sys" "app: loadgen"
	# 9090 is $ctl_port; the heredoc is quoted for the shell script.
	cat <<'EOF'
    ports:
    - name: control
      containerPort: 9090
      protocol: TCP
    command:
    - nc
    - -lk
    - -p
    - "9090"
    - -e
    - sh
    - -c
    - |
      while IFS= read -r l; do case $l in ""|"$(printf '\r')") break;; esac; done
      printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: close\r\n\r\nek8s-ctl\n'
EOF
} | k apply -f -
# Same image and pod securityContext, idling, without app=loadgen: no
# policy selects it.
curl_pod "$sys" smoke-other "app: smoke-other"
curl_pod "$ns" smoke-egress "app: egress-probe"
for p in "$sys/smoke-loadgen" "$sys/smoke-other" "$ns/smoke-egress"; do
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
# kx NS POD CMD ARGS...: run CMD in a smoke pod; kubectl exec exits with
# its status. kexec runs curl.
kx() { k -n "$1" exec "$2" -- "${@:3}"; }
kexec() { kx "$1" "$2" curl "${@:3}"; }

# --- (b) the stand-in reaches /healthz and /hash through the Service, by
# DNS name.
hdr=$(kexec "$sys" smoke-loadgen -sS --max-time 10 -o /dev/null -D - "$svc/healthz") ||
	die "(b) stand-in: /healthz request failed"
[[ $(head -n1 <<<"$hdr") == HTTP/1.1\ 200* ]] || die "(b) /healthz: $(head -n1 <<<"$hdr")"
tr -d '\r' <<<"$hdr" | grep -ix "x-ek8s-variant: $label" >/dev/null || die "(b) /healthz lacks X-EK8s-Variant: $label"
out=$(kexec "$sys" smoke-loadgen -sS --max-time 30 -H 'Content-Type: application/json' \
	-d '{"password":"variant-smoke"}' -w '\n%{http_code}' "$svc/hash") ||
	die "(b) stand-in: /hash request failed"
[ "$(tail -n1 <<<"$out")" = 200 ] || die "(b) /hash status $(tail -n1 <<<"$out"): $out"
grep -qF '"hash":"$argon2id$v=19$m=19456,t=2,p=1$' <<<"$out" || die "(b) /hash body: $out"
ok "(b) app=loadgen in $sys: $svc by DNS, /healthz 200 with the label, /hash 200"

# --- (c) the API server's service proxy reaches the control port. The
# EndpointSlice follows the pod's readiness asynchronously: allow it 30s.
proxy=/api/v1/namespaces/$sys/services/loadgen:control/proxy/
via_proxy() { [ "$(k get --raw "$proxy" 2>/dev/null)" = ek8s-ctl ]; }
deadline=$((SECONDS + 30))
until via_proxy; do
	[ "$SECONDS" -lt "$deadline" ] || die "(c) $proxy did not answer ek8s-ctl in 30s: $(k get --raw "$proxy" 2>&1)"
	sleep 2
done
ok "(c) $proxy answers through the API server"

# refused NS POD URL ARGS...: curl fails to connect (7) or times out (28).
# Any other status, an HTTP answer included, is not a refusal.
refused() {
	local rc=0
	kexec "$1" "$2" -sS --max-time 5 -o /dev/null "${@:4}" "$3" 2>/dev/null || rc=$?
	[ "$rc" -eq 7 ] || [ "$rc" -eq 28 ]
}

# --- (d) no pod reaches the control port. By ClusterIP: the variant pod
# has no DNS, and a failed lookup would prove nothing. Its own egress
# policy already denies this; the ek8s-system pod is the one only
# loadgen's ingress policy stops. (c) again afterwards shows the stand-in
# was answering throughout.
ctl_ip=$(k -n "$sys" get service loadgen -o jsonpath='{.spec.clusterIP}')
# From the Service, so a port that no longer matches it cannot time out
# as a false refusal.
svc_port=$(k -n "$sys" get service loadgen -o jsonpath='{.spec.ports[?(@.name=="control")].port}')
[ "$svc_port" = "$ctl_port" ] || die "(d) Service loadgen's control port is '$svc_port', the stand-in listens on $ctl_port"
for p in "$ns/smoke-egress" "$sys/smoke-other"; do
	refused "${p%/*}" "${p#*/}" "http://$ctl_ip:$ctl_port/" || die "(d) $p reached the control port"
done
via_proxy || die "(d) the proxy no longer reaches the stand-in, so the refusals prove nothing"
ok "(d) $ns/smoke-egress and $sys/smoke-other refused on $ctl_ip:$ctl_port; the proxy still answers"

# --- (e) a pod in ek8s-system without app=loadgen is refused by the
# variant, which admitted the stand-in in (b). By DNS name: no policy
# limits this pod's egress, so the lookup works and only the variant's
# ingress policy can refuse it.
refused "$sys" smoke-other "$svc/healthz" || die "(e) $sys/smoke-other reached $svc"
ok "(e) $sys/smoke-other (no app=loadgen) refused by $svc"

# --- (f) and (g): targets by IP; each is first shown reachable from
# smoke-other, which no policy selects.
external=http://1.1.1.1/
api_ip=$(k -n default get service kubernetes -o jsonpath='{.spec.clusterIP}')
api_ep=$(k -n default get endpointslices -l kubernetes.io/service-name=kubernetes \
	-o jsonpath='{.items[0].endpoints[0].addresses[0]}:{.items[0].ports[0].port}')
targets=("$external" "https://$api_ip:443/version" "https://$api_ep/version")
for target in "${targets[@]}"; do
	kexec "$sys" smoke-other -sS -k --max-time 10 -o /dev/null "$target" ||
		die "control: $target unreachable from $sys/smoke-other, so a refusal would prove nothing"
done

# --- (f) the stand-in's only egress is variants' 8080 and kube-dns.
for target in "${targets[@]}"; do
	refused "$sys" smoke-loadgen "$target" -k || die "(f) the stand-in reached $target"
	ok "(f) $sys/smoke-loadgen -> $target denied (reachable from $sys/smoke-other)"
done
# DNS to a resolver other than kube-dns: busybox nslookup, UDP 53.
lookup=(timeout 10 nslookup example.com 1.1.1.1)
kx "$sys" smoke-other "${lookup[@]}" >/dev/null 2>&1 ||
	die "control: $sys/smoke-other cannot resolve via 1.1.1.1, so a refusal would prove nothing"
! kx "$sys" smoke-loadgen "${lookup[@]}" >/dev/null 2>&1 || die "(f) the stand-in resolved via 1.1.1.1"
ok "(f) $sys/smoke-loadgen -> DNS at 1.1.1.1 denied (works from $sys/smoke-other)"

# --- (g) no egress from the variant namespace.
for target in "${targets[@]}"; do
	refused "$ns" smoke-egress "$target" -k || die "(g) egress from $ns reached $target"
	ok "(g) $ns -> $target denied (reachable from $sys/smoke-other)"
done
# Frees its 250m (LimitRange) for the second hasher pod in (h).
k -n "$ns" delete pod smoke-egress --wait=true >/dev/null

# --- (h) a deleted pod leaves the Service's endpoints at once, not on the
# readiness probe (3 x 2s, past cmd/service's 5s drain; DESIGN.md sec 11).
# HPA minReplicas 2 here only.
k -n "$ns" patch hpa hasher --type=merge -p '{"spec":{"minReplicas":2}}' >/dev/null
# eps: one line per endpoint, "<pod> <ready> <terminating>" (an absent
# condition prints empty).
eps() {
	k -n "$ns" get endpointslices -l kubernetes.io/service-name=hasher \
		-o jsonpath='{range .items[*].endpoints[*]}{.targetRef.name} {.conditions.ready} {.conditions.terminating}{"\n"}{end}'
}
deadline=$((SECONDS + 180))
until [ "$(eps | awk '$2 == "true"' | grep -c .)" -eq 2 ]; do
	[ "$SECONDS" -lt "$deadline" ] || die "(h) no 2 ready endpoints in 180s: $(eps)"
	sleep 2
done
read -r victim keep < <(eps | awk '$2 == "true" { n[++i] = $1 } END { print n[1], n[2] }')
t0=$(date +%s%N)
k -n "$ns" delete pod "$victim" --wait=false >/dev/null
while :; do
	e=$(eps)
	ms=$((($(date +%s%N) - t0) / 1000000))
	if awk -v v="$victim" -v k="$keep" '
		$1 == v && $2 == "false" && $3 == "true" { gone = 1 }
		$1 == k && $2 == "true" { kept = 1 }
		END { exit !(gone && kept) }' <<<"$e"; then
		break
	fi
	[ "$ms" -le 2000 ] || die "(h) ${ms}ms after deleting $victim the endpoints are: $e"
done
# The snapshot that passed must itself be within 2s.
[ "$ms" -le 2000 ] || die "(h) $victim left the endpoints only after ${ms}ms: $e"
ok "(h) ${ms}ms after deleting $victim: ready=false terminating=true; $keep still ready"

# --- (i) deleting the namespace removes everything in it.
k delete namespace "$ns" --wait=true --timeout=180s >/dev/null || die "(i) namespace $ns not deleted in 180s"
! k get namespace "$ns" >/dev/null 2>&1 || die "(i) namespace $ns still exists"
left=$(k get all,networkpolicies,limitranges,horizontalpodautoscalers -A --no-headers \
	--field-selector "metadata.namespace=$ns") || die "(i) cannot list objects left in $ns"
[ -z "$left" ] || die "(i) left in $ns: $left"
ok "(i) namespace $ns and its objects are gone"

k -n "$sys" delete pod smoke-loadgen smoke-other --wait=true >/dev/null
echo "variant-smoke.sh: all checks passed"
