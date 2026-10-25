package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// podHealth judges every pod of a Deployment (sorted by name) after load,
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
