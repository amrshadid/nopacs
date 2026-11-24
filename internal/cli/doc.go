// Package cli is the command line of NoPACs: the commands that operate an
// installation through its API, built with cobra.
//
// Every command is a client of the API and nothing else. It parses its
// arguments, makes one call (or pages through one list), and writes the
// answer as a table for a person or as one JSON document for a program. It
// holds no rule the server also holds: what a name may be, which role a grant
// may give and who may do what are the server's to say, so a command that
// disagreed with the server could never be right. Only what a client must do
// to keep a person from harm lives here: refusing to send a key over plain
// http, refusing a secret as an argument, and requiring --yes before a
// removal.
//
// The commands that must touch the data directory with no server running
// (init, serve, backup, tenant import) are not here: they belong to the
// nopacs-server program (internal/servecmd), and a hidden stand-in for each
// answers with the command to run there.
//
// The grammar is "nopacs <noun> <verb> [name] [flags]". Exit codes come from
// the kind of the error through fault.Kind.ExitCode: 0 done, 1 failed, 2 wrong
// usage, 3 refused, 4 not found, 5 a state that forbids the change (change it
// first), 6 a position that can no longer be answered (start over), 7 a bound
// of the server reached (send less, or later).
//
// This package must never take a secret as an argument or a flag value, never
// seal, hash or store one (it asks internal/secret only whether a key file is
// private), never print one except the new key that key create
// exists to show once, never write to the terminal text it got from the
// network without cleaning it, and never reach into a database or a storage:
// the API is the only way in.
package cli
