"""The steps of the proof of de-identification that judge the forms a value
is written in: a name planted in a JPEG 2000 comment segment, a name written
decomposed (Unicode NFD) in a free text value, and a value pattern written
decomposed. deid.py runs them."""

import json
import os
import struct
import unicodedata

from pydicom import dcmread
from pydicom.encaps import encapsulate, generate_pixel_data_frame
from pydicom.uid import generate_uid

import deidkit

J2K_TEXT = b"PROVE J2K Comfield Jaypegina 20240317"
FAMILY = "Müller"  # the family name written composed (NFC)
# The name a profile's patterns are written with in 12.3 and 12.4. A profile
# keeps its patterns in the tenant's database as configuration, so a name
# planted in a file must not be the one a pattern holds, or the search of the
# databases for planted values would find the pattern.
PATTERN_FAMILY = "Büchner"


def with_j2k_comment(ds):
    """Puts a comment segment (COM, ISO 15444-1 A.9.2) holding a name and a
    date right after the SIZ segment of each frame's main header, and takes
    out the institution, whose word the kept manufacturer holds again."""
    del ds.InstitutionName
    n = int(ds.get("NumberOfFrames", 1) or 1)
    out = []
    for f in generate_pixel_data_frame(ds.PixelData, n):
        assert f[:4] == b"\xff\x4f\xff\x51", "not a bare JPEG 2000 codestream"
        end = 4 + struct.unpack(">H", f[4:6])[0]
        # Rcom 1 says the comment is Latin text.
        seg = b"\xff\x64" + struct.pack(">H", len(J2K_TEXT) + 4) + b"\x00\x01" + J2K_TEXT
        out.append(f[:end] + seg + f[end:])
    ds.PixelData = encapsulate(out)
    ds["PixelData"].is_undefined_length = True


def j2k_comment(p):
    """12.1: a JPEG 2000 file whose comment segment holds a name is archived
    with the name gone: pydicom decodes its pixels, equal to the ones sent
    (the syntax is lossless), and no byte of the file holds the comment."""
    d = p.build("j2k-comment")
    path = deidkit.rewrite(p.corpus("MR_small_jp2klossless.dcm"), os.path.join(d, "j2k-comment.dcm"), generate_uid(), generate_uid(), 1,
                           patient="Comfield^Jaypegina", patient_id="PRV-J2K-0001", extra=with_j2k_comment)[0]
    nd = deidkit.Needles()
    nd.from_file(path, "j2k")
    nd.add_text("COM text", J2K_TEXT.decode())
    nd.add_text("COM name", "Comfield Jaypegina")
    p.needles.from_file(path, "j2k")
    p.needles.add_text("COM text", J2K_TEXT.decode())
    with open(path, "rb") as f:
        sent_had = J2K_TEXT in f.read()
    before = p.receptions("deid")
    st = p.send("deid", [path])
    recs = p.settle("deid", before)
    rows = p.archived(recs)
    ok, detail = False, "not archived; held %s" % [(h["reason"], h["details"]) for h in p.held(recs)]
    if len(rows) == 1:
        got = p.storage_path(rows[0]["storage"], rows[0]["storage_key"])
        ds = dcmread(got)
        same = bool((ds.pixel_array == dcmread(path).pixel_array).all())
        with open(got, "rb") as f:
            found = nd.search(f.read())
        ok = sent_had and same and not found and str(ds.file_meta.TransferSyntaxUID) == "1.2.840.10008.1.2.4.90"
        detail = ("the comment was in the file sent: %s; archived, still JPEG 2000 Lossless; pydicom decodes %s pixels equal to the sent "
                  "ones: %s; %d values of the comment and the file's names found in its bytes" % (sent_had, ds.pixel_array.shape, same, len(found)))
    p.result("12.1 a name planted in a JPEG 2000 comment segment is gone from the archived file, whose pixels pydicom decodes unchanged",
             st == [0] and ok, "status %s; %s" % (st, detail))


def decomposed_name(p):
    """12.2: a family name written decomposed (u and a combining diaeresis)
    in a free text value is cut as its composed form: the file is archived,
    and neither written form is in any byte of it or in its text values."""
    d = p.build("nfd")
    nfd = unicodedata.normalize("NFD", FAMILY)
    assert nfd != FAMILY

    def plant(ds):
        ds.SpecificCharacterSet = "ISO_IR 192"
        ds.StudyDescription = "Follow-up of %s, seen twice" % nfd
        ds.ImageComments = "Patient %s asked to repeat the scan" % nfd

    path = deidkit.rewrite(p.corpus("CT_small.dcm"), os.path.join(d, "nfd.dcm"), generate_uid(), generate_uid(), 1,
                           patient="%s^Renee" % FAMILY, patient_id="PRV-NFD-0001", extra=plant)[0]
    nd = deidkit.Needles()
    nd.add_text("composed name", FAMILY)
    nd.add_text("decomposed name", nfd)
    for w in (FAMILY, nfd):
        p.needles.add_text("name in two forms", w)
    p.needles.from_file(path, "nfd")
    with open(path, "rb") as f:
        sent = f.read()
    before = p.receptions("deid")
    st = p.send("deid", [path])
    recs = p.settle("deid", before)
    rows = p.archived(recs)
    ok, detail = False, "not archived; held %s" % [(h["reason"], h["details"]) for h in p.held(recs)]
    if len(rows) == 1:
        got = p.storage_path(rows[0]["storage"], rows[0]["storage_key"])
        ds = dcmread(got)
        with open(got, "rb") as f:
            found = nd.search(f.read())
        texts = [unicodedata.normalize("NFC", str(ds.get(k, ""))) for k in ("StudyDescription", "ImageComments", "PatientName")]
        in_text = [t for t in texts if FAMILY in t]
        ok = bool(nd.search(sent)) and not found and not in_text
        detail = ("the name was in the file sent in both forms' bytes: %s; archived; found in its bytes %d; in its text values (read by "
                  "pydicom, NFC) %d; Study Description now %r" % (bool(nd.search(sent)), len(found), len(in_text), str(ds.get("StudyDescription", ""))))
    p.result("12.2 a family name written decomposed in a text value is cut as its composed form, the file archived", st == [0] and ok,
             "status %s; %s" % (st, detail))


def decomposed_pattern(p):
    """12.3: a value pattern and a redact pattern written decomposed are
    refused, each as bad_pattern, and make no version; the same pattern
    written composed is accepted."""
    nfd = unicodedata.normalize("NFD", PATTERN_FAMILY)

    def doc(pattern):
        return {"bases": ["annex-e/basic:2026d"], "rules": [
            {"name": "by-value", "selector": {"value_pattern": {"pattern": pattern, "search": True}}, "action": {"redact": {"pattern": pattern}},
             "reason": "a family name typed anywhere"}]}
    path = os.path.join(p.work, "decomposed-pattern.json")
    with open(path, "w") as f:
        json.dump(doc(nfd), f)
    out = p.run("profile", "create", "lab", "decomposed", "--document", path, "--note", "a pattern written decomposed")
    text = out.stdout + out.stderr
    listed = p.run("profile", "list", "lab", "--json").stdout
    p.result("12.3 a value pattern and a redact pattern written decomposed are refused as bad_pattern and make no version",
             out.returncode != 0 and "bad_pattern" in text and "decomposed" not in listed,
             "exit %d; output %r; 'decomposed' among the profiles: %s" % (out.returncode, text.strip()[:200], "decomposed" in listed))
    ref = p.profile("composed", doc(PATTERN_FAMILY), "the same pattern written composed")
    p.result("12.4 the same pattern written composed is accepted", bool(ref), "created %s" % ref)


def run(p):
    j2k_comment(p)
    decomposed_name(p)
    decomposed_pattern(p)
