"""The groups of the proof of keeping the archive whole that follow an
instance through its corrections: what ResolveInstances answers for each
revision a platform kept, whether the references inside a segmentation
still resolve after its subject is relabeled and a referenced instance
replaced, and what a scan's sidecar says of the bytes it received. Each is
judged by hashlib, pydicom, curl, sqlite3 and dcmtk, never by NoPACs.

  resolve     current, superseded, deleted and unknown revisions; an old label
  references  a SEG's Referenced SOP Instance UIDs, de-identified with its CT,
              found by QIDO-RS in its tenant after corrections, and not in another
  provenance  received_form, the syntax, the checksums, the duplicate and the
              different files of one data set sent three ways
"""

import hashlib
import json
import os
import shutil
import subprocess
import time
import urllib.parse

from pydicom import dcmread
from pydicom.uid import generate_uid

from nopacs_scripts.dicom import sop_uids
from nopacs_scripts.interrupt import KeepingPeer
from nopacs_scripts.net import free_port
from nopacs_scripts.report import Failure
from nopacs_scripts.send import split_part10
from nopacs_scripts.sections.whole import instance_route, quarantine, row_of, scene, wait_quarantine
from nopacs_scripts.whole import make_study
from nopacs_scripts.wholemore import dcmtk, second_tenant, sidecar_lines, storescu

DISP = "INSTANCE_DISPOSITION_"


def corrected_copy(src, out, comment="corrected by the site"):
    """The file at src with one element changed, written by pydicom: the
    corrected file a site sends again under the same SOP Instance UID."""
    ds = dcmread(src)
    ds.PatientComments = comment
    ds.save_as(out, write_like_original=False)
    return out


def replace_by(p, port, path, reason="corrected file sent again by the site"):
    """Sends a corrected copy and replaces the archived instance with it;
    returns the correction."""
    before = {f["id"] for f in quarantine(p, "different-file")}
    p.storescu(port, [path])
    deadline = time.time() + 60
    new = []
    while time.time() < deadline and not new:
        new = [f for f in quarantine(p, "different-file") if f["id"] not in before]
        time.sleep(0.3)
    if not new:
        raise Failure("the corrected copy of %s was not quarantined as a different file" % os.path.basename(path))
    return p.mj("correction", "replace", "lab", new[0]["id"], "--reason", reason)["correction"]


def resolve(p, members):
    st, a = p.call("nopacs.v1.CorrectionService/ResolveInstances", {"tenant": "lab", "members": [{"instanceId": u, "revision": r} for u, r in members]})
    if st != 200:
        raise Failure("ResolveInstances: HTTP %d %s" % (st, a))
    return a.get("instances", [])


# ---- group: resolve ----

def group_resolve(p):
    p.setup()
    s = scene(p, deid=False)
    replaced, current, deleted = s.uids["be1"][0], s.uids["be1"][1], s.uids["al1"][0]
    be, al = row_of(p, replaced), row_of(p, deleted)
    corr = replace_by(p, p.raw_port, corrected_copy(s.files["be1"][0], os.path.join(p.work, "corrected.dcm")))
    dele = p.mj("correction", "delete", "lab", al["subject"], "--reason", "consent withdrawn", "--yes")["correction"]["id"]
    root = p.storage_root()
    r2 = "data/%s/%s/%s/%s.r2.dcm" % (be["subject"], be["session"], be["scan"], replaced)
    sha_r2 = hashlib.sha256(open(os.path.join(root, r2), "rb").read()).hexdigest()
    cur = row_of(p, current)
    sha_cur = hashlib.sha256(open(os.path.join(root, cur["storage_key"]), "rb").read()).hexdigest()
    unknown = generate_uid()
    got = resolve(p, [(replaced, 1), (replaced, 2), (current, 1), (deleted, 1), (unknown, 1), (current, 3)])
    d = [(g.get("disposition", "").replace(DISP, "").lower(), int(g.get("currentRevision", 0)), g.get("sha256", ""), g.get("correction", "")) for g in got]
    want = [("superseded", 2, sha_r2, corr["id"]), ("current", 2, sha_r2, ""), ("current", 1, sha_cur, ""), ("deleted", None, None, dele), ("unknown", None, None, ""),
            ("unknown", None, None, "")]
    ok = len(d) == 6 and all(g[0] == w[0] and (w[1] is None or g[1] == w[1]) and (w[2] is None or g[2] == w[2]) and (not w[3] or g[3] == w[3])
                             for g, w in zip(d, want))
    p.result("12 ResolveInstances answers superseded, current, deleted and unknown, each SHA-256 the stored object's (hashlib)", ok,
             "; ".join("%s r%d: %s%s%s" % (u[-6:], r, g[0], " r%d" % g[1] if g[1] else "", " sha %s" % g[2][:10] if g[2] else "")
                       for (u, r), g in zip([(replaced, 1), (replaced, 2), (current, 1), (deleted, 1), (unknown, 1), (current, 3)], d)),
             ["answered %s" % json.dumps(got)[:600], "the .r2.dcm object's SHA-256 %s, the current one's %s, the replace %s, the delete %s"
              % (sha_r2[:12], sha_cur[:12], corr["id"][-8:], dele[-8:])])
    old = be["subject"]
    # A correction on a target waits for the one before to be purged.
    p.m("correction", "purge", "lab", corr["id"], "--wait", "--yes")
    p.m("correction", "relabel", "lab", old, "--to", "sub-beta-new", "--reason", "wrong subject id at the scanner", "--wait")
    after = resolve(p, [(current, 1)])
    p.result("12 after a relabel the revision is still current, and names the new label", after and after[0].get("disposition") == DISP + "CURRENT"
             and after[0].get("subjectLabel") == "sub-beta-new", "answered %s, subject label %s" % (after[0].get("disposition") if after else None,
                                                                                                 after[0].get("subjectLabel") if after else None))
    code, _, body, _ = p.curl(instance_route(cur))
    st, a = p.call("nopacs.v1.CatalogService/ListSessions", {"tenant": "lab", "subjectLabel": old})
    msgs = [body.decode("latin-1"), a.get("message", "")]
    p.result("12 the old label answers not found naming the new one (curl of the instance, ListSessions)", code == 404 and st == 404
             and all("sub-beta-new" in m for m in msgs), "GET HTTP %d %s; ListSessions HTTP %d %s" % (code, msgs[0][:120], st, msgs[1][:120]))


# ---- group: references ----

def segmentation(src, out, ct_files):
    """The corpus segmentation rewritten to reference the CT files given:
    the same patient and study, a series and instance of its own, and every
    Referenced SOP Instance UID (its referenced series and each frame's
    source image) one of the CT instances, in order."""
    ds = dcmread(src)
    cts = [dcmread(f, stop_before_pixels=True) for f in ct_files]
    ds.PatientName, ds.PatientID = cts[0].PatientName, cts[0].PatientID
    ds.StudyInstanceUID = cts[0].StudyInstanceUID
    ds.SeriesInstanceUID = generate_uid()
    ds.SOPInstanceUID = ds.file_meta.MediaStorageSOPInstanceUID = generate_uid()
    ref = ds.ReferencedSeriesSequence[0]
    ref.SeriesInstanceUID = cts[0].SeriesInstanceUID
    for item, ct in zip(ref.ReferencedInstanceSequence, cts):
        item.ReferencedSOPClassUID, item.ReferencedSOPInstanceUID = ct.SOPClassUID, ct.SOPInstanceUID
    for frame, ct in zip(ds.PerFrameFunctionalGroupsSequence, cts):
        src_item = frame.DerivationImageSequence[0].SourceImageSequence[0]
        src_item.ReferencedSOPClassUID, src_item.ReferencedSOPInstanceUID = ct.SOPClassUID, ct.SOPInstanceUID
    if "FrameOfReferenceUID" in ds and "FrameOfReferenceUID" in cts[0]:
        ds.FrameOfReferenceUID = cts[0].FrameOfReferenceUID
    ds.save_as(out, write_like_original=False)
    return out


def referenced_uids(ds):
    """Every Referenced SOP Instance UID (0008,1155) in a data set, at any depth, read by pydicom."""
    out = []
    for e in ds.iterall():
        if e.tag == 0x00081155:
            out.append(str(e.value))
    return out


def qido(p, tenant, uid):
    """QIDO-RS for one SOP Instance UID in a tenant (PS3.18 10.6), by curl:
    (HTTP status, the SOP Instance UIDs answered)."""
    code, _, body, _ = p.curl("/tenants/%s/dicom-web/instances?%s" % (tenant, urllib.parse.urlencode({"SOPInstanceUID": uid})),
                              "-H", "Accept: application/dicom+json")
    try:
        rows = json.loads(body or b"[]") if code == 200 else []
    except ValueError:
        rows = []
    return code, [r.get("00080018", {}).get("Value", [""])[0] for r in rows]


def group_references(p):
    p.setup()
    sd = os.path.join(p.work, "studies")
    cts = make_study(p.args.pydicom_corpus, sd, "Ref^Seg", "RS-1", ["CT_small.dcm"] * 3)
    n, _ = p.storescu(p.deid_port, cts)
    if n != 3 or p.wait_held(3) != 3:
        raise Failure("the CT series: %d of 3 answered success, %d held" % (n, len(p.held())))
    seg = segmentation(os.path.join(p.args.pydicom_corpus, "liver_1frame.dcm"), os.path.join(sd, "seg.dcm"), cts)
    # -R proposes the file's own storage class, which storescu's default list lacks.
    n, out = storescu(p, p.deid_port, [seg], "-R")
    got = p.wait_held(4, timeout=60)
    q = quarantine(p)
    if got != 4:
        raise Failure("the SEG: %d of 1 answered success, %d of 4 held; quarantined: %s; storescu said: %s"
                      % (n, got, [(f["reason"], f.get("detail", "")[:80]) for f in q], " | ".join(out.splitlines()[-6:])))
    rows = p.held()
    seg_row = [r for r in rows if dcmread(os.path.join(p.storage_root(), r["storage_key"]), stop_before_pixels=True).SOPClassUID == "1.2.840.10008.5.1.4.1.1.66.4"]
    ct_rows = [r for r in rows if r not in seg_row]
    archived_seg = dcmread(os.path.join(p.storage_root(), seg_row[0]["storage_key"]))
    refs = referenced_uids(archived_seg)
    ct_uids = {r["uid"] for r in ct_rows}
    sent_uids = set(sop_uids(cts))
    sent_refs = referenced_uids(dcmread(seg))
    # The basic profile removes the Source Image Sequence (PS3.15 E.1-1, X),
    # so the references kept are the Referenced Series Sequence's.
    p.result("13 the SEG and its CT series, de-identified in two receptions, are held under one subject, the SEG's references the CT's new UIDs (pydicom)",
             len({r["subject"] for r in rows}) == 1 and set(refs) == ct_uids and not (set(refs) & sent_uids),
             "%d Referenced SOP Instance UIDs kept in the archived SEG (of %d sent; the profile removes the frames' Source Image Sequence) naming %d distinct "
             "instances, all among the %d CT instances held, none a UID as sent; subject %s"
             % (len(refs), len(sent_refs), len(set(refs)), len(ct_uids), rows[0]["subject"]), ["refs %s" % sorted(set(refs))[:3], "held %s" % sorted(ct_uids)[:3]])
    rel = p.mj("correction", "relabel", "lab", rows[0]["subject"], "--to", "sub-seg-fixed", "--reason", "wrong subject id at the scanner", "--wait")
    # A correction on a target waits for the one before to be purged.
    p.m("correction", "purge", "lab", rel["correction"]["id"], "--wait", "--yes")
    corr = replace_by(p, p.deid_port, corrected_copy(cts[1], os.path.join(sd, "ct-corrected.dcm")))
    found = {u: qido(p, "lab", u) for u in sorted(set(refs))}
    p.result("13 after the relabel and the replace of a referenced CT instance, QIDO-RS finds every referenced UID in the tenant (curl)",
             all(c == 200 and r == [u] for u, (c, r) in found.items()) and corr.get("newRevision") in (2, "2"),
             "the replace made revision %s; %s" % (corr.get("newRevision"), "; ".join("%s: HTTP %d, %d found" % (u[-8:], c, len(r)) for u, (c, r) in found.items())))
    other_port = free_port()
    second_tenant(p, "other", other_port)
    storescu(p, other_port, cts + [seg], "-R")
    deadline = time.time() + 60
    while time.time() < deadline and len(p.held("other")) < 4:
        time.sleep(0.3)
    missing = {u: qido(p, "other", u) for u in sorted(set(refs))}
    p.result("13 the same UIDs searched in a second tenant that holds the same study, de-identified by its own secrets, find nothing",
             len(p.held("other")) == 4 and all(c in (200, 204) and not r for c, r in missing.values()),
             "the other tenant holds %d instances; %s" % (len(p.held("other")), "; ".join("%s: HTTP %d, %d found" % (u[-8:], c, len(r)) for u, (c, r) in missing.items())))


# ---- group: provenance ----

def stow(p, files):
    """STOW-RS of files (PS3.18 10.5) by curl; returns (HTTP status, answer)."""
    boundary = "provenance-%d" % os.getpid()
    body = b""
    for f in files:
        body += b"--" + boundary.encode() + b"\r\nContent-Type: application/dicom\r\n\r\n" + open(f, "rb").read() + b"\r\n"
    body += b"--" + boundary.encode() + b"--\r\n"
    path = os.path.join(p.work, "stow.body")
    with open(path, "wb") as f:
        f.write(body)
    code, _, answer, _ = p.curl("/tenants/lab/dicom-web/studies", "-X", "POST", "--data-binary", "@" + path, "-H",
                                'Content-Type: multipart/related; type="application/dicom"; boundary=' + boundary, "-H", "Accept: application/dicom+json")
    return code, answer


def sha_file(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def group_provenance(p):
    p.setup()
    sd = os.path.join(p.work, "studies")
    tree = os.path.join(p.work, "tree")
    p.m("setting", "set", "import.roots", "old=" + tree)
    p.m("setting", "set", "import.allowed-roots", "old", "--tenant", "lab")
    p.m("receiver", "add", "lab", "web", "--type", "stow", "--storage", "main", "--allow-key", p.key_id, "--quiet-period", "5s", "--as-received", "--enabled")
    p.m("receiver", "add", "lab", "imp", "--type", "import", "--storage", "main", "--root", "old", "--layout", "plain", "--quiet-period", "5s",
        "--as-received", "--enabled")
    d1, d2, d3 = make_study(p.args.pydicom_corpus, sd, "Prov^Six", "PR-6", ["CT_small.dcm"] * 3)
    os.makedirs(os.path.join(tree, "batch"))
    shutil.copy(d3, os.path.join(tree, "batch", "d3.dcm"))
    n, _ = p.storescu(p.raw_port, [d1])
    code, answer = stow(p, [d2])
    job = p.mj("job", "import", "lab", "imp", "batch")["job"]["id"]
    if n != 1 or code != 200 or p.wait_held(3) != 3:
        raise Failure("the three arrivals: storescu %d, STOW-RS HTTP %d %s, import job %s; %d held" % (n, code, answer[:200], job, len(p.held())))
    time.sleep(1)
    root = p.storage_root()
    lines = sidecar_lines(root)
    held = {r["uid"]: r for r in p.held()}
    u1, u2, u3 = sop_uids([d1, d2, d3])
    peer = KeepingPeer(free_port())
    try:
        storescu(p, peer.port, [d1])
    finally:
        peer.stop()
    wire = (peer.bytes.get(u1) or [b""])[0]
    syntax = str(dcmread(d1, stop_before_pixels=True).file_meta.TransferSyntaxUID)
    for name, u, sent, form in (("storescu (DIMSE)", u1, d1, "dimse"), ("STOW-RS", u2, d2, "part10"), ("import", u3, d3, "part10")):
        e = (lines.get(u) or [{}])[-1]
        stored = os.path.join(root, held[u]["storage_key"])
        archived = sha_file(stored)
        received = e.get("received_sha256") or e.get("sha256")
        if form == "part10":
            same_input = sha_file(sent) == received
            how = "hashlib of the file sent %s" % ("equals it" if same_input else "differs: %s" % sha_file(sent)[:12])
        else:
            # A data set NoPACs wrapped in its own Part 10 header: the data set
            # bytes stored are the bytes storescu put on the wire, which a
            # pynetdicom peer sent the same file by the same storescu kept.
            same_input = split_part10(open(stored, "rb").read())[1] == wire
            how = "the stored data set bytes %s the data set storescu put on the wire (%d bytes, kept by a pynetdicom peer)" % (
                "equal" if same_input else "differ from", len(wire))
        p.result("14 %s: the sidecar says received_form %s and syntax %s; hashlib reproduces the archived checksum from the stored object and the received one from the input"
                 % (name, form, syntax), e.get("received_form") == form and e.get("received_syntax") == syntax and e.get("sha256") == archived == held[u]["sha256"]
                 and same_input, "received_form %r, received_syntax %r, transfer_syntax %r; sha256 %s = hashlib of the object %s = catalog %s; %s"
                 % (e.get("received_form"), e.get("received_syntax"), e.get("transfer_syntax"), e.get("sha256", "")[:12], archived[:12], held[u]["sha256"][:12], how),
                 ["sidecar line: %s" % json.dumps(e)[:400]])
    first_sha = sha_file(os.path.join(root, held[u1]["storage_key"]))
    # again, the same bytes; then another transfer syntax; then a corrected copy
    p.storescu(p.raw_port, [d1])
    implicit = os.path.join(sd, "d1-implicit.dcm")
    subprocess.run([dcmtk(p, "dcmconv"), "+ti", d1, implicit], check=True, capture_output=True)
    n_imp, out_imp = storescu(p, p.raw_port, [implicit], "-R")
    corrected = corrected_copy(d1, os.path.join(sd, "d1-corrected.dcm"))
    p.storescu(p.raw_port, [corrected])
    diff = wait_quarantine(p, 2, "different-file")
    mine = [f for f in diff if f.get("sopInstanceUid", u1) == u1]
    rs = p.mj("reception", "list", "lab", "--receiver", "raw", "--all").get("receptions", [])
    deadline = time.time() + 30
    while time.time() < deadline and not all(r["step"] in ("RECEPTION_STEP_DONE", "RECEPTION_STEP_QUARANTINED") for r in rs):
        time.sleep(0.5)
        rs = p.mj("reception", "list", "lab", "--receiver", "raw", "--all").get("receptions", [])
    dup = sum(int(r["counts"].get("filesDuplicate", 0)) for r in rs)
    imp_syntax = str(dcmread(implicit, stop_before_pixels=True).file_meta.TransferSyntaxUID)
    p.result("14 the same bytes again are counted a duplicate; the copy in %s (dcmconv, sent as it is with storescu -R) and the corrected copy are held as different_file"
             % imp_syntax, dup == 1 and len(mine) == 2 and n_imp == 1 and sha_file(os.path.join(root, held[u1]["storage_key"])) == first_sha and len(p.held()) == 3,
             "duplicates counted by the receptions: %d; different_file records of the instance: %d; the archived object unchanged: %s; %d held"
             % (dup, len(mine), sha_file(os.path.join(root, held[u1]["storage_key"])) == first_sha, len(p.held())),
             ["receptions: %s" % [(r["step"][15:], r["counts"]) for r in rs], "quarantine: %s" % [(f["reason"], f.get("sopInstanceUid")) for f in diff],
              "storescu -R: %s" % out_imp[-200:]])


GROUPS = {"resolve": group_resolve, "references": group_references, "provenance": group_provenance}
