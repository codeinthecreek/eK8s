package variant

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden from the current templates")

var valid = Params{ID: "7", Label: "ek8s-0123456789abcdef", Image: "ek8s-wrapper:0123456789ab"}

// TestValidate covers both sides of each shape check: each case changes
// one field of a valid Params.
func TestValidate(t *testing.T) {
	tests := []struct {
		name  string // starts with the field it changes: id, label or image
		value string
		ok    bool
	}{
		{"id 0", "0", true},
		{"id 1", "1", true},
		{"id 18 digits", "123456789012345678", true},
		{"id empty", "", false},
		{"id 19 digits", "1234567890123456789", false},
		{"id leading zero", "07", false},
		{"id negative", "-1", false},
		{"id plus", "+1", false},
		{"id hex", "0x1", false},
		{"id letters", "abc", false},
		{"id space", " 1", false},
		{"id trailing newline", "1\n", false},
		{"id yaml injection", "1\n  labels: {x: y}", false},
		{"id unicode digit", "١", false},

		{"label valid", "ek8s-fedcba9876543210", true},
		{"label gate prefix", "gate-0123456789abcdef", false},
		{"label uppercase hex", "ek8s-0123456789ABCDEF", false},
		{"label 15 hex", "ek8s-0123456789abcde", false},
		{"label 17 hex", "ek8s-0123456789abcdef0", false},
		{"label non-hex", "ek8s-0123456789abcdeg", false},
		{"label empty", "", false},
		{"label trailing newline", "ek8s-0123456789abcdef\n", false},
		{"label prefix only", "ek8s-", false},

		{"image valid", "ek8s-wrapper:fedcba987654", true},
		{"image 11 hex", "ek8s-wrapper:0123456789a", false},
		{"image 13 hex", "ek8s-wrapper:0123456789abc", false},
		{"image uppercase", "ek8s-wrapper:0123456789AB", false},
		{"image other repo", "busybox:0123456789ab", false},
		{"image registry", "docker.io/ek8s-wrapper:0123456789ab", false},
		{"image digest", "ek8s-wrapper@sha256:0123456789ab", false},
		{"image latest", "ek8s-wrapper:latest", false},
		{"image no tag", "ek8s-wrapper", false},
		{"image empty", "", false},
		{"image trailing newline", "ek8s-wrapper:0123456789ab\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid
			switch {
			case strings.HasPrefix(tt.name, "id"):
				p.ID = tt.value
			case strings.HasPrefix(tt.name, "label"):
				p.Label = tt.value
			case strings.HasPrefix(tt.name, "image"):
				p.Image = tt.value
			default:
				t.Fatalf("case name must start with id, label or image")
			}
			err := p.Validate()
			if (err == nil) != tt.ok {
				t.Errorf("Validate(%+v) = %v, want ok %v", p, err, tt.ok)
			}
			out, rerr := Render(p)
			if (rerr == nil) != tt.ok {
				t.Errorf("Render(%+v) error = %v, want ok %v", p, rerr, tt.ok)
			}
			if rerr != nil && out != nil {
				t.Errorf("Render returned output with an error")
			}
		})
	}
}

// TestRenderGolden compares the bundle with testdata. Run with -update
// after an intended template change and review the golden diff.
func TestRenderGolden(t *testing.T) {
	tests := []struct {
		golden string
		p      Params
	}{
		{"bundle-7.golden", valid},
		{"bundle-1234.golden", Params{ID: "1234", Label: "ek8s-fedcba9876543210", Image: "ek8s-wrapper:fedcba987654"}},
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

// TestRenderShape checks what the golden files only show: one document
// per template, namespace first, every value substituted and each document
// in the variant's namespace.
func TestRenderShape(t *testing.T) {
	out, err := Render(valid)
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(string(out), "---\n")
	wantKinds := []string{"Namespace", "LimitRange", "NetworkPolicy", "Deployment", "Service", "HorizontalPodAutoscaler"}
	if len(docs) != len(wantKinds) {
		t.Fatalf("got %d documents, want %d", len(docs), len(wantKinds))
	}
	for i, doc := range docs {
		if !strings.Contains(doc, "\nkind: "+wantKinds[i]+"\n") {
			t.Errorf("document %d is not kind %s:\n%s", i, wantKinds[i], doc)
		}
		ns := "  namespace: ek8s-v7\n"
		if i == 0 {
			ns = "  name: ek8s-v7\n"
		}
		if !strings.Contains(doc, ns) {
			t.Errorf("document %d (%s) lacks %q", i, wantKinds[i], ns)
		}
	}
	for _, want := range []string{
		"variant: ek8s-0123456789abcdef\n",
		"image: ek8s-wrapper:0123456789ab\n",
		"pod-security.kubernetes.io/enforce: restricted\n",
		// loadgen's egress policy (deploy/ek8s-system) selects on it.
		"ek8s.io/role: variant\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("bundle lacks %q", want)
		}
	}
	if strings.Contains(string(out), "\n  replicas:") {
		t.Errorf("Deployment has a replicas field; the HPA owns the count")
	}
	if strings.Contains(string(out), "<no value>") {
		t.Errorf("bundle has an unsubstituted value")
	}
}
