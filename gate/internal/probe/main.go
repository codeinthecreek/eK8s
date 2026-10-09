// Command probe is the gate's sandbox preflight stand-in for a candidate:
// fixed, gate-built code that the gate boots under the same bwrap argv,
// nsenter join and relay as a candidate, before any candidate boots, to
// prove the host can run that path at all. Like cmd/service, it listens
// on :8080 and answers GET /healthz with 200 "ok" and X-EK8s-Variant set
// from EK8S_VARIANT; it serves nothing else. Stdlib only.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	label := os.Getenv("EK8S_VARIANT")
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-EK8s-Variant", label)
		io.WriteString(w, "ok")
	})
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}
