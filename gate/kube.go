package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Kubernetes names this file matches, checked against k8s.io/api v0.37.0
// and kubernetes/kubernetes v1.37.0 unless marked otherwise.
const (
	condReady            = "Ready"
	condPodScheduled     = "PodScheduled"
	condDisruptionTarget = "DisruptionTarget"
	condReplicaFailure   = "ReplicaFailure"

	phaseRunning = "Running"
	phaseFailed  = "Failed"

	reasonEvicted           = "Evicted" // kubelet node-pressure eviction
	reasonUnschedulable     = "Unschedulable"
	reasonErrImageNeverPull = "ErrImageNeverPull"
	reasonCrashLoopBackOff  = "CrashLoopBackOff"
	// OOMKilled, Error and Completed are set by the container runtime,
	// not the kubelet (unverified).
	reasonOOMKilled = "OOMKilled"
	reasonError     = "Error"
	reasonCompleted = "Completed"
	// ContainerStatusUnknown is the kubelet's, set on a container whose
	// node restarted or lost it (checked against kubernetes v1.37.0
	// pkg/kubelet/kubelet_pods.go).
	reasonStatusUnknown = "ContainerStatusUnknown"
)

// knownReasons are the only reasons a problem or fault prints verbatim;
// any other prints as "other", so no free API text reaches a verdict.
var knownReasons = map[string]bool{
	reasonUnschedulable: true, reasonErrImageNeverPull: true,
	reasonCrashLoopBackOff: true, reasonOOMKilled: true, reasonError: true,
	"ContainerCreating": true, "PodInitializing": true,
	"ErrImagePull": true, "ImagePullBackOff": true, reasonCompleted: true,
	// kubelet admission rejections (pkg/kubelet/lifecycle/predicate.go)
	"OutOfcpu": true, "OutOfmemory": true, "OutOfpods": true, "OutOfephemeral-storage": true,
}

var knownPhases = map[string]bool{
	"Pending": true, phaseRunning: true, "Succeeded": true, phaseFailed: true, "Unknown": true,
}

func reasonText(r string) string {
	if knownReasons[r] {
		return r
	}
	return "other"
}

func phaseText(p string) string {
	if knownPhases[p] {
		return p
	}
	return "other"
}

// The types below decode only what the checks read. No message field is
// decoded: terminated.message is the candidate's termination log, text
// the candidate controls. encoding/json ignores every other field.

type kubeMeta struct {
	Name              string  `json:"name"`
	DeletionTimestamp *string `json:"deletionTimestamp"`
}

type kubeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type waitingState struct {
	Reason string `json:"reason"`
}

type terminatedState struct {
	Reason string `json:"reason"`
}

type containerState struct {
	Waiting    *waitingState    `json:"waiting"`
	Running    *struct{}        `json:"running"`
	Terminated *terminatedState `json:"terminated"`
}

type containerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int32          `json:"restartCount"`
	State        containerState `json:"state"`
	LastState    containerState `json:"lastState"`
}

type pod struct {
	Metadata kubeMeta `json:"metadata"`
	Status   struct {
		Phase                 string            `json:"phase"`
		Reason                string            `json:"reason"`
		Conditions            []kubeCondition   `json:"conditions"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type deployment struct {
	Metadata kubeMeta `json:"metadata"`
	Status   struct {
		Conditions []kubeCondition `json:"conditions"`
	} `json:"status"`
}

type kubeList struct {
	Kind  string            `json:"kind"`
	Items []json.RawMessage `json:"items"`
}

// parseDeployPods parses `kubectl get deploy,pods -o json`: a List of
// exactly one Deployment and any number of Pods, the Pods returned sorted
// by name.
func parseDeployPods(b []byte) (deployment, []pod, error) {
	var list kubeList
	if err := json.Unmarshal(b, &list); err != nil {
		return deployment{}, nil, fmt.Errorf("kubectl output: %w", err)
	}
	if list.Kind != "List" {
		return deployment{}, nil, fmt.Errorf("kubectl output: kind %q, want List", list.Kind)
	}
	var deps []deployment
	var pods []pod
	for i, raw := range list.Items {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return deployment{}, nil, fmt.Errorf("kubectl output: item %d: %w", i, err)
		}
		var err error
		switch head.Kind {
		case "Deployment":
			var d deployment
			err = json.Unmarshal(raw, &d)
			deps = append(deps, d)
		case "Pod":
			var p pod
			err = json.Unmarshal(raw, &p)
			pods = append(pods, p)
		default:
			return deployment{}, nil, fmt.Errorf("kubectl output: item %d has kind %q", i, head.Kind)
		}
		if err != nil {
			return deployment{}, nil, fmt.Errorf("kubectl output: item %d: %w", i, err)
		}
	}
	if len(deps) != 1 {
		return deployment{}, nil, fmt.Errorf("kubectl output: %d Deployments, want 1", len(deps))
	}
	slices.SortFunc(pods, func(a, b pod) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	return deps[0], pods, nil
}

func condition(conds []kubeCondition, typ string) (kubeCondition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return kubeCondition{}, false
}

func conditionTrue(conds []kubeCondition, typ string) bool {
	c, ok := condition(conds, typ)
	return ok && c.Status == "True"
}

// ctr is one container's status with the label a verdict names it by.
type ctr struct {
	label string
	init  bool
	containerStatus
}

// containersOf lists p's init containers, then its containers, each in
// status order.
func containersOf(p pod) []ctr {
	var cs []ctr
	for _, c := range p.Status.InitContainerStatuses {
		cs = append(cs, ctr{fmt.Sprintf("pod %s init container %s", p.Metadata.Name, c.Name), true, c})
	}
	for _, c := range p.Status.ContainerStatuses {
		cs = append(cs, ctr{fmt.Sprintf("pod %s container %s", p.Metadata.Name, c.Name), false, c})
	}
	return cs
}

// podFault reports a pod that something other than the candidate is
// removing or has failed. A deletionTimestamp is a fault because the
// candidate has no API access to delete its own pod
// (automountServiceAccountToken: false in
// deploy/variant/40-deployment.yaml.tmpl). An autoscaler scale-down also
// sets deletionTimestamp, without DisruptionTarget, but the default 300 s
// scale-down stabilisation keeps it out of the window a caller judges
// (inferred). DisruptionTarget True marks eviction through the API,
// preemption and other deletion by others. Phase Failed is a fault too:
// Deployment pods are restartPolicy Always, so only the node puts one
// there, by eviction (named apart) or by kubelet admission rejection such
// as OutOfcpu.
func podFault(p pod) string {
	switch name := p.Metadata.Name; {
	case p.Metadata.DeletionTimestamp != nil:
		return fmt.Sprintf("pod %s: deletionTimestamp set (deleted by others)", name)
	case p.Status.Reason == reasonEvicted:
		return fmt.Sprintf("pod %s: Evicted", name)
	case p.Status.Phase == phaseFailed:
		return fmt.Sprintf("pod %s: phase Failed (%s)", name, reasonText(p.Status.Reason))
	case conditionTrue(p.Status.Conditions, condDisruptionTarget):
		return fmt.Sprintf("pod %s: DisruptionTarget True", name)
	}
	for _, c := range containersOf(p) {
		if c.State.Terminated != nil && c.State.Terminated.Reason == reasonStatusUnknown ||
			c.LastState.Terminated != nil && c.LastState.Terminated.Reason == reasonStatusUnknown {
			return fmt.Sprintf("%s: ContainerStatusUnknown (the node lost it)", c.label)
		}
	}
	return ""
}

// readyFault is readyCheck's fault checks: the Deployment's, then each
// pod's, pods by name.
func readyFault(d deployment, pods []pod) string {
	// The ReplicaSet's pod creation was refused (admission); the
	// Deployment controller copies its ReplicaFailure condition.
	if conditionTrue(d.Status.Conditions, condReplicaFailure) {
		return "deployment: ReplicaFailure True (pod creation refused)"
	}
	for _, p := range pods {
		if f := podFault(p); f != "" {
			return f
		}
		for _, c := range containersOf(p) {
			if w := c.State.Waiting; w != nil && w.Reason == reasonErrImageNeverPull {
				return c.label + ": waiting ErrImageNeverPull (image not loaded into the node)"
			}
		}
	}
	return ""
}

func failedTermination(t *terminatedState) bool {
	return t != nil && (t.Reason == reasonOOMKilled || t.Reason == reasonError)
}

// containerFailure reports a container that restarted, is backing off
// after crashing, or has terminated: a non-init container for any reason,
// Completed included (a Deployment's containers never end normally), an
// init container only when OOMKilled or Error (Completed is its normal
// end).
func containerFailure(c ctr) string {
	var what string
	switch {
	case c.State.Terminated != nil && (!c.init || failedTermination(c.State.Terminated)):
		what = "terminated " + reasonText(c.State.Terminated.Reason) + ", "
	case failedTermination(c.LastState.Terminated):
		what = "last terminated " + c.LastState.Terminated.Reason + ", "
	case c.State.Waiting != nil && c.State.Waiting.Reason == reasonCrashLoopBackOff:
		what = "waiting " + reasonCrashLoopBackOff + ", "
	case c.RestartCount > 0:
	default:
		return ""
	}
	return fmt.Sprintf("%s: %srestarts %d", c.label, what, c.RestartCount)
}

// firstFailure is the first container failure, pods by name, then
// container order.
func firstFailure(pods []pod) string {
	for _, p := range pods {
		for _, c := range containersOf(p) {
			if f := containerFailure(c); f != "" {
				return f
			}
		}
	}
	return ""
}

// runningNotReady names the first pod that is Running with every
// container running but is not Ready.
func runningNotReady(pods []pod) string {
	for _, p := range pods {
		cs := p.Status.ContainerStatuses
		if p.Status.Phase != phaseRunning || len(cs) == 0 || conditionTrue(p.Status.Conditions, condReady) {
			continue
		}
		if !slices.ContainsFunc(cs, func(c containerStatus) bool { return c.State.Running == nil }) {
			return fmt.Sprintf("pod %s: Running, containers running, never Ready", p.Metadata.Name)
		}
	}
	return ""
}

// timeoutFault says why p is still not Ready at the timeout, naming the
// reason when one is known.
func timeoutFault(p pod) string {
	if c, ok := condition(p.Status.Conditions, condPodScheduled); ok && c.Status == "False" {
		return fmt.Sprintf("pod %s: not scheduled (%s) at timeout", p.Metadata.Name, reasonText(c.Reason))
	}
	for _, c := range containersOf(p) {
		if c.State.Waiting != nil {
			return fmt.Sprintf("%s: waiting %s at timeout", c.label, reasonText(c.State.Waiting.Reason))
		}
	}
	return fmt.Sprintf("pod %s: phase %s, not Ready at timeout", p.Metadata.Name, phaseText(p.Status.Phase))
}

// readyCheck judges a Deployment and its pods (sorted by name) while
// waiting for the candidate's first pod to be Ready; atTimeout says the
// wait is over. It returns ready, a problem (the candidate failed) or a
// fault (the cluster or a node failed: nothing is known about the
// candidate), or none of them while waiting is still worthwhile. Every
// fault check comes before any failure check, so a fault suppresses a
// problem.
func readyCheck(d deployment, pods []pod, atTimeout bool) (ready bool, problem string, fault error) {
	if f := readyFault(d, pods); f != "" {
		return false, "", errors.New(f)
	}
	if pr := firstFailure(pods); pr != "" {
		return false, pr, nil
	}
	if slices.ContainsFunc(pods, func(p pod) bool { return conditionTrue(p.Status.Conditions, condReady) }) {
		return true, "", nil
	}
	if !atTimeout {
		return false, "", nil
	}
	if pr := runningNotReady(pods); pr != "" {
		return false, pr, nil
	}
	if len(pods) == 0 {
		return false, "", errors.New("no pod at timeout")
	}
	// Unschedulable is a fault only here, at the timeout: before it, a
	// previous candidate's pods may still be releasing resources.
	return false, "", errors.New(timeoutFault(pods[0]))
}

// podHealth judges every pod it is given (sorted by name) after load,
// a pod the autoscaler added included. Pending and not-Ready pods are not
// judged: only a pod removed by others or failed by its node (a fault) or
// a container failure, an exit included (a problem). A fault suppresses a
// problem.
func podHealth(pods []pod) (problem string, fault error) {
	for _, p := range pods {
		if f := podFault(p); f != "" {
			return "", errors.New(f)
		}
	}
	return firstFailure(pods), nil
}

var (
	// readyPoll is the pause between polls of the Ready wait.
	readyPoll = 1 * time.Second
	// readyTimeout is how long a caller waits for the first pod to be
	// Ready (inferred; to be checked against real pod start times on the
	// workstation).
	readyTimeout = 120 * time.Second
	// readyGrace bounds each kubectl call of the Ready wait past its
	// timeout, so an API server that stops answering cannot hold the wait
	// for more than readyGrace plus toolWaitDelay (inferred; kubectl get
	// against a local kind API server answers in well under a second).
	readyGrace = 30 * time.Second
	// kubectlRequestTimeout bounds each API request of a kubectl call, so
	// a kubectl orphaned by a SIGKILLed gate still ends: kubectl's default
	// is no timeout (checked against v1.37.0 config_flags.go).
	kubectlRequestTimeout = 30 * time.Second
)

// kubectl calls kubectl against the kube context kubeCtx. --request-timeout
// and --context go last so runTool's error label stays "kubectl
// <subcommand>"; for that reason args must not contain "--", after which
// kubectl would pass both flags to a remote command, so it is an error and
// nothing is started.
func kubectl(ctx context.Context, kubeCtx string, stdin io.Reader, args ...string) (string, error) {
	if slices.Contains(args, "--") {
		return "", errors.New("kubectl: args contain \"--\"")
	}
	args = append(slices.Clone(args), "--request-timeout", kubectlRequestTimeout.String(), "--context", kubeCtx)
	return runTool(ctx, stdin, "kubectl", args...)
}

// kindLoad loads a locally built image into the kind cluster. The form
// `kind load docker-image <image> --name <cluster>` is checked against
// kind v0.33.0 (pkg/cmd/kind/load/docker-image).
func kindLoad(ctx context.Context, cluster, image string) error {
	_, err := runTool(ctx, nil, "kind", "load", "docker-image", image, "--name", cluster)
	return err
}

// getDeployPods reads the pods and Deployment matching selector in ns. An
// empty selector is an error, since it would match every pod of the
// namespace; nothing is started.
func getDeployPods(ctx context.Context, kubeCtx, ns, selector string) (deployment, []pod, error) {
	if selector == "" {
		return deployment{}, nil, errors.New("get deploy,pods: empty selector")
	}
	out, err := kubectl(ctx, kubeCtx, nil, "get", "deploy,pods", "-n", ns, "-l", selector, "-o", "json")
	if err != nil {
		return deployment{}, nil, err
	}
	return parseDeployPods([]byte(out))
}

// waitReady polls until the first of the pods matching selector in ns is
// Ready. A problem is a candidate failure, a fault is a gate fault (any
// kubectl, parse or context error included); the timeout rules live in
// readyCheck. The last poll is made at or after the deadline with
// atTimeout true, so a verdict always comes. Each kubectl call is cut off
// readyGrace after the deadline, so the wait ends by timeout + readyGrace +
// toolWaitDelay at the latest.
func waitReady(ctx context.Context, kubeCtx, ns, selector string, timeout time.Duration) (problem string, fault error) {
	deadline := time.Now().Add(timeout)
	wctx, cancel := context.WithDeadline(ctx, deadline.Add(readyGrace))
	defer cancel()
	for {
		d, pods, err := getDeployPods(wctx, kubeCtx, ns, selector)
		if err != nil {
			if ctx.Err() == nil && wctx.Err() != nil {
				return "", fmt.Errorf("wait ready: kubectl did not answer within the timeout plus readyGrace: %w", wctx.Err())
			}
			return "", fmt.Errorf("wait ready: %w", err)
		}
		now := time.Now()
		ready, problem, fault := readyCheck(d, pods, !now.Before(deadline))
		if ready {
			return "", nil
		}
		if problem != "" || fault != nil {
			return problem, fault
		}
		timer := time.NewTimer(min(readyPoll, deadline.Sub(now)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("wait ready: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// podHealthCheck judges every pod matching selector in ns with podHealth.
func podHealthCheck(ctx context.Context, kubeCtx, ns, selector string) (problem string, fault error) {
	_, pods, err := getDeployPods(ctx, kubeCtx, ns, selector)
	if err != nil {
		return "", fmt.Errorf("pod health: %w", err)
	}
	return podHealth(pods)
}

// podsRunning returns nil only if the pods matching selector in ns are
// exactly want, each phase Running and not being deleted. Every error is
// a fault for the caller. An empty selector is an error; nothing is
// started.
func podsRunning(ctx context.Context, kubeCtx, ns, selector string, want int) error {
	if selector == "" {
		return errors.New("get pods: empty selector")
	}
	out, err := kubectl(ctx, kubeCtx, nil, "get", "pods", "-n", ns, "-l", selector, "-o", "json")
	if err != nil {
		return err
	}
	var l kubeList
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return fmt.Errorf("kubectl output: %w", err)
	}
	if l.Kind != "List" {
		return fmt.Errorf("kubectl output: kind %q, want List", l.Kind)
	}
	pods := make([]pod, 0, len(l.Items))
	for _, raw := range l.Items {
		var k struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &k); err != nil {
			return fmt.Errorf("kubectl output: %w", err)
		}
		if k.Kind != "Pod" {
			return fmt.Errorf("kubectl output: unexpected item kind %q", k.Kind)
		}
		var p pod
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("kubectl output: %w", err)
		}
		pods = append(pods, p)
	}
	if len(pods) != want {
		return fmt.Errorf("%d pods match %q, want %d", len(pods), selector, want)
	}
	slices.SortFunc(pods, func(a, b pod) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	for _, p := range pods {
		if p.Metadata.DeletionTimestamp != nil {
			return fmt.Errorf("pod %s: deletionTimestamp set", p.Metadata.Name)
		}
		if p.Status.Phase != phaseRunning {
			return fmt.Errorf("pod %s: phase %s, want Running", p.Metadata.Name, phaseText(p.Status.Phase))
		}
	}
	return nil
}

// nodeFault reads the output of kubectl get nodes -o json and returns an
// error unless there is at least one node and every node's Ready
// condition is True. A NotReady node says nothing about a candidate, so
// Stage 5 asks before it scores a Running-but-never-Ready or restarted
// pod against one (docs/decisions.md, the PR #48 pod-state entry).
func nodeFault(b []byte) error {
	var l struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata kubeMeta `json:"metadata"`
			Status   struct {
				Conditions []kubeCondition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return fmt.Errorf("kubectl output: %w", err)
	}
	if l.Kind != "List" {
		return fmt.Errorf("kubectl output: kind %q, want List", l.Kind)
	}
	if len(l.Items) == 0 {
		return errors.New("no nodes")
	}
	for _, n := range l.Items {
		if !conditionTrue(n.Status.Conditions, condReady) {
			return fmt.Errorf("node %s: not Ready", n.Metadata.Name)
		}
	}
	return nil
}

// nodesReady is nil only if every node is Ready (nodeFault).
func nodesReady(ctx context.Context, kubeCtx string) error {
	out, err := kubectl(ctx, kubeCtx, nil, "get", "nodes", "-o", "json")
	if err != nil {
		return fmt.Errorf("get nodes: %w", err)
	}
	return nodeFault([]byte(out))
}
