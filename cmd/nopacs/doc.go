// Command nopacs is the NoPACs command line: a client of the server's API and
// nothing else.
//
// The commands are built in internal/cli; tenant, key, grant, setting and
// audit with their verbs, whoami and version, and the rest, are calls of the
// API, reached on the server's local socket or over the network with a key,
// and cobra provides the help and the shell completion; see "nopacs --help".
// With no arguments, when input and output are a terminal, the interactive
// program opens (internal/tui). The commands that must touch the data
// directory with the server stopped (init, serve, backup, tenant import) are
// in cmd/nopacs-server; typed here, each answers with the command to run
// there. This program links no database or storage driver
// (TestNoServerCodeIsLinked).
//
// This package wires the parts of NoPACs together and does nothing else. It
// must never hold logic of its own, because what is only here cannot be tested
// without building the binary.
package main
