// Package pathname holds the rules of the names and paths a person writes
// that the server later uses as names of files and directories, or compares
// to tell whether two directories are one: the rule of a name (a tenant's,
// a storage's, a receiver's, a root's), the device names Windows reserves,
// the segments a file system on Windows changes behind the server's back,
// and whether two paths nest. Each rule is written here once, so the master,
// a tenant's store, the settings and the checks of where a storage may be
// cannot come to disagree about it.
//
// # What this package must never do
//
// It must never do I/O or import anything of NoPACs': it is part of the
// core (.golangci.yml, depguard "core"), so every package may import it,
// including the settings, which import nothing else. It works on the text it
// is given; where a path really leads, through symbolic links, is for the
// caller that may touch the file system to find out first.
package pathname
