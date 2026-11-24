// Package blobtest is the contract suite of blob.Store: the tests every
// storage driver must pass. A driver's own test calls Run with a function
// that opens a fresh, empty store; a driver that passes is interchangeable
// with every other, because what callers rely on is pinned here and not in
// any one driver's tests.
//
// It is an ordinary package, not a _test file, so that each driver's tests,
// in their own packages, can import it.
//
// The suite assumes no file system: it never looks behind the Store at a
// file, a directory or a request. What only some storages can know, such as
// the free space of a volume, a driver declares in Driver, and the suite
// holds it to what it declared. A driver's own tests check what lies behind
// the port (files on a disk, the requests to a bucket), and how it survives
// a crash.
//
// # What this package must never do
//
// It must never import a driver, the os package's file functions or a cloud
// SDK: a suite that knew which storage it ran on could be bent to pass on
// that one. A test changed to make one driver pass is a change to the
// contract, said so in the pull request that makes it. It never holds real
// patient data: every object it writes is generated.
package blobtest
