// Package loadgen renders loadgen's Deployment (DESIGN.md sec 10) in
// namespace ek8s-system from the embedded deployment.yaml.tmpl.
//
// Every value is checked against a fixed shape before rendering, so
// nothing a caller passes can add YAML structure to the manifest. The
// rate is checked by loadgen's own Config.Validate, so loadgen's limits
// (its maxRate and its burst size cap) reach the renderer; the renderer
// has no rate limit of its own.
package loadgen

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"text/template"
	"time"

	lg "github.com/codeinthecreek/eK8s/loadgen"
)

//go:embed deployment.yaml.tmpl
var deploymentTmpl string

// W is the window length loadgen runs with. The PR #45 W entry
// (docs/decisions.md) sets it; the memory sum behind loadgen's maxRate
// assumes it.
const W = 5 * time.Minute

// burstFactor is how many times r the burst rate b is: b = 4r.
const burstFactor = 4

// targetURL is loadgen's -target-url: a variant's Service, where {id} is
// the variant id.
const targetURL = "http://hasher.ek8s-v{id}.svc.cluster.local:8080"

// standInCanary is the canary address Validate is given: the manifest
// leaves -canary-addr to the kubelet, so Validate would otherwise see no
// address. An IP literal host:port, from the TEST-NET-1 documentation
// range (RFC 5737).
const standInCanary = "192.0.2.1:443"

// defaultCPURequest is loadgen's CPU request and limit, one whole CPU: an
// integer CPU on a Guaranteed pod is an exclusive core under the static CPU
// manager (docs/decisions.md, PR A r3a-static-cpu; DESIGN.md sec 10).
const defaultCPURequest = "1"

// Params are the Deployment's values. Rate and Image are required.
type Params struct {
	// Rate is r, each window stream's sends per second, in the shortest
	// decimal form that parses back to it (strconv 'g', -1), e.g. 2 or
	// 2.5, not 2.50 or 1e1.
	Rate string
	// Image is the loadgen image, ek8s-loadgen:<12 hex>, already loaded
	// into the kind node.
	Image string
	// CPURequest is loadgen's CPU request, "1" when empty; the limit is
	// always 1 CPU. CI size lowers the request only (deploy.yml,
	// EK8S_LOADGEN_CPU_REQUEST), e.g. "100m": the 2-CPU runner cannot
	// request a whole core, and there is no static CPU manager there.
	CPURequest string
}

// cpuRequestRE is a whole number of CPUs or of millicores.
var cpuRequestRE = regexp.MustCompile(`^[1-9][0-9]*m?$`)

// imageRE is the image shape, adapted from deploy/variant's: the
// repository ek8s-loadgen and a 12-character lowercase hex tag.
var imageRE = regexp.MustCompile(`^ek8s-loadgen:[0-9a-f]{12}$`)

var tmpl = template.Must(
	template.New("deployment.yaml.tmpl").Option("missingkey=error").Parse(deploymentTmpl))

// values are what the template reads, all already formatted.
type values struct {
	Image      string
	Rate       string
	Window     string
	BurstRate  string
	TargetURL  string
	CPURequest string
}

// Validate reports the first value that does not match its shape, or the
// error loadgen's Config.Validate gives for the Config the manifest
// would start.
func (p Params) Validate() error {
	_, err := p.values()
	return err
}

// values checks p and returns the formatted template values.
func (p Params) values() (values, error) {
	if !imageRE.MatchString(p.Image) {
		return values{}, fmt.Errorf("loadgen image %q: want ek8s-loadgen:<12 lowercase hex>", p.Image)
	}
	cpu := p.CPURequest
	if cpu == "" {
		cpu = defaultCPURequest
	}
	if !cpuRequestRE.MatchString(cpu) {
		return values{}, fmt.Errorf("loadgen cpu request %q: want whole CPUs (1) or millicores (100m)", cpu)
	}
	r, err := strconv.ParseFloat(p.Rate, 64)
	if err != nil {
		return values{}, fmt.Errorf("loadgen rate %q: %w", p.Rate, err)
	}
	if got := formatFloat(r); got != p.Rate {
		return values{}, fmt.Errorf("loadgen rate %q: want the shortest decimal form, %q", p.Rate, got)
	}
	cfg := lg.Config{
		Rate:       r,
		Window:     W,
		BurstRate:  burstFactor * r,
		TargetURL:  targetURL,
		CanaryAddr: standInCanary,
	}
	if err := cfg.Validate(); err != nil {
		return values{}, err
	}
	return values{
		Image:      p.Image,
		Rate:       formatFloat(cfg.Rate),
		Window:     cfg.Window.String(),
		BurstRate:  formatFloat(cfg.BurstRate),
		TargetURL:  cfg.TargetURL,
		CPURequest: cpu,
	}, nil
}

// formatFloat is the form the flags are written in.
func formatFloat(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }

// Render validates p and returns the Deployment as one YAML document, for
// kubectl apply -f -.
func Render(p Params) ([]byte, error) {
	v, err := p.values()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
