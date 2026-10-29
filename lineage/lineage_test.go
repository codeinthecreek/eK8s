package lineage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/hostclock"
)

func newRun(t *testing.T) *Run {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "run"), nil, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return r
}

var wrapperFiles = map[string][]byte{
	"doc.go": []byte("package wrapper\n"), "hasher.go": []byte("// pinned\n"),
	"server.go": []byte("// server\n"), "tunables.go": []byte("// tunables\n"),
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestCreateOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := Create(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if mode(t, dir) != 0o700 || mode(t, filepath.Join(dir, attemptsFile)) != 0o600 || mode(t, filepath.Join(dir, genomesDir)) != 0o700 {
		t.Error("run directory modes are not 0700 and 0600")
	}
	if _, err := Create(dir, nil, false); err == nil {
		t.Error("Create on an existing directory succeeded")
	}
	if _, err := Open(dir); err != nil {
		t.Errorf("Open: %v", err)
	}
	if r.Dir() != dir {
		t.Errorf("Dir = %q", r.Dir())
	}
	if err := os.WriteFile(filepath.Join(dir, runFile), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Error("Open accepted version 2")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("Open accepted a missing directory")
	}
}

// TestRunMetaHostClock: run.json carries the host clocks and the override,
// and a run.json written before they existed still opens.
func TestRunMetaHostClock(t *testing.T) {
	gov, nt := "powersave", "0"
	st := hostclock.State{Governors: map[string]string{"cpu0": gov}, NoTurbo: &nt}
	dir := filepath.Join(t.TempDir(), "run")
	if _, err := Create(dir, st, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, runFile))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version       int
		HostClock     hostclock.State `json:"host_clock"`
		AllowUnpinned bool            `json:"allow_unpinned"`
	}
	if err := strictDecode(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != version || !m.AllowUnpinned || m.HostClock.Governors["cpu0"] != gov || *m.HostClock.NoTurbo != nt {
		t.Errorf("run.json = %s", b)
	}
	for _, key := range []string{`"host_clock"`, `"allow_unpinned":true`, `"intel_pstate_no_turbo":"0"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("run.json lacks %s: %s", key, b)
		}
	}
	if _, err := Open(dir); err != nil {
		t.Errorf("Open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, runFile), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Errorf("Open of a run.json without host_clock: %v", err)
	}
}

// TestCreateUnmarshalableHostClock: a host clock value that cannot be
// recorded is refused before anything is created.
func TestCreateUnmarshalableHostClock(t *testing.T) {
	for name, v := range map[string]any{"func": func() {}, "chan": make(chan int)} {
		dir := filepath.Join(t.TempDir(), "run")
		if _, err := Create(dir, v, false); err == nil {
			t.Errorf("%s: Create succeeded", name)
		}
		if _, err := os.Lstat(dir); err == nil {
			t.Errorf("%s: a run directory was created", name)
		}
	}
}

func TestDigest(t *testing.T) {
	a := Digest(map[string][]byte{"a": []byte("bc")})
	for name, files := range map[string]map[string][]byte{
		"moved boundary": {"ab": []byte("c")},
		"other content":  {"a": []byte("bd")},
		"extra file":     {"a": []byte("bc"), "b": nil},
		"length framing": {"a": []byte("2\x00bc")},
	} {
		if Digest(files) == a {
			t.Errorf("%s: same digest as {a: bc}", name)
		}
	}
	x := map[string][]byte{"x": []byte("1"), "y": []byte("2"), "z": []byte("3")}
	for range 20 { // map order is random
		if Digest(x) != Digest(map[string][]byte{"z": []byte("3"), "y": []byte("2"), "x": []byte("1")}) {
			t.Fatal("digest depends on map order")
		}
	}
	if len(a) != 64 {
		t.Errorf("digest %q is not 64 hex", a)
	}
}

func TestArchiveGenome(t *testing.T) {
	r := newRun(t)
	sum, err := r.ArchiveGenome("7", wrapperFiles)
	if err != nil {
		t.Fatal(err)
	}
	if sum != Digest(wrapperFiles) || !r.HasGenome("7") || r.HasGenome("8") || r.HasGenome("x") {
		t.Fatalf("archive digest %q or HasGenome wrong", sum)
	}
	got, gsum, err := r.Genome("7")
	if err != nil || gsum != sum || len(got) != 4 || string(got["server.go"]) != "// server\n" {
		t.Fatalf("Genome = %v, %q, %v", got, gsum, err)
	}
	if mode(t, filepath.Join(r.dir, genomesDir, "7", filesDir, "doc.go")) != 0o600 {
		t.Error("archived file is not 0600")
	}
	if _, err := r.ArchiveGenome("7", map[string][]byte{"doc.go": []byte("changed")}); !errors.Is(err, ErrExists) {
		t.Errorf("second archive of 7 = %v, want ErrExists", err)
	}
	if got, _, _ := r.Genome("7"); string(got["doc.go"]) != "package wrapper\n" {
		t.Error("an archived genome changed")
	}
	ents, _ := os.ReadDir(filepath.Join(r.dir, genomesDir))
	if len(ents) != 1 {
		t.Errorf("genomes has %d entries after a refused second archive, want 1", len(ents))
	}
}

func TestArchiveGenomeRefusals(t *testing.T) {
	big := make([]byte, MaxGenomeFile+1)
	many := map[string][]byte{}
	for i := range MaxGenomeFiles + 1 {
		many[fmt.Sprintf("f%d.go", i)] = nil
	}
	for _, tc := range []struct {
		name, id string
		files    map[string][]byte
	}{
		{"leading zero id", "07", wrapperFiles},
		{"path id", "../x", wrapperFiles},
		{"empty id", "", wrapperFiles},
		{"no files", "1", nil},
		{"traversal", "1", map[string][]byte{"../x.go": nil}},
		{"subdir", "1", map[string][]byte{"a/b.go": nil}},
		{"dot file", "1", map[string][]byte{".hidden": nil}},
		{"empty name", "1", map[string][]byte{"": nil}},
		{"too big", "1", map[string][]byte{"a.go": big}},
		{"too many", "1", many},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRun(t)
			if _, err := r.ArchiveGenome(tc.id, tc.files); err == nil {
				t.Fatal("ArchiveGenome succeeded")
			}
			if ents, _ := os.ReadDir(filepath.Join(r.dir, genomesDir)); len(ents) != 0 {
				t.Errorf("a refused archive left %d entries", len(ents))
			}
		})
	}
}

// TestGenomeTamper: an archived genome that changed on disk is refused.
func TestGenomeTamper(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(dir string) error
	}{
		{"edited file", func(d string) error {
			return os.WriteFile(filepath.Join(d, filesDir, "server.go"), []byte("evil"), 0o600)
		}},
		{"removed file", func(d string) error { return os.Remove(filepath.Join(d, filesDir, "tunables.go")) }},
		{"extra file", func(d string) error { return os.WriteFile(filepath.Join(d, filesDir, "x.go"), []byte("x"), 0o600) }},
		{"wrong digest", func(d string) error {
			return os.WriteFile(filepath.Join(d, digestFile), []byte(strings.Repeat("0", 64)+"\n"), 0o600)
		}},
		{"no digest", func(d string) error { return os.Remove(filepath.Join(d, digestFile)) }},
		{"symlink", func(d string) error {
			p := filepath.Join(d, filesDir, "server.go")
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.Symlink("/etc/hostname", p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRun(t)
			if _, err := r.ArchiveGenome("3", wrapperFiles); err != nil {
				t.Fatal(err)
			}
			if err := tc.tamper(filepath.Join(r.dir, genomesDir, "3")); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.Genome("3"); err == nil {
				t.Error("Genome accepted a tampered archive")
			}
		})
	}
	if _, _, err := newRun(t).Genome("9"); err == nil {
		t.Error("Genome of an unarchived id succeeded")
	}
}

func TestTranscript(t *testing.T) {
	r := newRun(t)
	raw := []byte(`{"type":"result","result":"ok"}`)
	if err := r.WriteTranscript("4", raw); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Transcript("4"); err != nil || string(got) != string(raw) {
		t.Fatalf("Transcript = %q, %v", got, err)
	}
	if mode(t, filepath.Join(r.dir, transcripts, "4.json")) != 0o600 {
		t.Error("transcript is not 0600")
	}
	if err := r.WriteTranscript("4", []byte("{}")); !errors.Is(err, ErrExists) {
		t.Errorf("second transcript 4 = %v, want ErrExists", err)
	}
	if err := r.WriteTranscript("x/y", raw); err == nil {
		t.Error("transcript id with a slash accepted")
	}
	if err := r.WriteTranscript("5", make([]byte, MaxTranscript+1)); err == nil {
		t.Error("over-cap transcript accepted")
	}
	if _, err := r.Transcript("6"); err == nil {
		t.Error("missing transcript read")
	}
	ents, _ := os.ReadDir(filepath.Join(r.dir, transcripts))
	if len(ents) != 1 {
		t.Errorf("transcripts has %d entries, want 1 (no temp files)", len(ents))
	}
}

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.FixedZone("x", 3600))

func sha() string { return strings.Repeat("a", 64) }

func passed(id, parent string) Attempt {
	return Attempt{
		GenerationID: id, ParentID: parent, Time: t0, Gate: Gate{Passed: true}, GenomeSHA256: sha(),
		Fitness: &Fitness{Score: 0.8, Baseline: &Half{SuccessRate: 0.99, P95LatencyMS: 120, CPUPerSuccess: 0.4}},
		Action:  ActionPromoted, SelectionMode: ModeTournament, EverLive: true, CurrentlyLive: true,
		Usage: Usage{NumTurns: 5, InputTokens: 1000, OutputTokens: 200, DurationMS: 30000, CostUSD: 0.12}, Rationale: "raised the cap",
	}
}

func failed(id string) Attempt {
	return Attempt{
		GenerationID: id, ParentID: "1", Time: t0, Gate: Gate{FailStage: 4, FailCheck: "race"},
		Action: ActionDiscarded, SelectionMode: ModeTournament,
	}
}

func TestAttempts(t *testing.T) {
	r := newRun(t)
	if got, err := r.Attempts(); err != nil || len(got) != 0 {
		t.Fatalf("empty run: %v, %v", got, err)
	}
	for _, a := range []Attempt{passed("1", ""), failed("2"), passed("3", "1")} {
		if err := r.AppendAttempt(a); err != nil {
			t.Fatalf("AppendAttempt %s: %v", a.GenerationID, err)
		}
	}
	// A later record supersedes: variant 1 is culled.
	culled := passed("1", "")
	culled.CurrentlyLive = false
	culled.Action = ActionArchived
	if err := r.AppendAttempt(culled); err != nil {
		t.Fatal(err)
	}
	got, err := r.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].GenerationID != "1" || got[1].GenerationID != "2" || got[2].GenerationID != "3" {
		t.Fatalf("Attempts order = %+v", got)
	}
	if got[0].CurrentlyLive || got[0].Action != ActionArchived || !got[0].EverLive {
		t.Errorf("record 1 = %+v, want the superseding record", got[0])
	}
	if !got[0].Time.Equal(t0) || got[0].Time.Location() != time.UTC {
		t.Errorf("time %v, want %v in UTC", got[0].Time, t0)
	}
	if got[1].Gate.FailStage != 4 || got[1].Gate.FailCheck != "race" || got[1].Fitness != nil {
		t.Errorf("record 2 = %+v", got[1])
	}
	if got[2].Fitness == nil || got[2].Fitness.Baseline.P95LatencyMS != 120 || got[2].Fitness.Stress != nil || got[2].ParentID != "1" {
		t.Errorf("record 3 = %+v", got[2])
	}
	r2, err := Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := r2.Attempts(); err != nil || len(again) != 3 {
		t.Errorf("reopened: %d, %v", len(again), err)
	}
}

func TestAttemptRefusals(t *testing.T) {
	mut := func(base Attempt, f func(a *Attempt)) Attempt { f(&base); return base }
	nan := 0.0
	nan = nan / nan
	for name, a := range map[string]Attempt{
		"bad id":              mut(passed("1", ""), func(a *Attempt) { a.GenerationID = "01" }),
		"bad parent":          mut(passed("1", ""), func(a *Attempt) { a.ParentID = "x" }),
		"no time":             mut(passed("1", ""), func(a *Attempt) { a.Time = time.Time{} }),
		"bad action":          mut(passed("1", ""), func(a *Attempt) { a.Action = "kept" }),
		"bad mode":            mut(passed("1", ""), func(a *Attempt) { a.SelectionMode = "" }),
		"long rationale":      mut(passed("1", ""), func(a *Attempt) { a.Rationale = strings.Repeat("x", MaxRationale+1) }),
		"free text reason":    mut(passed("1", ""), func(a *Attempt) { a.Disqualified = "Request body: secret" }),
		"disq promoted":       mut(passed("1", ""), func(a *Attempt) { a.Disqualified = "verify_mismatch" }),
		"live never live":     mut(passed("1", ""), func(a *Attempt) { a.EverLive = false }),
		"negative usage":      mut(passed("1", ""), func(a *Attempt) { a.Usage.NumTurns = -1 }),
		"nan cost":            mut(passed("1", ""), func(a *Attempt) { a.Usage.CostUSD = nan }),
		"nan score":           mut(passed("1", ""), func(a *Attempt) { a.Fitness.Score = nan }),
		"passed with stage":   mut(passed("1", ""), func(a *Attempt) { a.Gate.FailStage = 2 }),
		"passed no digest":    mut(passed("1", ""), func(a *Attempt) { a.GenomeSHA256 = "" }),
		"passed bad digest":   mut(passed("1", ""), func(a *Attempt) { a.GenomeSHA256 = "ABC" }),
		"failed stage 0":      mut(failed("2"), func(a *Attempt) { a.Gate.FailStage = 0 }),
		"failed stage 6":      mut(failed("2"), func(a *Attempt) { a.Gate.FailStage = 6 }),
		"failed no check":     mut(failed("2"), func(a *Attempt) { a.Gate.FailCheck = "" }),
		"failed detail check": mut(failed("2"), func(a *Attempt) { a.Gate.FailCheck = "boot: panic: secret\nline" }),
		"failed promoted":     mut(failed("2"), func(a *Attempt) { a.Action = ActionPromoted }),
		"failed with fitness": mut(failed("2"), func(a *Attempt) { a.Fitness = &Fitness{} }),
		"failed live":         mut(failed("2"), func(a *Attempt) { a.EverLive = true }),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRun(t)
			if err := r.AppendAttempt(a); err == nil {
				t.Fatal("AppendAttempt accepted it")
			}
			if b, _ := os.ReadFile(filepath.Join(r.dir, attemptsFile)); len(b) != 0 {
				t.Errorf("a refused record wrote %q", b)
			}
		})
	}
}

func TestAttemptsCorrupt(t *testing.T) {
	for name, content := range map[string]string{
		"unknown field": `{"generation_id":"1","detail":"secret"}` + "\n",
		"not json":      "nonsense\n",
		"two values":    `{} {}` + "\n",
		"invalid":       `{"generation_id":"1"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRun(t)
			if err := os.WriteFile(filepath.Join(r.dir, attemptsFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := r.Attempts()
			if err == nil || !strings.Contains(err.Error(), "line 1") {
				t.Errorf("Attempts error = %v, want one naming line 1", err)
			}
		})
	}
}

func TestTornTail(t *testing.T) {
	r := newRun(t)
	if err := r.AppendAttempt(passed("1", "")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(r.dir, attemptsFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"generation_id":"2"`)
	f.Close()
	if _, err := Open(r.dir); err == nil || !strings.Contains(err.Error(), "torn") {
		t.Errorf("Open on a torn tail = %v", err)
	}
	if _, err := r.Attempts(); err == nil || !strings.Contains(err.Error(), "torn") {
		t.Errorf("Attempts on a torn tail = %v", err)
	}
}

func TestConcurrentAppends(t *testing.T) {
	r := newRun(t)
	const n = 64
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			a := passed(fmt.Sprint(i), "")
			a.Rationale = strings.Repeat("x", 4000)
			if err := r.AppendAttempt(a); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err := r.Attempts()
	if err != nil || len(got) != n {
		t.Fatalf("%d attempts, %v; want %d", len(got), err, n)
	}
}

// TestTranscriptConcurrent: of several writers of one id, exactly one
// wins and the file holds that writer's bytes.
func TestTranscriptConcurrent(t *testing.T) {
	r := newRun(t)
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = r.WriteTranscript("5", []byte(fmt.Sprintf("writer %d", i))) })
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = i
		case err == nil:
			t.Fatalf("writers %d and %d both succeeded", winner, i)
		case !errors.Is(err, ErrExists):
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if got, err := r.Transcript("5"); winner < 0 || err != nil || string(got) != fmt.Sprintf("writer %d", winner) {
		t.Fatalf("transcript %q, %v; winner %d", got, err, winner)
	}
	if ents, _ := os.ReadDir(filepath.Join(r.dir, transcripts)); len(ents) != 1 {
		t.Errorf("transcripts has %d entries, want 1", len(ents))
	}
}

// TestLimitsAccepted: the limits are inclusive.
func TestLimitsAccepted(t *testing.T) {
	r := newRun(t)
	files := map[string][]byte{"big.go": make([]byte, MaxGenomeFile)}
	for i := 1; i < MaxGenomeFiles; i++ {
		files[fmt.Sprintf("f%d.go", i)] = nil
	}
	if _, err := r.ArchiveGenome("999999999999999999", files); err != nil {
		t.Errorf("16 files with a MaxGenomeFile file and an 18-digit id: %v", err)
	}
	if _, _, err := r.Genome("999999999999999999"); err != nil {
		t.Errorf("reading it back: %v", err)
	}
	if _, err := r.ArchiveGenome("1000000000000000000", wrapperFiles); err == nil {
		t.Error("a 19-digit id was accepted")
	}
	if err := r.WriteTranscript("1", make([]byte, MaxTranscript)); err != nil {
		t.Errorf("a MaxTranscript transcript: %v", err)
	}
	tok40, tok41 := strings.Repeat("a", 40), strings.Repeat("a", 41)
	for stage := 1; stage <= 5; stage += 4 {
		a := failed("2")
		a.Gate.FailStage, a.Gate.FailCheck = stage, tok40
		a.Rationale = strings.Repeat("x", MaxRationale)
		if err := r.AppendAttempt(a); err != nil {
			t.Errorf("stage %d, a 40-character check, a MaxRationale rationale: %v", stage, err)
		}
	}
	a := failed("3")
	a.Gate.FailCheck = tok41
	if err := r.AppendAttempt(a); err == nil {
		t.Error("a 41-character check was accepted")
	}
	if got, err := r.Attempts(); err != nil || len(got) != 1 || got[0].Gate.FailStage != 5 {
		t.Errorf("Attempts = %+v, %v", got, err)
	}
}

func TestDisqualified(t *testing.T) {
	r := newRun(t)
	ok := passed("1", "")
	ok.Disqualified, ok.Action, ok.CurrentlyLive = "verify_mismatch", ActionArchived, false
	if err := r.AppendAttempt(ok); err != nil {
		t.Fatalf("a passed, archived, disqualified attempt: %v", err)
	}
	for name, mut := range map[string]func(a *Attempt){
		"promoted": func(a *Attempt) { a.Action = ActionPromoted },
		"live":     func(a *Attempt) { a.CurrentlyLive = true },
	} {
		a := ok
		mut(&a)
		if err := r.AppendAttempt(a); err == nil {
			t.Errorf("a disqualified attempt that is %s was accepted", name)
		}
	}
}

// TestAppendAfterTornTail: an append after a torn line is refused, not
// made to corrupt a second record.
func TestAppendAfterTornTail(t *testing.T) {
	r := newRun(t)
	f, err := os.OpenFile(filepath.Join(r.dir, attemptsFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"generation_id":"1"`)
	f.Close()
	if err := r.AppendAttempt(passed("2", "")); err == nil || !strings.Contains(err.Error(), "torn") {
		t.Errorf("AppendAttempt after a torn tail = %v", err)
	}
}

func TestGenomeSymlinkedDir(t *testing.T) {
	r := newRun(t)
	if _, err := r.ArchiveGenome("3", wrapperFiles); err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(r.dir, genomesDir, "3")
	other := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(filepath.Join(g, filesDir), other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(g, filesDir)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Genome("3"); err == nil {
		t.Error("Genome followed a symlinked files directory")
	}
}

func TestGenomeOversizedFile(t *testing.T) {
	r := newRun(t)
	if _, err := r.ArchiveGenome("3", wrapperFiles); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(r.dir, genomesDir, "3", filesDir, "server.go")
	if err := os.WriteFile(p, make([]byte, MaxGenomeFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Genome("3"); err == nil {
		t.Error("Genome read an oversized file")
	}
}

func noCandidate(id, why string) Attempt {
	return Attempt{
		GenerationID: id, ParentID: "1", Time: t0, Mutation: why, Action: ActionDiscarded, SelectionMode: ModeTournament,
		Usage: Usage{NumTurns: 30, OutputTokens: 900}, Rationale: "left it as is",
	}
}

func TestNoCandidate(t *testing.T) {
	r := newRun(t)
	for _, a := range []Attempt{noCandidate("2", MutationNoChange), noCandidate("3", MutationFailed)} {
		if err := r.AppendAttempt(a); err != nil {
			t.Fatalf("attempt %s (%s): %v", a.GenerationID, a.Mutation, err)
		}
	}
	got, err := r.Attempts()
	if err != nil || len(got) != 2 || got[0].Mutation != MutationNoChange || got[1].Mutation != MutationFailed {
		t.Fatalf("Attempts = %+v, %v", got, err)
	}
	mut := func(f func(a *Attempt)) Attempt { a := noCandidate("4", MutationFailed); f(&a); return a }
	for name, a := range map[string]Attempt{
		"unknown reason": mut(func(a *Attempt) { a.Mutation = "timeout" }),
		"gate result":    mut(func(a *Attempt) { a.Gate = Gate{FailStage: 1, FailCheck: "build"} }),
		"passed gate":    mut(func(a *Attempt) { a.Gate.Passed = true }),
		"promoted":       mut(func(a *Attempt) { a.Action = ActionPromoted }),
		"fitness":        mut(func(a *Attempt) { a.Fitness = &Fitness{} }),
		"live":           mut(func(a *Attempt) { a.EverLive = true }),
		"genome":         mut(func(a *Attempt) { a.GenomeSHA256 = sha() }),
		"disqualified":   mut(func(a *Attempt) { a.Disqualified = "salt_reused" }),
	} {
		if err := r.AppendAttempt(a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSummary(t *testing.T) {
	r := newRun(t)
	if _, err := r.ReadSummary(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadSummary of a running run: %v", err)
	}
	s := Summary{Reason: "usage_ceiling", Generations: 7, Usage: Usage{NumTurns: 120, InputTokens: 5}, Time: t0}
	if err := r.WriteSummary(s); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteSummary(s); !errors.Is(err, ErrExists) {
		t.Errorf("second WriteSummary: %v, want ErrExists", err)
	}
	got, err := r.ReadSummary()
	if err != nil || got.Reason != s.Reason || got.Generations != 7 || got.Usage != s.Usage || !got.Time.Equal(t0) {
		t.Errorf("ReadSummary = %+v, %v", got, err)
	}
	if mode(t, filepath.Join(r.dir, summaryFile)) != 0o600 {
		t.Error("summary.json is not 0600")
	}
	for name, bad := range map[string]Summary{
		"free text": {Reason: "fault: kubectl said x", Time: t0},
		"negative":  {Reason: "fault", Generations: -1, Time: t0},
		"no time":   {Reason: "fault"},
		"usage":     {Reason: "fault", Time: t0, Usage: Usage{OutputTokens: -1}},
	} {
		if err := newRun(t).WriteSummary(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
