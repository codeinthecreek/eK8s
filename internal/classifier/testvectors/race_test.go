//go:build race

package testvectors

// raceEnabled reports whether the race detector is on; the Argon2-heavy
// tests skip under it.
const raceEnabled = true
