// Package tenanttest is the contract suite of tenant.Store: the tests every
// database driver must pass for a tenant's database. A driver's own test
// calls Run with a Driver, whose functions open fresh, migrated stores; Run
// is the suite's one entry point, so a driver runs all of it. A driver that
// passes is interchangeable with every other, because what callers rely on is
// pinned here and not in any one driver's tests.
//
// It is an ordinary package, not a _test file, so that each driver's tests,
// in their own packages, can import it.
//
// # What this package must never do
//
// It must never import a driver, database/sql or package sqlkit: a suite that
// knew which database it ran on could be bent to pass on that one. It speaks
// only through tenant.Store. A test changed to make one driver pass is a
// change to the contract, said so in the pull request that makes it. It never
// holds a real secret or real patient data.
package tenanttest
