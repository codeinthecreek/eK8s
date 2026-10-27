package orchestrator

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// stubKube is a Kube whose kubectl answers from outs in call order (the
// last repeats) and records each call's args.
func stubKube(outs ...string) (*Kube, *[][]string) {
	var calls [][]string
	k := &Kube{Context: "kind-x"}
	k.run = func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		out := outs[min(len(calls), len(outs))-1]
		if out == "ERR" {
			return "", errors.New("kubectl failed")
		}
		return out, nil
	}
	return k, &calls
}

const (
	noPods       = `{"kind":"List","items":[]}`
	oneRunning   = `{"kind":"List","items":[{"kind":"Pod","metadata":{"name":"s"},"status":{"phase":"Running"}}]}`
	onePending   = `{"kind":"List","items":[{"kind":"Pod","metadata":{"name":"s"},"status":{"phase":"Pending"}}]}`
	oneDeleting  = `{"kind":"List","items":[{"kind":"Pod","metadata":{"name":"s","deletionTimestamp":"2026-10-27T00:00:00Z"},"status":{"phase":"Running"}}]}`
	kindnetReady = `{"kind":"DaemonSet","metadata":{"generation":2},"status":{"observedGeneration":2,"desiredNumberScheduled":1,"updatedNumberScheduled":1,"numberReady":1,"numberAvailable":1}}`
	oneNodeJSON  = `{"kind":"List","items":[{"metadata":{"name":"ek8s-control-plane"}}]}`
	metricsText  = "# HELP pod_cpu_usage_seconds_total x\npod_cpu_usage_seconds_total{namespace=\"ek8s-v1\",pod=\"hasher-a\"} 1.5 1700000000000\n"
)

func TestStressSettled(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want int
		ok   bool
	}{
		{noPods, 0, true}, {oneRunning, 0, false}, {oneDeleting, 0, false},
		{oneRunning, 1, true}, {onePending, 1, false}, {oneDeleting, 1, false}, {noPods, 1, false},
	} {
		got, err := stressSettled([]byte(tc.out), tc.want)
		if err != nil || got != tc.ok {
			t.Errorf("stressSettled(%.60s, %d) = %v, %v; want %v", tc.out, tc.want, got, err, tc.ok)
		}
	}
	for _, bad := range []string{`{`, `{"kind":"Pod"}`, `{"kind":"List","items":[{"kind":"Service"}]}`} {
		if _, err := stressSettled([]byte(bad), 0); err == nil {
			t.Errorf("stressSettled(%s): want an error", bad)
		}
	}
}

func TestDaemonSetReady(t *testing.T) {
	ready := func(s string) bool {
		t.Helper()
		ok, err := daemonSetReady([]byte(s))
		if err != nil {
			t.Fatalf("daemonSetReady(%s): %v", s, err)
		}
		return ok
	}
	if !ready(kindnetReady) {
		t.Error("a Ready DaemonSet reads not Ready")
	}
	for _, from := range []string{`"numberReady":1`, `"observedGeneration":2`, `"updatedNumberScheduled":1`, `"numberAvailable":1`} {
		to := strings.Replace(strings.Replace(from, ":1", ":0", 1), ":2", ":1", 1)
		if ready(strings.Replace(kindnetReady, from, to, 1)) {
			t.Errorf("with %s: Ready, want not", to)
		}
	}
	zero := strings.NewReplacer(":1,", ":0,", ":1}", ":0}").Replace(kindnetReady)
	if ready(zero) {
		t.Error("a DaemonSet with no pod scheduled reads Ready")
	}
	if _, err := daemonSetReady([]byte(`{"kind":"Deployment"}`)); err == nil {
		t.Error("a Deployment: want an error")
	}
}

func TestOneNode(t *testing.T) {
	if n, err := oneNode([]byte(oneNodeJSON)); n != "ek8s-control-plane" || err != nil {
		t.Errorf("oneNode = %q, %v", n, err)
	}
	for _, bad := range []string{
		`{"kind":"List","items":[]}`,
		`{"kind":"List","items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}}]}`,
		`{"kind":"List","items":[{"metadata":{"name":"a/b"}}]}`,
		`{"kind":"List","items":[{"metadata":{"name":""}}]}`,
	} {
		if _, err := oneNode([]byte(bad)); err == nil {
			t.Errorf("oneNode(%s): want an error", bad)
		}
	}
}

// TestKubeCalls checks the kubectl argv of each Cluster method, that
// every call names the context and a request timeout, and that the node
// is looked up once.
func TestKubeCalls(t *testing.T) {
	ctx := t.Context()
	k, calls := stubKube("", noPods)
	if err := k.StressOff(ctx); err != nil {
		t.Fatalf("StressOff: %v", err)
	}
	k2, calls2 := stubKube("", onePending, oneRunning)
	if err := k2.StressOn(ctx); err != nil {
		t.Fatalf("StressOn: %v", err)
	}
	k3, calls3 := stubKube(kindnetReady)
	if ok, err := k3.KindnetReady(ctx); !ok || err != nil {
		t.Fatalf("KindnetReady = %v, %v", ok, err)
	}
	k4, calls4 := stubKube(oneNodeJSON, metricsText)
	for range 2 {
		s, err := k4.PodCPU(ctx)
		if err != nil || len(s) != 1 {
			t.Fatalf("PodCPU = %v, %v", s, err)
		}
	}
	k5, calls5 := stubKube("")
	if err := k5.DeleteNamespace(ctx, "ek8s-v7"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	if err := k5.DeleteNamespace(ctx, "kube-system"); err == nil {
		t.Error("DeleteNamespace(kube-system): want a refusal")
	}

	want := [][]string{
		{"-n", "ek8s-stress", "scale", "deploy/stress-ng", "--replicas=0"},
		{"get", "pods", "-n", "ek8s-stress", "-l", "app=stress-ng", "-o", "json"},
		{"-n", "ek8s-stress", "scale", "deploy/stress-ng", "--replicas=1"},
		{"get", "pods", "-n", "ek8s-stress", "-l", "app=stress-ng", "-o", "json"},
		{"get", "pods", "-n", "ek8s-stress", "-l", "app=stress-ng", "-o", "json"},
		{"get", "daemonset", "kindnet", "-n", "kube-system", "-o", "json"},
		{"get", "nodes", "-o", "json"},
		{"get", "--raw", "/api/v1/nodes/ek8s-control-plane/proxy/metrics/resource"},
		{"get", "--raw", "/api/v1/nodes/ek8s-control-plane/proxy/metrics/resource"},
		{"delete", "namespace", "ek8s-v7", "--ignore-not-found", "--wait=true", "--timeout=3m0s"},
	}
	var got [][]string
	for _, c := range [][][]string{*calls, *calls2, *calls3, *calls4, *calls5} {
		got = append(got, c...)
	}
	if len(got) != len(want) {
		t.Fatalf("%d kubectl calls, want %d: %q", len(got), len(want), got)
	}
	for i, args := range got {
		tail := []string{"--request-timeout", "30s", "--context", "kind-x"}
		if !slices.Equal(args, append(slices.Clone(want[i]), tail...)) {
			t.Errorf("call %d: %q, want %q + %q", i, args, want[i], tail)
		}
	}
}

func TestKubeFaults(t *testing.T) {
	ctx := t.Context()
	if _, err := (&Kube{}).KindnetReady(ctx); err == nil || !strings.Contains(err.Error(), "empty kube context") {
		t.Errorf("no context: %v", err)
	}
	k, _ := stubKube("ERR")
	if err := k.StressOff(ctx); err == nil {
		t.Error("StressOff with kubectl failing: want an error")
	}
	if _, err := k.PodCPU(ctx); err == nil {
		t.Error("PodCPU with kubectl failing: want an error")
	}
	k, _ = stubKube("", "ERR")
	if err := k.StressOn(ctx); err == nil {
		t.Error("StressOn with the pod poll failing: want an error")
	}
	if _, err := k.kubectl(ctx, "exec", "--", "sh"); err == nil {
		t.Error(`args with "--": want a refusal`)
	}
}

func TestPollCtx(t *testing.T) {
	k, _ := stubKube("", oneRunning)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := k.StressOff(ctx); err == nil {
		t.Error("StressOff with ctx done and stress never stopping: want an error")
	}
}
