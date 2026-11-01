// Command render prints loadgen's Deployment (package deploy/loadgen) to
// stdout, for kubectl apply -f - by hand and in the deploy script.
//
// Usage, from the repo root:
//
//	go run ./deploy/loadgen/cmd/render -rate 2 -image ek8s-loadgen:<12 hex>
//
// Exit status: 0 rendered, 2 bad usage or a value that fails validation.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/codeinthecreek/eK8s/deploy/loadgen"
)

func main() {
	var p loadgen.Params
	flag.StringVar(&p.Rate, "rate", "", "r: each window stream's sends per second, shortest decimal form (2.5, not 2.50)")
	flag.StringVar(&p.Image, "image", "", "image, ek8s-loadgen:<12 hex>, already kind-loaded")
	flag.StringVar(&p.CPURequest, "cpu-request", "", "loadgen's CPU request: 1 (default) or 1m to 1000m (100m at CI size); the limit is always 1")
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	out, err := loadgen.Render(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(2)
	}
	os.Stdout.Write(out)
}
