// Package fault gives an error a kind: what sort of failure it is, so that the
// API can answer with one code and the command line can exit with one status
// for it, whichever package the error came from.
//
// An error made here carries two texts. Its message is written for the caller
// and is safe to show one: it says what failed in the archive's own words. The
// error it wraps, when there is one, is for the log only, because it may name
// a file, a database or an address the caller has no business seeing. Message
// returns the first; Error returns both, for the log.
//
// An error with no kind is Internal: a failure nobody classified is treated as
// the archive's own fault, never as the caller's, so it is not hidden as a
// refusal and not reported to the caller in detail.
//
// This package must never import another package of NoPACs', and never log
// or print: every other package depends on it, and it decides nothing but the
// shape of an error. The API code of a kind is mapped in the API package, so
// that this package does not depend on the RPC library.
package fault
