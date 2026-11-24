//go:build !race

package postgres

// raceDetector reports whether the tests were built with the race detector.
const raceDetector = false
