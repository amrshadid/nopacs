//go:build race

package events

// raceDetector reports whether the tests were built with the race detector,
// which allows at most 8128 goroutines alive at once: the test of 10,000
// watches runs without it.
const raceDetector = true
