//go:build !race

package receive

// raceEnabled says the race detector is on, so a scale test leaves itself
// out and the crash matrix runs fewer rounds.
const raceEnabled = false
