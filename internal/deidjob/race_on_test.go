//go:build race

package deidjob

// raceOn says the race detector is on, whose own bookkeeping grows the
// heap, so a measure of memory means nothing under it.
const raceOn = true
