package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/lineage"
)

// Cluster is what a window needs of the kind cluster besides loadgen:
// the stress-ng switch, the CNI first look, the kubelet's CPU
// counters and namespace deletion. Every error is a fault of the
// cluster, never a variant's. Kube is the implementation; tests fake it.
type Cluster interface {
	// StressOff scales stress-ng to 0 and returns once no stress-ng pod
	// is left, terminating ones included: a window starts only once they
	// are gone (DESIGN.md sec 11).
	StressOff(ctx context.Context) error
	// StressOn scales stress-ng to 1 and returns once exactly one
	// stress-ng pod is Running and not terminating: gate Stage 5 runs
	// against stress (DESIGN.md sec 7).
	StressOn(ctx context.Context) error
	// CNIReady reports whether the CNI's DaemonSet (Calico's
	// calico-node) is Ready, a cheap first look before a half; the
	// policy canary decides (DESIGN.md sec 11).
	CNIReady(ctx context.Context) (bool, error)
	// PodCPU reads the kubelet's cumulative pod CPU counters
	// (/metrics/resource through the API server, DESIGN.md sec 9).
	PodCPU(ctx context.Context) (fitness.Sample, error)
	// DeleteNamespace deletes namespace ns and waits until it is gone;
	// a namespace already gone is no error.
	DeleteNamespace(ctx context.Context, ns string) error
}

// Names Kube reads from deploy/stress and deploy/calico.
const (
	stressNS         = "ek8s-stress"
	stressDeployment = "stress-ng"
	stressSelector   = "app=stress-ng"
	cniNS            = "kube-system"
	cniDaemonSet     = "calico-node" // the CNI's DaemonSet (checked against Calico v3.33.0's manifest)
	cniContainer     = "calico-node" // its container whose log D3b archives (the same manifest)
)

// Kube timings: kubectlRequestTimeout bounds each API request of a
// kubectl call (kubectl's default is none; gate/kube.go has the same);
// stressBound and namespaceBound bound how long stress-ng may take to
// stop or start and a namespace to go (deploy/smoke.sh waits 60 s for
// stop and 120 s for start; inferred); kubePollEvery is the wait
// between polls.
const (
	kubectlRequestTimeout = 30 * time.Second
	stressBound           = 3 * time.Minute
	namespaceBound        = 3 * time.Minute
	kubePollEvery         = time.Second
)

// Kube is Cluster through kubectl against one kube context. It is not
// safe for concurrent use.
type Kube struct {
	// Context is the kubectl context (kind-ek8s); it must not be empty,
	// since kubectl would fall back to the kubeconfig's current context.
	Context string

	// run runs kubectl with args and returns its stdout as written; nil
	// means runKubectl. Tests replace it.
	run func(ctx context.Context, args ...string) (string, error)

	node string // the one node's name, read once
}

// kubectl runs kubectl with args against k.Context and returns its
// stdout with surrounding white space trimmed.
func (k *Kube) kubectl(ctx context.Context, args ...string) (string, error) {
	out, err := k.kubectlRaw(ctx, args...)
	return strings.TrimSpace(out), err
}

// kubectlRaw is kubectl without the trim: stdout byte for byte, for a
// log whose length and last byte matter (CNILog).
func (k *Kube) kubectlRaw(ctx context.Context, args ...string) (string, error) {
	if k.Context == "" {
		return "", errors.New("orchestrator: kubectl: empty kube context")
	}
	if slices.Contains(args, "--") {
		return "", errors.New("orchestrator: kubectl: args contain \"--\"")
	}
	args = append(slices.Clone(args), "--request-timeout", kubectlRequestTimeout.String(), "--context", k.Context)
	if k.run != nil {
		return k.run(ctx, args...)
	}
	return runKubectl(ctx, args...)
}

// kubectlWaitDelay bounds Wait once kubectl's context ends or it exits,
// as gate's toolWaitDelay does.
const kubectlWaitDelay = 5 * time.Second

// runKubectl runs kubectl with args and returns its stdout as written; a
// failed run's error carries the trimmed stderr, unless ctx ended.
func runKubectl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.WaitDelay = kubectlWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		label := "kubectl"
		if len(args) > 0 {
			label += " " + args[0]
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s: %w", label, ctx.Err())
		}
		return "", fmt.Errorf("%s: %w: %s", label, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (k *Kube) StressOff(ctx context.Context) error {
	return k.stress(ctx, 0)
}

func (k *Kube) StressOn(ctx context.Context) error {
	return k.stress(ctx, 1)
}

// stress scales stress-ng to replicas and polls its pods until
// stressSettled, at most stressBound.
func (k *Kube) stress(ctx context.Context, replicas int) error {
	if _, err := k.kubectl(ctx, "-n", stressNS, "scale", "deploy/"+stressDeployment, fmt.Sprintf("--replicas=%d", replicas)); err != nil {
		return fmt.Errorf("orchestrator: stress-ng to %d: %w", replicas, err)
	}
	return poll(ctx, stressBound, func(ctx context.Context) (bool, error) {
		out, err := k.kubectl(ctx, "get", "pods", "-n", stressNS, "-l", stressSelector, "-o", "json")
		if err != nil {
			return false, err
		}
		return stressSettled([]byte(out), replicas)
	}, fmt.Sprintf("stress-ng at %d replicas", replicas))
}

// podList is the part of kubectl get pods -o json that Kube reads.
type podList struct {
	Kind  string `json:"kind"`
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name              string  `json:"name"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

// stressSettled reads kubectl get pods -o json: with want 0, settled
// once no pod is listed; with want 1, once exactly one is, Running and
// not being deleted.
func stressSettled(b []byte, want int) (bool, error) {
	var l podList
	if err := json.Unmarshal(b, &l); err != nil {
		return false, fmt.Errorf("kubectl output: %w", err)
	}
	if l.Kind != "List" {
		return false, fmt.Errorf("kubectl output: kind %q, want List", l.Kind)
	}
	for _, p := range l.Items {
		if p.Kind != "Pod" {
			return false, fmt.Errorf("kubectl output: item kind %q", p.Kind)
		}
	}
	if want == 0 || len(l.Items) != want {
		return len(l.Items) == want, nil
	}
	p := l.Items[0]
	return p.Metadata.DeletionTimestamp == nil && p.Status.Phase == "Running", nil
}

func (k *Kube) CNIReady(ctx context.Context) (bool, error) {
	out, err := k.kubectl(ctx, "get", "daemonset", cniDaemonSet, "-n", cniNS, "-o", "json")
	if err != nil {
		return false, fmt.Errorf("orchestrator: cni: %w", err)
	}
	return daemonSetReady([]byte(out))
}

// daemonSetReady reads kubectl get daemonset -o json: Ready when its
// controller has seen the current spec and every scheduled pod is
// updated, ready and available, with at least one scheduled.
func daemonSetReady(b []byte) (bool, error) {
	var d struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Status struct {
			ObservedGeneration     int64 `json:"observedGeneration"`
			DesiredNumberScheduled int   `json:"desiredNumberScheduled"`
			UpdatedNumberScheduled int   `json:"updatedNumberScheduled"`
			NumberReady            int   `json:"numberReady"`
			NumberAvailable        int   `json:"numberAvailable"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return false, fmt.Errorf("orchestrator: cni: kubectl output: %w", err)
	}
	if d.Kind != "DaemonSet" {
		return false, fmt.Errorf("orchestrator: cni: kubectl output: kind %q, want DaemonSet", d.Kind)
	}
	s := d.Status
	n := s.DesiredNumberScheduled
	return n > 0 && s.ObservedGeneration >= d.Metadata.Generation &&
		s.UpdatedNumberScheduled == n && s.NumberReady == n && s.NumberAvailable == n, nil
}

// CNILog reads the log of calico-node's container in its DaemonSet
// (the cluster's one node) with kubelet timestamps, from since floored
// to the second less one second, asking for at most 2 x
// lineage.MaxCNILog bytes: a read past the cap is how
// lineage.Run.WriteCNILog knows to mark a cut, and the API's limitBytes
// may return slightly more or slightly less than asked, so the margin is
// wide enough that a short read of a longer log still passes the cap.
// The bytes are kubectl's stdout untrimmed. A zero since is refused (it
// would ask for the whole log).
func (k *Kube) CNILog(ctx context.Context, since time.Time) ([]byte, error) {
	if since.IsZero() {
		return nil, errors.New("orchestrator: cni log: zero since")
	}
	out, err := k.kubectlRaw(ctx, "-n", cniNS, "logs", "ds/"+cniDaemonSet, "-c", cniContainer, "--timestamps",
		"--since-time="+cniLogSince(since), fmt.Sprintf("--limit-bytes=%d", 2*lineage.MaxCNILog))
	if err != nil {
		return nil, fmt.Errorf("orchestrator: cni log: %w", err)
	}
	return []byte(out), nil
}

// cniLogSince is since for kubectl logs --since-time: RFC 3339 UTC,
// floored to the second, less one second.
func cniLogSince(t time.Time) string {
	return t.UTC().Truncate(time.Second).Add(-time.Second).Format(time.RFC3339)
}

func (k *Kube) PodCPU(ctx context.Context) (fitness.Sample, error) {
	if k.node == "" {
		out, err := k.kubectl(ctx, "get", "nodes", "-o", "json")
		if err != nil {
			return nil, fmt.Errorf("orchestrator: pod cpu: %w", err)
		}
		if k.node, err = oneNode([]byte(out)); err != nil {
			return nil, fmt.Errorf("orchestrator: pod cpu: %w", err)
		}
	}
	out, err := k.kubectl(ctx, "get", "--raw", "/api/v1/nodes/"+k.node+"/proxy/metrics/resource")
	if err != nil {
		return nil, fmt.Errorf("orchestrator: pod cpu: %w", err)
	}
	return fitness.ParseResource(strings.NewReader(out))
}

// oneNode reads kubectl get nodes -o json and returns the name of its
// one node: the cluster has one node (DESIGN.md sec 11), so every
// variant's pods are on it.
func oneNode(b []byte) (string, error) {
	var l struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return "", fmt.Errorf("kubectl output: %w", err)
	}
	if l.Kind != "List" || len(l.Items) != 1 {
		return "", fmt.Errorf("kubectl output: %d nodes (kind %q), want one", len(l.Items), l.Kind)
	}
	name := l.Items[0].Metadata.Name
	if name == "" || strings.ContainsAny(name, "/?#%") {
		return "", fmt.Errorf("kubectl output: node name %q", name)
	}
	return name, nil
}

// VariantNamespaces lists the namespaces labelled as a variant's
// (DESIGN.md sec 11), sorted: a new run starts with none, since Stage 5
// creates ek8s-v<id> and refuses one that exists.
func (k *Kube) VariantNamespaces(ctx context.Context) ([]string, error) {
	out, err := k.kubectl(ctx, "get", "namespaces", "-l", "ek8s.io/role=variant", "-o", "name")
	if err != nil {
		return nil, err
	}
	names := strings.Fields(out)
	for i, n := range names {
		names[i] = strings.TrimPrefix(n, "namespace/")
	}
	slices.Sort(names)
	return names, nil
}

func (k *Kube) DeleteNamespace(ctx context.Context, ns string) error {
	if !strings.HasPrefix(ns, "ek8s-v") {
		return fmt.Errorf("orchestrator: refusing to delete namespace %q, not a variant's", ns)
	}
	_, err := k.kubectl(ctx, "delete", "namespace", ns, "--ignore-not-found", "--wait=true",
		"--timeout="+namespaceBound.String())
	if err != nil {
		return fmt.Errorf("orchestrator: deleting namespace %s: %w", ns, err)
	}
	return nil
}

// poll calls f until it reports done, every kubePollEvery, at most
// bound; what names the condition in the timeout error. f's error ends
// the poll.
func poll(ctx context.Context, bound time.Duration, f func(context.Context) (bool, error), what string) error {
	deadline := time.Now().Add(bound)
	for {
		done, err := f(ctx)
		if err != nil {
			return fmt.Errorf("orchestrator: waiting for %s: %w", what, err)
		}
		if done {
			return nil
		}
		if !time.Now().Add(kubePollEvery).Before(deadline) {
			return fmt.Errorf("orchestrator: %s not reached within %v", what, bound)
		}
		if err := sleep(ctx, kubePollEvery); err != nil {
			return fmt.Errorf("orchestrator: waiting for %s: %w", what, err)
		}
	}
}

// sleep waits d or until ctx ends, returning ctx's error then.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
