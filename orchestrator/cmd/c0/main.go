// Command c0 measures C0 as DESIGN.md sec 10 defines it: the highest
// rate at which generation 0, on a single pod (the variant's HPA held at
// minReplicas = maxReplicas = 1) with no stress, has at most 1% of its
// scored requests end as graceful_shed or failed, with the p95 latency of
// its successes at most HangTimeout/4, in a window that was scored. The
// search walks -ladder upward to the first failing rate, bisects integer
// rates between the last pass and that fail, then runs -confirm more
// windows at the last pass. The run's r is 0.5 x C0 (a pass at loadgen's
// top rate gives C0 >= that rate).
//
// Gen 0's wrapper has 2 slots per pod: under the 1000m CPU limit Go sets
// GOMAXPROCS to 2 (it rounds a cgroup limit up, with a minimum of 2;
// docs/decisions-archive.md, PR #26 variant-resources entry), and the
// slots are min(GOMAXPROCS, memory limit / 2 / 20 MiB), 2 here.
//
// The tool does not build the image: -image is an ek8s-wrapper:<12 hex>
// already loaded into the kind node (deploy/README.md, "C0"). From the
// repo root, with the cluster up (deploy/up.sh), stress-ng at 0 replicas
// and no variant namespace left:
//
//	go run ./orchestrator/cmd/c0 -image ek8s-wrapper:<12 hex> -out c0.json
//
// Each step redeploys loadgen at its rate (deploy/loadgen-up.sh), so a
// step starts a new loadgen with no target and a new boot_id. A step
// whose window was excluded, or whose pod, HPA or restart checks failed,
// is not measured: it is rerun once, and a second invalid step halts. A
// disqualification of generation 0, or any fault, halts at once. The
// result file is rewritten after every step. On exit the variant's
// namespace and loadgen target are deleted, unless -keep; loadgen stays
// deployed at the last step's rate.
//
// Exit status: 0 for a C0 or a lower bound, 1 when the search halted
// without one, 2 on a fault, an interrupt, or when it could not start.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	lgdeploy "github.com/codeinthecreek/eK8s/deploy/loadgen"
	"github.com/codeinthecreek/eK8s/deploy/variant"
	"github.com/codeinthecreek/eK8s/fitness"
	"github.com/codeinthecreek/eK8s/gate"
	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/hostclock"
	"github.com/codeinthecreek/eK8s/orchestrator"
)

// controlTimeout is the control API client's per-request timeout, as
// cmd/orchestrator's.
const controlTimeout = 30 * time.Second

// Bounds on the waits that are not the window.
const (
	podBound  = 3 * time.Minute // a pod Ready, the HPA settled
	bootBound = 30 * time.Second
)

// stressNS and stressDeployment name stress-ng (deploy/stress).
const (
	stressNS         = "ek8s-stress"
	stressDeployment = "stress-ng"
)

type config struct {
	image, id, out, repo, kubeCtx string
	ladder                        []int
	confirm, workers              int
	keep                          bool
	// sysfs is the sysfs root the host clocks are read under;
	// allowUnpinned measures on a host whose clocks are not pinned.
	sysfs         string
	allowUnpinned bool
}

func main() {
	var c config
	ladder := flag.String("ladder", "6,9,12,15,18,19", "rates to walk upward, comma-separated integers, strictly increasing")
	flag.StringVar(&c.image, "image", "", "generation 0's image, ek8s-wrapper:<12 hex>, already kind-loaded (not built here)")
	flag.StringVar(&c.id, "id", "900001", "the variant id; its namespace is ek8s-v<id>")
	flag.IntVar(&c.confirm, "confirm", 2, "extra windows at the last passing rate, all of which must pass")
	flag.StringVar(&c.out, "out", "", "the JSON result file to create; it must not exist")
	flag.BoolVar(&c.keep, "keep", false, "keep the variant's namespace and loadgen target on exit")
	flag.StringVar(&c.repo, "repo", ".", "repo checkout (deploy/loadgen-up.sh runs from it)")
	flag.StringVar(&c.kubeCtx, "kube-context", "kind-ek8s", "the kubectl context")
	flag.IntVar(&c.workers, "workers", 0, "host Argon2 goroutines (0: GOMAXPROCS)")
	flag.BoolVar(&c.allowUnpinned, "allow-unpinned", false, "measure although the host clocks are not pinned (governor performance, turbo off); recorded in the result")
	flag.Parse()
	c.sysfs = hostclock.DefaultRoot
	if flag.NArg() > 0 || c.image == "" || c.out == "" {
		flag.Usage()
		os.Exit(2)
	}
	var err error
	if c.ladder, err = parseLadder(*ladder); err != nil {
		fail(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	v, err := run(ctx, c)
	switch {
	case err != nil:
		fail(err)
	case v.Halted():
		fmt.Println("halted:", haltText(v))
		os.Exit(1)
	default:
		fmt.Println(resultText(v))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "c0:", err)
	os.Exit(2)
}

// parseLadder reads "6,9,12" as strictly increasing positive integers.
func parseLadder(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("ladder %q: %w", s, err)
		}
		if n < 1 || (len(out) > 0 && n <= out[len(out)-1]) {
			return nil, fmt.Errorf("ladder %q: want positive integers, strictly increasing", s)
		}
		out = append(out, n)
	}
	return out, nil
}

// loadgenImageStandIn is a well-formed loadgen image for validating a
// rate: deploy/loadgen checks the rate with loadgen's own Config.Validate,
// where maxRate lives (unexported in package loadgen).
const loadgenImageStandIn = "ek8s-loadgen:000000000000"

// checkRates refuses a ladder rate loadgen would not accept, maxRate
// included.
func checkRates(ladder []int) error {
	for _, r := range ladder {
		p := lgdeploy.Params{Rate: strconv.FormatFloat(float64(r), 'g', -1, 64), Image: loadgenImageStandIn}
		if err := p.Validate(); err != nil {
			return fmt.Errorf("ladder rate %d: %w", r, err)
		}
	}
	return nil
}

// passRule is the C0 rule of DESIGN.md sec 10 on one scored window:
// (sheds + failures) / requests <= 1% and the successes' p95 at most
// HangTimeout/4. why names the halves that failed.
func passRule(st fitness.Stats) (pass bool, why string) {
	var bad []string
	if st.Requests == 0 {
		return false, "no requests"
	}
	if (st.Sheds+st.Failures)*100 > st.Requests {
		bad = append(bad, fmt.Sprintf("shed+failed %d of %d requests (%.2f%%) over 1%%",
			st.Sheds+st.Failures, st.Requests, 100*float64(st.Sheds+st.Failures)/float64(st.Requests)))
	}
	if st.P95Latency > classifier.HangTimeout/4 {
		bad = append(bad, fmt.Sprintf("p95 %v over %v", st.P95Latency, classifier.HangTimeout/4))
	}
	return len(bad) == 0, strings.Join(bad, "; ")
}

// verdictResult maps a Verdict to the record's Result.
func verdictResult(v Verdict) *Result {
	switch v.Kind {
	case Resolved:
		return &Result{C0: strconv.Itoa(v.C0), R: halve(v.C0), Pass: v.Pass, Fail: v.Fail}
	case AtLeast:
		return &Result{C0: ">= " + strconv.Itoa(v.C0), R: halve(v.C0), Pass: v.Pass}
	}
	return &Result{C0: "unresolved", Pass: v.Pass, Fail: v.Fail}
}

// halve is 0.5 x c0 in strconv 'g', -1 form.
func halve(c0 int) string { return strconv.FormatFloat(0.5*float64(c0), 'g', -1, 64) }

func resultText(v Verdict) string {
	res := verdictResult(v)
	return fmt.Sprintf("C0 %s, r = %s", res.C0, res.R)
}

func haltText(v Verdict) string {
	switch v.Kind {
	case LadderTooHigh:
		return fmt.Sprintf("ladder too high: the first rate, %d, failed", v.Fail)
	case InvalidTwice:
		return fmt.Sprintf("rate %d was invalid twice (last pass %d, first fail %d)", v.Rate, v.Pass, v.Fail)
	case Unresolved:
		if v.Fail == 0 {
			return fmt.Sprintf("C0 unresolved: a confirm window at the top rate, %d, failed (last pass below it %d)", v.Rate, v.Pass)
		}
		return fmt.Sprintf("C0 unresolved: a confirm window at %d failed (first fail %d)", v.Rate, v.Fail)
	}
	return string(v.Kind)
}

// runner holds one run's cluster state.
type runner struct {
	c      config
	kc     kubectl
	kube   *orchestrator.Kube
	proxy  *gate.ControlProxy
	core   *hashcore.Core
	clock  hostclock.State // read at start, before the preflight
	rec    *Record
	ns     string
	label  string
	boot   string // the last boot_id seen
	sess   *orchestrator.Session
	target bool // PutTarget succeeded on the last session's loadgen
	// created is set once kubectl create made the bundle: only then is
	// the namespace ours to delete.
	created bool
}

func run(ctx context.Context, c config) (Verdict, error) {
	if err := (variant.Params{ID: c.id, Label: "ek8s-0000000000000000", Image: c.image}).Validate(); err != nil {
		return Verdict{}, err
	}
	if err := checkRates(c.ladder); err != nil {
		return Verdict{}, err
	}
	// deploy/loadgen-up.sh deploys to kind-ek8s whatever this says.
	if c.kubeCtx != "kind-ek8s" {
		return Verdict{}, fmt.Errorf("kube context %q: loadgen-up.sh deploys to kind-ek8s only", c.kubeCtx)
	}
	repo, err := filepath.Abs(c.repo)
	if err != nil {
		return Verdict{}, err
	}
	c.repo = repo
	if _, err := os.Lstat(c.out); err == nil {
		return Verdict{}, fmt.Errorf("%s exists; the result goes to a new file", c.out)
	}
	if _, err := os.Stat(filepath.Dir(c.out)); err != nil {
		return Verdict{}, err
	}

	r := &runner{c: c, kc: kubectl{context: c.kubeCtx}, kube: &orchestrator.Kube{Context: c.kubeCtx},
		core: hashcore.New(), ns: variant.Params{ID: c.id}.Namespace()}
	b := make([]byte, 8)
	rand.Read(b) // never returns an error (Go 1.24+)
	r.label = "ek8s-" + hex.EncodeToString(b)
	if r.clock, err = hostclock.Preflight(c.sysfs, c.allowUnpinned); err != nil {
		return Verdict{}, err
	}
	if err := r.preflight(ctx); err != nil {
		return Verdict{}, err
	}
	rec, err := r.newRecord(ctx)
	if err != nil {
		return Verdict{}, err
	}
	r.rec = rec
	if err := rec.save(c.out); err != nil {
		return Verdict{}, err
	}
	proxy, err := gate.StartControlProxy(ctx, c.kubeCtx, controlTimeout)
	if err != nil {
		return Verdict{}, fmt.Errorf("control API: %w", err)
	}
	defer proxy.Close()
	r.proxy = proxy

	defer r.cleanup()
	if err := r.setup(ctx); err != nil {
		return r.finish(Verdict{}, err)
	}
	v, err := Search(c.ladder, c.confirm, func(rate, attempt int) (Obs, error) {
		return r.step(ctx, rate, attempt)
	})
	return r.finish(v, err)
}

// finish records the search's end and returns it.
func (r *runner) finish(v Verdict, err error) (Verdict, error) {
	now := time.Now().UTC()
	r.rec.Ended = &now
	switch {
	case err != nil:
		r.rec.Halt = err.Error()
		r.rec.Result = &Result{C0: "unresolved"}
	default:
		r.rec.Result = verdictResult(v)
		if v.Halted() {
			r.rec.Halt = haltText(v)
		}
	}
	if serr := r.rec.save(r.c.out); serr != nil && err == nil {
		err = serr
	}
	return v, err
}

// preflight refuses to start on a cluster the measurement would not be
// clean on: stress-ng running, or a variant namespace left over (a
// second variant would share the node, and Stage 5 style isolation is
// gone).
func (r *runner) preflight(ctx context.Context) error {
	out, err := r.kc.run(ctx, nil, "get", "deploy", stressDeployment, "-n", stressNS, "-o", "jsonpath={.spec.replicas}")
	if err != nil {
		return fmt.Errorf("preflight: reading stress-ng replicas: %w", err)
	}
	if out != "0" {
		return fmt.Errorf("preflight: stress-ng has replicas %q, want 0 (kubectl -n %s scale deploy/%s --replicas=0)", out, stressNS, stressDeployment)
	}
	if out, err := r.kc.run(ctx, nil, "get", "namespace", r.ns, "--ignore-not-found", "-o", "name"); err != nil {
		return fmt.Errorf("preflight: %w", err)
	} else if out != "" {
		return fmt.Errorf("preflight: namespace %s exists", r.ns)
	}
	left, err := r.kube.VariantNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if len(left) > 0 {
		return fmt.Errorf("preflight: variant namespaces are left: %s (kubectl --context %s delete namespace -l ek8s.io/role=variant)",
			strings.Join(left, " "), r.c.kubeCtx)
	}
	return nil
}

// newRecord fills the fixed header of the result file.
func (r *runner) newRecord(ctx context.Context) (*Record, error) {
	git := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", r.c.repo}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("repo HEAD: %w", err)
	}
	dirty, err := git("status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("repo status: %w", err)
	}
	cpu, err := r.kc.run(ctx, nil, "get", "nodes", "-o", "jsonpath={.items[*].status.allocatable.cpu}")
	if err != nil {
		return nil, fmt.Errorf("node allocatable cpu: %w", err)
	}
	return &Record{
		Head: head, Dirty: dirty != "", Image: r.c.image, ID: r.c.id, Label: r.label, NProc: runtime.NumCPU(), NodeCPU: cpu,
		Ladder: r.c.ladder, Confirm: r.c.confirm, HostClock: r.clock, AllowUnpinned: r.c.allowUnpinned, Started: time.Now().UTC(),
	}, nil
}

// setup creates the variant's bundle, pins its HPA to one replica and
// waits for exactly one Ready pod.
func (r *runner) setup(ctx context.Context) error {
	manifest, err := variant.Render(variant.Params{ID: r.c.id, Label: r.label, Image: r.c.image})
	if err != nil {
		return err
	}
	// Set before the create: a create that fails part-way leaves the
	// namespace behind. Preflight showed it did not exist.
	r.created = true
	if _, err := r.kc.run(ctx, manifest, "create", "-f", "-"); err != nil {
		return fmt.Errorf("creating the bundle: %w", err)
	}
	// A merge patch rather than a variant.Params field: only this tool
	// wants a single-pod HPA, and the bundle stays one shape.
	if _, err := r.kc.run(ctx, nil, "patch", "hpa", "hasher", "-n", r.ns, "--type", "merge",
		"-p", `{"spec":{"minReplicas":1,"maxReplicas":1}}`); err != nil {
		return fmt.Errorf("patching the HPA: %w", err)
	}
	if err := r.kc.waitOnePod(ctx, r.ns, podBound); err != nil {
		return err
	}
	return r.waitPinned(ctx, podBound)
}

// waitPinned polls until the HPA reports 1/1/1/1 (its status follows the
// patch on the controller's next sync).
func (r *runner) waitPinned(ctx context.Context, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		ok, why, err := r.kc.hpaPinned(ctx, r.ns)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().Add(time.Second).After(deadline) {
			return fmt.Errorf("c0: %s: %s after %v", r.ns, why, bound)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// cleanup deletes the loadgen target and the namespace, unless -keep.
// Session.Drop does both; when it cannot (no window read the target's
// records), the namespace goes alone and the target goes with the next
// loadgen, which holds none.
func (r *runner) cleanup() {
	if !r.created {
		return
	}
	if r.c.keep {
		fmt.Fprintf(os.Stderr, "c0: kept namespace %s and loadgen target %s\n", r.ns, r.c.id)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Minute)
	defer cancel()
	if r.sess != nil && r.target {
		if err := r.sess.Drop(ctx, r.c.id); err == nil {
			return
		} else {
			fmt.Fprintf(os.Stderr, "c0: dropping the loadgen target: %v\n", err)
		}
	}
	if err := r.kube.DeleteNamespace(ctx, r.ns); err != nil {
		fmt.Fprintf(os.Stderr, "c0: %v\n", err)
	}
}

// step runs one window at rate and records it. An error is a fault or a
// disqualification and halts the search; an invalid step is not an error.
func (r *runner) step(ctx context.Context, rate, attempt int) (Obs, error) {
	sr := StepRecord{Rate: rate, Attempt: attempt, Started: time.Now().UTC()}
	o, err := r.measure(ctx, rate, &sr)
	sr.Ended = time.Now().UTC()
	sr.Valid, sr.Pass = o.Valid, o.Pass
	if err != nil {
		sr.Error = err.Error()
	}
	sr.BootID = r.boot
	r.rec.Steps = append(r.rec.Steps, sr)
	if serr := r.rec.save(r.c.out); serr != nil && err == nil {
		err = serr
	}
	fmt.Fprintln(os.Stderr, progress(sr))
	return o, err
}

func progress(s StepRecord) string {
	head := fmt.Sprintf("c0: r=%d attempt=%d:", s.Rate, s.Attempt)
	switch {
	case s.Error != "":
		return head + " halted: " + s.Error
	case !s.Valid:
		return head + " invalid: " + s.WhyNot
	case s.Pass:
		return fmt.Sprintf("%s pass: %d requests, %d shed, %d failed, p95 %.0f ms, cpu/success %.4f s",
			head, s.Requests, s.Sheds, s.Failures, s.P95LatencyMS, s.CPUPerSuccess)
	}
	return fmt.Sprintf("%s fail: %s (%d requests, %d shed, %d failed)", head, s.PassWhyNot, s.Requests, s.Sheds, s.Failures)
}

// measure is one step's body, filling sr as it goes.
func (r *runner) measure(ctx context.Context, rate int, sr *StepRecord) (Obs, error) {
	rateStr := strconv.FormatFloat(float64(rate), 'g', -1, 64)
	r.sess, r.target = nil, false
	if err := r.redeployLoadgen(ctx, rateStr); err != nil {
		return Obs{}, err
	}
	cl := r.proxy.Client()
	sess, err := orchestrator.NewSession(ctx, orchestrator.SessionConfig{
		Client: cl, Cluster: r.kube, Core: r.core,
		Want:   orchestrator.Shape{Rate: float64(rate), WindowS: lgdeploy.W.Seconds(), BurstRate: 4 * float64(rate)},
		Params: fitness.DefaultParams(), Workers: r.c.workers,
	})
	if err != nil {
		return Obs{}, err
	}
	r.sess, r.target = sess, false
	if _, created, err := cl.PutTarget(ctx, r.c.id); err != nil {
		return Obs{}, fmt.Errorf("putting target %s: %w", r.c.id, err)
	} else if !created {
		return Obs{}, fmt.Errorf("target %s existed in the new loadgen", r.c.id)
	}
	r.target = true
	if err := sess.Adopt(r.c.id, nil); err != nil {
		return Obs{}, err
	}

	pre, why, err := r.check(ctx, nil)
	if err != nil {
		return Obs{}, err
	}
	sr.Pod, sr.RestartsStart, sr.RestartsEnd = pre.Name, pre.Restarts, pre.Restarts
	if why != "" {
		sr.WhyNot = "before the window: " + why
		return Obs{}, nil
	}
	batches, err := sess.Batches(ctx, 1)
	if err != nil {
		return Obs{}, err
	}
	res, err := sess.RunBaseline(ctx, []string{r.c.id}, batches)
	if err != nil {
		return Obs{}, err
	}
	vr := res.Variants[r.c.id]
	if vr.Disqualified != classifier.ReasonNone {
		return Obs{}, fmt.Errorf("generation 0 disqualified for %s at record %d: an integrity bug, not a measurement",
			vr.Disqualified, vr.DisqualifiedIndex)
	}
	sr.Excluded = res.Excluded
	post, why, err := r.check(ctx, &pre)
	if err != nil {
		return Obs{}, err
	}
	sr.RestartsEnd = post.Restarts

	var invalid []string
	if !res.Scored() {
		invalid = append(invalid, "window excluded: "+strings.Join(res.Excluded, ","))
	}
	if why != "" {
		invalid = append(invalid, "after the window: "+why)
	}
	if vr.Baseline != nil {
		st := vr.Baseline.Stats
		sr.Requests, sr.Successes, sr.Sheds, sr.Failures, sr.Negatives = st.Requests, st.Successes, st.Sheds, st.Failures, st.Negatives
		sr.P95LatencyMS = float64(st.P95Latency) / float64(time.Millisecond)
		sr.CPUPerSuccess, sr.CPUUnmeasured = st.CPUPerSuccess, vr.Baseline.CPUUnmeasured
		if st.Successes > 0 && vr.Baseline.CPUUnmeasured == "" {
			sr.CPUSeconds = st.CPUPerSuccess * float64(st.Successes)
		}
	}
	if len(invalid) > 0 {
		sr.WhyNot = strings.Join(invalid, "; ")
		return Obs{}, nil
	}
	if vr.Baseline == nil {
		return Obs{}, errors.New("a scored window without a score for generation 0")
	}
	pass, failWhy := passRule(vr.Baseline.Stats)
	sr.PassWhyNot = failWhy
	return Obs{Valid: true, Pass: pass}, nil
}

// check reads the pod and the HPA. why is non-empty when a check failed:
// not exactly one Ready pod, the HPA not 1/1, or (with before set) the
// pod replaced or restarted since.
func (r *runner) check(ctx context.Context, before *podState) (podState, string, error) {
	s, err := r.kc.pods(ctx, r.ns)
	if err != nil {
		return podState{}, "", err
	}
	if s.Pods != 1 || s.Ready != 1 {
		return s, fmt.Sprintf("%d pods, %d Ready, want exactly one Ready", s.Pods, s.Ready), nil
	}
	ok, why, err := r.kc.hpaPinned(ctx, r.ns)
	if err != nil {
		return s, "", err
	}
	if !ok {
		return s, why, nil
	}
	if before != nil {
		switch {
		case s.Name != before.Name:
			return s, fmt.Sprintf("pod %s replaced by %s", before.Name, s.Name), nil
		case s.Restarts != before.Restarts:
			return s, fmt.Sprintf("restart count %d, was %d", s.Restarts, before.Restarts), nil
		}
	}
	return s, "", nil
}

// redeployLoadgen runs deploy/loadgen-up.sh at rate and waits for a
// loadgen whose boot_id is new. Applying an unchanged manifest (the same
// rate as the last step) leaves the old pod running, with its target and
// salts; a rollout restart then gives the step a new one.
func (r *runner) redeployLoadgen(ctx context.Context, rate string) error {
	cl := r.proxy.Client()
	if r.boot == "" {
		cl.ExpectAny()
		if cfg, err := cl.Config(ctx); err == nil {
			r.boot = cfg.BootID
		}
	}
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(r.c.repo, "deploy", "loadgen-up.sh"), rate)
	cmd.Dir = r.c.repo
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("deploy/loadgen-up.sh %s: %w", rate, err)
	}
	boot, same, err := r.awaitBoot(ctx)
	if err != nil {
		return err
	}
	if same {
		if _, err := r.kc.run(ctx, nil, "-n", "ek8s-system", "rollout", "restart", "deploy/loadgen"); err != nil {
			return err
		}
		if _, err := r.kc.run(ctx, nil, "-n", "ek8s-system", "rollout", "status", "deploy/loadgen", "--timeout=180s"); err != nil {
			return err
		}
		if boot, same, err = r.awaitBoot(ctx); err != nil {
			return err
		} else if same {
			return errors.New("loadgen's boot_id did not change after a rollout restart")
		}
	}
	r.boot = boot
	return nil
}

// awaitBoot polls GET /config until a boot_id other than r.boot answers,
// at most bootBound (the loadgen-smoke.sh pattern). same is true when the
// old boot_id still answered at the end.
func (r *runner) awaitBoot(ctx context.Context) (boot string, same bool, err error) {
	cl := r.proxy.Client()
	deadline := time.Now().Add(bootBound)
	var last error
	for {
		cl.ExpectAny()
		cfg, err := cl.Config(ctx)
		switch {
		case err != nil:
			last = err
		case cfg.BootID != r.boot:
			return cfg.BootID, false, nil
		default:
			same, last = true, nil
		}
		if time.Now().Add(time.Second).After(deadline) {
			if same {
				return r.boot, true, nil
			}
			return "", false, fmt.Errorf("no loadgen answered GET /config within %v: %v", bootBound, last)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return "", false, err
		}
	}
}
