"""The interruption harness: the real server killed at each named durable
step of a correction and a rebuild, started again, and judged from outside.

The server is built with the build tag "interrupt" and started with
NOPACS_INTERRUPT_AT naming one step; it exits with status 86 the moment that
step's durable write is done, as a crash there would leave it. Readers run
across each kill and the restart: curl whole downloads and an If-Range resume
begun before the kill and finished after it, a DICOMweb retrieve and the
ReadFiles stream read with Python's standard library, the catalog read by
SQLite itself, os.walk and hashlib on the storage directory, the correction
records read as JSON, a pynetdicom peer a send delivers to. Each check
prints one line, "ok" or "FAIL", with what it saw; the run ends with a table
and exits 1 when one failed.

  interrupt.py --go GO --dcmtk DIR [--python PY] [--only replace,relabel,rebuild,delete]   (make prove-interrupt)

Groups, each with its own installation:
  replace  a replace killed after each of its steps, with readers across it:
           every read is one revision's bytes (hashlib equals the ETag and
           one revision's checksum), never back to the old once the new was
           served, a resume across the kill included; after the restart
           catalog, manifest and storage name one revision; then the purge of
           a replace's old revision killed at each of its steps
  relabel  a relabel killed after each of its steps: at every catalog read
           the whole target is under one label; after the restart it is
           published whole, every object under the new label unchanged; then
           the purge of the old tree killed at each of its steps
  rebuild  a rebuild that replaces the catalog, killed while it builds, after
           it activates and after it drops its shadow, with catalog lists,
           QIDO, downloads and a send running: every answer counts the old
           totals or the new, never a mix, and never the old after the new;
           then a rebuild compare killed at each of its steps
  delete   a delete, its purge and its undo killed after each step: after the
           restart the live catalog and a rebuild compare from the storages
           agree; then a backup from before the deletes restored, with one
           delete left prepared: no deleted subject comes back, an abandoned
           or undone one shows, the prepared one is hidden and unresolved

Every group ends with two checks of its server logs: no put lost its
temporary file to another store of the same storage, and no other storage
write failed.

It needs no container: SQLite and a directory storage.
Needs: Go, dcmtk, and a Python with pydicom, pynetdicom and cryptography
(scripts/requirements/prove-reception.txt).
"""

import argparse
import os
import subprocess
import sys
import time

from pydicom import dcmread

from nopacs_scripts import whole as wholekit
from nopacs_scripts.digest import sha256_file
from nopacs_scripts.interrupt import (CUT, CURL_CUT, PURGE_STEPS, Harness, Loop, curl_range, cut_out, dicom_uid, flips, key_of, labels_of,
                                      order_problems, read_files, setup, sha, sides, studies, wado, whole)
from nopacs_scripts.report import Failure
from nopacs_scripts.sections.interrupt_delete import group_delete
from nopacs_scripts.sections.interrupt_rebuild import group_rebuild
from nopacs_scripts.sections.whole import instance_route, row_of
from nopacs_scripts.whole import listing


def judge_whole(answers, allowed):
    """Sorts curl's whole downloads: (good, dropped, problems, revisions seen
    in order). A good one is 200 with its SHA-256 (hashlib) its ETag's and
    one of allowed."""
    good, bad, seen = 0, [], []
    answers, cut = cut_out(answers)
    for t, a in answers:
        code, etag, body, rc = a
        digest = sha(body)
        if code == 200 and etag.strip('"') == digest and digest in allowed:
            good += 1
            seen.append(digest)
        else:
            bad.append("HTTP %d, curl exit %d, %d bytes, SHA-256 %s, ETag %s" % (code, rc, len(body), digest[:12], etag[:14]))
    return good, cut, bad, seen


def state_of(c):
    return c.get("state", "").replace("CORRECTION_STATE_", "").lower()


# ---- group: replace ----

REPLACE_STEPS = ["prepared", "records", "object", "meta", "committed"]


def group_replace(p):
    setup(p)
    # Two scans: a correction of a scan is refused while another of it is
    # not yet purged, and the replace published at "committed" is not; the
    # purge rounds each purge theirs before the next.
    ((files, uids),) = studies(p, "Replace", 1, ["CT_small.dcm"] * len(REPLACE_STEPS))
    for step, path, uid in zip(REPLACE_STEPS, files, uids):
        replace_round(p, step, path, uid)
    ((files, uids),) = studies(p, "Purged", 1, ["CT_small.dcm"] * len(PURGE_STEPS))
    for step, path, uid in zip(PURGE_STEPS, files, uids):
        replace_purge_round(p, step, path, uid)


def corrected_copy(p, path, uid, tag):
    """A corrected copy of the file sent again, held as different_file:
    returns the quarantined file's id, or "" when it was not held."""
    ds = dcmread(path)
    ds.PatientComments = "corrected (%s)" % tag
    corrected = os.path.join(p.work, "corrected-%s.dcm" % tag)
    ds.save_as(corrected, write_like_original=False)
    p.storescu(p.raw_port, [corrected])
    deadline = time.time() + 60
    while time.time() < deadline:
        q = [f for f in p.mj("quarantine", "list", "lab", "--all").get("files", [])
             if f.get("sopInstanceUid") == uid and f["reason"] == "QUARANTINE_REASON_DIFFERENT_FILE"]
        if q:
            return q[0]["id"]
        time.sleep(0.3)
    return ""


def replace_purge_round(p, step, path, uid):
    """A replace published, then its purge of the old revision killed at
    step: after the restart only the new revision's object is left, the one
    the catalog, the manifest and a download name."""
    name = "replace purge killed at %s" % step
    qid = corrected_copy(p, path, uid, "purge-" + step)
    a = p.mj("correction", "replace", "lab", qid, "--reason", "corrected file sent again") if qid else {}
    cid = a.get("correction", {}).get("id", "")
    if not cid:
        p.result(name, False, "no replace was made to purge (quarantined %r)" % qid)
        return
    row = row_of(p, uid)
    p.arm("correction:replace:" + step)
    res = p.run("correction", "purge", "lab", cid, "--yes")
    killed, seen = p.died()
    p.restart()
    p.result("%s: the server stopped there" % name, killed, "%s; the request answered exit %d" % (seen, res.returncode))
    c = p.wait_state(cid, ("purged",))
    st, rec = state_of(c), p.records(cid)
    objs = sorted(k for k in listing(p.storage_root()) if k.startswith(key_of(row, uid)[:-len(".dcm")] + "."))
    entry = p.manifest_entry(row, uid)
    w = whole(p, instance_route(row))
    new = c.get("newSha256", "")
    ok = st == "purged" and rec == "purged" and objs == [key_of(row, uid, int(c.get("newRevision", 2)))] and entry and \
        entry["sha256"] == new == row["sha256"] and isinstance(w, tuple) and w[0] == 200 and sha(w[2]) == new
    p.result("%s: after the restart the old revision is gone and the new one is what catalog, manifest and download name" % name, ok,
             "state %s, records %s; revision objects left (os.walk) %s; catalog %s, manifest %s, GET %s"
             % (st, rec, [o.rsplit("/", 1)[1][-14:] for o in objs], row["sha256"][:12], entry["sha256"][:12] if entry else None,
                sha(w[2])[:12] if isinstance(w, tuple) else w), p.warnings())


def replace_round(p, step, path, uid):
    name = "replace killed at %s" % step
    row = row_of(p, uid)
    route = instance_route(row)
    old = row["sha256"]
    ds = dcmread(path)
    study, series = str(ds.StudyInstanceUID), str(ds.SeriesInstanceUID)
    qid = corrected_copy(p, path, uid, step)
    if not qid:
        p.result(name, False, "the corrected file was not quarantined as different_file")
        return
    p.arm("correction:replace:" + step)
    first = curl_range(p, route, 0, 1023)
    with Loop(lambda: whole(p, route)) as wl, Loop(lambda: wado(p, study, series, uid)) as wd, \
            Loop(lambda: read_files(p, row["subject"]), pause=0.05) as rf:
        time.sleep(0.3)  # reads before the request, so some are read before the kill whatever step it is
        r = p.run("correction", "replace", "lab", qid, "--reason", "corrected file sent again")
        killed, seen = p.died()
        p.restart()
        time.sleep(1.0)
    mine = [c for c in p.corrections("replace") if c.get("quarantinedFile") == qid]
    want_state = "committed" if step == "committed" else "abandoned"
    corr = p.wait_state(mine[0]["id"], (want_state,)) if mine else {}
    new = corr.get("newSha256", "")
    revisions = {old, new} - {""}
    want = new if step == "committed" else old
    p.result("%s: the server stopped there" % name, killed, "%s; the request it cut answered exit %d" % (seen, r.returncode))

    # every read during and after: one revision's bytes, never going back
    good, cut, bad, order = judge_whole(wl.answers, revisions)
    bad += order_problems(order, old, new, step == "committed")
    before, after = sides(wl.answers, p.died_at, p.up_at)
    if not before or not after:
        bad.append("whole downloads read %d before the kill and %d after the restart: both sides are needed" % (before, after))
    wanswers, wcut = cut_out(wd.answers)
    fanswers, fcut = cut_out(rf.answers)
    for label, n in (("whole downloads", cut), ("WADO-RS retrieves", wcut), ("ReadFiles streams", fcut)):
        if n > 1:
            bad.append("%d %s dropped by the server across one kill" % (n, label))
    wgood = 0
    for t, a in wanswers:
        st, got = a
        if st == 200 and len(got) == 1 and sha(got[0]) in revisions and dicom_uid(got[0]) == uid:
            wgood += 1
        else:
            bad.append("WADO-RS HTTP %d, %d parts, SHA-256 %s" % (st, len(got), [sha(x)[:12] for x in got]))
    fgood = 0
    for t, a in fanswers:
        if isinstance(a, dict) and uid in a and a[uid][0] == sha(a[uid][1]) and a[uid][0] in revisions:
            fgood += 1
        else:
            bad.append("ReadFiles: %s" % (a if not isinstance(a, dict) else
                                           "announced %s, read %s" % (a.get(uid, ("", b""))[0][:12], sha(a.get(uid, ("", b""))[1])[:12])))
    # the resume begun before the kill, finished after it with If-Range
    resumed = ""
    if not isinstance(first, tuple) or first[0] != 206:
        bad.append("the first part was not answered 206: %s" % (first[:2] if isinstance(first, tuple) else first))
    else:
        rest = curl_range(p, route, 1024, None, if_range=first[1])
        rest = rest if isinstance(rest, tuple) else None
        if rest and rest[0] == 206:
            full = first[2] + rest[2]
            resumed = "206, the two parts' SHA-256 %s" % ("the ETag's" if sha(full) == first[1].strip('"') else "NOT the ETag's")
            if sha(full) != first[1].strip('"') or sha(full) not in revisions:
                bad.append("the resumed download's SHA-256 %s is not its ETag %s" % (sha(full)[:12], first[1][:14]))
        elif rest and rest[0] == 200:
            resumed = "200 whole (the ETag changed), its SHA-256 %s" % ("the new ETag's" if sha(rest[2]) == rest[1].strip('"') else "NOT its ETag's")
            if sha(rest[2]) != rest[1].strip('"') or sha(rest[2]) not in revisions:
                bad.append("the restarted download's SHA-256 is not its ETag")
        else:
            bad.append("the resume answered %s" % (rest[:2] if rest else "nothing"))
    p.result("%s: every read across the kill and restart is one revision's bytes" % name, not bad and good > 0 and wgood > 0 and fgood > 0,
             "%d whole downloads (%d before the kill, %d after the restart) with SHA-256 (hashlib) equal to the ETag and to one revision's, "
             "revision changed %d times and never back; %d WADO-RS retrieves and %d ReadFiles streams whole; dropped by the kill: %d, %d, %d; "
             "the If-Range resume: %s" % (good, before, after, flips(order), wgood, fgood, cut, wcut, fcut, resumed), bad)

    # after the restart: the catalog, the manifest and the storage agree
    now = row_of(p, uid)
    entry = p.manifest_entry(now, uid)
    root = p.storage_root()
    objs = sorted(k for k in listing(root) if k.startswith("data/%s/%s/%s/%s." % (now["subject"], now["session"], now["scan"], uid)))
    want_objs = sorted([key_of(now, uid)] + ([key_of(now, uid, int(corr.get("newRevision", 2)))] if step == "committed" else []))
    obj_sha = sha256_file(os.path.join(root, entry["key"])) if entry and os.path.exists(os.path.join(root, entry["key"])) else ""
    w = whole(p, route)
    st = state_of(corr)
    rec = p.records(corr.get("id", "")) if corr else None
    ok = (st == want_state and rec == want_state and now["sha256"] == want and entry and entry["sha256"] == want and obj_sha == want
          and objs == want_objs and isinstance(w, tuple) and w[0] == 200 and sha(w[2]) == want == w[1].strip('"'))
    p.result("%s: after the restart the replace is %s and catalog, manifest, object and download name one revision" % (name, want_state), ok,
             "state %s, records %s; catalog (sqlite3) %s, manifest %s, the object it names (hashlib) %s, GET %s; revision objects %s"
             % (st, rec, now["sha256"][:12], entry["sha256"][:12] if entry else None, obj_sha[:12], sha(w[2])[:12] if isinstance(w, tuple) else w,
                [o.rsplit("/", 1)[1][-14:] for o in objs]),
             ["want %s, revision objects %s" % (want[:12], want_objs)] + p.warnings())


# ---- group: relabel ----

RELABEL_STEPS = ["prepared", "records", "relabel-object", "relabel-metadata", "relabel-manifest", "copied", "committed"]


def group_relabel(p):
    setup(p)
    made = studies(p, "Relabel", len(RELABEL_STEPS), ["CT_small.dcm", "MR_small.dcm"])
    done = [relabel_round(p, step, uids, "sub-moved-%d" % j) for j, (step, (files, uids)) in enumerate(zip(RELABEL_STEPS, made))]
    for step, (cid, old, new) in zip(PURGE_STEPS, [d for d in done if d[0]]):
        relabel_purge_round(p, step, cid, old, new)


def relabel_purge_round(p, step, cid, old, new):
    """The purge of a published relabel's old tree killed at step: after
    the restart nothing is left under the old label and the new tree is
    unchanged."""
    name = "relabel purge killed at %s" % step
    root = p.storage_root()
    before = listing(root)
    new_tree = {k: v for k, v in before.items() if k.startswith(("data/%s/" % new, "meta/%s/" % new))}
    p.arm("correction:relabel:" + step)
    res = p.run("correction", "purge", "lab", cid, "--yes")
    killed, seen = p.died()
    p.restart()
    p.result("%s: the server stopped there" % name, killed, "%s; the request answered exit %d" % (seen, res.returncode))
    c = p.wait_state(cid, ("purged",))
    st, rec = state_of(c), p.records(cid)
    after = listing(root)
    left = [k for k in after if k.startswith(("data/%s/" % old, "meta/%s/" % old))]
    kept = sum(1 for k, v in new_tree.items() if after.get(k) == v)
    p.result("%s: after the restart nothing is left under the old label and the new tree is unchanged" % name,
             st == "purged" and rec == "purged" and not left and new_tree and kept == len(new_tree),
             "state %s, records %s; %d keys left under %s (os.walk); %d of %d objects under %s unchanged (hashlib)"
             % (st, rec, len(left), old, kept, len(new_tree), new), left[:4] + p.warnings())


def relabel_round(p, step, uids, new):
    """A relabel killed at step with readers across it; returns (its id
    when it ended published, the old label, the new)."""
    name = "relabel killed at %s" % step
    rows = [row_of(p, u) for u in uids]
    old = rows[0]["subject"]
    shas = {r["uid"]: r["sha256"] for r in rows}
    db = p.catalog()
    moved = dict(rows[0], subject=new)

    def pair():
        a, b = whole(p, instance_route(rows[0])), whole(p, instance_route(moved))
        if a is None or b is None:
            return None
        return CUT if CUT in (a, b) or a[3] in CURL_CUT or b[3] in CURL_CUT else (a, b)

    p.arm("correction:relabel:" + step)
    with Loop(lambda: labels_of(db, uids), pause=0.005) as cat, Loop(pair) as http:
        time.sleep(0.3)  # reads before the request, so some are read before the kill whatever step it is
        r = p.run("correction", "relabel", "lab", old, "--to", new, "--reason", "wrong subject id at the scanner")
        killed, seen = p.died()
        p.restart()
        mine = [c for c in p.corrections("relabel") if c.get("toLabel") == new]
        corr = p.wait_state(mine[0]["id"], ("committed",)) if mine else {}
        time.sleep(0.5)
    p.result("%s: the server stopped there" % name, killed, "%s; the request answered exit %d" % (seen, r.returncode))
    n = len(uids)
    bad = ["catalog read %s" % a for t, a in cat.answers if a not in ({old: n}, {new: n})]
    olds = sum(1 for t, a in cat.answers if a == {old: n})
    news = sum(1 for t, a in cat.answers if a == {new: n})
    back = [i for i, (t, a) in enumerate(cat.answers) if a == {old: n} and any(b == {new: n} for _, b in cat.answers[:i])]
    if back:
        bad.append("the old label read again after the new, %d times" % len(back))
    whole_reads = 0
    pairs, cut = cut_out(http.answers)
    if cut > 1:
        bad.append("%d download pairs dropped by the server across one kill" % cut)
    for t, (a, b) in pairs:
        if a[0] == 404 and b[0] == 404:
            bad.append("both labels answered not found")
        for x in (a, b):
            if x[0] == 200:
                if sha(x[2]) == x[1].strip('"') == shas[uids[0]]:
                    whole_reads += 1
                else:
                    bad.append("a download under one label is not the instance's bytes")
            elif x[0] != 404:
                bad.append("HTTP %d" % x[0])
    before, after_restart = sides(http.answers, p.died_at, p.up_at)
    if not before or not after_restart:
        bad.append("download pairs read %d before the kill and %d after the restart: both sides are needed" % (before, after_restart))
    p.result("%s: at every point the whole target is under one label" % name, not bad and olds > 0 and news > 0 and whole_reads > 0,
             "%d catalog reads (sqlite3): %d with all %d instances under %s, %d all under %s, none split; %d downloads by either label whole "
             "(SHA-256 = ETag = the instance's) in %d pairs (%d before the kill, %d after the restart), never both labels not found"
             % (len(cat.answers), olds, n, old, news, new, whole_reads, len(pairs), before, after_restart), bad)
    after = listing(p.storage_root())
    unchanged = [u for u in uids if after.get(key_of(dict(rows[0], subject=new, session=row_of(p, u)["session"], scan=row_of(p, u)["scan"]), u),
                                              (0, ""))[1] == shas[u]]
    st = state_of(corr)
    rec = p.records(corr.get("id", "")) if corr else None
    gone = whole(p, instance_route(rows[0]))
    p.result("%s: after the restart the relabel is published whole and every object under the new label unchanged" % name,
             st == "committed" and rec == "committed" and len(unchanged) == n and isinstance(gone, tuple) and gone[0] == 404,
             "state %s, records %s; %d of %d objects under %s with their SHA-256 (hashlib); the old label answers HTTP %s"
             % (st, rec, len(unchanged), n, new, gone[0] if isinstance(gone, tuple) else gone), p.warnings())
    return (corr.get("id", ""), old, new) if st == "committed" else ("", old, new)


GROUPS = {"replace": group_replace, "relabel": group_relabel, "rebuild": group_rebuild, "delete": group_delete}


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--go", default="go")
    ap.add_argument("--dcmtk", default="", help="the directory of dcmtk's programs")
    ap.add_argument("--python", default=sys.executable, help="the Python that runs the tools beside this one")
    ap.add_argument("--pydicom-corpus", default=os.environ.get("PYDICOM_TEST_FILES", wholekit.DEFAULT_CORPUS), help="pydicom's test files directory")
    ap.add_argument("--binary", default="", help="a built nopacs to run, in place of building the working tree's; needs --server-binary")
    ap.add_argument("--server-binary", default="", help="a nopacs-server built with -tags interrupt, in place of building the working tree's")
    ap.add_argument("--only", default="", help="groups to run, comma separated: %s (default all)" % ", ".join(GROUPS))
    args = ap.parse_args(argv)
    names = [n for n in args.only.split(",") if n] or list(GROUPS)
    bad = [n for n in names if n not in GROUPS]
    if bad:
        raise Failure("no group %s; groups are %s" % (bad, ", ".join(GROUPS)))
    table = []
    for n in names:
        p = Harness(args)
        print("== %s" % n, flush=True)
        t0 = time.time()
        finished = False
        try:
            GROUPS[n](p)
            finished = True
        finally:
            p.stop()
            if not finished:
                print("the group stopped on an error; its work directory is kept: %s" % p.work, flush=True)
        lost = p.lost_puts()
        p.result("%s: no put to a storage lost its temporary file to another store of the same storage cleaning it" % n, not lost,
                 "%d server starts, %d such failures in their logs" % (p.logs, len(lost)), lost)
        failed = p.storage_failures()
        p.result("%s: no other storage write failed during the run" % n, not failed,
                 "%d server starts, %d warnings naming a storage that failed" % (p.logs, len(failed)),
                 ["a storage write failed during the run: %s" % x for x in failed])
        table.append((n, p.counts["ok"], p.counts["FAIL"], time.time() - t0))
        if p.counts["FAIL"]:
            print("the work directory is kept: %s" % p.work)
        else:
            subprocess.run(["rm", "-rf", p.work])
    print()
    print("%-10s %4s %5s %8s" % ("group", "ok", "FAIL", "seconds"))
    for n, ok, failed, secs in table:
        print("%-10s %4d %5d %8.0f" % (n, ok, failed, secs))
    ok, failed = sum(x[1] for x in table), sum(x[2] for x in table)
    print("%-10s %4d %5d" % ("all", ok, failed))
    if failed:
        return 1
