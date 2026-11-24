// Package interrupt ends the server at a named durable step, in a build made
// for testing alone, so an outside harness can kill the real binary exactly
// where a crash is worst and check what it left from outside.
//
// A server built with the build tag "interrupt" reads the environment
// variable NOPACS_INTERRUPT_AT once, at its start. When it names a step, the
// process exits with status 86 the first time that step's durable write is
// done: os.Exit, so no deferred function runs, no transaction is rolled back
// by the process and no file is closed by it, as a power cut would leave it.
// Before exiting it writes one line, "interrupt: stopped at <step>", to
// standard error. A name the build does not know stops the server at its
// start, so a misspelled step never lets a run pass without the kill.
//
// Built without the tag, At is an empty function the compiler removes at
// every call: the variable is never read and nothing of this package is in
// the binary. The step names and what is durable at each are listed in one
// place, the steps table of on.go.
//
// Why a build tag and not a seam: the steps live in packages whose own tests
// already stop them through unexported seams (export_test.go), but an
// outside harness runs the built binary, which no test seam reaches. A tag
// keeps the kill out of every binary that is shipped, which a flag or an
// environment variable read by the normal build could not.
package interrupt

// Env is the environment variable naming the step to stop at.
const Env = "NOPACS_INTERRUPT_AT"

// Status is the exit status of a process stopped at a step: one no other
// path of the server exits with, so a harness tells the kill from a failure.
const Status = 86
