//go:build !race

package registry

// raceDetector reports whether the tests were built with the race detector,
// which the 1,000-tenant test leaves to a run of its own (make test-scale).
const raceDetector = false
