package classifier

import "time"

// HangTimeout is the shared hang-timeout and latency-normalization
// ceiling (DESIGN.md sec 8, sec 9): a response slower than this
// classifies as failed, and it is the upper bound of the fitness
// function's latency normalization. The wrapper imports it to bound its
// own request-timeout behavior, so the two can never drift apart.
//
// The value is a starting guess pending real telemetry (DESIGN.md
// sec 15), not a derived number.
const HangTimeout = 2 * time.Second

// MaxBodyBytes is the most a 2xx response body may hold (DESIGN.md
// sec 8): a 2xx /hash or /verify body longer than this classifies as
// malformed_body. Callers read at most MaxBodyBytes+1 bytes of any body
// and pass them as Observation.Body with Err nil when they hit that
// cap; Err is for failed reads only. Non-2xx bodies are not
// size-checked, and a Negative's 2xx is negative_accepted whatever its
// body.
const MaxBodyBytes = 4 << 10
