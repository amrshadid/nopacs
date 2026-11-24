//go:build race

package engine

// raceEnabled says the race detector is on, which slows matching some
// twenty times, so a test that measures the work of patterns leaves
// itself out.
const raceEnabled = true
