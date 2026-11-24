"""The steps of the proof of de-identification that judge rules beyond the
bases: pixel rules filling a band in every frame, JPEG comment segments
removed, routing by calling AE title to two storages under two versions,
and value patterns with redact. deid.py runs them."""

import json
import os
import subprocess

import numpy as np
from pydicom import dcmread
from pydicom.encaps import generate_pixel_data_frame
from pydicom.uid import generate_uid

from nopacs_scripts import deid as deidkit
from nopacs_scripts.sections.deid_faults import held_by_name


def pixel_rules(p, st):
    """3: the burned-in images of the main study have their band filled in
    every frame and the rest of each frame unchanged, compared by pydicom
    on the decoded pixels; the JPEG image has lost its comment segment and
    kept its image data."""
    for name in ("us-native", "us-rle"):
        if name not in st["arch"]:
            p.result("3 %s: the band is filled in every frame" % name, False, "not archived: %s" % (st["held"].get(name),))
            continue
        r, ds = st["arch"][name]
        orig = dcmread(st["files"][name])
        frames, inside, outside = deidkit.pixel_rule_holds(orig, ds)
        ts = str(ds.file_meta.TransferSyntaxUID)
        p.result("3 %s (%s as sent): the band is filled in every frame, the rest unchanged" % (name, orig.file_meta.TransferSyntaxUID.name),
                 frames == 3 and inside and outside and ts == "1.2.840.10008.1.2.1" and ds.BurnedInAnnotation == "NO",
                 "%d frames decoded by pydicom: inside the 64x12 band all 0: %s; outside it equal to the original: %s; written as %s; "
                 "Burned In Annotation %s" % (frames, inside, outside, ds.file_meta.TransferSyntaxUID.name, ds.get("BurnedInAnnotation")))
    if "jpeg-com" not in st["arch"]:
        p.result("3 jpeg-com: the comment segment is removed", False, "not archived: %s" % (st["held"].get("jpeg-com"),))
        return
    r, ds = st["arch"]["jpeg-com"]
    orig = dcmread(st["files"]["jpeg-com"])
    had = [deidkit.jpeg_markers(f) for f in generate_pixel_data_frame(orig.PixelData, 1)]
    has = [deidkit.jpeg_markers(f) for f in generate_pixel_data_frame(ds.PixelData, 1)]
    path = p.storage_path(r["storage"], r["storage_key"])
    with open(path, "rb") as f:
        text_gone = b"PROVE COMMENT" not in f.read()
    out = os.path.join(p.work, "jpeg-com-decoded.dcm")
    dj = subprocess.run([p.dcmtk("dcmdjpeg"), path, out], capture_output=True, text=True, timeout=60)
    same = bool(np.array_equal(orig.pixel_array, ds.pixel_array))
    p.result("3 jpeg-com: the JPEG comment segment is removed, the image data kept",
             any(0xFE in m for m in had) and not any(0xFE in m for m in has) and text_gone and dj.returncode == 0 and same,
             "markers before the scan, sent %s, archived %s; the comment's text in the archived file: %s; dcmdjpeg decodes it: exit %d; "
             "pydicom's decoded pixels equal the original's: %s" % (["%02X" % x for x in had[0]], ["%02X" % x for x in has[0]],
                                                                   "absent" if text_gone else "present", dj.returncode, same))


def routing(p):
    """4: one receiver routes studies by calling AE title to two storages
    under two profile versions; a late arrival of a session, from the other
    title, follows its session."""
    d = p.build("routing")
    src = p.corpus("CT_small.dcm")
    sa, sb, series_a = generate_uid(), generate_uid(), generate_uid()
    a = [deidkit.rewrite(src, os.path.join(d, "a%d.dcm" % i), sa, series_a, i + 1, patient="Routa^Sitea", patient_id="PRV-ROUTE-A")[0]
         for i in range(2)]
    b = [deidkit.rewrite(src, os.path.join(d, "b%d.dcm" % i), sb, generate_uid(), i + 1, patient="Routb^Siteb", patient_id="PRV-ROUTE-B")[0]
         for i in range(2)]
    late = deidkit.rewrite(src, os.path.join(d, "late.dcm"), sa, series_a, 3, patient="Routa^Sitea", patient_id="PRV-ROUTE-A")[0]
    for f in a + b + [late]:
        p.needles.from_file(f, "routing")
    before = p.receptions("routed")
    st = p.send("routed", a, aet="SITE_A") + p.send("routed", b, aet="SITE_B")
    recs = p.settle("routed", before)
    rows = p.archived(recs)

    def fate(rows):
        out = []
        for r in rows:
            ds = p.read_archived(r)
            method = "\\".join(ds.get("DeidentificationMethod", []))
            out.append((r["storage"], "research@1" if "research@1" in method else "basic@1" if "basic@1" in method else method[:30],
                        str(ds.get("StudyDate", "")), r["session"]))
        return out
    got = fate(rows)
    on_a = [g for g in got if g[0] == "a"]
    on_b = [g for g in got if g[0] == "b"]
    good = (st == [0] * 4 and len(on_a) == 2 and len(on_b) == 2 and all(g[1] == "research@1" and g[2] for g in on_a)
            and all(g[1] == "basic@1" and g[2] == "" for g in on_b))
    p.result("4.1 studies from SITE_A and SITE_B land on storages a and b, under research@1 and basic@1", good,
             "storage a: %s; storage b: %s (research@1 shifts the study date, basic@1 empties it)"
             % ([g[1:3] for g in on_a], [g[1:3] for g in on_b]))
    session_a = on_a[0][3] if on_a else None
    before = p.receptions("routed")
    st = p.send("routed", [late], aet="SITE_B")
    recs = p.settle("routed", before)
    got = fate(p.archived(recs))
    held = held_by_name(p, recs, {"late": late})
    p.result("4.2 a late arrival of the SITE_A session, sent as SITE_B, follows its session to storage a under research@1",
             st == [0] and len(got) == 1 and got[0][0] == "a" and got[0][1] == "research@1" and got[0][3] == session_a,
             "status %s; archived %s; held %s; the session's own %s" % (st, [(g[0], g[1], g[3]) for g in got],
                                                                      [(h["reason"], h["details"]) for h in held.values()], session_a))


def patterns(p):
    """5: a value pattern and redact cut a record number from the study
    description and from a value no base names, keeping the rest of the
    text; a copy of the number kept elsewhere holds the file."""
    d = p.build("patterns")
    src = p.corpus("CT_small.dcm")

    def mrn(ds):
        ds.StudyDescription = "HEAD MRN482913 FOLLOW"
        ds.ConvolutionKernel = "STD MRN482913"

    def copy(ds):
        mrn(ds)
        ds.SeriesDescription = "MRN482913 AXIAL"
    cut = deidkit.rewrite(src, os.path.join(d, "cut.dcm"), generate_uid(), generate_uid(), 1, patient="Patterna^Recordia",
                          patient_id="PRV-MRN-0001", extra=mrn)[0]
    kept = deidkit.rewrite(src, os.path.join(d, "kept.dcm"), generate_uid(), generate_uid(), 1, patient="Patterna^Copyola",
                           patient_id="PRV-MRN-0002", extra=copy)[0]
    for f in (cut, kept):
        p.needles.from_file(f, "patterns")
    p.needles.add_text("record number", "MRN482913")
    before = p.receptions("mrn")
    st = p.send("mrn", [cut, kept])
    recs = p.settle("mrn", before)
    rows = p.archived(recs)
    held = held_by_name(p, recs, {"cut": cut, "kept": kept})
    ds = p.read_archived(rows[0]) if len(rows) == 1 else None
    data = b""
    if ds is not None:
        with open(p.storage_path(rows[0]["storage"], rows[0]["storage_key"]), "rb") as f:
            data = f.read()
    good = (ds is not None and "MRN482913" not in str(ds.get("StudyDescription")) and str(ds.get("StudyDescription")).split() == ["HEAD", "FOLLOW"]
            and str(ds.get("ConvolutionKernel")).strip() == "STD" and b"MRN482913" not in data)
    p.result("5.1 redact cuts the record number from the study description and, by a value pattern, from a value no base names",
             st == [0, 0] and good, "archived %d; Study Description %r, Convolution Kernel %r; the number in the file's bytes: %s"
             % (len(rows), str(ds.get("StudyDescription")) if ds else "-", str(ds.get("ConvolutionKernel")) if ds else "-",
                "absent" if b"MRN482913" not in data else "present"))
    h = held.get("kept")
    det = json.loads(h["details"]) if h else {}
    p.result("5.2 a copy of the cut number kept in the series description holds the file", h is not None and h["reason"] == "residual_value"
             and det.get("path") == "0008103E", "record %s %s" % (h["reason"] if h else "-", json.dumps(det)))
    ex = p.mj("profile", "explain", "lab", "--profile", "research@2", cut)
    by = {e["keyword"]: (e["action"], e.get("ruleName")) for e in ex["elements"]}
    p.result("5.3 profile explain shows the value pattern deciding the convolution kernel and the keyword rule the study description",
             by.get("ConvolutionKernel") == ("redact", "mrn-anywhere") and by.get("StudyDescription") == ("redact", "mrn-study"),
             "Convolution Kernel %s, Study Description %s, Series Description %s" % (by.get("ConvolutionKernel"), by.get("StudyDescription"),
                                                                                   by.get("SeriesDescription")))
