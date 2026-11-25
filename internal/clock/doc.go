// Package clock is the server's notion of "now" for what it records and
// decides by the calendar: a time written to a catalog, a grace that ends, a
// key that expires, a retention that falls due. A proof that must see a
// purge after thirty days, or a record kept for ten years, moves this time
// forward in a build made for testing alone, without waiting.
//
// A server built with the build tag "clock" reads two environment variables
// once, at its start. NOPACS_CLOCK_OFFSET is a fixed offset added to the
// system's time ("720h", "400d", "-36h"; a number of days takes a "d"). With
// NOPACS_CLOCK_FILE naming a file, the offset is read from that file (the
// same forms) at most every 200 ms, so a proof moves the time of a running
// server by rewriting the file; a file that does not exist yet is an offset of
// zero, and a file whose text is not an offset ends the process with status 2
// so a misspelled value never lets a run pass on the real time. The file wins
// over the variable once it exists. Only the points in time move: waits,
// timers and the deadlines of connections stay on the system's clock, so a
// jump of a year does not make a server wait a year or cut every connection.
//
// Built without the tag, Now is time.Now and the variables are never read:
// nothing of the offset is in the binary. Why a build tag and not a seam: the
// code that needs the moved time is the server's own wiring, built the same
// with the tag or without it, and an outside harness runs the built binary,
// which no test seam reaches; a flag or a variable read by the normal build
// would let a shipped server be told another time.
package clock

import "time"

// EnvOffset is the variable naming a fixed offset.
const EnvOffset = "NOPACS_CLOCK_OFFSET"

// EnvFile is the variable naming the file the offset is read from while the
// server runs.
const EnvFile = "NOPACS_CLOCK_FILE"

// System is the clock of the server for a package whose configuration asks
// for a Clock with Now and After: Now is this package's Now and After waits
// on the system's time.
type System struct{}

// Now returns the server's time, Now of this package.
func (System) Now() time.Time { return Now() }

// After returns time.After(d): a wait is a length of real time.
func (System) After(d time.Duration) <-chan time.Time { return time.After(d) }
