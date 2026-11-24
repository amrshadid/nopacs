// Package driver opens a database from its address, choosing the database
// type by the address's scheme: "sqlite:<path>" or "postgres://...". It is
// the one place that knows the list of database types, so the rest of the
// binary asks for a master.Store and never names one, and a database type is
// added as a package below this one without changing its callers. For tenants
// it gives one opener per type (SQLiteTenants), which the tenant registry is
// handed.
//
// # What this package must never do
//
// It must never hold the logic of a database type: each type is a package
// below this one, and this package only picks it. It never repeats an
// address in an error, since an address may hold a password.
package driver
