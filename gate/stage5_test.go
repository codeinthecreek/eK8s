package gate

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/loadgen/client"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

const (
	nodesOK       = `{"kind":"List","items":[{"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`
	nodesNotReady = `{"kind":"List","items":[{"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`
	stressRunning = `{"kind":"List","items":[{"kind":"Pod","metadata":{"name":"s1"},"status":{"phase":"Running"}}]}`
	stressPending = `{"kind":"List","items":[{"kind":"Pod","metadata":{"name":"s1"},"status":{"phase":"Pending"}}]}`
	unknownP1     = `{"kind":"Pod","metadata":{"name":"p1"},"status":{"phase":"Running","containerStatuses":[{"name":"svc","restartCount":0,"state":{"terminated":{"reason":"ContainerStatusUnknown","exitCode":137}}}]}}`
)

// fakeKubectlKind puts a fake kubectl and a fake kind on PATH (and only
// those, plus cat and sleep) and returns their directories. kubectl
// prints kouts in call order.
func fakeKubectlKind(t *testing.T, kouts []string, kexits map[int]int) (kubectlDir, kindDir string) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"cat", "sleep"} {
		p, err := exec.LookPath(tool)
		must(t, err)
		must(t, os.Symlink(p, filepath.Join(bin, tool)))
	}
	mk := func(name string, outs []string, exits map[int]int) string {
		dir := fakeToolDir(t, outs, exits)
		must(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+fmt.Sprintf(fakeScript, dir)), 0o755))
		return dir
	}
	kubectlDir = mk("kubectl", kouts, kexits)
	kindDir = mk("kind", nil, nil)
	t.Setenv("PATH", bin)
	return kubectlDir, kindDir
}

// fakeToolDir is fakeTool's directory setup, without installing a tool.
func fakeToolDir(t *testing.T, outs []string, exits map[int]int) string {
	t.Helper()
	dir := t.TempDir()
	for i, o := range outs {
		must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("out.%d", i+1)), []byte(o), 0o644))
	}
	for n, c := range exits {
		must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("exit.%d", n)), []byte(fmt.Sprintf("%d\n", c)), 0o644))
	}
	return dir
}

// stage5Fixture is a Stage5 over a fake loadgen, kubectl and kind.
type stage5Fixture struct {
	f       *fakeLoadgen
	cfg     Stage5Config
	kubectl string
	kind    string
}

func newStage5(t *testing.T, kouts []string, kexits map[int]int) *stage5Fixture {
	t.Helper()
	fx := &stage5Fixture{f: newFake()}
	fx.kubectl, fx.kind = fakeKubectlKind(t, kouts, kexits)
	srv := httptest.NewServer(fx.f.handler())
	t.Cleanup(srv.Close)
	old := openControl
	openControl = func(context.Context, string) (*client.Client, func(), error) {
		return client.New(srv.URL, srv.Client()), func() {}, nil
	}
	t.Cleanup(func() { openControl = old })
	oldReady := readyPoll
	readyPoll = 1
	t.Cleanup(func() { readyPoll = oldReady })
	fx.cfg = Stage5Config{
		KubeContext: "kind-x", Cluster: "x", ID: "7", Image: "ek8s-wrapper:0123456789ab",
		Rate: 2, WindowS: 300, BurstRate: 30, StressPods: 1, Core: hashcore.New(),
	}
	return fx
}

func (fx *stage5Fixture) run(t *testing.T) (Report, error) {
	t.Helper()
	var r Report
	err := Stage5(t.Context(), fx.cfg, &r)
	return r, err
}

func summary(r Report) string {
	var parts []string
	for _, res := range r.Results {
		state := "pass"
		if !res.Pass {
			state = "FAIL"
		}
		parts = append(parts, res.Check+" "+state)
	}
	return strings.Join(parts, ", ")
}

var readyList = list(testDeploy, readyP1)

func TestStage5Pass(t *testing.T) {
	fx := newStage5(t, []string{nodesOK, stressRunning, "", readyList, readyList}, nil)
	r, err := fx.run(t)
	if err != nil {
		t.Fatalf("Stage5: %v", err)
	}
	if got, want := summary(r), "pod-health pass, responses pass, hash-verify pass"; got != want {
		t.Errorf("results %q, want %q", got, want)
	}
	if got := readFile(t, filepath.Join(fx.kind, "argv")); !strings.HasPrefix(got, "load\ndocker-image\nek8s-wrapper:0123456789ab\n--name\nx\n") {
		t.Errorf("kind argv %q", got)
	}
	if got := readFile(t, filepath.Join(fx.kubectl, "stdin")); !strings.Contains(got, "ek8s-v7") || !strings.Contains(got, "ek8s-wrapper:0123456789ab") {
		t.Errorf("kubectl stdin lacks the bundle for ek8s-v7: %.200q", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(fx.kubectl, "argv")), "create\n-f\n-\n") {
		t.Error("the bundle was not made with kubectl create -f -")
	}
	if len(fx.f.deletes) != 0 {
		t.Errorf("a passing candidate's target was deleted: %v", fx.f.deletes)
	}
}

func TestStage5CandidateFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kouts   []string
		records func(target, run string, n int) []wire.Record
		want    string
		deleted bool
	}{
		{"never_ready", []string{nodesOK, stressRunning, "", list(testDeploy, restartP1), nodesOK}, nil, "deploy-ready FAIL", false},
		{"restarted_under_load", []string{nodesOK, stressRunning, "", readyList, list(testDeploy, restartP1), nodesOK}, nil, "pod-health FAIL", true},
		{"failed_response", []string{nodesOK, stressRunning, "", readyList, readyList}, func(target, run string, n int) []wire.Record {
			r := cleanRecords(target, run, n)
			r[2].Bucket, r[2].Reason = "failed", "bad_status"
			return r
		}, "pod-health pass, responses FAIL", true},
		{"bad_hash", []string{nodesOK, stressRunning, "", readyList, readyList}, func(target, run string, n int) []wire.Record {
			r := cleanRecords(target, run, n)
			r[2].Kind = kindHash
			r[2].Pair = &wire.Pair{Password: "pw", Hash: "$argon2id$not-a-hash"}
			return r
		}, "pod-health pass, responses pass, hash-verify FAIL", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newStage5(t, tc.kouts, nil)
			fx.f.records = tc.records
			r, err := fx.run(t)
			if err != nil {
				t.Fatalf("Stage5: %v", err)
			}
			if got := summary(r); got != tc.want {
				t.Errorf("results %q, want %q", got, tc.want)
			}
			if tc.deleted != (len(fx.f.deletes) == 1 && fx.f.deletes[0] == "7:29") {
				t.Errorf("deletes %v, want one 7:29: %v", fx.f.deletes, tc.deleted)
			}
		})
	}
}

func TestStage5Faults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kouts []string
		exits map[int]int
		set   func(fx *stage5Fixture)
		want  string
	}{
		{"node_not_ready", []string{nodesNotReady}, nil, nil, "node n1: not Ready"},
		{"stress_not_running", []string{nodesOK, stressPending}, nil, nil, "stress-ng"},
		{"create_fails", []string{nodesOK, stressRunning}, map[int]int{3: 1}, nil, "stage 5: create"},
		{"never_ready_on_a_notready_node", []string{nodesOK, stressRunning, "", list(testDeploy, restartP1), nodesNotReady}, nil, nil, "node n1: not Ready"},
		{"restart_on_a_notready_node", []string{nodesOK, stressRunning, "", readyList, list(testDeploy, restartP1), nodesNotReady}, nil, nil, "node n1: not Ready"},
		{"container_status_unknown", []string{nodesOK, stressRunning, "", readyList, list(testDeploy, unknownP1)}, nil, nil, "ContainerStatusUnknown"},
		{"loadgen_config", []string{nodesOK, stressRunning, "", readyList}, nil, func(fx *stage5Fixture) { fx.f.cfg.Rate = 3 }, "loadgen config"},
		{"no_stress_pods_asked", nil, nil, func(fx *stage5Fixture) { fx.cfg.StressPods = 0 }, "stress pods"},
		{"bad_id", nil, nil, func(fx *stage5Fixture) { fx.cfg.ID = "007" }, "variant id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newStage5(t, tc.kouts, tc.exits)
			if tc.set != nil {
				tc.set(fx)
			}
			r, err := fx.run(t)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Stage5 error = %v, results %q; want a fault containing %q", err, summary(r), tc.want)
			}
			if len(r.Results) != 0 {
				t.Errorf("results %q with a fault, want none", summary(r))
			}
		})
	}
}

func TestStage5CtxCancelled(t *testing.T) {
	fx := newStage5(t, []string{nodesOK, stressRunning, "", readyList, readyList}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var r Report
	if err := Stage5(ctx, fx.cfg, &r); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage5 error = %v, want context.Canceled", err)
	}
}

func TestNodeFault(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"ready", nodesOK, ""},
		{"not_ready", nodesNotReady, "node n1: not Ready"},
		{"no_conditions", `{"kind":"List","items":[{"metadata":{"name":"n1"},"status":{}}]}`, "node n1: not Ready"},
		{"no_nodes", `{"kind":"List","items":[]}`, "no nodes"},
		{"not_a_list", `{"kind":"Node"}`, "want List"},
		{"not_json", `x`, "kubectl output"},
		{"one_of_two", `{"kind":"List","items":[{"metadata":{"name":"a"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"b"},"status":{"conditions":[{"type":"Ready","status":"Unknown"}]}}]}`, "node b: not Ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := nodeFault([]byte(tc.in))
			if (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("nodeFault = %v, want %q", err, tc.want)
			}
		})
	}
}
