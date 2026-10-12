//go:build !race

package gate

// raceEnabled reports whether the race detector is on; the Stage 4
// seeded-failure tests skip under it.
const raceEnabled = false
