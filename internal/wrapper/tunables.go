package wrapper

import "github.com/codeinthecreek/eK8s/internal/classifier"

// The genome: every evolvable knob of this wrapper, as named constants.
// Generation-0 values are simple starting points, not tuned.
const (
	// perHashMemory estimates the memory one in-flight Hasher call
	// needs: Argon2id's 19 MiB plus a little headroom.
	perHashMemory int64 = 20 << 20

	// memoryFractionNum/memoryFractionDen is the share of the pod memory
	// limit budgeted for in-flight hashing; the rest is left for the Go
	// runtime, buffers and GC headroom.
	memoryFractionNum int64 = 1
	memoryFractionDen int64 = 2

	// acquireWait is how long a validated request waits for a hashing
	// slot before it is shed with 503. It is a fraction of the shared
	// hang-timeout, so a queued request can still finish in time.
	acquireWait = classifier.HangTimeout / 4

	// retryAfterSeconds is the Retry-After value sent with a 503 shed.
	retryAfterSeconds = 1
)
