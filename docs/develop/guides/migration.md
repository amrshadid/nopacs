# Changing a schema

[Documentation index](../../README.md) · [Architecture](../architecture.md) · [Testing](../testing.md) · Guides: [a database driver](database-driver.md) · [a storage driver](storage-driver.md) · [an API method](api-method.md) · [an event type](event-type.md)

Each database (the master, a tenant) has one baseline schema per driver,
`0001_baseline.sql` in `internal/driver/sqlite/migrations/<master|tenant>/`
and `internal/driver/postgres/migrations/<master|tenant>/`: every table
written once, whole, after any table it refers to, with a comment saying
what it holds and why its constraints exist. In the master the grants refer
to the keys and the tenants by foreign key; no tenant table refers to
another, so the tenant's tables are grouped by what uses them, in the order
a reader meets them. The migration
runner (`internal/driver/sqlkit`) applies it as migration 1 and records its
SHA-256 in the database's `schema_migrations` table.

## Until the first release: edit the baseline

Nothing is released, so no installation has data to carry across a change.
A schema change edits the baseline in place, in both drivers:

1. **Change the table where it is written**, in the SQLite and the Postgres
   baseline alike, and keep its comment true. Text columns that hold
   identifiers use the `C` collation on Postgres; constraints on Postgres
   are named.
2. **Update the pinned checksums**: `TestShippedMigrationsUnchanged` (the
   master's, in `internal/driver/sqlite/master_test.go` and
   `internal/driver/postgres/postgres_test.go`) and
   `TestShippedTenantMigrationsUnchanged` (each driver's `tenant_test.go`)
   pin each baseline's SHA-256, so the edit is a deliberate one.
3. **Make every development database again.** The binary refuses to open a
   database whose recorded baseline differs from its own, by name or
   SHA-256 (`sqlkit.ErrEarlierBuild`): "the database was made by an earlier
   development build ... make it again". It is never migrated across and
   never half opened, and a backup or an export made with the server stopped
   refuses it too, before it writes anything. A backup or a tenant's export records the fingerprint
   of the baselines its sections were read from (`driver.SchemaFingerprints`),
   and a restore or an import refuses a file made from other baselines before
   it writes anything.
4. **Update the outside checks** if they name what changed: the checks of
   each database with its own tools (`make check-master-sqlite`,
   `check-tenant-sqlite`, `check-master-postgres`, `check-tenant-postgres`)
   and the proof's tenants section (`make prove`) list the tables they
   expect, and on Postgres the indexes and the `C`-collated columns.

## From the first release on: a forward migration

The released baseline is never edited again; its checksum stays pinned. A
change is a new file after it, forward only, applied once, never edited after
it is merged:

1. **Write it twice**, once per driver, as the next number:
   `<number>_<what>.sql`, numbered after the highest file there. Start the
   file with a comment saying why the change is needed.
2. **Keep it safe on a database in use**: a new column has a default or is
   nullable; an index on a large table is created so it does not block
   writes for long; data is moved in the same migration only when it is small.
3. **Record its checksum** in the same tests, after the baseline's, so a
   merged migration that is edited fails them (`sqlkit.ErrChecksum` at open).
4. **Update the outside checks** as for a change of the baseline (step 4
   above): `make check-master-sqlite`, `check-tenant-sqlite`,
   `check-master-postgres`, `check-tenant-postgres` and the proof's tenants
   section list tables, indexes and collations by name.
5. **Test it against the runner's rules**: each driver's `migrate_test.go`
   shows a migration is applied once, a changed file, another baseline and a
   newer database are refused, and the lock holds across processes. A
   migration that moves data is tested on a database of the schema before it,
   with rows in it; from the first release a sample database is kept for each
   release, and every later binary migrates it.
