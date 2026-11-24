//go:build race

package postgres

// raceDetector reports whether the tests were built with the race detector,
// which the tests at millions of rows leave to a run of their own: under the
// detector they would measure the detector.
const raceDetector = true
