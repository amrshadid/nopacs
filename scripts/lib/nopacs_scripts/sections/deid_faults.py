"""The steps of the proof of de-identification that judge failure: every
way a file is held instead of archived, each with its reason and its bytes
kept, the releases that then archive it, and the server killed with signal
9 while it archives. deid.py runs them."""

import json
import os
import time

from pydicom import dcmread
from pydicom.uid import generate_uid

from nopacs_scripts import deid as deidkit
from nopacs_scripts.send import split_part10


def ct(p, d, name, n, patient, patient_id, extra=None):
    """A copy of pydicom's CT_small in a study of its own."""
    path, _ = deidkit.rewrite(p.corpus("CT_small.dcm"), os.path.join(d, name + ".dcm"), generate_uid(),
                              generate_uid(), n, patient=patient, patient_id=patient_id, extra=extra)
    return path


def kept(p, row, sent):
    """Whether a held file's bytes are kept: its record says so, and the
    file in the incoming area is the one sent, its data set compared by
    pydicom and byte for byte."""
    path = p.incoming_path(row)
    if not row["content_kept"] or not os.path.exists(path):
        return False, "not kept"
    with open(path, "rb") as f:
        mine = f.read()
    with open(sent, "rb") as f:
        theirs = f.read()
    same = dcmread(path) == dcmread(sent)
    exact = split_part10(mine)[1] == split_part10(theirs)[1]
    return same, "pydicom-equal %s, data set byte-equal %s" % (same, exact)


def held_by_name(p, recs, sent):
    """{case: quarantine row} for the receptions given, each row matched to
    the file sent by the original SOP Instance UID in its kept bytes."""
    by_sop = {deidkit.sop_of(path): name for name, path in sent.items()}
    out = {}
    for h in p.held(recs):
        try:
            out[by_sop.get(deidkit.sop_of(p.incoming_path(h)), h["id"])] = h
        except (OSError, AttributeError):
            out[h["id"]] = h
    return out


def release(p, receiver, qid, *flags):
    """Releases a held file and waits until what it started has ended;
    returns the receptions it made."""
    before = p.receptions(receiver)
    out = p.m("quarantine", "release", "lab", qid, "--reason", "the proof releases it", *flags)
    recs = p.settle(receiver, before)
    return recs, out.strip().splitlines()[0] if out.strip() else ""


# Each case: the file, the reason and the detail expected.
CASES = [
    ("residual", "residual_value", ("kind", "name")),
    ("refused", "profile_refused", ("rule", "refuse-flagged")),
    ("burned", "burned_in_risk", ("cause", "burned_in_annotation")),
    ("no-subject", "deid_failed", ("cause", "no_subject")),
    ("outside", "deid_failed", ("cause", "pixel_region_outside")),
    ("frames", "deid_failed", ("cause", "frames_mismatch")),
]


def fail_closed(p):
    """2: each way de-identification fails closed holds the file with its
    reason and keeps its bytes; none is archived. The holds a setting or a
    person can lift are then released and archived de-identified. Returns
    the held records by case."""
    d = p.build("faults")

    def residual(ds):
        ds.ManufacturerModelName = "Residua Namesake 3000"

    def refused(ds):
        ds.private_block(0x0029, "PROVE REFUSE", create=True).add_new(0x01, "LO", "do not archive")

    def no_subject(ds):
        del ds.PatientID

    def no_institution(ds):
        # Its institution, TOSHIBA, is a word of the manufacturer other
        # files keep; looked for everywhere at the end, it would be found
        # there.
        del ds.InstitutionName
    sent = {
        "residual": ct(p, d, "residual", 1, "Residua^Namesake", "PRV-FAULT-0001", residual),
        "refused": ct(p, d, "refused", 1, "Refusa^Flaggard", "PRV-FAULT-0002", refused),
        "burned": deidkit.burned_in(os.path.join(d, "burned.dcm"), generate_uid(), generate_uid(), 1, manufacturer="OTHER US",
                                    patient="Burnley^Inscribed", patient_id="PRV-FAULT-0003"),
        "no-subject": ct(p, d, "no-subject", 1, "Nobody^Withoutid", "PRV-FAULT-0004", no_subject),
        "outside": deidkit.burned_in(os.path.join(d, "outside.dcm"), generate_uid(), generate_uid(), 1, manufacturer="PROVE OUTSIDE",
                                     patient="Outsider^Farbeyond", patient_id="PRV-FAULT-0005"),
        "jpeg2000": deidkit.rewrite(p.corpus("MR_small_jp2klossless.dcm"), os.path.join(d, "jpeg2000.dcm"), generate_uid(),
                                    generate_uid(), 1, patient="Jaypeg^Twothousand", patient_id="PRV-FAULT-0006", extra=no_institution)[0],
        "frames": deidkit.burned_in(os.path.join(d, "frames.dcm"), generate_uid(), generate_uid(), 1, frames=2, extra_frames=1,
                                    patient="Framer^Toomany", patient_id="PRV-FAULT-0007"),
        "deflated": deidkit.rewrite(p.corpus("image_dfl.dcm"), os.path.join(d, "deflated.dcm"), generate_uid(), generate_uid(), 1,
                                    patient="Deflata^Zipped", patient_id="PRV-FAULT-0008")[0],
    }
    for path in sent.values():
        p.needles.from_file(path, "fault")
    # The file with no patient ID goes to a receiver whose subject label is
    # made from the study, since the default label, from the patient's ID,
    # would hold it first as bad_label.
    before, before_study = p.receptions("deid"), p.receptions("bystudy")
    statuses = p.send("deid", [v for k, v in sent.items() if k != "no-subject"]) + p.send("bystudy", [sent["no-subject"]])
    recs = p.settle("deid", before) | p.settle("bystudy", before_study)
    held = held_by_name(p, recs, sent)
    archived = p.archived(recs)
    for name, reason, (key, want) in CASES:
        h = held.get(name)
        details = json.loads(h["details"] or "{}") if h else {}
        good_bytes, how = kept(p, h, sent[name]) if h else (False, "no record")
        p.result("2 %s: held as %s (%s %s), its bytes kept" % (name, reason, key, want),
                 h is not None and h["reason"] == reason and details.get(key) == want and h["state"] == "held" and good_bytes,
                 "record: %s %s, state %s; %s" % (h["reason"] if h else "-", json.dumps(details) if h else "-", h["state"] if h else "-", how))
    h = held.get("deflated")
    p.note("a deflated file (pydicom's image_dfl.dcm, Deflated Explicit VR Little Endian): %s"
           % ("held as %s %s" % (h["reason"], h["details"]) if h else "archived" if len(archived) else "neither held nor archived"))
    # The JPEG 2000 file is walked (its comment, CPF and CRG segments removed)
    # and archived; the only archived file of this send.
    j2k = [r for r in archived if str(dcmread(p.storage_path(r["storage"], r["storage_key"]), stop_before_pixels=True).file_meta
                                       .TransferSyntaxUID) == JPEG2000_LOSSLESS]
    p.result("2 none of the files that fail is archived", len(archived) == len(j2k) == 1 and all(s == 0 for s in statuses),
             "%d sent, each acknowledged: %s; archived: %d (the JPEG 2000 file alone); held: %d"
             % (len(sent), all(s == 0 for s in statuses), len(archived), len(held)))
    jpeg2000_archived(p, sent["jpeg2000"], j2k)
    too_large(p, d)
    unrouted(p, d)
    recorded_as_received(p, d)
    return held


JPEG2000_LOSSLESS = "1.2.840.10008.1.2.4.90"


def jpeg2000_archived(p, sent, rows):
    """A JPEG 2000 file is archived, not held: pydicom decodes the archived
    pixels, which equal the ones sent (the syntax is lossless), and neither the
    name nor the ID sent is in any byte of the file."""
    nd = deidkit.Needles()
    nd.from_file(sent, "jpeg2000")
    ok, detail = False, "not archived"
    if len(rows) == 1:
        path = p.storage_path(rows[0]["storage"], rows[0]["storage_key"])
        ds = dcmread(path)
        same = bool((ds.pixel_array == dcmread(sent).pixel_array).all())
        with open(path, "rb") as f:
            found = nd.search(f.read())
        ok = same and not found and not str(ds.get("PatientName", ""))
        detail = "archived; pydicom decodes %s pixels equal to the sent ones: %s; %d sent values found in its bytes; Patient's Name %r" % (
            ds.pixel_array.shape, same, len(found), str(ds.get("PatientName", "")))
    p.result("2 jpeg2000: archived (its codestream walked), decodes in pydicom, carries no name", ok, detail)


def too_large(p, d):
    """pixels_too_large: an image a pixel rule fills, larger decoded than
    deid.pixel-limit, is held; once the setting is raised, its release
    archives it with the band filled in every frame."""
    path = deidkit.burned_in(os.path.join(d, "large.dcm"), generate_uid(), generate_uid(), 1, frames=8, rows=512, cols=512,
                             patient="Largely^Toobig", patient_id="PRV-FAULT-0009")
    p.needles.from_file(path, "large")
    p.setting("deid.pixel-limit", "1MiB")
    try:
        before = p.receptions("deid")
        p.send("deid", [path])
        recs = p.settle("deid", before)
        h = held_by_name(p, recs, {"large": path}).get("large")
        details = json.loads(h["details"] or "{}") if h else {}
        good_bytes, how = kept(p, h, path) if h else (False, "no record")
        p.result("2 pixels_too_large: an image of 2 MiB decoded under a limit of 1 MiB is held, its bytes kept",
                 h is not None and h["reason"] == "deid_failed" and details.get("cause") == "pixels_too_large" and good_bytes,
                 "record: %s %s; %s" % (h["reason"] if h else "-", json.dumps(details), how))
    finally:
        p.setting("deid.pixel-limit", "1GiB")
    if not h:
        return
    recs, said = release(p, "deid", h["id"])
    rows = p.archived(recs)
    ok, detail = False, "not archived"
    if rows:
        frames, inside, outside = deidkit.pixel_rule_holds(dcmread(path), p.read_archived(rows[0]))
        ok = frames == 8 and inside and outside
        detail = "archived; %d frames compared by pydicom: band filled %s, the rest unchanged %s" % (frames, inside, outside)
    p.result("2 pixels_too_large released after deid.pixel-limit is raised to 1 GiB: archived, filled", ok, "release: %s; %s" % (said, detail))


def unrouted(p, d):
    """unrouted: a study no route of the receiver chooses is held, every
    file; a release naming a storage archives it there."""
    a = ct(p, d, "unrouted", 1, "Unrouta^Nowhere", "PRV-FAULT-0010")
    p.needles.from_file(a, "unrouted")
    before = p.receptions("routed")
    p.send("routed", [a], aet="OTHER")
    recs = p.settle("routed", before)
    h = held_by_name(p, recs, {"unrouted": a}).get("unrouted")
    details = json.loads(h["details"] or "{}") if h else {}
    good_bytes, how = kept(p, h, a) if h else (False, "no record")
    p.result("2 unrouted: a study no route chooses is held, its bytes kept",
             h is not None and h["reason"] == "unrouted" and details.get("cause") == "no_match" and good_bytes and not p.archived(recs),
             "calling AE OTHER; record: %s %s; %s" % (h["reason"] if h else "-", json.dumps(details), how))
    if not h:
        return
    recs, said = release(p, "routed", h["id"], "--storage", "b")
    rows = p.archived(recs)
    ds = p.read_archived(rows[0]) if rows else None
    p.result("2 unrouted released naming storage b: archived there, de-identified by the receiver's version",
             bool(rows) and rows[0]["storage"] == "b" and rows[0]["version"] == p.versions["research@1"] and ds.PatientIdentityRemoved == "YES",
             "release: %s; archived %d on storage %s, version id %s (research@1 is %d), Patient Identity Removed %s"
             % (said, len(rows), rows[0]["storage"] if rows else "-", rows[0]["version"] if rows else "-", p.versions["research@1"],
                ds.get("PatientIdentityRemoved") if ds else "-"))


def recorded_as_received(p, d):
    """recorded_as_received: a file that arrived while its receiver archived
    as received, whose receiver de-identifies before the file is archived,
    is held; its release de-identifies it."""
    path = ct(p, d, "flipped", 1, "Flipped^Recorda", "PRV-FAULT-0011", deidkit.foreign)
    before = p.receptions("flip")
    st = p.send("flip", [path])
    p.m("receiver", "set", "lab", "flip", "--profile", "research@1")
    p.wait_up("flip")
    recs = p.settle("flip", before, timeout=240)
    h = held_by_name(p, recs, {"flipped": path}).get("flipped")
    details = json.loads(h["details"] or "{}") if h else {}
    good_bytes, how = kept(p, h, path) if h else (False, "no record")
    p.result("2 recorded_as_received: a file received as received, its receiver set to de-identify before archiving, is held",
             st == [0] and h is not None and h["reason"] == "deid_failed" and details.get("cause") == "recorded_as_received" and good_bytes
             and not p.archived(recs), "status %s; record: %s %s; %s; archived %d"
             % (st, h["reason"] if h else "-", json.dumps(details), how, len(p.archived(recs))))
    if not h:
        return
    recs, said = release(p, "flip", h["id"])
    rows = p.archived(recs)
    found = []
    ds = None
    if rows:
        with open(p.storage_path(rows[0]["storage"], rows[0]["storage_key"]), "rb") as f:
            nd = deidkit.Needles()
            nd.from_file(path)
            found = nd.search(f.read())
        ds = p.read_archived(rows[0])
    p.result("2 recorded_as_received released: archived de-identified", bool(rows) and not found and ds.PatientIdentityRemoved == "YES",
             "release: %s; archived %d, Patient Identity Removed %s, values of the original found %d"
             % (said, len(rows), ds.get("PatientIdentityRemoved") if ds else "-", len(found)), found)


def crash(p, rounds=3):
    """8: the server killed with signal 9 while a de-identifying receiver
    archives, then started again: every acknowledged file is archived once
    or held, and none archived identified."""
    bad, kills, caught = [], 0, 0
    for r in range(rounds):
        d = p.build("crash%d" % r)
        study, series = generate_uid(), generate_uid()
        src = p.corpus("CT_small.dcm")
        files = [deidkit.rewrite(src, os.path.join(d, "%03d.dcm" % i), study, series, i + 1, patient="Crashley^Kilroy%d" % r,
                                 patient_id="PRV-CRASH-%04d" % r)[0] for i in range(40)]
        nd = deidkit.Needles()
        for f in files:
            nd.from_file(f, "crash")
            p.needles.from_file(f, "crash")
        before = p.receptions("deid")
        statuses = p.send("deid", files)
        deadline = time.time() + 30
        state = None
        while time.time() < deadline:
            rows = [x for x in p.query("SELECT id, state FROM receptions WHERE receiver = 'deid'") if x["id"] not in before]
            state = rows[0]["state"] if rows else None
            if state not in (None, "receiving", "complete"):
                break
            time.sleep(0.005)
        caught += state == "identified"
        p.kill()
        kills += 1
        p.serve()
        p.wait_up("deid")
        recs = p.settle("deid", before)
        acked = [i + 1 for i, s in enumerate(statuses) if s == 0]
        arch = p.archived(recs)
        numbers, leaks = [], []
        for row in arch:
            path = p.storage_path(row["storage"], row["storage_key"])
            numbers.append(int(dcmread(path, stop_before_pixels=True).InstanceNumber))
            with open(path, "rb") as f:
                leaks += nd.search(f.read())
        held = [int(dcmread(p.incoming_path(h), stop_before_pixels=True).InstanceNumber) for h in p.held(recs)]
        lost = [n for n in acked if n not in numbers and n not in held]
        twice = len(numbers) - len(set(numbers))
        if lost or twice or leaks or len(acked) != 40:
            bad.append("round %d (killed at %s): %d acknowledged, %d archived (%d twice), %d held, lost %s, identified values found %s"
                       % (r + 1, state, len(acked), len(numbers), twice, len(held), lost[:5], leaks[:3]))
        p.note("round %d: killed while the reception was %s; %d acknowledged, then %d archived and %d held"
               % (r + 1, state, len(acked), len(numbers), len(held)))
    p.result("8 killed with signal 9 while archiving through a de-identifying receiver: each acknowledged file archived once or held, "
             "none identified", not bad and caught > 0, "%d kills and restarts, %d caught in the archive step" % (kills, caught), bad)
