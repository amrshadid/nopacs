"""The proof that the archive is kept whole, from outside the binary.

It builds and runs the real program, sets it up with its own command line,
receives studies built from the public corpus with dcmtk's storescu, changes
the archive on purpose with the correction commands, damages it by hand, and
reads everything back only with tools that are not NoPACs: hashlib and os.walk
on the storage directory, Python's sqlite3 on the tenant's catalog, pydicom,
curl, dcmtk, scripts/prove/catalog_dump.py and scripts/prove/backup_open.py.
Each check prints one line, "ok" or "FAIL", with what it saw; the run ends
with a count and exits 1 when one failed.

  whole.py --go GO --dcmtk DIR [--python PY] [--docker PROGRAM] [--s3-image IMAGE] [--only GROUP,...]   (make prove-whole)

Groups, each with its own installation and under three minutes on a laptop
(those after records are in the modules whole_*.py beside this one):
  rebuild  a relabel and a replace made and checked on the storage, then the
           catalog made again from the storages by a restore and compared
           with the dump taken before (an empty diff)
  scrub    a byte flipped, an object removed and one added, found by a scrub
           and by nothing else changed; a read of the damaged instance fails
  delete   a deleted subject gone from the storage and the catalog, its
           audit entry, a re-send held back, and allowed again
  restore  a backup restored into a destroyed data directory: a changed
           backup restores nothing, keys suspended, a later delete held
  records  a correction record written by hand is not applied by a rebuild
  versions     purges on a bucket with versioning and object lock, a versioned
               one set to keep versions, an unversioned one and a directory,
               read by boto3 (an S3 server in a container)
  fresh        a restore onto a fresh data directory: the losses per receiver,
               the credentials to give again, the time taken
  cursor       an event cursor from before a restore refused; the new history
               begins with catalog_rebuilt
  writer       one writer per storage across an export and an import
  jobs         a 200-instance send meeting a delete and a replace
  resolve      ResolveInstances through a replace, a delete and a relabel
  references   a SEG's references found after corrections, in its tenant only
  provenance   what the sidecar says of the bytes received three ways
  client       a damaged 64 MiB instance on every read path, before and after
               the scrub marks it
  room         an object not online (InvalidObjectState, through a proxy to
               the S3 server), a grace changed after a delete, one clock

Every group but two needs no container: SQLite and a directory storage.
versions and room start an S3 server (Silo, the maintained fork of MinIO) in
a container named for the run and remove it at the end; they need docker
and boto3.

Needs: Go, dcmtk, openssl, and a Python with pydicom, pynetdicom,
cryptography and boto3 (scripts/requirements/prove-whole.txt).
"""

import argparse
import hashlib
import json
import os
import subprocess
import sys
import time

from pydicom import dcmread

from nopacs_scripts import whole as wholekit
from nopacs_scripts.db import query
from nopacs_scripts.dicom import sop_uids
from nopacs_scripts.digest import sha256_file
from nopacs_scripts.paths import PROVE
from nopacs_scripts.report import Failure
from nopacs_scripts.whole import Proof, listing, make_study
from nopacs_scripts.wholemore import SILO_IMAGE

CORPUS_DEFAULT = wholekit.DEFAULT_CORPUS


# ---- the scene every group starts from ----

class Scene:
    """What a group sent: its files, and the subject and instance labels the
    catalog gave them."""

    def __init__(self):
        self.files = {}  # name -> list of paths
        self.uids = {}  # name -> list of SOP Instance UIDs


def send(p, scene, name, port, paths):
    n, out = p.storescu(port, paths)
    scene.files[name] = paths
    scene.uids[name] = sop_uids(paths)
    return n


def scene(p, deid=True):
    """Two patients as received (the first in two sessions) and, optionally,
    one through the de-identifying receiver in two sessions."""
    s = Scene()
    sd = os.path.join(p.work, "studies")
    c = p.args.pydicom_corpus
    n = send(p, s, "al1", p.raw_port, make_study(c, sd, "Alpha^One", "AL-1", ["CT_small.dcm", "CT_small.dcm"]))
    n += send(p, s, "al2", p.raw_port, make_study(c, sd, "Alpha^One", "AL-1", ["MR_small.dcm", "MR_small.dcm"]))
    n += send(p, s, "be1", p.raw_port, make_study(c, sd, "Beta^Two", "BE-2", ["CT_small.dcm", "MR_small.dcm"]))
    total = 6
    if deid:
        n += send(p, s, "ga1", p.deid_port, make_study(c, sd, "Gamma^Three", "GA-3", ["CT_small.dcm", "MR_small.dcm"]))
        n += send(p, s, "ga2", p.deid_port, make_study(c, sd, "Gamma^Three", "GA-3", ["CT_small.dcm", "CT_small.dcm"]))
        total += 4
    got = p.wait_held(total)
    if got != total or n != total:
        raise Failure("the scene: %d of %d answered success, %d of %d held" % (n, total, got, total))
    return s


def row_of(p, uid):
    for r in p.held():
        if r["uid"] == uid:
            return r
    return None


def objects_under(listed, *prefixes):
    return sorted(k for k in listed if any(k.startswith(x) for x in prefixes))


def instance_route(r, label=None):
    return "/tenants/lab/instances/%s/%s/%s/%s" % (label or r["subject"], r["session"], r["scan"], r["uid"])


def quarantine(p, reason=None):
    a = ["quarantine", "list", "lab", "--all"] + (["--reason", reason] if reason else [])
    files = p.mj(*a).get("files", [])
    # The command takes the reason as a word; the answer carries the contract's name of it.
    return [f for f in files if not reason or f["reason"] == "QUARANTINE_REASON_" + reason.upper().replace("-", "_")]


def wait_quarantine(p, n, reason, timeout=60):
    deadline = time.time() + timeout
    files = []
    while time.time() < deadline:
        files = quarantine(p, reason)
        if len(files) >= n:
            return files
        time.sleep(0.5)
    return files


def tallies(job):
    return {t["key"]: int(t.get("count", 0)) for t in job.get("tallies", [])}


# ---- group: rebuild ----

def group_rebuild(p):
    p.setup()
    s = scene(p)
    al = row_of(p, s.uids["al1"][0])
    be = row_of(p, s.uids["be1"][0])
    # relabel
    before = listing(p.storage_root())
    old_objects = objects_under(before, "data/%s/" % al["subject"], "meta/%s/" % al["subject"])
    out = p.mj("correction", "relabel", "lab", al["subject"], "--to", "sub-alpha-fixed", "--reason", "wrong subject id at the scanner", "--wait")
    relabel_id = out["correction"]["id"]
    after = listing(p.storage_root())
    new_prefix = {"data/%s/" % al["subject"]: "data/sub-alpha-fixed/", "meta/%s/" % al["subject"]: "meta/sub-alpha-fixed/"}
    moved, bad = 0, []
    for k in old_objects:
        for old, new in new_prefix.items():
            if k.startswith(old):
                nk = new + k[len(old):]
                # A manifest and a metadata file name the label inside: made again under the new one, not copied.
                if nk in after and (k.startswith("meta/") or (after[nk][1] == before[k][1] and after[nk][0] == before[k][0])):
                    moved += 1
                else:
                    bad.append(nk)
    new_objects = objects_under(after, "data/sub-alpha-fixed/", "meta/sub-alpha-fixed/")
    data_objects = [k for k in old_objects if k.startswith("data/")]
    p.result("1 a relabel copies each instance object to its key under the new label, bytes equal (hashlib), and writes the manifests again",
             moved == len(old_objects) and not bad and len(new_objects) == len(old_objects),
             "%d instance objects of %s found under sub-alpha-fixed with the same size and SHA-256, and %d manifest and metadata files; %d new keys"
             % (len(data_objects), al["subject"], moved - len(data_objects), len(new_objects)),
             ["not equal or missing: %s" % bad[:4]])
    p.result("1 the old keys stay, unserved, until the purge", all(k in after for k in old_objects),
             "%d old keys still listed" % sum(1 for k in old_objects if k in after))
    rows = query(p.catalog(), "SELECT count(*) AS n FROM subjects WHERE label = ?", al["subject"])
    new_rows = query(p.catalog(), "SELECT count(*) AS n FROM subjects WHERE label = 'sub-alpha-fixed'")
    st_old = p.curl(instance_route(al))[0]
    moved_row = dict(al)
    moved_row["subject"] = "sub-alpha-fixed"
    code, hdr, body, _ = p.curl(instance_route(moved_row))
    p.result("1 sqlite3 and curl show the new label served and the old one not found", rows[0]["n"] == 0 and new_rows[0]["n"] == 1 and st_old == 404
             and code == 200 and hashlib.sha256(body).hexdigest() == after["data/sub-alpha-fixed/%s/%s/%s.dcm" % (al["session"], al["scan"], al["uid"])][1],
             "subjects named %s: %d, named sub-alpha-fixed: %d; GET old label HTTP %d, new label HTTP %d with the SHA-256 of its object"
             % (al["subject"], rows[0]["n"], new_rows[0]["n"], st_old, code))
    p.m("correction", "purge", "lab", relabel_id, "--wait", "--yes")
    purged = listing(p.storage_root())
    left = [k for k in old_objects if k in purged]
    kept = [k for k in new_objects if k in purged and purged[k] == after[k]]
    p.result("1 after the purge the old keys are gone and the new ones unchanged", not left and len(kept) == len(new_objects),
             "%d of %d old keys left; %d of %d new objects unchanged (size and SHA-256)" % (len(left), len(old_objects), len(kept), len(new_objects)), left[:4])
    # replace
    be_key = "data/%s/%s/%s/%s.dcm" % (be["subject"], be["session"], be["scan"], be["uid"])
    old_sha = purged[be_key][1]
    etag1 = p.curl(instance_route(be), "-I")[1].get("etag", "")
    corrected = os.path.join(p.work, "corrected.dcm")
    ds = dcmread(s.files["be1"][0])
    ds.PatientComments = "corrected by the site"
    ds.save_as(corrected, write_like_original=False)
    p.storescu(p.raw_port, [corrected])
    files = wait_quarantine(p, 1, "different-file")
    out = p.mj("correction", "replace", "lab", files[0]["id"], "--reason", "corrected file sent again by the site") if files else {}
    corr = out.get("correction", {})
    after2 = listing(p.storage_root())
    r2 = be_key[:-len(".dcm")] + ".r2.dcm"
    got = dcmread(os.path.join(p.storage_root(), r2)) if r2 in after2 else None
    sent = dcmread(corrected)
    same = got is not None and got.PatientComments == "corrected by the site" and got.SOPInstanceUID == sent.SOPInstanceUID \
        and bytes(got.PixelData) == bytes(sent.PixelData) and {e.keyword for e in got} ^ {e.keyword for e in sent} <= {"DataSetTrailingPadding"}
    etag2 = p.curl(instance_route(be), "-I")[1].get("etag", "")
    code, hdr, body, _ = p.curl(instance_route(be))
    p.result("2 a replace puts the corrected file at the instance's .r2.dcm key, read by pydicom", same and be_key in after2 and after2[be_key][1] == old_sha,
             "%s listed: %s; pydicom reads PatientComments %r, the same SOP Instance UID, pixel data and element names as the file sent (bar a trailing padding element); revision 1 kept "
             "unchanged for now: %s" % (r2[-40:], r2 in after2, getattr(got, "PatientComments", None), be_key in after2 and after2[be_key][1] == old_sha),
             ["same %s, old key listed %s, old sha %s, now %s" % (same, be_key in after2, old_sha[:12], after2.get(be_key, ("", ""))[1][:12])])
    p.result("2 the instance's ETag changed, and a download is the corrected object (curl)", etag1 and etag2 and etag1 != etag2 and code == 200
             and hashlib.sha256(body).hexdigest() == after2[r2][1], "ETag %s -> %s; GET HTTP %d, SHA-256 equal to the .r2.dcm object" % (etag1, etag2, code))
    p.result("2 the record holds the old checksum and the revisions", corr.get("oldSha256", "") == old_sha and int(corr.get("newRevision", 0)) == 2
             and int(corr.get("oldRevision", 0)) == 1, "oldSha256 %s (hashlib of the old object: %s), revision %s -> %s"
             % (corr.get("oldSha256", "")[:16], old_sha[:16], corr.get("oldRevision"), corr.get("newRevision")))
    p.m("correction", "purge", "lab", corr["id"], "--wait", "--yes")
    after3 = listing(p.storage_root())
    p.result("2 after the purge the old revision's key is gone and the .r2.dcm object stays", be_key not in after3 and after3.get(r2) == after2[r2],
             "%s listed: %s; %s listed: %s" % ("revision 1", be_key in after3, "revision 2", r2 in after3))
    # a rebuild from the storages
    job = p.job("lab", "rebuild", {"mode": "REBUILD_JOB_MODE_COMPARE"})
    differs = {k: v for k, v in tallies(job).items() if k.startswith("differs|")}
    p.result("3 a compare rebuild of the live catalog from the storages finds no difference", job["state"] == "JOB_STATE_DONE" and not differs,
             "job %s, scans %s; differences tallied: %s" % (job["state"].replace("JOB_STATE_", "").lower(), job.get("counts", {}).get("scans"), differs or "none"))
    dump1 = p.dump("before.json")
    dump1t = p.dump("before-times.json", times=True)
    p.stop()
    backup, keyfile = os.path.join(p.work, "whole.backup"), os.path.join(p.work, "key.copy")
    p.m("backup", "create", "--out", backup)
    subprocess.run(["cp", os.path.join(p.data, "key"), keyfile], check=True)
    subprocess.run(["rm", "-rf", p.data])
    r = p.run("backup", "restore", backup, "--key-file", keyfile)
    dump2 = p.dump("after.json")
    dump2t = p.dump("after-times.json", times=True)
    dt = subprocess.run(["diff", dump1t, dump2t], capture_output=True, text=True)
    print("     (for the cause) with the time columns too, the two dumps differ in %d lines: %s" % (len([x for x in dt.stdout.splitlines() if x[:1] in "<>"]),
                                                                                  " ".join(dt.stdout.split())[:700]), flush=True)
    d = subprocess.run(["diff", dump1, dump2], capture_output=True, text=True)
    p.result("3 the catalog made again by a restore from the storages is the one dumped before (diff empty)", r.returncode == 0 and d.returncode == 0
             and os.path.getsize(dump1) > 1000, "restore exit %d; %d bytes of dump, diff exit %d, %d differing lines" %
             (r.returncode, os.path.getsize(dump1), d.returncode, len([x for x in d.stdout.splitlines() if x[:1] in "<>"])),
             (r.stderr + d.stdout).splitlines()[:10])
    print("     the restore reported: %s" % " | ".join(x.strip() for x in r.stdout.strip().splitlines()[-6:]), flush=True)


# ---- group: scrub ----

def group_scrub(p):
    p.setup()
    s = scene(p, deid=False)
    root = p.storage_root()
    rows = {u: row_of(p, u) for u in s.uids["al1"] + s.uids["al2"] + s.uids["be1"]}
    damaged, missing, good = rows[s.uids["al1"][0]], rows[s.uids["al2"][0]], rows[s.uids["be1"][0]]
    key = lambda r: "data/%s/%s/%s/%s.dcm" % (r["subject"], r["session"], r["scan"], r["uid"])
    # a byte flipped, in the middle of the pixel data, the length unchanged
    path = os.path.join(root, key(damaged))
    with open(path, "r+b") as f:
        f.seek(os.path.getsize(path) // 2)
        b = f.read(1)
        f.seek(-1, 1)
        f.write(bytes([b[0] ^ 0xFF]))
    with open(path, "rb") as f:
        bad_bytes = f.read()
    cat = query(p.catalog(), "SELECT lower(hex(coalesce(archived_sha256, sha256))) AS h FROM instances WHERE sop_instance_uid = ?", damaged["uid"])[0]["h"]
    seen = hashlib.sha256(bad_bytes).hexdigest()
    p.result("4 a byte flipped in a stored object: hashlib's SHA-256 differs from the catalog's (sqlite3)", seen != cat and len(bad_bytes) > 1000,
             "the object's SHA-256 %s, the catalog's %s" % (seen[:16], cat[:16]))
    os.remove(os.path.join(root, key(missing)))
    orphan = "data/%s/%s/%s/1.2.826.0.1.3680043.8.498.99999.dcm" % (good["subject"], good["session"], good["scan"])
    with open(os.path.join(root, orphan), "wb") as f:
        f.write(b"an object no scan names\n")
    before = listing(root)
    job = p.job("lab", "scrub", {"storage": "main"})
    found = p.findings()
    kinds = {(f["kind"].replace("SCRUB_FINDING_KIND_", "").lower(), f["key"]) for f in found}
    want = {("damaged", key(damaged)), ("missing", key(missing)), ("orphan", orphan)}
    p.result("4 a scrub names the damaged, the missing and the orphan object, and nothing else", job["state"] == "JOB_STATE_PARTIAL" and kinds == want,
             "job %s; findings %s" % (job["state"].replace("JOB_STATE_", "").lower(), sorted((k, x[-12:]) for k, x in kinds)),
             ["wanted %s" % sorted(want), "found %s" % sorted(kinds)])
    after = listing(root)
    p.result("4 the scrub changed nothing on the storage (a listing with SHA-256 before and after)", before == after,
             "%d objects listed before and after; differences: %s" % (len(after), sorted(set(before) ^ set(after))[:3] or "none"))
    code, hdr, body, rc = p.curl(instance_route(damaged))
    p.result("4 curl of the damaged instance fails with the data-loss error and returns none of the bad bytes", code >= 500 and b"data_loss" in body
             and bad_bytes[:512] not in body and len(body) < 2000, "HTTP %d, %d bytes of body: %s" % (code, len(body), body[:90]))
    code2, _, body2, _ = p.curl(instance_route(missing))
    p.result("4 curl of the missing instance fails the same way", code2 >= 500 and b"data_loss" in body2 and len(body2) < 2000,
             "HTTP %d, %d bytes of body: %s" % (code2, len(body2), body2[:90]))
    code3, _, body3, _ = p.curl(instance_route(good))
    p.result("4 an undamaged instance is served, its SHA-256 the object's", code3 == 200 and hashlib.sha256(body3).hexdigest() == before[key(good)][1],
             "HTTP %d, %d bytes" % (code3, len(body3)))


# ---- group: delete ----

def group_delete(p):
    p.setup()
    s = scene(p, deid=False)
    root = p.storage_root()
    al = row_of(p, s.uids["al1"][0])
    subject = al["subject"]
    mine = [r for r in p.held() if r["subject"] == subject]
    sha_before = {r["storage_key"]: sha256_file(os.path.join(root, r["storage_key"])) for r in mine}
    before = listing(root)
    others = {k: v for k, v in before.items() if not k.startswith(("data/%s/" % subject, "meta/%s/" % subject))}
    out = p.mj("correction", "delete", "lab", subject, "--reason", "consent withdrawn", "--yes")
    cid = out["correction"]["id"]
    code = p.curl(instance_route(al))[0]
    p.result("5 a delete hides the subject at once: curl answers not found while the bytes are still on the storage", code == 404 and
             all(k in listing(root) for k in sha_before), "GET HTTP %d; %d objects still listed" % (code, len(sha_before)))
    p.m("correction", "purge", "lab", cid, "--wait", "--yes")
    after = listing(root)
    left = objects_under(after, "data/%s/" % subject, "meta/%s/" % subject)
    p.result("5 after the purge nothing is left under data/<subject>/ or meta/<subject>/ (os.walk)", not left and all(after.get(k) == v for k, v in others.items()),
             "%d keys left of %d; the %d other objects unchanged" % (len(left), len(before) - len(others), len(others)), left[:4])
    rows = {t: query(p.catalog(), "SELECT count(*) AS n FROM %s" % q, subject)[0]["n"] for t, q in
            (("subjects", "subjects WHERE label = ?"), ("sessions", "sessions WHERE subject_id IN (SELECT id FROM subjects WHERE label = ?)"),
             ("scans", "scans WHERE subject_label = ?"), ("instances", "instances WHERE scan_id IN (SELECT id FROM scans WHERE subject_label = ?)"))}
    totals = query(p.catalog(), "SELECT subjects, sessions, scans, instances FROM catalog_totals")[0]
    counted = [query(p.catalog(), "SELECT count(*) AS n FROM %s" % t)[0]["n"] for t in ("subjects", "sessions", "scans", "instances")]
    p.result("5 sqlite3 shows no row of the subject, and the totals equal the counts", not any(rows.values()) and list(totals) == counted,
             "rows of the subject %s; totals %s, counted %s" % (rows, list(totals), counted))
    objs = {(r["object_key"], r["sha256"].hex() if r["sha256"] else ""): r["disposition"] for r in
            query(p.catalog(), "SELECT object_key, sha256, disposition FROM correction_objects WHERE correction_id = ?", cid)}
    p.result("5 the purge recorded each removed object with the checksum hashlib computed before", len(objs) >= len(sha_before) and
             all((k, h) in objs and objs[(k, h)] == "removed" for k, h in sha_before.items()),
             "%d recorded objects, %d of the %d instance objects with their SHA-256 and the disposition removed"
             % (len(objs), sum(1 for k, h in sha_before.items() if objs.get((k, h)) == "removed"), len(sha_before)))
    entries = p.mj("audit", "list", "--tenant", "lab", "--all")["entries"]
    made = [e for e in entries if e["method"].endswith("/Delete") and e.get("details", {}).get("validate_only") == "false"]
    p.result("5 the audit entry of the delete holds the reason, the target and the correction's id", len(made) == 1 and
             made[0]["details"].get("reason") == "consent withdrawn" and made[0]["details"].get("target") == subject and made[0].get("object") == cid,
             "%d entries of the delete (the preview wrote its own, validate_only true): reason %r, target %s, object %s" %
             (len(made), made[0]["details"].get("reason") if made else None, made[0]["details"].get("target") if made else None,
              made[0].get("object", "")[-8:] if made else None), [json.dumps(e)[:300] for e in entries if e["method"].endswith("/Delete")])
    # a re-send
    resend = s.files["al1"] + s.files["al2"]
    p.storescu(p.raw_port, resend)
    held = wait_quarantine(p, len(resend), "was-deleted")
    again = [r for r in p.held() if r["subject"] == subject]
    p.result("5 a re-send of the study is quarantined as was_deleted, none of it archived", len(held) == len(resend) and not again
             and not objects_under(listing(root), "data/%s/" % subject), "%d of %d files quarantined was_deleted; %d instances of the subject held"
             % (len(held), len(resend), len(again)))
    # allowed again
    shown = p.mj("correction", "show", "lab", cid)["correction"]["holds"]
    flag = {"HELD_VALUE_KIND_SUBJECT": "--subject", "HELD_VALUE_KIND_STUDY": "--study", "HELD_VALUE_KIND_SERIES": "--series",
            "HELD_VALUE_KIND_INSTANCE": "--instance"}
    for h in shown:
        p.m("correction", "allow", "lab", cid, flag[h["kind"]], h["value"], "--reason", "consent given again")
    p.storescu(p.raw_port, resend)
    deadline = time.time() + 60
    while time.time() < deadline and len([r for r in p.held() if r["subject"] == subject]) < len(s.files["al1"]) + len(s.files["al2"]):
        time.sleep(0.5)
    back = {r["uid"]: r for r in p.held() if r["subject"] == subject}
    equal = all(dcmread(os.path.join(root, back[u]["storage_key"])).SOPInstanceUID == u for u in back)
    ok_bytes = all(bytes(dcmread(os.path.join(root, back[u]["storage_key"])).PixelData) == bytes(dcmread(f).PixelData)
                   for names in ("al1", "al2") for f, u in zip(s.files[names], s.uids[names]) if u in back)
    p.result("5 allowing the held values lets the study arrive again, its pixel data the file sent (pydicom)", len(back) == len(resend) and equal and ok_bytes,
             "%d held values allowed (%s); %d of %d instances held again under %s" % (len(shown), ", ".join(sorted(h["kind"].replace("HELD_VALUE_KIND_", "").lower() for h in shown)),
                                                                                  len(back), len(resend), subject))


# ---- group: restore ----

def group_restore(p):
    p.setup()
    s = scene(p)
    root = p.storage_root()
    second = p.m("key", "create", "--holder", "second holder", "--no-expiry").splitlines()[0].strip()
    second_id = second.split("_")[1]
    p.m("grant", "set", second_id, "--role", "reader", "--tenant", "lab")
    p.stop()
    backup, keyfile = os.path.join(p.work, "whole.backup"), os.path.join(p.work, "key.copy")
    p.m("backup", "create", "--out", backup)
    subprocess.run(["cp", os.path.join(p.data, "key"), keyfile], check=True)
    opened = subprocess.run([p.args.python, os.path.join(PROVE, "backup_open.py"), backup, "--key-file", keyfile, "--sections"], capture_output=True, text=True)
    p.result("6 the backup opens with backup_open.py (AES-GCM chunks, every section's SHA-256), nothing of NoPACs'", opened.returncode == 0,
             "exit %d: %s" % (opened.returncode, (opened.stdout.strip().splitlines() or [opened.stderr.strip()])[-1][:120]))
    p.serve()
    # after the backup: more arrivals, a delete, a revoked key
    send(p, s, "ne1", p.raw_port, make_study(p.args.pydicom_corpus, os.path.join(p.work, "studies"), "Delta^Four", "DE-4", ["MR_small.dcm", "CT_small.dcm"]))
    while len(p.held()) < 12:
        time.sleep(0.3)
    be = row_of(p, s.uids["be1"][0])
    dele = p.mj("correction", "delete", "lab", be["subject"], "--reason", "consent withdrawn", "--yes")["correction"]["id"]
    p.m("key", "revoke", second_id)
    p.stop()
    subprocess.run(["rm", "-rf", p.data])
    # a changed backup restores nothing
    bad = os.path.join(p.work, "changed.backup")
    data = bytearray(open(backup, "rb").read())
    data[len(data) // 2] ^= 0x01
    with open(bad, "wb") as f:
        f.write(data)
    r = p.run("backup", "restore", bad, "--key-file", keyfile)
    left = os.listdir(p.data) if os.path.isdir(p.data) else []
    refused = subprocess.run([p.args.python, os.path.join(PROVE, "backup_open.py"), bad, "--key-file", keyfile], capture_output=True, text=True)
    p.result("6 a backup with one byte changed restores nothing (ls of the target)", r.returncode != 0 and not [x for x in left if x != "key"]
             and refused.returncode == 1, "restore exit %d (%s); the data directory holds %s; backup_open.py exit %d" %
             (r.returncode, r.stderr.strip()[:80], left or "nothing", refused.returncode))
    # the real restore
    r = p.run("backup", "restore", backup, "--key-file", keyfile)
    print("     the restore reported: %s" % " | ".join(x.strip() for x in (r.stdout + r.stderr).strip().splitlines() if x.strip())[:1800], flush=True)
    report = r.stdout + r.stderr
    p.serve()
    code, _, _, _ = p.curl("/tenants/lab/instances/x/y/z/w")
    code2, _, _, _ = p.curl("/tenants/lab/instances/x/y/z/w", key=second)
    p.result("6 after the restore the keys are suspended: curl answers 401 for each", r.returncode == 0 and code == 401 and code2 == 401,
             "restore exit %d; the admin key HTTP %d; the other key (revoked after the backup) HTTP %d" % (r.returncode, code, code2))
    rot = p.m("key", "rotate", p.key_id).splitlines()[0].strip()
    code3 = p.curl("/tenants/lab/instances/x/y/z/w", key=rot)[0]
    p.result("6 the local operator rotates one key and its successor works (not 401), the old one still refused", code3 != 401 and
             p.curl("/tenants/lab/instances/x/y/z/w")[0] == 401, "successor HTTP %d" % code3)
    corrs = p.mj("correction", "list", "lab")["corrections"]
    mine = [c for c in corrs if c["id"] == dele]
    be_rows = query(p.catalog(), "SELECT count(*) AS n FROM subjects WHERE label = ?", be["subject"])[0]["n"]
    p.result("6 the delete made after the backup is held for an admin as unresolved, its subject hidden", len(mine) == 1 and mine[0].get("unresolved") is True
             and be_rows == 0 and "unresolved_correction 1" in report and "1 with no row in the backup" in report,
             "the report says so (one record with no row in the backup, unresolved_correction 1); the correction %s: state %s, unresolved %s; rows of its subject: %d" %
             (dele[-8:], mine[0]["state"].replace("CORRECTION_STATE_", "") if mine else "missing", mine[0].get("unresolved") if mine else None, be_rows),
             [json.dumps(c)[:200] for c in corrs])
    # the catalog equals the storages' manifests
    in_manifests = set()
    for d, _, names in os.walk(os.path.join(root, "meta")):
        if "manifest.json" in names:
            with open(os.path.join(d, "manifest.json")) as f:
                in_manifests |= {e["sop_instance_uid"] for e in json.load(f)["files"]}
    held = {r["uid"] for r in p.held()}
    hidden = set(s.uids["be1"])
    p.result("6 the catalog equals the instances the storage's manifests list, but the deleted subject's, and holds the later arrival",
             held == in_manifests - hidden and set(s.uids["ne1"]) <= held, "%d held, %d in manifests, %d hidden by the delete; later arrival held: %s"
             % (len(held), len(in_manifests), len(hidden), set(s.uids["ne1"]) <= held))


# ---- group: records ----

def group_records(p):
    p.setup()
    s = scene(p, deid=False)
    root = p.storage_root()
    al = row_of(p, s.uids["al1"][0])
    be = row_of(p, s.uids["be1"][0])
    p.m("correction", "delete", "lab", al["subject"], "--reason", "consent withdrawn", "--yes")
    records = os.path.join(root, "meta", ".corrections")
    real = sorted(os.listdir(records))
    with open(os.path.join(records, real[0])) as f:
        template = json.load(f)
    placed = []
    for i, mac in enumerate(("0" * 64, None)):
        rec = dict(template)
        rec.update(id="01m81jzzzzzzzzzzzzzzzzzz%02d" % i, state="committed", scans=[{"subject": be["subject"], "session": be["session"], "scan": be["scan"],
                                                                                    "study_uid": be["study"], "series_uid": template["scans"][0]["series_uid"]}])
        rec.pop("mac", None)
        if mac:
            rec["mac"] = mac
        path = os.path.join(records, rec["id"] + ".json")
        with open(path, "w") as f:
            json.dump(rec, f)
        placed.append(path)
    job = p.job("lab", "rebuild", {"mode": "REBUILD_JOB_MODE_COMPARE"})
    t = tallies(job)
    differs = {k: v for k, v in t.items() if k.startswith("differs|")}
    held = {r["uid"] for r in p.held()}
    p.result("7 a correction record written by hand is not applied by a rebuild: its tally counts the two planted, and only the real delete's instances are left out", job["state"] in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL") and
             t.get("unauthenticated_correction") == 2 and t.get("left_out|deleted") == len(s.uids["al1"]), "job %s; tallies %s" % (job["state"].replace("JOB_STATE_", "").lower(), t))
    p.result("7 the subject the planted records named is not hidden: the rebuild finds no difference and the catalog still holds it", not differs
             and set(s.uids["be1"]) <= held, "differences tallied: %s; %d instances of the subject held" % (differs or "none", len(set(s.uids["be1"]) & held)))
    p.result("7 the planted files are still on the storage (a rebuild reads and changes nothing there)", all(os.path.exists(x) for x in placed) and
             len(os.listdir(records)) == len(real) + 2, "%d records listed" % len(os.listdir(records)))


GROUPS = {"rebuild": group_rebuild, "scrub": group_scrub, "delete": group_delete, "restore": group_restore, "records": group_records}


def all_groups():
    """The groups of this module and those of the modules beside it, which
    import this one for the scene, so they are joined here at run time."""
    from nopacs_scripts.sections import whole_client, whole_host, whole_jobs, whole_reconcile, whole_refs, whole_room, whole_versions
    out = dict(GROUPS)
    for m in (whole_versions, whole_host, whole_jobs, whole_refs, whole_client, whole_room, whole_reconcile):
        out.update(m.GROUPS)
    return out


def main(argv):
    groups = all_groups()
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--go", default="go")
    ap.add_argument("--dcmtk", default="", help="the directory of dcmtk's programs")
    ap.add_argument("--python", default=sys.executable, help="the Python that runs the tools beside this one")
    ap.add_argument("--pydicom-corpus", default=os.environ.get("PYDICOM_TEST_FILES", CORPUS_DEFAULT), help="pydicom's test files directory")
    ap.add_argument("--binary", default="", help="a built nopacs to run, in place of building the working tree's; needs --server-binary")
    ap.add_argument("--server-binary", default="", help="a built nopacs-server to run, in place of building the working tree's")
    ap.add_argument("--docker", default="docker", help="the docker program the groups that need a container run (versions)")
    ap.add_argument("--s3-image", default=SILO_IMAGE, help="the S3 server's image, MinIO's or a fork's (versions)")
    ap.add_argument("--only", default="", help="groups to run, comma separated: %s (default all)" % ", ".join(groups))
    args = ap.parse_args(argv)
    names = [n for n in args.only.split(",") if n] or list(groups)
    bad = [n for n in names if n not in groups]
    if bad:
        raise Failure("no group %s; groups are %s" % (bad, ", ".join(groups)))
    ok = failed = skipped = 0
    for n in names:
        p = Proof(args)
        p.counts["skip"] = 0
        print("== %s" % n, flush=True)
        t0 = time.time()
        try:
            groups[n](p)
        finally:
            p.stop()
            for f in getattr(p, "cleanups", []):
                try:
                    f()
                except Exception as e:  # noqa: BLE001 - a peer or container already stopped is no failure of the run
                    print("   (cleanup: %r)" % e, flush=True)
        print("   (%s took %.0f s)" % (n, time.time() - t0), flush=True)
        ok += p.counts["ok"]
        failed += p.counts["FAIL"]
        if p.counts["FAIL"]:
            print("the work directory is kept: %s" % p.work)
        else:
            subprocess.run(["rm", "-rf", p.work])
        skipped += p.counts["skip"]
    print()
    print("%d ok, %d FAIL" % (ok, failed) + (", %d skip" % skipped if skipped else ""))
    if failed:
        return 1

