// Package mastertest is the contract suite of master.Store: the tests every
// database driver must pass, and the benchmarks every driver reports. A
// driver's own test calls Run with a function that opens a fresh, migrated
// store; a driver that passes is interchangeable with every other, because
// what callers rely on is pinned here and not in any one driver's tests.
//
// It is an ordinary package, not a _test file, so that each driver's tests,
// in their own packages, can import it.
//
// # What this package must never do
//
// It must never import a driver, database/sql or package sqlkit: a suite that
// knew which database it ran on could be bent to pass on that one. It speaks
// only through master.Store. A test changed to make one driver pass is a
// change to the contract, said so in the pull request that makes it.
//
// It must never hold a real secret. The sample's key secret is a constant
// made for the outside check, which proves that only its hash reaches the
// database.
package mastertest
