// Package uid decides whether a value is a well-formed DICOM unique
// identifier (PS3.5 9.1), before it is used as the identity of a session,
// a scan or an instance, or as the name of a file in a storage key.
//
// It is a package of its own because the naming rule, the layout of keys and
// the manifest all take UIDs from outside, and all must refuse the same
// malformed ones; a check written in each would drift.
//
// A UID that passes is also a valid label (package label): digits and dots
// only, never starting or ending with a dot, at most 64 bytes, and never a
// Windows device name, so a key may use it as a path segment unchanged.
//
// # What this package must never do
//
// It must never do I/O, read the clock, or import anything of NoPACs'
// outside the core (.golangci.yml, depguard "core"). It must never trim or
// repair a value: the padding byte a UI value carries on the wire (a single
// trailing NUL, PS3.5 6.2) is removed by whoever decodes the data set, and
// anything that reaches this package with it is refused, so two spellings of
// one UID never both pass.
package uid
