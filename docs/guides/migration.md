# Adding a migration

[Documentation index](../README.md) · [Architecture](../architecture.md) · [Testing](../testing.md) · Guides: [a database driver](database-driver.md) · [a storage driver](storage-driver.md) · [an API method](api-method.md) · [an event type](event-type.md)

A database's schema changes only by a new migration: a SQL file, forward only,
applied once, never edited after it is merged.

1. **Write it twice**, once per driver, as the next number in
   `internal/driver/sqlite/migrations/<master|tenant>/` and
   `internal/driver/postgres/migrations/<master|tenant>/`: `<number>_<what>.sql`,
   numbered after the highest file there.
   Start the file with a comment saying why the change is needed. Text columns
   that hold identifiers use the `C` collation on Postgres.
2. **Keep it safe on a database in use**: a new column has a default or is
   nullable; an index on a large table is created so it does not block
   writes for long; data is moved in the same migration only when it is small.
3. **Record its checksum**: each driver lists every shipped migration with
   its checksum, the tenant's in `tenant_test.go` and the master's in
   `master_test.go` (SQLite) or `postgres_test.go` (Postgres), so a merged
   migration that is edited fails the tests.
4. **Update the outside checks**: `scripts/check/tenant-sqlite.sh`,
   `tenant-postgres.sh` (and the master's) and the proof's
   `scripts/prove/03-tenants.sh` list the tables, the number of migrations,
   and on Postgres the indexes and the `C`-collated columns.
5. **Test it against the runner's rules**: each driver's `migrate_test.go`
   shows a migration is applied once, a changed file and a newer database are
   refused, and the lock holds across processes. No test yet opens a
   database made by an earlier release, with rows in it, and migrates it:
   that comes with the first release, from a sample database kept for each
   release. Until then, test such a migration by hand on a copy.
