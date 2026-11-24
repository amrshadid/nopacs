//go:build !race

package check

// raceEnabled says the race detector is on, whose own memory hides the
// heap's, so a test that measures what the check holds leaves itself out.
const raceEnabled = false
