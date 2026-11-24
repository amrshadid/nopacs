// Command nopacs-server is the server of a NoPACs installation and the
// commands that must touch its data directory while the server is stopped.
//
// init makes an installation, serve runs its server, backup creates, checks
// and restores a backup, and tenant import loads a tenant another
// installation exported; they are built in internal/servecmd. Everything else
// is done through the server's API with the nopacs command line
// (cmd/nopacs), which links no database or storage driver. Both programs are
// built and released together and print the same version.
//
// This package wires the parts of NoPACs together and does nothing else. It
// must never hold logic of its own, because what is only here cannot be tested
// without building the binary.
package main
