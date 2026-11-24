"""The delete group of the interruption harness (sections/interrupt.py): a
delete, its purge and its undo killed at each step, the live catalog and a
rebuild compare from the storages agreeing after each restart; then a backup
from before the deletes restored after them.
"""

import json
import os
import subprocess

from nopacs_scripts.db import query
from nopacs_scripts.interrupt import PURGE_STEPS, key_of, setup, studies, whole
from nopacs_scripts.sections.whole import instance_route, row_of, tallies
from nopacs_scripts.whole import listing

DELETE_STEPS = ["prepared", "records", "committed"]


def compare(p, name):
    job = p.job("lab", "rebuild", {"mode": "REBUILD_JOB_MODE_COMPARE"})
    t = tallies(job)
    differs = {k: v for k, v in t.items() if k.startswith("differs|")}
    # A scan a delete hides is left out of the shadow and tallied, which
    # ends the compare partial: what was left out is the delete's, not a
    # difference.
    others = {k: v for k, v in t.items() if k.startswith(("left_out|", "unreadable", "unauthenticated")) and k != "left_out|deleted"}
    p.result("%s: a rebuild compare from the storages agrees with the live catalog" % name,
             job["state"] in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL") and not differs and not others and not job.get("failures"),
             "job %s; differences tallied: %s; scans left out as deleted: %s; other scans left out: %s"
             % (job["state"].replace("JOB_STATE_", "").lower(), differs or "none", t.get("left_out|deleted", 0), others or "none"))


def subject_rows(p, subject):
    return query(p.catalog(), "SELECT count(*) AS n FROM instances WHERE scan_id IN (SELECT id FROM scans WHERE subject_label = ?)", subject)[0]["n"]


def find_delete(p, subject, instance=""):
    mine = [c for c in p.corrections("delete") if c.get("target", {}).get("subjectLabel") == subject
            and c.get("target", {}).get("instanceId", "") == instance]
    return mine[0]["id"] if mine else ""


def state_of(c):
    return c.get("state", "").replace("CORRECTION_STATE_", "").lower()


def group_delete(p):
    setup(p)
    # One subject per round: the delete steps, the purge steps, an instance
    # purge, an undo, and a delete left prepared at the restore.
    made = studies(p, "Delete", len(DELETE_STEPS) + len(PURGE_STEPS) + 3, ["CT_small.dcm", "MR_small.dcm"])
    rows = [row_of(p, uids[0]) for _, uids in made]
    p.stop()
    backup, keyfile = os.path.join(p.work, "before.backup"), os.path.join(p.work, "key.copy")
    p.m("backup", "create", "--out", backup)
    subprocess.run(["cp", os.path.join(p.data, "key"), keyfile], check=True)
    p.serve()
    for step, r in zip(DELETE_STEPS, rows):
        name = "delete killed at %s" % step
        p.arm("correction:delete:" + step)
        res = p.run("correction", "delete", "lab", r["subject"], "--reason", "consent withdrawn", "--yes")
        killed, seen = p.died()
        p.restart()
        p.result("%s: the server stopped there" % name, killed, "%s; the request answered exit %d" % (seen, res.returncode))
        cid = find_delete(p, r["subject"])
        want = "committed" if step == "committed" else "abandoned"
        c = p.wait_state(cid, (want,)) if cid else {}
        st, rec = state_of(c), p.records(cid) if cid else None
        held, code = subject_rows(p, r["subject"]), whole(p, instance_route(r))[0]
        shown = want == "abandoned"
        p.result("%s: after the restart the delete is %s, its records say so, the subject %s" % (name, want, "shown" if shown else "hidden"),
                 st == want and rec == want and (held == 2 and code == 200 if shown else held == 0 and code == 404),
                 "state %s, records (json) %s; instances of the subject (sqlite3) %d, GET HTTP %s" % (st, rec, held, code), p.warnings())
        compare(p, name)
    n = len(DELETE_STEPS) + len(PURGE_STEPS)
    for step, r in zip(PURGE_STEPS, rows[len(DELETE_STEPS):n]):
        purge_round(p, "purge killed at %s" % step, "correction:delete:" + step, r, None)
    purge_round(p, "instance purge killed at purge-meta", "correction:delete:purge-meta", rows[n], made[n][1])
    undo_round(p, rows[n + 1])
    restore_older(p, backup, keyfile, rows, made)


def undo_round(p, r):
    """A delete committed, then its undo killed once the row is undone: after
    the restart the undo holds, its records say so, the subject is shown."""
    name = "undo killed at undone"
    cid = p.mj("correction", "delete", "lab", r["subject"], "--reason", "consent withdrawn", "--yes")["correction"]["id"]
    p.arm("correction:delete:undone")
    res = p.run("correction", "undo", "lab", cid, "--reason", "withdrawn by mistake")
    killed, seen = p.died()
    p.restart()
    p.result("%s: the server stopped there" % name, killed, "%s; the request answered exit %d" % (seen, res.returncode))
    c = p.wait_state(cid, ("undone",))
    st, rec = state_of(c), p.records(cid)
    held, got = subject_rows(p, r["subject"]), whole(p, instance_route(r))
    p.result("%s: after the restart the delete is undone, its records say so, the subject shown" % name,
             st == "undone" and rec == "undone" and held == 2 and isinstance(got, tuple) and got[0] == 200,
             "state %s, records (json) %s; instances of the subject (sqlite3) %d, GET HTTP %s"
             % (st, rec, held, got[0] if isinstance(got, tuple) else got), p.warnings())
    compare(p, name)


def purge_round(p, name, step, r, uids):
    """A delete committed, then its purge killed at step (of a subject, or of
    the first of uids when given): after the restart the purge ends and
    nothing of what it owns is left."""
    target = r["subject"] if uids is None else "%s/%s/%s" % (r["subject"], r["session"], r["scan"])
    extra = [] if uids is None else ["--instance", uids[0]]
    p.m("correction", "delete", "lab", target, *extra, "--reason", "consent withdrawn", "--yes")
    cid = find_delete(p, r["subject"], uids[0] if uids else "")
    root = p.storage_root()
    before = listing(root)
    p.arm(step)
    res = p.run("correction", "purge", "lab", cid, "--yes")
    killed, seen = p.died()
    p.restart()
    p.result("%s: the server stopped there" % name, killed, "%s; the request answered exit %d" % (seen, res.returncode))
    c = p.wait_state(cid, ("purged",))
    st, rec = state_of(c), p.records(cid)
    after = listing(root)
    if uids is None:
        left = [k for k in after if k.startswith(("data/%s/" % r["subject"], "meta/%s/" % r["subject"]))]
        others = {k: v for k, v in before.items() if not k.startswith(("data/%s/" % r["subject"], "meta/%s/" % r["subject"], "meta/.corrections/"))}
        ok = st == "purged" and rec == "purged" and not left and subject_rows(p, r["subject"]) == 0 and all(after.get(k) == v for k, v in others.items())
        p.result("%s: after the restart the purge ended, nothing of the subject is left on the storage or in the catalog" % name, ok,
                 "state %s, records %s; %d keys left under the subject (os.walk), %d rows (sqlite3); the %d other objects unchanged"
                 % (st, rec, len(left), subject_rows(p, r["subject"]), len(others)), left[:4] + p.warnings())
    else:
        entry_gone = p.manifest_entry(r, uids[0]) is None
        kept = p.manifest_entry(r, uids[1])
        kept_key = key_of(r, uids[1])
        ok = st == "purged" and rec == "purged" and entry_gone and kept and key_of(r, uids[0]) not in after and \
            after.get(kept_key, (0, ""))[1] == kept["sha256"] == before[kept_key][1]
        p.result("%s: after the restart the purge ended, the manifest lists the other instance alone, its object unchanged" % name, ok,
                 "state %s, records %s; deleted instance in the manifest: %s, its object listed: %s; the other's object SHA-256 (hashlib) unchanged: %s"
                 % (st, rec, not entry_gone, key_of(r, uids[0]) in after, kept and after.get(kept_key, (0, ""))[1] == kept["sha256"]),
                 p.warnings())
    compare(p, name)


def restore_older(p, backup, keyfile, rows, made):
    """A backup from before every delete restored after them, with one more
    delete killed with its record still prepared and never recovered. The
    restore reads each verified record that has no row as it says:
    committed or purged hides, abandoned or undone shows, and only the one
    still prepared is taken as committed and listed unresolved."""
    n = len(DELETE_STEPS) + len(PURGE_STEPS)
    shown = {x["subject"] for x in rows[:len(DELETE_STEPS) - 1]} | {rows[n + 1]["subject"]}  # abandoned, and the one undone
    deleted = {x["subject"] for x in rows[len(DELETE_STEPS) - 1:n]}
    pending, pending_uids = rows[n + 2]["subject"], set(made[n + 2][1])
    live_uids = {x["uid"] for x in p.held()}
    live = p.dump("live.json")
    # The last delete, killed with its record written prepared; the server
    # is not started again before the restore, so nothing settles it.
    p.arm("correction:delete:records")
    res = p.run("correction", "delete", "lab", pending, "--reason", "consent withdrawn", "--yes")
    killed, seen = p.died()
    rec = p.records(record_naming(p, pending) or "-")
    p.result("delete killed at records, then not restarted: the server stopped there, its record left prepared", killed and rec == "prepared",
             "%s; the request answered exit %d; the record (json) says %s" % (seen, res.returncode, rec))
    subprocess.run(["rm", "-rf", p.data])
    r = p.run("backup", "restore", backup, "--key-file", keyfile)
    restored = p.dump("restored.json")
    d = subprocess.run(["diff", live, restored], capture_output=True, text=True)
    held = p.held()
    back, back_uids = {x["subject"] for x in held}, {x["uid"] for x in held}
    p.serve()
    corrs = p.mj("correction", "list", "lab")["corrections"]
    unresolved = {c["target"]["subjectLabel"] for c in corrs if c.get("unresolved")}
    p.result("an older backup restored after the deletes and purges brings no committed or purged subject back", r.returncode == 0
             and not (deleted & back), "restore exit %d; of the %d subjects deleted or purged after the backup %d came back (sqlite3); "
             "%d of them listed unresolved (not asserted: whether a terminal record with no row is listed unresolved is the restore's own rule)"
             % (r.returncode, len(deleted), len(deleted & back), len(deleted & unresolved)), r.stderr.splitlines()[:6])
    p.result("an older backup restored: a delete whose record is still prepared and has no row is hidden and listed unresolved",
             pending not in back and not (pending_uids & back_uids) and pending in unresolved,
             "the subject held (sqlite3): %s; listed unresolved: %s" % (pending in back, pending in unresolved))
    p.result("an older backup restored: the subjects whose delete was abandoned or undone are shown, and the catalog is the live one "
             "but the prepared delete's", back_uids == live_uids - pending_uids and shown <= back,
             "%d instances held, live %d less the prepared delete's %d; of the %d subjects whose delete was abandoned or undone %d shown, "
             "%d listed unresolved; the catalog dumps (catalog_dump.py) differ in %d lines (the prepared delete's subject among them)"
             % (len(back_uids), len(live_uids), len(pending_uids), len(shown), len(shown & back), len(shown & unresolved),
                len([x for x in d.stdout.splitlines() if x[:1] in "<>"])), d.stdout.splitlines()[:6])


def record_naming(p, subject):
    """The id of the correction record on the storage that names subject, read as JSON."""
    path = os.path.join(p.storage_root(), "meta", ".corrections")
    for name in sorted(os.listdir(path)):
        with open(os.path.join(path, name)) as f:
            if any(s.get("subject") == subject for s in json.load(f).get("scans", [])):
                return name[:-len(".json")]
    return ""
