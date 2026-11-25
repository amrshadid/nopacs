"""The group of the proof of keeping the archive whole that says what a
client can rely on when a stored object is damaged: two 64 MiB synthetic
instances, one with a byte flipped near its start and one at its end, read
before any scrub has looked at them and again after a scrub has marked them,
by every path that hands out archived bytes: a whole download and a Range by
curl, a DICOMweb retrieve read by Python's multipart reader (the email
package), the ReadFiles stream, and a send to a DICOM peer of pynetdicom.

  client  a damaged instance before and after the scrub, on every read path
"""

import hashlib
import json
import os
from email.parser import BytesParser

from pydicom import dcmread
from pydicom.uid import generate_uid

from nopacs_scripts.interrupt import KeepingPeer
from nopacs_scripts.net import free_port
from nopacs_scripts.report import Failure
from nopacs_scripts.sections.whole import instance_route
from nopacs_scripts.wholemore import error_text, failures, read_files_raw, sidecar_lines, wait_job

ROWS, COLUMNS = 4096, 8192  # 16-bit pixels: 64 MiB of pixel data
CURL_SHORT = 18  # curl's exit for a body short of its Content-Length


def big_instances(corpus, out_dir, n=2):
    """n instances of one new series, each with 64 MiB of synthetic pixel
    data and its own SOP Instance UID, written by pydicom."""
    os.makedirs(out_dir, exist_ok=True)
    study, series = generate_uid(), generate_uid()
    pixels = bytes(range(256)) * (ROWS * COLUMNS * 2 // 256)
    out = []
    for i in range(n):
        ds = dcmread(os.path.join(corpus, "CT_small.dcm"))
        ds.StudyInstanceUID, ds.SeriesInstanceUID = study, series
        ds.SOPInstanceUID = ds.file_meta.MediaStorageSOPInstanceUID = generate_uid()
        ds.PatientName, ds.PatientID, ds.InstanceNumber = "Big^Seven", "BIG-7", i + 1
        ds.Rows, ds.Columns, ds.BitsAllocated, ds.BitsStored, ds.HighBit, ds.SamplesPerPixel = ROWS, COLUMNS, 16, 16, 15, 1
        ds.PixelData = pixels
        path = os.path.join(out_dir, "big-%d.dcm" % i)
        ds.save_as(path, write_like_original=False)
        out.append(path)
    return out


def flip(path, offset):
    with open(path, "r+b") as f:
        f.seek(offset)
        b = f.read(1)
        f.seek(offset)
        f.write(bytes([b[0] ^ 0xFF]))


def wado(p, r):
    """A WADO-RS retrieve of one instance by curl, the body read by Python's
    multipart reader: (HTTP status, curl's exit, bytes, the reader's defects, parts read)."""
    code, hdr, body, rc = p.curl("/tenants/lab/dicom-web/studies/%s/series/%s/instances/%s" % (r["study"], r["series"], r["uid"]),
                                 "-H", 'Accept: multipart/related; type="application/dicom"')
    ct = hdr.get("content-type", "")
    defects, parts = [], 0
    if code == 200 and ct.startswith("multipart/"):
        msg = BytesParser().parsebytes(b"Content-Type: " + ct.encode() + b"\r\n\r\n" + body)
        defects = sorted({type(d).__name__ for d in msg.defects})
        parts = len(msg.get_payload()) if msg.is_multipart() else 0
    return code, rc, body, defects, parts


def send_to_peer(p, subject):
    """A send job of the subject to a pynetdicom peer: (the job, its failures, the UIDs the peer holds)."""
    first = not getattr(p, "peer_port", 0)
    if first:
        p.peer_port = free_port()
    peer = KeepingPeer(p.peer_port)
    try:
        if first:
            p.m("setting", "set", "sender.destinations", "127.0.0.1 ports=%d" % peer.port)
            p.m("sender", "add", "lab", "peer", "--type", "dimse", "--host", "127.0.0.1", "--port", str(peer.port), "--called-aet", "PEER",
                "--calling-aet", "NOPACS", "--enabled")
        jid = p.mj("job", "create", "lab", "--sender", "peer", subject)["job"]["id"]
        job = wait_job(p, "lab", jid, timeout=170)
        return job, failures(p, "lab", jid), set(peer.bytes)
    finally:
        peer.stop()


def ranges(p, r, size, pieces=4):
    """The instance fetched in pieces by Range requests (curl): (statuses, the assembled bytes, the ETag)."""
    step, statuses, out, etag = size // pieces + 1, [], b"", ""
    for first in range(0, size, step):
        code, hdr, body, _ = p.curl(instance_route(r), "-r", "%d-%d" % (first, min(size, first + step) - 1))
        statuses.append(code)
        out += body
        etag = hdr.get("etag", etag)
    return statuses, out, etag.strip('"')


def group_client(p):
    p.setup()
    p.m("setting", "set", "scrub.interval", "0", "--tenant", "lab")  # no scrub but the group's own
    files = big_instances(p.args.pydicom_corpus, os.path.join(p.work, "big"))
    n, out = p.storescu(p.raw_port, files)
    if n != 2 or p.wait_held(2, timeout=150) != 2:
        raise Failure("the two 64 MiB instances: %d of 2 answered success, %d held: %s" % (n, len(p.held()), out[-300:]))
    root = p.storage_root()
    rows = []
    for r in p.held():
        ds = dcmread(os.path.join(root, r["storage_key"]), stop_before_pixels=True)
        rows.append(dict(r, series=str(ds.SeriesInstanceUID), number=int(ds.InstanceNumber)))
    rows.sort(key=lambda r: r["number"])
    sc = sidecar_lines(root)
    start, end = rows[0], rows[1]
    p_start = os.path.join(root, start["storage_key"])
    p_end = os.path.join(root, end["storage_key"])
    pix = (sc.get(start["uid"]) or [{}])[-1].get("pixel_data") or {"offset": 1024}
    flip(p_start, pix["offset"] + 64)
    size = os.path.getsize(p_end)
    flip(p_end, size - 1)
    damaged = {start["uid"]: open(p_start, "rb").read(), end["uid"]: open(p_end, "rb").read()}
    p.result("16 two 64 MiB instances stored, one byte flipped near the start of one (in its first 64 KiB) and at the end of the other: hashlib differs from the catalog",
             all(hashlib.sha256(damaged[r["uid"]]).hexdigest() != r["sha256"] for r in rows) and size > 64 * 2**20,
             "sizes %d and %d bytes; flipped at byte %d and byte %d" % (len(damaged[start["uid"]]), size, pix["offset"] + 64, size - 1))
    # ---- before the scrub ----
    for name, r in (("near the start", start), ("at the end", end)):
        code, hdr, body, rc = p.curl(instance_route(r))
        cl = int(hdr.get("content-length", -1))
        p.result("16 not yet scrubbed, flipped %s: a whole download ends short (curl exit 18, fewer bytes than Content-Length), never a whole wrong body"
                 % name, code == 200 and rc == CURL_SHORT and 0 <= len(body) < cl and len(body) < len(damaged[r["uid"]]),
                 "HTTP %d, curl exit %d, %d of %d bytes" % (code, rc, len(body), cl))
        code, rc, body, defects, parts = wado(p, r)
        p.result("16 not yet scrubbed, flipped %s: a WADO-RS retrieve ends short, Python's multipart reader finds no closing boundary" % name,
                 code == 200 and rc == CURL_SHORT and "CloseBoundaryNotFoundDefect" in defects and len(body) < len(damaged[r["uid"]]),
                 "HTTP %d, curl exit %d, %d bytes, the reader's defects %s, %d parts" % (code, rc, len(body), defects or "none", parts))
    st, got, err, chunks = read_files_raw(p, {"tenant": "lab", "subjectLabel": start["subject"]})
    whole = [u for u, (_, b) in got.items() if len(b) == len(damaged.get(u, b""))]
    p.result("16 not yet scrubbed: the ReadFiles stream ends in an error, never handing a damaged instance whole", err is not None and not whole,
             "HTTP %d; %d files announced, %d read whole; %d chunks; the stream ended with %s" % (st, len(got), len(whole), chunks, " ".join(error_text(err).split())[:160]))
    job, fails, held = send_to_peer(p, start["subject"])
    p.result("16 not yet scrubbed: a send ends without the peer holding either instance (pynetdicom), the job naming both as failed",
             not (held & set(damaged)) and len(fails) == 2, "job %s; the peer holds %d of them; %d failures %s" % (job["state"].replace("JOB_STATE_", "").lower(),
                                                                                                   len(held & set(damaged)), len(fails), sorted({f.get("code") for f in fails})))
    statuses, assembled, etag = ranges(p, start, len(damaged[start["uid"]]))
    mid = len(damaged[start["uid"]]) // 2
    code, _, body, _ = p.curl(instance_route(start), "-r", "%d-%d" % (mid, mid + 65535))
    p.result("16 not yet scrubbed: a Range is answered unchecked as the contract says (206, the stored bytes), and the client's own check of what it assembles fails "
             "against the ETag", code == 206 and body == damaged[start["uid"]][mid:mid + 65536] and set(statuses) == {206}
             and hashlib.sha256(assembled).hexdigest() != etag == start["sha256"],
             "the middle 64 KiB: HTTP %d, equal to the stored bytes: %s; the whole in %d ranges %s: hashlib %s, ETag %s"
             % (code, body == damaged[start["uid"]][mid:mid + 65536], len(statuses), statuses, hashlib.sha256(assembled).hexdigest()[:12], etag[:12]))
    # ---- the scrub marks them ----
    sj = p.job("lab", "scrub", {"storage": "main"})
    found = {(f["kind"].replace("SCRUB_FINDING_KIND_", "").lower(), f["key"]) for f in p.findings()}
    p.result("16 a scrub marks both damaged", found == {("damaged", start["storage_key"]), ("damaged", end["storage_key"])},
             "job %s; findings %s" % (sj["state"].replace("JOB_STATE_", "").lower(), sorted((k, x[-16:]) for k, x in found)))
    # ---- after the scrub: refused before the first byte ----
    for name, r in (("near the start", start), ("at the end", end)):
        code, hdr, body, rc = p.curl(instance_route(r))
        code2, rc2, body2, _, _ = wado(p, r)
        code3, _, body3, _ = p.curl(instance_route(r), "-r", "%d-%d" % (mid, mid + 65535))
        refused = [(c, b) for c, b in ((code, body), (code2, body2), (code3, body3)) if c >= 500 and b"data_loss" in b and len(b) < 2000 and b"DICM" not in b]
        p.result("16 after the scrub, flipped %s: a whole download, a WADO-RS retrieve and a Range are each refused with data loss before any byte (curl)" % name,
                 len(refused) == 3, "download HTTP %d %s; WADO-RS HTTP %d %s; Range HTTP %d %s" % (code, body[:60], code2, body2[:60], code3, body3[:60]))
    st, got, err, chunks = read_files_raw(p, {"tenant": "lab", "subjectLabel": start["subject"]})
    p.result("16 after the scrub: ReadFiles refuses with data loss before the first chunk", err is not None and "data_loss" in error_text(err) and chunks == 0,
             "HTTP %d; %d files announced, %d chunks; %s" % (st, len(got), chunks, " ".join(error_text(err).split())[:160]))
    job, fails, held = send_to_peer(p, start["subject"])
    p.result("16 after the scrub: a send hands the peer nothing (pynetdicom), each instance a failure", not (held & set(damaged)) and len(fails) == 2,
             "job %s; the peer holds %d of them; failures %s" % (job["state"].replace("JOB_STATE_", "").lower(), len(held & set(damaged)),
                                                                json.dumps([{k: v for k, v in f.items() if k in ("code", "message")} for f in fails])[:240]))


GROUPS = {"client": group_client}
