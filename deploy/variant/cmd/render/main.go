// Command render prints one variant's Kubernetes bundle (package variant)
// to stdout, for kubectl apply -f - by hand and in the deploy smoke test;
// the orchestrator calls package variant directly.
//
// Usage, from the repo root:
//
//	go run ./deploy/variant/cmd/render -id 1 -label ek8s-<16 hex> -image ek8s-wrapper:<12 hex>
//
// Exit status: 0 rendered, 2 bad usage or a value of the wrong shape.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/codeinthecreek/eK8s/deploy/variant"
)

func main() {
	var p variant.Params
	flag.StringVar(&p.ID, "id", "", "variant id (namespace ek8s-v<id>)")
	flag.StringVar(&p.Label, "label", "", "variant label, ek8s-<16 hex>")
	flag.StringVar(&p.Image, "image", "", "image, ek8s-wrapper:<12 hex>, already kind-loaded")
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	out, err := variant.Render(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(2)
	}
	os.Stdout.Write(out)
}
