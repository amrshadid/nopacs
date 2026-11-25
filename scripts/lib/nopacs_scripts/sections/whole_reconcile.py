"""The group of the proof of keeping the archive whole that reconciles
databases a site restored by its own point-in-time recovery: the SQLite
databases copied while the server runs by the sqlite3 program's .backup
(SQLite's online backup API), then a delete and a key revocation, then the
copies put back with the server stopped and nopacs-server backup restore
--reconcile-only run on the installation in place. Each outcome is read
with tools that are not NoPACs: sqlite3, curl, catalog_dump.py, os.walk and
the event feed read by Python's standard library.

  reconcile  the delete made after the copy applied (its subject hidden, the
             correction unresolved for an admin), every key refused (curl
             401) until the local operator reinstates it, the catalog equal
             to the dump taken before the copy was put back and to the
             storage's manifests, a cursor from before refused with
             EVENT_HISTORY_CHANGED
"""

import filecmp
import json
import os
import subprocess

from nopacs_scripts.db import query
from nopacs_scripts.sections.whole import row_of, scene
from nopacs_scripts.sections.whole_host import events, history
from nopacs_scripts.wholemore import error_text


def databases(p):
    """The installation's SQLite databases: the master and the tenant's."""
    return [os.path.join(p.data, "master.db"), p.catalog()]


def online_copy(db, out):
    """Copies a database while the server writes it, by the sqlite3 program's
    .backup, which SQLite's online backup API makes consistent."""
    subprocess.run(["sqlite3", db, ".backup '%s'" % out], check=True, capture_output=True, timeout=120)


def put_back(db, copy):
    """Puts a copy in place of a database, the server stopped, removing the
    files SQLite keeps beside the database it replaces."""
    for side in (db + "-wal", db + "-shm"):
        if os.path.exists(side):
            os.remove(side)
    subprocess.run(["cp", copy, db], check=True)


def in_manifests(root):
    """The SOP Instance UIDs every manifest on the storage lists (os.walk)."""
    out = set()
    for d, _, names in os.walk(os.path.join(root, "meta")):
        if "manifest.json" in names:
            with open(os.path.join(d, "manifest.json")) as f:
                out |= {e["sop_instance_uid"] for e in json.load(f)["files"]}
    return out


def group_reconcile(p):
    p.setup()
    s = scene(p, deid=False)
    root = p.storage_root()
    second = p.m("key", "create", "--holder", "second holder", "--no-expiry").splitlines()[0].strip()
    second_id = second.split("_")[1]
    p.m("grant", "set", second_id, "--role", "reader", "--tenant", "lab")
    _, evs, alive, _ = events(p, {"scope": {"installation": {}}, "resume": True})
    mine = [e for e in evs if e.get("tenant") == "lab"]
    old_cursor = mine[-1]["cursor"] if mine else ""
    _, old_history = history(p, alive)

    # The site's copy, taken while the server runs.
    copies = []
    for db in databases(p):
        out = os.path.join(p.work, os.path.basename(db) + ".copy")
        online_copy(db, out)
        copies.append(out)
    be = row_of(p, s.uids["be1"][0])
    subject = be["subject"]
    cid = p.mj("correction", "delete", "lab", subject, "--reason", "consent withdrawn", "--yes")["correction"]["id"]
    p.m("key", "revoke", second_id)
    live = p.dump("live.json")
    p.stop()

    for db, copy in zip(databases(p), copies):
        put_back(db, copy)
    rows = query(p.catalog(), "SELECT count(*) AS n FROM subjects WHERE label = ?", subject)[0]["n"]
    revoked = query(os.path.join(p.data, "master.db"), "SELECT revoked, suspended FROM api_keys WHERE id = ?", second_id)
    corr = query(p.catalog(), "SELECT count(*) AS n FROM corrections WHERE id = ?", cid)[0]["n"]
    p.result("11 the copy put back is the installation before the delete and the revocation (sqlite3)", rows > 0 and corr == 0 and revoked and
             revoked[0]["revoked"] is None and revoked[0]["suspended"] is None,
             "rows of the deleted subject %d, rows of its correction %d, the revoked key's revoked %s, suspended %s" %
             (rows, corr, revoked[0]["revoked"] if revoked else "missing", revoked[0]["suspended"] if revoked else "missing"))

    r = p.run("backup", "restore", "--reconcile-only")
    report = r.stdout + r.stderr
    print("     the reconcile reported: %s" % " | ".join(x.strip() for x in report.strip().splitlines() if x.strip())[:1800], flush=True)
    p.result("11 backup restore --reconcile-only reconciles the installation in place, reading no backup", r.returncode == 0 and
             "Reconciled installation" in report and "No backup was read" in report and "1 with no row in the restored database" in report,
             "exit %d; %s" % (r.returncode, " ".join(report.split())[:200]))
    p.serve()
    code = p.curl("/tenants/lab/instances/x/y/z/w")[0]
    code2 = p.curl("/tenants/lab/instances/x/y/z/w", key=second)[0]
    p.result("11 after the reconcile every key is refused: curl answers 401 for the admin's and for the one revoked after the copy", code == 401 and code2 == 401,
             "the admin key HTTP %d; the key revoked after the copy HTTP %d" % (code, code2))
    p.m("key", "reinstate", p.key_id)
    code3 = p.curl("/tenants/lab/instances/x/y/z/w")[0]
    code4 = p.curl("/tenants/lab/instances/x/y/z/w", key=second)[0]
    p.result("11 the local operator reinstates the admin's key and it works (not 401); the other stays refused", code3 != 401 and code4 == 401,
             "reinstated key HTTP %d; the other HTTP %d" % (code3, code4))
    corrs = p.mj("correction", "list", "lab")["corrections"]
    made = [c for c in corrs if c["id"] == cid]
    rows = query(p.catalog(), "SELECT count(*) AS n FROM subjects WHERE label = ?", subject)[0]["n"]
    p.result("11 the delete made after the copy is applied: its subject hidden (sqlite3), the correction committed and unresolved for an admin",
             len(made) == 1 and made[0].get("unresolved") is True and made[0]["state"] == "CORRECTION_STATE_COMMITTED" and rows == 0,
             "the correction %s: state %s, unresolved %s; rows of its subject %d" % (cid[-8:], made[0]["state"].replace("CORRECTION_STATE_", "") if made else "missing",
                                                                                     made[0].get("unresolved") if made else None, rows))
    after = p.dump("reconciled.json")
    held = {x["uid"] for x in p.held()}
    listed = in_manifests(root)
    p.result("11 the catalog equals the dump taken before the copy was put back (catalog_dump.py) and the storage's manifests but the deleted subject's",
             filecmp.cmp(live, after, shallow=False) and held == listed - set(s.uids["be1"]),
             "dumps equal: %s; %d held, %d in manifests, %d hidden by the delete" % (filecmp.cmp(live, after, shallow=False), len(held), len(listed), len(s.uids["be1"])))
    st, evs2, _, end = events(p, {"scope": {"tenant": "lab"}, "cursor": old_cursor}, seconds=8)
    text = error_text(end.get("error")) if st == 200 else error_text(end)
    _, evs3, alive3, _ = events(p, {"scope": {"installation": {}}, "resume": True})
    _, new_history = history(p, alive3)
    p.result("11 a cursor from before the copy is refused with EVENT_HISTORY_CHANGED, the feed under a new history", old_cursor and
             "EVENT_HISTORY_CHANGED" in text and not evs2 and new_history and new_history != old_history,
             "HTTP %d, %d events, ended with: %s; history %r -> %r" % (st, len(evs2), " ".join(text.split())[:200] or end, old_history, new_history))


GROUPS = {"reconcile": group_reconcile}
