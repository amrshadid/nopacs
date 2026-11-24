# Choosing a database

[Documentation index](../README.md) · [Running NoPACs](running.md) · [Backup, restore and moving a tenant](backup.md) · [Settings](settings.md)

NoPACs keeps its records in two kinds of database, and each can be SQLite or
Postgres:

- the **master database**, one for the installation: tenants, keys (as hashes),
  grants, settings and the installation's audit log;
- a **tenant database** for each tenant: its storages, receivers, senders and
  profiles, its receptions, quarantine and jobs, the catalog of subjects, sessions
  and scans, its events and its audit log.

The archived DICOM files are in none of them: they are in the tenant's storages
(a directory or an S3 bucket).

## The choice

| | SQLite | Postgres |
|---|---|---|
| For | development, tests and small single-site trials | **production** |
| Needs | nothing: it is in the program | a Postgres server you run and back up |
| Writers | one: a second process on the same file is safe for the data but may wait and fail as busy | many |
| Where | a local disk only (not a network file system) | anywhere the server can reach |
| A tenant is | one file in `<data-dir>/tenants/` | a database or schema of its own |
| Size | the ceiling has not been measured and published; a tenant of ten million instances has been tried and its file was about 2 GB | tried to ten million instances per tenant |

SQLite is the default so that a first trial needs nothing. Anything that holds data
others depend on uses Postgres. Both pass the same contract tests on every change,
so no behavior is meant to differ; the drivers are chosen when a database is opened
and nothing else in the code knows which one it is talking to.

## How to choose each

```sh
# the master, in Postgres: an address in an environment variable, never an argument
MASTER_DB='postgres://...' nopacs-server init --data-dir /var/lib/nopacs --master-env MASTER_DB
MASTER_DB='postgres://...' nopacs-server serve --data-dir /var/lib/nopacs --master-env MASTER_DB

# a tenant, in Postgres
nopacs tenant create lab2 --db-env LAB2_DB
nopacs tenant create lab3 --db-file /etc/nopacs/lab3.dsn
```

Name the same `--master-env` every time you start the server, and for `backup` and
`restore`. A tenant's address is sealed with the installation's key file; the master's
is not stored at all, which is why the server must be told where it is each time.
The server refuses a master that is not the one the installation was made with (a
mistyped address to an empty database, or to another installation's), and changes
nothing.

Use TLS to the database (`sslmode=verify-full` in the address) when it is not on the
same host.

## Moving from SQLite to Postgres

There is no in-place conversion. The way across is a backup and a restore of the
installation, or a move of one tenant:

- `nopacs-server backup create`, then `nopacs-server backup restore --master-env VAR`
  into an empty Postgres database ([backup](backup.md)). A backup is made from either
  database in one logical format for both.
- for one tenant, `nopacs tenant export` and `nopacs-server tenant import --db-env VAR`
  loads it into an empty Postgres schema on the target.

**Weak:** a restore of a SQLite backup into Postgres has not yet been proven by a
test run of its own; the format is the same, but do it on a copy and read what the
restore prints before you rely on it.

## What is not decided here

Postgres version support, sizing and tuning, and replication of the database are the
site's. NoPACs needs a database that keeps what it was told to keep: take backups of it
as you do of any database, as well as the `backup` command, which holds the
configuration and not the catalog (which is rebuilt from the storages).
