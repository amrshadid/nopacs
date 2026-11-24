#!/usr/bin/env python3
"""Dump a tenant's catalog from its database into one canonical JSON, so two
catalogs (the live one and one rebuilt from the storages) are compared with
`diff`.

    catalog_dump.py --sqlite TENANT.db [--out FILE] [--skip-times] [--skip-receptions]
    catalog_dump.py --postgres DSN [--schema NAME] [--out FILE] [...]
    catalog_dump.py --self-test

The catalog is the subjects, sessions, scans and instances the archive keeps
(subjects, sessions, scans, instances), the lists derived from them
(session_modalities) and the running totals (catalog_totals, storage_totals,
reception_scans). A table the database does not have is left out. Every
column is dumped, so a column a later version adds is compared too. Rows are
matched by what names them and not by integer ids, which differ between two
databases: the database's own id columns (and catalog_totals' one-row key) are dropped, and a reference by id
becomes the label or UID it points at (subject_id becomes "subject", the
subject's label; session_id becomes "session", the Study Instance UID; scan_id
becomes "scan", the Series Instance UID; profile_version_id becomes "profile",
name@version). Rows are sorted by their JSON text, one row a line, keys
sorted, so equal catalogs give equal bytes. Byte columns are lowercase hex;
times are whole microseconds since 1970-01-01T00:00:00Z in either database; the
output says nothing of which database it came from, so a SQLite catalog and a
Postgres one compare too.

--skip-times drops every time column (created, updated, held_at, claimed_at,
latest_created): a rebuilt catalog is made at another time. --skip-receptions
drops reception_id and the reception_scans table: a rebuild has no reception.
The database is opened read-only; SQLite through the sqlite3 module, Postgres
through the psql command ($PSQL, else psql, and it may carry arguments such as
a container's exec; give the password in $PGPASSWORD, not in the DSN, which a
process list shows).

Exit status 0 with the JSON on standard output (or in --out); 1 for a database
that cannot be read; 2 for a mistake in the command line.
"""

import argparse
import json
import os
import shlex
import sqlite3
import subprocess
import sys
import urllib.parse

CATALOG = ("catalog_totals", "instances", "reception_scans", "scans", "session_modalities", "sessions",
           "storage_totals", "subjects")
# The tables read to resolve a reference, not dumped.
LOOKUP = ("profile_versions",)
# The column that holds catalog_totals to one row, named differently by the two databases.
SINGLETON_KEYS = ("one", "only_row")
TIME_COLUMNS =("created", "updated", "held_at", "claimed_at", "latest_created")


class Unreadable(Exception):
    """The database cannot be read."""


class Sqlite:
    """A SQLite database, read-only."""

    def __init__(self, path):
        if not os.path.exists(path):
            raise Unreadable("no such file: %s" % path)
        try:
            self.db = sqlite3.connect("file:%s?mode=ro" % urllib.parse.quote(os.path.abspath(path)), uri=True)
        except sqlite3.Error as e:
            raise Unreadable(str(e))

    def table(self, name):
        """(columns, rows) of a table, or None when it does not exist."""
        try:
            cur = self.db.execute("SELECT * FROM %s" % name)
        except sqlite3.OperationalError as e:
            if "no such table" in str(e):
                return None
            raise Unreadable(str(e))
        columns = [d[0] for d in cur.description]
        return columns, [[hex_of(v) for v in row] for row in cur.fetchall()]


def hex_of(v):
    return v.hex() if isinstance(v, (bytes, memoryview)) else v


class Postgres:
    """A Postgres database, read through psql."""

    def __init__(self, dsn, schema=None):
        self.dsn, self.schema = dsn, schema
        # PSQL may be a command with arguments, such as one that runs psql in a container.
        self.psql = shlex.split(os.environ.get("PSQL", "psql"))

    def query(self, sql):
        if self.schema:
            sql = 'SET search_path TO "%s"; %s' % (self.schema.replace('"', '""'), sql)
        try:
            run = subprocess.run(self.psql + ["-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "--dbname=" + self.dsn,
                                              "-c", sql], capture_output=True, text=True)
        except OSError as e:
            raise Unreadable("cannot run psql: %s" % e)
        if run.returncode != 0:
            raise Unreadable("psql: %s" % run.stderr.strip())
        return run.stdout

    def table(self, name):
        cols = json.loads(self.query(
            "SELECT coalesce(json_agg(json_build_array(column_name, data_type) ORDER BY ordinal_position), '[]'::json) "
            "FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = '%s'" % name))
        if not cols:
            return None
        select = []
        for column, kind in cols:
            ident = '"%s"' % column
            if kind == "bytea":
                select.append("encode(%s, 'hex') AS %s" % (ident, ident))
            elif kind == "timestamp with time zone":
                select.append("(extract(epoch FROM %s)::numeric * 1000000)::bigint AS %s" % (ident, ident))
            else:
                select.append(ident)
        out = self.query("SELECT coalesce(json_agg(json_build_array(%s)), '[]'::json) FROM %s"
                         % (", ".join('t."%s"' % c for c, _ in cols), "(SELECT %s FROM %s) AS t" % (", ".join(select), name)))
        return [c for c, _ in cols], json.loads(out)


def canonical(source, skip_times=False, skip_receptions=False):
    """The catalog of source as the canonical text."""
    found = {}
    for name in CATALOG + LOOKUP:
        if skip_receptions and name == "reception_scans":
            continue
        got = source.table(name)
        if got is not None:
            found[name] = got
    as_dicts = {name: [dict(zip(cols, row)) for row in rows] for name, (cols, rows) in found.items()}
    subjects = {r["id"]: r["label"] for r in as_dicts.get("subjects", [])}
    sessions = {r["id"]: r["study_uid"] for r in as_dicts.get("sessions", [])}
    scans = {r["id"]: r["series_uid"] for r in as_dicts.get("scans", [])}
    profiles = {r["id"]: "%s@%d" % (r["name"], r["version"]) for r in as_dicts.get("profile_versions", [])}
    refs = {"subject_id": ("subject", subjects), "session_id": ("session", sessions), "scan_id": ("scan", scans),
            "profile_version_id": ("profile", profiles)}
    out = {}
    for name in CATALOG:
        if name not in as_dicts:
            continue
        rows = []
        for row in as_dicts[name]:
            new = {}
            for column, value in row.items():
                if column == "id" or (name == "catalog_totals" and column in SINGLETON_KEYS) or (skip_times and column in TIME_COLUMNS) or (skip_receptions and column == "reception_id"):
                    continue
                if column in refs:
                    label, table = refs[column]
                    if value is not None and value not in table:
                        raise Unreadable("%s.%s = %r points at no row" % (name, column, value))
                    new[label] = None if value is None else table[value]
                else:
                    new[column] = value
            rows.append(json.dumps(new, sort_keys=True, ensure_ascii=False))
        out[name] = sorted(rows)
    lines = ["{"]
    for i, name in enumerate(sorted(out)):
        lines.append(' "%s": [' % name)
        lines.append(",\n".join("  " + r for r in out[name]))
        lines.append(" ]" + ("," if i < len(out) - 1 else ""))
    lines.append("}")
    return "\n".join(l for l in lines if l != "") + "\n"


def main(argv):
    p = argparse.ArgumentParser(description="Dump a tenant catalog into a canonical JSON for diff; "
                                "see this tool's docstring.")
    p.add_argument("--sqlite", help="a tenant SQLite database file")
    p.add_argument("--postgres", help="a Postgres DSN (password in $PGPASSWORD)")
    p.add_argument("--schema", help="the Postgres schema holding the tenant's tables (default: the DSN's search path)")
    p.add_argument("--out", help="write the JSON here instead of standard output")
    p.add_argument("--skip-times", action="store_true", help="leave out every time column")
    p.add_argument("--skip-receptions", action="store_true", help="leave out reception ids and the reception_scans table")
    p.add_argument("--self-test", action="store_true", help="check the tool against catalogs it builds")
    args = p.parse_args(argv)
    if args.self_test:
        sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
        import catalog_dump_selftest
        return catalog_dump_selftest.run()
    if bool(args.sqlite) == bool(args.postgres):
        p.error("exactly one of --sqlite and --postgres")
    try:
        source = Sqlite(args.sqlite) if args.sqlite else Postgres(args.postgres, args.schema)
        text = canonical(source, args.skip_times, args.skip_receptions)
    except Unreadable as e:
        print("catalog_dump: %s" % e, file=sys.stderr)
        return 1
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(text)
    else:
        sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
