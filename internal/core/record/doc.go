// Package record is the form of the three small records a storage holds
// beside the scans' trees, so that the catalog, its holds and the right to
// write can be found again from the storage alone, without the tenant's
// database:
//
//	meta/.corrections/<operation id>.json   what a correction does and how far it got
//	meta/.holds/<hold id>.json              a hold, what it covers, and whether it holds
//	meta/.owner.json                        the installation that may write the tenant's objects
//
// A correction (a delete, a relabel, a replace) is made and committed in the
// database under an operation id before anything is written to a storage; the
// record on each storage it touches says which correction that is, what it
// targets by labels and UIDs (a delete's level, and each scan's series and
// session's study, so a restore knows which later arrivals are of what was
// removed), and its state. A record names no instance
// beyond what an instance delete or a replace needs, and never carries the
// reason: only its SHA-256, so the reason stays in the audit entry of the
// installation that made it.
//
// The owner record names the installation and the generation allowed to
// write a tenant's objects. A server that finds another installation, or a
// higher generation than its own database holds, stops writing.
//
// A hold record is written before its hold's row commits and rewritten
// when the hold is released or a relabel moves what it names; like a
// correction record it carries the reason's SHA-256, never the reason.
//
// Every record is JSON, written whole, read within a bound, and refused
// when they hold an unknown field, a trailing byte, or a value that breaks a
// rule below. Keys are built and parsed by package layout.
//
// # What this package must never do
//
// It must never do I/O beyond the reader it is given, read the clock, or
// import anything of NoPACs' outside the core (.golangci.yml, depguard
// "core"). It must never accept a record it would not write.
package record
