package gate

import (
	"strings"
	"testing"
)

const testDeploy = `{"kind":"Deployment","metadata":{"name":"svc"},"status":{"conditions":[{"type":"Available","status":"False","reason":"MinimumReplicasUnavailable"}]}}`

// Pods the cases share.
const (
	readyP1     = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","conditions":[{"type":"PodScheduled","status":"True"},{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"svc","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-10-25T00:00:00Z"}}}]}}`
	readyP2     = `{"kind":"Pod","metadata":{"name":"p2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"svc","ready":true,"restartCount":0,"state":{"running":{}}}]}}`
	notReadyP1  = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"name":"svc","ready":false,"restartCount":0,"state":{"running":{}}}]}}`
	notReadyP2  = `{"kind":"Pod","metadata":{"name":"p2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"name":"svc","ready":false,"restartCount":0,"state":{"running":{}}}]}}`
	unschedP1   = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable"}]}}`
	pendingP2   = `{"kind":"Pod","metadata":{"name":"p2"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable"}]}}`
	restartP1   = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":1,"state":{"running":{}}}]}}`
	restartP2   = `{"kind":"Pod","metadata":{"name":"p2"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":1,"state":{"running":{}}}]}}`
	oomLastP1   = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":1,"state":{"running":{}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}`
	oomLastP2   = `{"kind":"Pod","metadata":{"name":"p2"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":1,"state":{"running":{}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}`
	evictedP1   = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Failed","reason":"Evicted"}}`
	evictedP2   = `{"kind":"Pod","metadata":{"name":"p2"},"status":{"phase":"Failed","reason":"Evicted"}}`
	disruptedP1 = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","conditions":[{"type":"DisruptionTarget","status":"True","reason":"PreemptionByScheduler"}]}}`
	deletingP2  = `{"kind":"Pod","metadata":{"name":"p2","deletionTimestamp":"2026-10-25T00:00:00Z"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`
	completedP1 = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":0,"state":{"terminated":{"reason":"Completed","exitCode":0}}}]}}`
	completedP2 = `{"kind":"Pod","metadata":{"name":"p2"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":0,"state":{"terminated":{"reason":"Completed","exitCode":0}}}]}}`
	oomRejectP2 = `{"kind":"Pod","metadata":{"name":"p2"},"status":{"phase":"Failed","reason":"OutOfmemory"}}`
)

// kubeJSON wraps items in the List that kubectl get deploy,pods -o json
// prints.
func kubeJSON(items ...string) []byte {
	return []byte(`{"apiVersion":"v1","kind":"List","items":[` + strings.Join(items, ",") + `],"metadata":{"resourceVersion":""}}`)
}

type verdictCase struct {
	name      string
	deploy    string // "" means testDeploy
	pods      []string
	atTimeout bool
	ready     bool
	problem   string // exact
	fault     bool
	want      []string // substrings of the problem or fault text
}

func parseCase(t *testing.T, tc verdictCase) (deployment, []pod) {
	t.Helper()
	dep := tc.deploy
	if dep == "" {
		dep = testDeploy
	}
	d, pods, err := parseDeployPods(kubeJSON(append([]string{dep}, tc.pods...)...))
	if err != nil {
		t.Fatal(err)
	}
	return d, pods
}

func checkVerdict(t *testing.T, tc verdictCase, ready bool, problem string, fault error) {
	t.Helper()
	if ready != tc.ready || problem != tc.problem || (fault != nil) != tc.fault {
		t.Fatalf("got (%v, %q, %v), want (%v, %q, fault %v)", ready, problem, fault, tc.ready, tc.problem, tc.fault)
	}
	text := problem
	if fault != nil {
		text = fault.Error()
	}
	for _, s := range tc.want {
		if !strings.Contains(text, s) {
			t.Errorf("%q lacks %q", text, s)
		}
	}
}

func TestReadyCheck(t *testing.T) {
	cases := []verdictCase{
		{name: "ready pod", pods: []string{readyP1}, ready: true},
		{name: "pending before timeout", pods: []string{unschedP1}},
		{name: "pending Unschedulable at timeout", pods: []string{unschedP1}, atTimeout: true, fault: true, want: []string{"pod p1", "Unschedulable"}},
		{name: "ContainerCreating at timeout", atTimeout: true, fault: true, want: []string{"pod p1 container svc", "waiting ContainerCreating"},
			pods: []string{`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"}],"containerStatuses":[{"name":"svc","restartCount":0,"state":{"waiting":{"reason":"ContainerCreating"}}}]}}`}},
		{name: "no pods before timeout"},
		{name: "no pods at timeout", atTimeout: true, fault: true, want: []string{"no pod"}},
		{name: "Running not Ready before timeout", pods: []string{notReadyP1}},
		{name: "Running not Ready at timeout", pods: []string{notReadyP1}, atTimeout: true, problem: "pod p1: Running, containers running, never Ready"},
		{name: "ErrImageNeverPull before timeout", fault: true, want: []string{"pod p1 container svc", "ErrImageNeverPull"},
			pods: []string{`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Pending","containerStatuses":[{"name":"svc","restartCount":0,"state":{"waiting":{"reason":"ErrImageNeverPull"}}}]}}`}},
		{name: "ReplicaFailure True", fault: true, want: []string{"ReplicaFailure"},
			deploy: `{"kind":"Deployment","metadata":{"name":"svc"},"status":{"conditions":[{"type":"ReplicaFailure","status":"True","reason":"FailedCreate"}]}}`},
		{name: "Evicted", pods: []string{evictedP1}, fault: true, want: []string{"pod p1", "Evicted"}},
		{name: "DisruptionTarget True", pods: []string{disruptedP1}, fault: true, want: []string{"pod p1", "DisruptionTarget"}},
		{name: "deletionTimestamp", pods: []string{readyP1, deletingP2}, fault: true, want: []string{"pod p2", "deletionTimestamp"}},
		{name: "restartCount 1", pods: []string{restartP1}, problem: "pod p1 container svc: restarts 1"},
		{name: "lastState OOMKilled", pods: []string{oomLastP1}, problem: "pod p1 container svc: last terminated OOMKilled, restarts 1"},
		{name: "terminated Error", problem: "pod p1 container svc: terminated Error, restarts 0",
			pods: []string{`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":0,"state":{"terminated":{"reason":"Error","exitCode":1}}}]}}`}},
		{name: "CrashLoopBackOff", problem: "pod p1 container svc: waiting CrashLoopBackOff, restarts 2",
			pods: []string{`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":2,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`}},
		{name: "init container restart", problem: "pod p1 init container setup: restarts 1",
			pods: []string{`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Pending","initContainerStatuses":[{"name":"setup","restartCount":1,"state":{"running":{}}}],"containerStatuses":[{"name":"svc","restartCount":0,"state":{"waiting":{"reason":"PodInitializing"}}}]}}`}},
		{name: "main container terminated Completed before timeout", pods: []string{completedP1}, problem: "pod p1 container svc: terminated Completed, restarts 0"},
		{name: "init container Completed, main Ready", ready: true,
			pods: []string{`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"initContainerStatuses":[{"name":"setup","restartCount":0,"state":{"terminated":{"reason":"Completed","exitCode":0}}}],"containerStatuses":[{"name":"svc","ready":true,"restartCount":0,"state":{"running":{}}}]}}`}},
		{name: "phase Failed OutOfcpu", fault: true, want: []string{"pod p1", "phase Failed (OutOfcpu)"},
			pods: []string{`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Failed","reason":"OutOfcpu"}}`}},
		{name: "evicted pod plus a restarting pod", pods: []string{restartP1, evictedP2}, fault: true, want: []string{"pod p2", "Evicted"}},
		{name: "unknown waiting reason at timeout", atTimeout: true, fault: true, want: []string{"pod p1 container svc", "waiting other"},
			pods: []string{`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"}],"containerStatuses":[{"name":"svc","restartCount":0,"state":{"waiting":{"reason":"SomethingNew"}}}]}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, pods := parseCase(t, tc)
			ready, problem, fault := readyCheck(d, pods, tc.atTimeout)
			checkVerdict(t, tc, ready, problem, fault)
		})
	}
}

func TestPodHealth(t *testing.T) {
	cases := []verdictCase{
		{name: "one Ready pod", pods: []string{readyP1}},
		{name: "second pod Ready", pods: []string{readyP1, readyP2}},
		{name: "second pod Pending", pods: []string{readyP1, pendingP2}},
		{name: "second pod Running not Ready", pods: []string{readyP1, notReadyP2}},
		{name: "restart on the second pod", pods: []string{readyP1, restartP2}, problem: "pod p2 container svc: restarts 1"},
		{name: "OOMKilled lastState on the second pod", pods: []string{readyP1, oomLastP2}, problem: "pod p2 container svc: last terminated OOMKilled, restarts 1"},
		{name: "Evicted second pod", pods: []string{readyP1, evictedP2}, fault: true, want: []string{"pod p2", "Evicted"}},
		{name: "deletionTimestamp", pods: []string{readyP1, deletingP2}, fault: true, want: []string{"pod p2", "deletionTimestamp"}},
		{name: "fault plus failure", pods: []string{oomLastP1, evictedP2}, fault: true, want: []string{"pod p2", "Evicted"}},
		{name: "second pod terminated Completed", pods: []string{readyP1, completedP2}, problem: "pod p2 container svc: terminated Completed, restarts 0"},
		{name: "second pod phase Failed OutOfmemory", pods: []string{readyP1, oomRejectP2}, fault: true, want: []string{"pod p2", "phase Failed (OutOfmemory)"}},
		{name: "phase Failed pod plus a restarting pod", pods: []string{restartP1, oomRejectP2}, fault: true, want: []string{"pod p2", "OutOfmemory"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, pods := parseCase(t, tc)
			problem, fault := podHealth(pods)
			checkVerdict(t, tc, false, problem, fault)
		})
	}
}

func TestParseDeployPods(t *testing.T) {
	bad := map[string][]byte{
		"zero Deployments": kubeJSON(readyP1),
		"two Deployments":  kubeJSON(testDeploy, testDeploy, readyP1),
		"unknown kind":     kubeJSON(testDeploy, `{"kind":"ReplicaSet","metadata":{"name":"svc-1"}}`),
		"not a List":       []byte(testDeploy),
	}
	for name, b := range bad {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseDeployPods(b); err == nil {
				t.Fatal("no error")
			}
		})
	}
	t.Run("pods sorted", func(t *testing.T) {
		d, pods, err := parseDeployPods(kubeJSON(readyP2, testDeploy, unschedP1))
		if err != nil {
			t.Fatal(err)
		}
		if d.Metadata.Name != "svc" || len(pods) != 2 || pods[0].Metadata.Name != "p1" || pods[1].Metadata.Name != "p2" {
			t.Fatalf("got %q and %d pods %+v", d.Metadata.Name, len(pods), pods)
		}
	})
}

// TestParseDeployPodsCandidateText: message fields and unknown reasons
// never reach a problem or fault.
func TestParseDeployPodsCandidateText(t *testing.T) {
	pods := []string{
		`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False","reason":"SECRET-REASON","message":"SECRET-MARKER"}],"containerStatuses":[{"name":"svc","restartCount":1,"state":{"waiting":{"reason":"SECRET-REASON","message":"SECRET-MARKER"}},"lastState":{"terminated":{"reason":"Error","message":"SECRET-MARKER","exitCode":1}}}]}}`,
		`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"SECRET-REASON","message":"SECRET-MARKER"}]}}`,
		`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":0,"state":{"terminated":{"reason":"SECRET-REASON","message":"SECRET-MARKER","exitCode":0}}}]}}`,
		`{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Failed","reason":"SECRET-REASON","message":"SECRET-MARKER"}}`,
	}
	want := []verdictCase{
		{problem: "pod p1 container svc: last terminated Error, restarts 1"},
		{fault: true, want: []string{"pod p1", "not scheduled (other)"}},
		{problem: "pod p1 container svc: terminated other, restarts 0"},
		{fault: true, want: []string{"pod p1", "phase Failed (other)"}},
	}
	for i, p := range pods {
		tc := want[i]
		tc.pods = []string{p}
		d, parsed := parseCase(t, tc)
		ready, problem, fault := readyCheck(d, parsed, true)
		checkVerdict(t, tc, ready, problem, fault)
		hp, hf := podHealth(parsed)
		texts := []string{problem, hp}
		for _, f := range []error{fault, hf} {
			if f != nil {
				texts = append(texts, f.Error())
			}
		}
		for _, s := range texts {
			if strings.Contains(s, "SECRET") {
				t.Errorf("pod %d: candidate text in %q", i, s)
			}
		}
	}
}
