// Package ascii holds the classes of ASCII text the server's rules are
// written in, each stated once, so the settings and a tenant's store read a
// value people type (an endpoint's URL, a key's id) by the same rule.
//
// # What this package must never do
//
// It must never do I/O or import anything of NoPACs': it is part of the
// core (.golangci.yml, depguard "core"), which every package may import.
package ascii
