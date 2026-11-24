//go:build !race

package sqlite

// raceDetector reports whether the tests were built with the race detector,
// which the tests at millions of rows leave to a run of their own (make
// test-scale): under the detector they would measure the detector.
const raceDetector = false
