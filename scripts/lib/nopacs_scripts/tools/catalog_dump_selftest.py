"""The self-test of catalog_dump.py: the SQLite tenant migrations of the
repository are applied to two fresh databases, the same catalog is written
into them in different orders (so their integer ids differ), and the dumps must
be equal; a changed label, UID, count or checksum must make them differ. When
NOPACS_TEST_POSTGRES names a Postgres server, the Postgres migrations are
applied to a scratch schema with psql, the same catalog is written there, and
that dump must equal SQLite's byte for byte; the schema is dropped after. Run
by `catalog_dump.py --self-test`.
"""

import glob
import os
import re
import shlex
import sqlite3
import subprocess
import sys
import tempfile
import uuid

from nopacs_scripts.paths import ROOT
from nopacs_scripts.report import Failure
from nopacs_scripts.tools import catalog_dump as cd

T0 = 1_790_000_000_000_001


class Ref:
    """The id of the row of table whose column col is value."""

    def __init__(self, table, col, value):
        self.table, self.col, self.value = table, col, value


def lit(dialect, v):
    if v is None:
        return "NULL"
    if isinstance(v, Ref):
        return "(SELECT id FROM %s WHERE %s = %s)" % (v.table, v.col, lit(dialect, v.value))
    if isinstance(v, bytes):
        return "X'%s'" % v.hex() if dialect == "sqlite" else "'\\x%s'::bytea" % v.hex()
    if isinstance(v, str):
        return "'%s'" % v.replace("'", "''")
    return str(v)


def when(dialect, us):
    return str(us) if dialect == "sqlite" else "('epoch'::timestamptz + interval '1 microsecond' * %d)" % us


def insert(dialect, table, row, times=()):
    names = list(row)
    values = [when(dialect, row[n]) if n in times else lit(dialect, row[n]) for n in names]
    return "INSERT INTO %s (%s) VALUES (%s);" % (table, ", ".join(names), ", ".join(values))


def model():
    """The catalog, as a list of independent groups (a subject with its
    sessions, scans and instances), so groups can be written in any order."""
    groups = []
    for s in range(3):
        subject = "Subj-%d" % s
        statements = [("subjects", {"label": subject, "folded": subject.lower(), "created": T0 + s,
                                    "session_count": 2, "instance_count": 4, "bytes": 4096, "updated": T0 + 99}, ("created", "updated"))]
        for q in range(2):
            study = "1.2.%d.%d" % (s, q)
            sess = "ses-%d" % q
            statements.append(("sessions", {"study_uid": study, "subject_id": Ref("subjects", "folded", subject.lower()), "label": sess,
                                            "folded": sess, "storage": "main", "created": T0 + 10 + q, "subject_label": subject,
                                            "subject_folded": subject.lower(), "study_date": "2024010%d" % q, "receiver": "rx",
                                            "scan_count": 1, "instance_count": 2, "bytes": 2048, "updated": T0 + 50}, ("created", "updated")))
            series = "1.3.%d.%d" % (s, q)
            statements.append(("scans", {"session_id": Ref("sessions", "study_uid", study), "series_uid": series, "label": "scan-1",
                                         "folded": "scan-1", "instances": 2, "bytes": 2048, "manifest_sha256": bytes([s, q]) * 16,
                                         "created": T0 + 20, "updated": T0 + 60, "subject_label": subject,
                                         "subject_folded": subject.lower(), "session_label": sess, "session_folded": sess,
                                         "storage": "main", "modality": "MR", "series_description": "T1 \u00e9",
                                         "description_folded": "t1 \u00e9", "series_number": None if q else 3, "receiver": "rx",
                                         "study_uid": study}, ("created", "updated")))
            statements.append(("session_modalities", {"session_id": Ref("sessions", "study_uid", study), "modality": "MR",
                                                      "created": T0 + 10 + q}, ("created",)))
            for i in range(2):
                statements.append(("instances", {"scan_id": Ref("scans", "series_uid", series),
                                                 "sop_instance_uid": "1.4.%d.%d.%d" % (s, q, i), "reception_id": "rcp-%d" % s,
                                                 "sha256": bytes([i]) * 32, "archived_sha256": None if i else bytes([9]) * 32,
                                                 "storage_key": "", "byte_offset": 0, "byte_length": 1024, "size": 1024,
                                                 "held_at": T0 + 70 + i}, ("held_at",)))
            statements.append(("reception_scans", {"reception_id": "rcp-%d" % s, "scan_id": Ref("scans", "series_uid", series),
                                                   "instances": 2, "bytes": 2048}, ()))
        groups.append(statements)
    return groups


def script(dialect, order, mutate=None):
    out = []
    for g in order:
        for table, row, times in model()[g]:
            if mutate:
                row = mutate(table, dict(row))
            out.append(insert(dialect, table, row, times))
    out.append("UPDATE catalog_totals SET subjects = 3, sessions = 6, scans = 6, instances = 12;")
    out.append("INSERT INTO storage_totals (storage, bytes) VALUES ('main', 12288);")
    return "\n".join(out)


def sqlite_db(path, order, mutate=None, pad=0):
    db = sqlite3.connect(path)
    for f in sorted(glob.glob(os.path.join(ROOT, "internal", "driver", "sqlite", "migrations", "tenant", "*.sql"))):
        with open(f, encoding="utf-8") as h:
            db.executescript(h.read())
    for i in range(pad):  # shift every id by rows later removed
        db.execute("INSERT INTO subjects (label, folded, created) VALUES (?, ?, 1)", ("pad%d" % i, "pad%d" % i))
        db.execute("DELETE FROM subjects WHERE folded = ?", ("pad%d" % i,))
    db.executescript(script("sqlite", order, mutate))
    db.commit()
    db.close()


def postgres_dump(dsn, order):
    schema = "catalog_dump_selftest_" + uuid.uuid4().hex[:10]
    psql = shlex.split(os.environ.get("PSQL", "psql"))

    def run(sql):
        """One script on psql's standard input, so a containerized psql reads it too."""
        res = subprocess.run(psql + ["-X", "-q", "-v", "ON_ERROR_STOP=1", "--dbname=" + dsn, "-f", "-"],
                             input=sql, capture_output=True, text=True)
        if res.returncode:
            raise Failure("self-test: psql failed: " + res.stderr.strip())

    use = "SET search_path TO %s;\n" % schema
    run("CREATE SCHEMA " + schema)
    try:
        for f in sorted(glob.glob(os.path.join(ROOT, "internal", "driver", "postgres", "migrations", "tenant", "*.sql"))):
            with open(f, encoding="utf-8") as h:
                run(use + h.read())
        run(use + script("postgres", order))
        return cd.canonical(cd.Postgres(dsn, schema))
    finally:
        run("DROP SCHEMA %s CASCADE" % schema)


def run():
    if not glob.glob(os.path.join(ROOT, "internal", "driver", "sqlite", "migrations", "tenant", "0001_*.sql")):
        print("catalog_dump self-test: the repository's migrations are not here", file=sys.stderr)
        return 1
    with tempfile.TemporaryDirectory() as tmp:
        a, b, c = (os.path.join(tmp, n) for n in ("a.db", "b.db", "c.db"))
        sqlite_db(a, [0, 1, 2])
        sqlite_db(b, [2, 0, 1], pad=5)
        da, db_ = cd.canonical(cd.Sqlite(a)), cd.canonical(cd.Sqlite(b))
        raw_a = sqlite3.connect(a).execute("SELECT id FROM scans ORDER BY series_uid").fetchall()
        raw_b = sqlite3.connect(b).execute("SELECT id FROM scans ORDER BY series_uid").fetchall()
        assert raw_a != raw_b, "the two databases must have different integer ids"
        assert da == db_, "the same catalog written in another order must dump the same"
        assert not re.search(r'"(id|subject_id|session_id|scan_id|profile_version_id)"', da), "an integer id is left"
        for table in cd.CATALOG:
            assert '"%s": [' % table in da, table
        print("self-test: same catalog, other order and ids -> equal dumps (%d lines, tables %s)"
              % (da.count("\n"), ", ".join(t for t in cd.CATALOG if '"%s"' % t in da)))

        for what, mutate in (("a scan label", lambda t, r: dict(r, label="scan-X") if t == "scans" and r["series_uid"] == "1.3.0.0" else r),
                             ("an instance checksum", lambda t, r: dict(r, sha256=b"\x07" * 32) if t == "instances" and r["sop_instance_uid"] == "1.4.2.1.1" else r),
                             ("a session count", lambda t, r: dict(r, scan_count=2) if t == "sessions" and r["study_uid"] == "1.2.1.1" else r),
                             ("a modality", lambda t, r: dict(r, modality="CT") if t == "session_modalities" and r["session_id"].value == "1.2.0.1" else r)):
            sqlite_db(c, [0, 1, 2], mutate)
            if cd.canonical(cd.Sqlite(c)) == da:
                raise Failure("self-test: a changed %s gives the same dump" % what)
            os.unlink(c)
            print("self-test: changed %-22s -> dump differs" % what)
        skipped = cd.canonical(cd.Sqlite(a), skip_times=True, skip_receptions=True)
        assert "created" not in skipped and "reception" not in skipped
        print("self-test: --skip-times and --skip-receptions leave those columns and table out")
        assert cd.main(["--sqlite", os.path.join(tmp, "missing.db")]) == 1
        print("self-test: a missing database exits 1")

        dsn = os.environ.get("NOPACS_TEST_POSTGRES")
        if dsn:
            pg = postgres_dump(dsn, [1, 2, 0])
            if pg != da:
                open(os.path.join(tmp, "pg.json"), "w").write(pg)
                open(os.path.join(tmp, "sqlite.json"), "w").write(da)
                diff = subprocess.run(["diff", os.path.join(tmp, "sqlite.json"), os.path.join(tmp, "pg.json")],
                                      capture_output=True, text=True).stdout
                raise Failure("self-test: Postgres dump differs from SQLite's:\n" + diff[:2000])
            print("self-test: Postgres (psql, scratch schema) dump equals SQLite's byte for byte")
        else:
            print("self-test: NOPACS_TEST_POSTGRES is not set; the Postgres comparison was skipped")
    print("catalog_dump self-test: all passed")
    return 0
