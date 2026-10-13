// Package variant renders one variant's Kubernetes bundle (DESIGN.md
// sec 11): namespace ek8s-v<id> (Pod Security restricted), LimitRange,
// NetworkPolicy, Deployment hasher, Service hasher and HPA, from the
// embedded *.yaml.tmpl files in file-name order.
//
// Every value is checked against a fixed shape before rendering, so
// nothing a caller passes can add YAML structure to the bundle.
package variant

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"text/template"
)

//go:embed *.yaml.tmpl
var templateFS embed.FS

// Params are the per-variant values. All three are required.
type Params struct {
	// ID is the variant id, a decimal number without leading zeros; the
	// namespace is ek8s-v<ID>.
	ID string
	// Label is the variant label, ek8s-<16 hex>, random per deployment
	// (DESIGN.md sec 5). It names no variant id.
	Label string
	// Image is the candidate image the gate built, ek8s-wrapper:<12 hex>
	// (gate/docker.go imageTag), already loaded into the kind node.
	Image string
}

var (
	// At most 18 digits: fits an int64, and ek8s-v<id> stays far under
	// the 63-character namespace name limit.
	idRE    = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})$`)
	labelRE = regexp.MustCompile(`^ek8s-[0-9a-f]{16}$`)
	imageRE = regexp.MustCompile(`^ek8s-wrapper:[0-9a-f]{12}$`)
)

// Validate reports the first value that does not match its shape.
func (p Params) Validate() error {
	switch {
	case !idRE.MatchString(p.ID):
		return fmt.Errorf("variant id %q: want a decimal number without leading zeros, at most 18 digits", p.ID)
	case !labelRE.MatchString(p.Label):
		return fmt.Errorf("variant label %q: want ek8s-<16 lowercase hex>", p.Label)
	case !imageRE.MatchString(p.Image):
		return fmt.Errorf("variant image %q: want ek8s-wrapper:<12 lowercase hex>", p.Image)
	}
	return nil
}

// Namespace is the variant's namespace, ek8s-v<ID>.
func (p Params) Namespace() string { return "ek8s-v" + p.ID }

var templates = template.Must(
	template.New("").Option("missingkey=error").ParseFS(templateFS, "*.yaml.tmpl"))

// Render validates p and returns the bundle as one multi-document YAML
// stream, the namespace first, for kubectl apply -f -.
func Render(p Params) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	names, err := fs.Glob(templateFS, "*.yaml.tmpl") // sorted
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for i, name := range names {
		if i > 0 {
			out.WriteString("---\n")
		}
		if err := templates.ExecuteTemplate(&out, name, p); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}
