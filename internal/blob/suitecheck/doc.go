// Package suitecheck tests the storage contract suite itself: that a
// correct in-memory driver passes package blobtest, and that a driver
// breaking any one promise of blob.Store fails it. It holds no code but
// its tests. It is a package of its own because checking the suite means
// running it in a child process, which the suite's package, kept free of
// anything that touches files or processes, may not do.
//
// # What this package must never do
//
// It must never be imported, and never hold a real driver: its drivers are
// in memory and wrong on purpose, except one.
package suitecheck
