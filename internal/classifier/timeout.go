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
