package loadgen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden from the current template")

const validImage = "ek8s-loadgen:0123456789ab"

// TestRenderGolden compares the manifest with testdata. Run with -update
// after an intended template change and review the golden diff.
func TestRenderGolden(t *testing.T) {
	tests := []struct {
		golden string
		p      Params
	}{
		{"deployment-2.golden", Params{Rate: "2", Image: validImage}},
		{"deployment-2.5.golden", Params{Rate: "2.5", Image: "ek8s-loadgen:fedcba987654"}},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			got, err := Render(tt.p)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", tt.golden)
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Render(%+v) differs from %s; if intended, rerun with -update and review the diff", tt.p, path)
			}
		})
	}
}

// TestRenderRates checks the flag values the rates render to, and that
// the largest rate loadgen accepts renders.
func TestRenderRates(t *testing.T) {
	tests := []struct{ rate, burst string }{
		{"2", "8"},
		{"2.5", "10"},
		{"19", "76"},
	}
	for _, tt := range tests {
		t.Run(tt.rate, func(t *testing.T) {
			out, err := Render(Params{Rate: tt.rate, Image: validImage})
			if err != nil {
				t.Fatal(err)
			}
			if got := arg(t, out, "rate"); got != tt.rate {
				t.Errorf("-rate=%s, want %s", got, tt.rate)
			}
			if got := arg(t, out, "burst-rate"); got != tt.burst {
				t.Errorf("-burst-rate=%s, want %s", got, tt.burst)
			}
			if got := arg(t, out, "window"); got != "5m0s" {
				t.Errorf("-window=%s, want 5m0s", got)
			}
		})
	}
}

// TestRenderRefusals covers a rate that is not in canonical form and a
// rate loadgen's Config.Validate refuses; each case changes one field of
// a valid Params.
func TestRenderRefusals(t *testing.T) {
	tests := []struct {
		name  string // starts with the field it changes: rate or image
		value string
		want  string // a substring of the error
	}{
		{"rate trailing zero", "2.50", `want the shortest decimal form, "2.5"`},
		{"rate exponent", "1e1", `want the shortest decimal form, "10"`},
		{"rate leading zero", "02", `want the shortest decimal form, "2"`},
		{"rate plus", "+2", `want the shortest decimal form, "2"`},
		{"rate hex float", "0x1p1", `want the shortest decimal form, "2"`},
		{"rate space", " 2", "invalid syntax"},
		{"rate trailing newline", "2\n", "invalid syntax"},
		{"rate yaml injection", "2\n  x: y", "invalid syntax"},
		{"rate empty", "", "invalid syntax"},
		{"rate word", "abc", "invalid syntax"},
		// These are canonical, so Validate refuses them.
		{"rate zero", "0", "must both be positive"},
		{"rate negative", "-1", "must both be positive"},
		{"rate NaN", "NaN", "want 1 to"},
		{"rate Inf", "+Inf", "over the 19 the memory sum covers"},
		{"rate over maxRate", "20", "loadgen: rate 20 over the 19 the memory sum covers"},
		{"rate just over maxRate", "19.000000000000004", "over the 19 the memory sum covers"},

		{"image other repo", "busybox:0123456789ab", "ek8s-loadgen:<12 lowercase hex>"},
		{"image wrapper repo", "ek8s-wrapper:0123456789ab", "ek8s-loadgen:<12 lowercase hex>"},
		{"image registry", "docker.io/ek8s-loadgen:0123456789ab", "ek8s-loadgen:<12 lowercase hex>"},
		{"image digest", "ek8s-loadgen@sha256:0123456789ab", "ek8s-loadgen:<12 lowercase hex>"},
		{"image latest", "ek8s-loadgen:latest", "ek8s-loadgen:<12 lowercase hex>"},
		{"image no tag", "ek8s-loadgen", "ek8s-loadgen:<12 lowercase hex>"},
		{"image 11 hex", "ek8s-loadgen:0123456789a", "ek8s-loadgen:<12 lowercase hex>"},
		{"image 13 hex", "ek8s-loadgen:0123456789abc", "ek8s-loadgen:<12 lowercase hex>"},
		{"image uppercase", "ek8s-loadgen:0123456789AB", "ek8s-loadgen:<12 lowercase hex>"},
		{"image trailing newline", "ek8s-loadgen:0123456789ab\n", "ek8s-loadgen:<12 lowercase hex>"},
		{"image empty", "", "ek8s-loadgen:<12 lowercase hex>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Params{Rate: "2", Image: validImage}
			switch {
			case strings.HasPrefix(tt.name, "rate"):
				p.Rate = tt.value
			case strings.HasPrefix(tt.name, "image"):
				p.Image = tt.value
			default:
				t.Fatalf("case name must start with rate or image")
			}
			out, err := Render(p)
			if err == nil {
				t.Fatalf("Render(%+v) succeeded:\n%s", p, out)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Render(%+v) error = %q, want it to contain %q", p, err, tt.want)
			}
			if out != nil {
				t.Errorf("Render returned output with an error")
			}
			if verr := p.Validate(); verr == nil || verr.Error() != err.Error() {
				t.Errorf("Validate(%+v) = %v, want the Render error %q", p, verr, err)
			}
		})
	}
}

// TestRenderManifest checks what the golden files only show: the fields
// the rest of the deployment relies on (DESIGN.md sec 10, 11). Like
// deploy/variant's tests, it checks the text, not a parsed document.
func TestRenderManifest(t *testing.T) {
	out, err := Render(Params{Rate: "2", Image: validImage})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	// One port, the control API's: the NetworkPolicy and the control
	// Service select on it, and loadgen listens on no other.
	if n := strings.Count(s, "containerPort:"); n != 1 {
		t.Errorf("%d containerPort fields, want 1", n)
	}
	if n := strings.Count(s, "ports:"); n != 1 {
		t.Errorf("%d ports lists, want 1", n)
	}
	for _, want := range []string{"- name: control\n", "containerPort: 9090\n", "protocol: TCP\n"} {
		if strings.Count(s, want) != 1 {
			t.Errorf("manifest lacks exactly one %q", want)
		}
	}

	// A termination grace period over cmd/loadgen's 10s shutdown.
	const field = "terminationGracePeriodSeconds: "
	i := strings.Index(s, field)
	if i < 0 || strings.Count(s, field) != 1 {
		t.Fatalf("manifest lacks exactly one %q", field)
	}
	secs, _, _ := strings.Cut(s[i+len(field):], "\n")
	if n, err := strconv.Atoi(secs); err != nil || n <= 10 {
		t.Errorf("%s%s, want a number over 10", field, secs)
	}

	for _, want := range []string{
		"  strategy:\n    type: Recreate\n",
		"      enableServiceLinks: false\n",
		"      automountServiceAccountToken: false\n",
		"  replicas: 1\n",
		"  namespace: ek8s-system\n",
		"image: " + validImage + "\n",
		"imagePullPolicy: Never\n",
	} {
		if strings.Count(s, want) != 1 {
			t.Errorf("manifest lacks exactly one %q", want)
		}
	}
	// Guaranteed at the workstation size: 1 CPU and 384Mi, requests =
	// limits, so an exclusive core under the static CPU manager.
	for _, want := range []string{"cpu: 1\n", "memory: 384Mi\n"} {
		if n := strings.Count(s, want); n != 2 {
			t.Errorf("manifest has %d %q, want 2 (request and limit)", n, want)
		}
	}
	// CI size lowers the request only.
	ci, err := Render(Params{Rate: "2", Image: validImage, CPURequest: "100m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ci), "requests:\n            cpu: 100m\n") || strings.Count(string(ci), "cpu: 1\n") != 1 {
		t.Errorf("CI-size manifest: want request 100m and limit 1:\n%s", ci)
	}
	for _, bad := range []string{"0", "1.5", "-1", "1Gi", "m", "0m"} {
		if _, err := Render(Params{Rate: "2", Image: validImage, CPURequest: bad}); err == nil {
			t.Errorf("CPURequest %q accepted", bad)
		}
	}
	// The NetworkPolicy and the control Service select app=loadgen.
	if n := strings.Count(s, "app: loadgen\n"); n != 3 {
		t.Errorf("%d app: loadgen labels, want 3 (metadata, selector, pod template)", n)
	}
	if strings.Contains(s, "livenessProbe") {
		t.Errorf("manifest has a livenessProbe; a restart would lose every unread record")
	}
	if strings.Contains(s, "<no value>") {
		t.Errorf("manifest has an unsubstituted value")
	}

	// The canary address is expanded by the kubelet, not rendered.
	if got, want := arg(t, out, "canary-addr"), "$(KUBERNETES_SERVICE_HOST):$(KUBERNETES_SERVICE_PORT)"; got != want {
		t.Errorf("-canary-addr=%s, want %s", got, want)
	}
	if got, want := arg(t, out, "target-url"), "http://hasher.ek8s-v{id}.svc.cluster.local:8080"; got != want {
		t.Errorf("-target-url=%s, want %s", got, want)
	}
}

// arg returns the value of the one -name=value argument in the rendered
// manifest's args list.
func arg(t *testing.T, manifest []byte, name string) string {
	t.Helper()
	prefix := "- -" + name + "="
	var found []string
	for _, line := range strings.Split(string(manifest), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d -%s arguments, want 1: %q", len(found), name, found)
	}
	return found[0]
}
