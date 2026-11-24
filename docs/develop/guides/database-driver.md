# Adding a database driver

[Documentation index](../../README.md) · [Architecture](../architecture.md) · [Testing](../testing.md) · Guides: [a storage driver](storage-driver.md) · [an API method](api-method.md) · [a migration](migration.md) · [an event type](event-type.md)

A database driver implements both database ports, `master.Store` (the
installation's database) and `tenant.Pipeline` (one tenant's), for one
database. SQLite and Postgres are the two today; read them side by side, they
have the same files.

1. **Make the package** `internal/driver/<name>`, with the same files as the
   others: `open.go` (opening an address), `base.go` (the pool and failure
   classification), `write.go` (the write transaction), `migrate.go`,
   `master.go` and `master_*.go`, `tenant.go` and `pipeline_*.go`,
   `values.go`, `shared.go`, and the embedded `migrations/master/*.sql` and
   `migrations/tenant/*.sql`.
2. **Write the schema as a baseline**, `0001_baseline.sql` for the master and
   for a tenant, the same tables, columns and constraints as the other
   drivers' in this database's types, run by `internal/driver/sqlkit`'s
   migration runner under a lock that holds across processes; later changes
   are forward-only files after it. See [changing a schema](migration.md). The contract of each port is in [master](../../../internal/master/doc.go) and [tenant](../../../internal/tenant/doc.go).
3. **Keep the domain out of the driver.** A rule of what may be stored
   (a label's form, a state's moves, a bound) belongs in the port
   (`internal/master`, `internal/tenant`); a helper both SQL drivers need goes
   in `internal/driver/sqlkit`. The driver turns operations into SQL and its
   errors into fault kinds.
4. **Pass the contract suites**: call `mastertest.Run` and
   `mastertest.RunShared`, and `tenanttest.Run` with a `tenanttest.Driver`
   (its `Open` gives each test a database of its own; its `OpenPair` gives
   two stores on one database, as two processes would). The tenant suite
   has that one entry point, so a driver runs all of it or none. Every test
   must pass; a skip needs its reason in the suite, not in the driver.
5. **Register the address scheme** in `internal/driver/open.go`, so
   `OpenMaster` and the tenants' opener choose the driver by the address's
   prefix.
6. **Allow the database library in lint**: add the driver's package to the
   rule that keeps its library to it (as `postgres-library` does for pgx) in
   `.golangci.yml`; no other package may import it.
7. **Add the outside checks**: a script under `scripts/check` that reads the
   tables the driver wrote with the database's own tool (as `master-postgres.sh`
   does with psql and pg_dump), and a CI job that runs the driver's tests
   against a real server.
8. **Prove it**: the proof's tenants on the new database
   (`scripts/prove/03-tenants.sh`, `scripts/prove/reception.py`), and the
   benchmark's backends (`internal/receive/bench_test.go`).
