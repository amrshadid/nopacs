"""The steps of the proof of de-identification that judge the main study:
no planted value survives, a trial run agrees with what was archived, the
archive passes its own check while a file edited on the storage does not,
a dry run changes nothing, a forward to Orthanc carries nothing, and no
planted value is anywhere else. deid.py runs them."""

import datetime
import glob
import hashlib
import http.client
import json
import os
import re
import subprocess
import time
import urllib.parse

from pydicom import dcmread
from pydicom.uid import generate_uid

from nopacs_scripts import deid as deidkit
from nopacs_scripts.deid import PATIENT_ID

# Public corpus files sent in the main study, from this repository's
# receive corpus or pydicom's test files: every storage class and syntax
# the corpus offers that a person might send, private sequences and UN
# among them.
CORPUS = ["MR_small.dcm", "MR_small_implicit.dcm", "MR_small_RLE.dcm", "MR_small_jpeg_ls_lossless.dcm", "rtplan.dcm",
          "rtstruct.dcm", "rtdose.dcm", "test-SR.dcm", "reportsi.dcm", "waveform_ecg.dcm", "examples_overlay.dcm",
          "liver_1frame.dcm", "nested_priv_SQ.dcm", "priv_SQ.dcm", "UN_sequence.dcm"]
# pydicom's character set files: a person's name in Arabic, French,
# German, Greek, Japanese (three ways), Hebrew, Korean, Russian and
# Chinese, in the character sets of PS3.5 Annex H to K.
CHARSETS = ["chrArab.dcm", "chrFren.dcm", "chrGerm.dcm", "chrGreek.dcm", "chrH31.dcm", "chrH32.dcm", "chrHbrw.dcm",
            "chrI2.dcm", "chrRuss.dcm", "chrX1.dcm", "chrX2.dcm", "chrJapMulti.dcm", "chrKoreanMulti.dcm"]
SYNTHETIC = ["rich", "rich-ref", "implicit-un", "us-native", "us-rle", "jpeg-com"]
# The codes (PS3.16 CID 7050) of the bases of nopacs/longitudinal:1: the
# basic profile, clean descriptors, retain longitudinal with modified
# dates, retain patient characteristics, retain device identity, retain
# safe private; 113101, clean pixel data, only where a pixel rule filled.
# The actions whose value comes from a tenant's keys, which a trial run
# replaces with its own.
KEYED = ("uid", "shift", "hash", "lookup", "remove_unless_required")
BINARY = ("US", "SS", "UL", "SL", "FL", "FD", "AT", "UV", "SV")
LONGITUDINAL_CODES = {"113100", "113105", "113107", "113108", "113109", "113111"}


DATE_HIT = re.compile(r"date \(b'(\d{8})'\)$")


def shifted_dates(arch):
    """The dates (YYYYMMDD) every archived file holds, read by pydicom."""
    out = set()
    for _, ds in arch.values():
        for e in deidkit.walk(ds):
            if e.VR in ("DA", "DT") and e.value:
                for v in (e.value if e.VM > 1 else [e.value]):
                    out.add(str(v)[:8])
    return out


def coincidences(hits, shifted):
    """hits without the dates an original of the study held and that a
    shifted date of another file now equals by chance (every date moves
    back by a random offset): a date found is a leak only when no archived
    file holds it as its own."""
    out = []
    for h in hits:
        m = DATE_HIT.search(h)
        if not (m and m.group(1) in shifted):
            out.append(h)
    return out


def unshifted_dates(arch, files):
    """What a coincidence would excuse, checked directly: every date of a
    file sent that the archived file still holds in the same element."""
    out = []
    for name, (_, ds) in arch.items():
        if name not in files:
            continue
        orig = dcmread(files[name], stop_before_pixels=True, force=True)
        for e in orig:
            if e.VR == "DA" and e.value and e.tag in ds and ds[e.tag].value == e.value:
                out.append("%s: %s is still %s" % (name, e.keyword, e.value))
    return out


def build_main(p):
    """The main study: the synthetic files with planted values, then the
    corpus files rewritten into it. Returns {name: path}."""
    d = p.build("main")
    study = generate_uid()
    ct_series = generate_uid()
    src = p.corpus("CT_small.dcm")
    files = {}
    rich, rich_uid = deidkit.rewrite(src, os.path.join(d, "rich.dcm"), study, ct_series, 1, extra=deidkit.plant)

    def ref(ds):
        deidkit.plant(ds)
        from pydicom.dataset import Dataset
        from pydicom.sequence import Sequence
        item = Dataset()
        item.ReferencedSOPClassUID = ds.SOPClassUID
        item.ReferencedSOPInstanceUID = rich_uid
        ds.ReferencedInstanceSequence = Sequence([item])
    files["rich"] = rich
    files["rich-ref"] = deidkit.rewrite(src, os.path.join(d, "rich-ref.dcm"), study, ct_series, 2, extra=ref)[0]
    files["implicit-un"] = deidkit.implicit_private(os.path.join(d, "implicit-un.dcm"), study, generate_uid(), 3)
    files["us-native"] = deidkit.burned_in(os.path.join(d, "us-native.dcm"), study, generate_uid(), 4)
    files["us-rle"] = deidkit.burned_in(os.path.join(d, "us-rle.dcm"), study, generate_uid(), 5, rle=True)
    files["jpeg-com"] = deidkit.with_comment(p.corpus("SC_rgb_jpeg_dcmtk.dcm"),
                                             os.path.join(d, "jpeg-com.dcm"), study, generate_uid(), 6)
    n = 7
    for name in CORPUS:
        files[name] = deidkit.rewrite(p.corpus(name), os.path.join(d, name), study, generate_uid(), n)[0]
        n += 1
    for name in CHARSETS:
        files[name] = deidkit.rewrite(p.corpus(name), os.path.join(d, name), study, generate_uid(), n, keep_names=True)[0]
        n += 1
    return files, study


def needles_of(files):
    """The values to look for: every identifying value of each file sent,
    and the planted text no element of a standard name holds."""
    nd = deidkit.Needles()
    for name, path in files.items():
        nd.from_file(path, name)
    nd.add_name("private LO", deidkit.PRIVATE_NAME)
    nd.add_text("UN private", deidkit.UN_TEXT)
    nd.add_text("JPEG comment", deidkit.COM_TEXT.decode())
    nd.add_text("JPEG comment", "PROVE COMMENT")
    nd.add_text("patient ID in a private element", PATIENT_ID)
    for k in ("PatientAddress", "InstitutionAddress", "PatientTelephoneNumbers"):
        nd.add_text(k, deidkit.PLANTED[k])
    return nd


def no_value_survives(p):
    """1: the main study through the de-identifying receiver: every planted
    value absent from every archived file, sidecar and manifest; UIDs
    consistent and new; the method record right."""
    files, study = build_main(p)
    nd = needles_of(files)
    for name, path in files.items():
        p.needles.from_file(path, name)
    for k in ("PatientBirthDate", "StudyDate"):
        p.needles.add_date(k, deidkit.PLANTED[k])
    for k in ("PatientAddress", "InstitutionAddress", "PatientTelephoneNumbers", "InstitutionName"):
        p.needles.add_text(k, deidkit.PLANTED[k])
    p.needles.add_name("private LO", deidkit.PRIVATE_NAME)
    p.needles.add_text("UN private", deidkit.UN_TEXT)
    p.needles.add_text("JPEG comment", "PROVE COMMENT")
    by_number = {int(dcmread(f, stop_before_pixels=True, force=True).InstanceNumber): name for name, f in files.items()}
    by_sop = {deidkit.sop_of(f): name for name, f in files.items()}
    before = p.receptions("deid")
    order = list(files)
    statuses = p.send("deid", [files[n] for n in order])
    recs = p.settle("deid", before)
    rows = p.archived(recs)
    held = p.held(recs)
    arch = {}
    for r in rows:
        ds = p.read_archived(r)
        arch[by_number.get(int(ds.get("InstanceNumber", 0) or 0), r["uid"])] = (r, ds)
    held_names = {}
    for h in held:
        try:
            held_names[by_sop.get(deidkit.sop_of(p.incoming_path(h)), h["id"])] = (h["reason"], json.loads(h["details"] or "{}"))
        except (OSError, ValueError, AttributeError) as e:
            held_names[h["id"]] = (h["reason"], {"unread": str(e)})
    acked = sum(1 for s in statuses if s == 0)
    synthetic_ok = [n for n in SYNTHETIC if n in arch]
    # rtplan.dcm alone is held: its institution, "Here", is a whole word of
    # the Manufacturer the profile keeps ("Manufacturer name here").
    rtplan = held_names.get("rtplan.dcm", ("", {}))
    expected_hold = (set(held_names) == {"rtplan.dcm"} and rtplan[0] == "residual_value"
                     and rtplan[1].get("kind") == "identifier" and rtplan[1].get("path") == "00080070")
    p.result("1.1 the main study is acknowledged and each file archived or held, the synthetic files archived, rtplan.dcm alone "
             "held", acked == len(files) and len(arch) + len(held_names) == len(files) and len(synthetic_ok) == len(SYNTHETIC)
             and expected_hold,
             "%d files sent (%d synthetic, %d corpus, %d character sets), %d acknowledged; %d archived, %d held: %s"
             % (len(files), len(SYNTHETIC), len(CORPUS), len(CHARSETS), acked, len(arch), len(held_names),
                ", ".join("%s %s/%s" % (n, r, d.get("cause") or d.get("kind") or d.get("rule") or "") for n, (r, d) in sorted(held_names.items()))),
             ["statuses %s" % statuses, "synthetic archived: %s" % synthetic_ok])

    # 1.2 no planted value in any archived file, read whole, pixel data and
    # file meta included; each file read by pydicom and dcmdump.
    hits, unreadable, warned = [], [], []
    for name, (r, ds) in sorted(arch.items()):
        path = p.storage_path(r["storage"], r["storage_key"])
        with open(path, "rb") as f:
            data = f.read()
        hits += ["%s: %s" % (name, h) for h in nd.search(data)]
        code, _, bad = p.dcmdump(path)
        _, _, was = p.dcmdump(files[name]) if name in files else (0, "", [])
        new = sorted(set(b.split(":", 1)[1].strip()[:80] for b in bad) - set(b.split(":", 1)[1].strip()[:80] for b in was))
        if code != 0:
            unreadable.append("%s: dcmdump exit %d" % (name, code))
        elif new:
            warned.append("%s: %s" % (name, new[:2]))
    shifted = shifted_dates(arch)
    hits = coincidences(hits, shifted) + unshifted_dates(arch, files)
    p.result("1.2 no planted value is in any archived file, and each reads in pydicom and dcmdump", not hits and not unreadable
             and not warned, "%d values in %d written forms looked for in %d archived files (whole bytes): %d found; dcmdump +E reads "
             "every file: %s; warnings the original had not: %d" % (len(set(l for l, _ in nd.forms.values())), len(nd), len(arch),
                                                                   len(hits), "yes" if not unreadable else "no", len(warned)),
             hits + unreadable + warned)

    # 1.3 sidecars and manifests of the study's scans.
    scans = {(r["storage"], r["subject"], r["session"], r["scan"]) for r, _ in arch.values()}
    side_hits, missing = [], []
    for storage, sub, ses, scan in sorted(scans):
        for name in ("manifest.json", "metadata.json"):
            path = p.storage_path(storage, "/".join(["meta", sub, ses, scan, name]))
            deadline = time.time() + 60
            while not os.path.exists(path) and time.time() < deadline:
                time.sleep(0.5)
            if not os.path.exists(path):
                missing.append(path)
                continue
            with open(path, "rb") as f:
                side_hits += ["%s %s: %s" % (scan, name, h) for h in nd.search(f.read())]
    side_hits = coincidences(side_hits, shifted)
    p.result("1.3 no planted value is in a scan's manifest or sidecar", not side_hits and not missing,
             "%d scans, %d manifests and sidecars read: %d found, %d missing" % (len(scans), 2 * len(scans) - len(missing),
                                                                               len(side_hits), len(missing)), side_hits + missing)

    # 1.4 UIDs: one new study UID; the CT series one series; the frame of
    # reference shared; a reference between files kept pointing at the
    # archived instance; every UID new.
    studies = {str(ds.StudyInstanceUID) for _, ds in arch.values()}
    rich, ref = arch.get("rich", (None, None))[1], arch.get("rich-ref", (None, None))[1]
    ok_ref = bool(rich and ref and str(ref.ReferencedInstanceSequence[0].ReferencedSOPInstanceUID) == str(rich.SOPInstanceUID))
    same_series = bool(rich and ref and rich.SeriesInstanceUID == ref.SeriesInstanceUID)
    same_for = bool(rich and ref and rich.FrameOfReferenceUID == ref.FrameOfReferenceUID
                    and str(rich.FrameOfReferenceUID) != str(dcmread(files["rich"], stop_before_pixels=True).FrameOfReferenceUID))
    new_form = all(str(ds.SOPInstanceUID).startswith("2.25.") and str(ds.file_meta.MediaStorageSOPInstanceUID) == str(ds.SOPInstanceUID)
                   for _, ds in arch.values())
    subjects = {r["subject"] for r, _ in arch.values()}
    p.result("1.4 UIDs are remapped and consistent across the study", len(studies) == 1 and study not in studies and ok_ref
             and same_series and same_for and new_form and len(subjects) == 1,
             "%d study UID(s) among %d files (%s, the original %s); the two CT slices one series: %s; their frame of reference shared "
             "and new: %s; rich-ref's Referenced SOP Instance UID is rich's archived UID: %s; every SOP Instance UID 2.25. and equal to "
             "the meta group's: %s; subjects: %s" % (len(studies), len(arch), sorted(studies)[0][:24] + "..." if studies else "-",
                                                     study[:24] + "...", same_series, same_for, ok_ref, new_form, sorted(subjects)))

    # 1.5 the method record, the dates shifted per subject, the catalog's
    # version.
    vid = p.versions["research@1"]
    bad, shifts = [], {}
    for name, (r, ds) in arch.items():
        codes = {str(i.CodeValue) for i in ds.get("DeidentificationMethodCodeSequence", [])}
        filled = name in ("us-native", "us-rle")
        want = LONGITUDINAL_CODES | ({"113101"} if filled else set())
        method = "\\".join(ds.get("DeidentificationMethod", [])) if ds.get("DeidentificationMethod") else ""
        if ds.get("PatientIdentityRemoved") != "YES" or "research@1" not in method or codes != want \
                or ds.get("LongitudinalTemporalInformationModified") != "MODIFIED" or r["version"] != vid:
            bad.append("%s: identity removed %r, method %r, codes %s, longitudinal %r, catalog version %r" % (
                name, ds.get("PatientIdentityRemoved"), method[:60], sorted(codes), ds.get("LongitudinalTemporalInformationModified"),
                r["version"]))
        orig = dcmread(files[name], stop_before_pixels=True, force=True) if name in files else None
        if orig is not None and orig.get("StudyDate") and ds.get("StudyDate"):
            days = (datetime.date(*map(int, (orig.StudyDate[:4], orig.StudyDate[4:6], orig.StudyDate[6:8])))
                    - datetime.date(*map(int, (ds.StudyDate[:4], ds.StudyDate[4:6], ds.StudyDate[6:8])))).days
            shifts.setdefault(str(orig.get("IssuerOfPatientID", "")), set()).add(days)
    rich_bd = (rich.get("PatientBirthDate") if rich else None)
    good_shift = all(len(s) == 1 and min(s) >= 1 for s in shifts.values()) and bool(shifts)
    p.result("1.5 each file records Patient Identity Removed, the version and its codes; dates move back by one offset per subject",
             not bad and good_shift, "%d files: (0012,0062) YES, (0012,0063) names research@1, (0012,0064) the six codes of the "
             "bases (and 113101 where pixels were filled), (0028,0303) MODIFIED, the catalog's version id %d: %d differ; days moved back "
             "per issuer of the patient's ID: %s; rich's birth date now %r (was %s)"
             % (len(arch), vid, len(bad), {k or "(none)": sorted(v) for k, v in shifts.items()}, rich_bd, deidkit.PLANTED["PatientBirthDate"]),
             bad)
    # 1.7 vocabulary UIDs: a public coding scheme's kept, a local one's
    # remapped.
    vocab = {}
    for name, (_, ds) in arch.items():
        for e in deidkit.walk(ds):
            if e.keyword == "CodingSchemeUID" and e.value:
                vocab.setdefault(name, []).append(str(e.value))
    kept_local = ["%s: %s" % (n, u) for n, us in vocab.items() for u in us
                  if u not in deidkit.PUBLIC_VOCABULARY and not u.startswith("2.25.")]
    rich_schemes = vocab.get("rich", [])
    report_schemes = vocab.get("reportsi.dcm", [])
    p.result("1.7 a public coding scheme's UID is kept, a local one's remapped", not kept_local and deidkit.SNOMED in rich_schemes
             and deidkit.LOCAL_SCHEME not in rich_schemes and any(u.startswith("2.25.") for u in rich_schemes)
             and bool(report_schemes) and all(u.startswith("2.25.") for u in report_schemes),
             "rich's Coding Scheme UIDs %s (sent SNOMED CT %s and a local %s); reportsi.dcm's %s (sent DCMTK's local scheme "
             "1.2.276.0.7230010.3.0.0.1); local ones kept anywhere: %d" % (rich_schemes, deidkit.SNOMED, deidkit.LOCAL_SCHEME,
                                                                         report_schemes, len(kept_local)), kept_local)
    if len({d for v in shifts.values() for d in v}) > 1:
        p.note("the files of one subject label carry two date offsets, one per issuer of the patient's ID: the offset is keyed by "
               "issuer and ID, the subject label by the ID alone")
    return {"files": files, "arch": arch, "held": held_names, "needles": nd, "study": study, "receptions": recs,
            "subject": sorted(subjects)[0] if subjects else ""}


def trial_agrees(p, st, version):
    """6: profile try on a file the receiver archived gives, element by
    element, the values the archive holds, keyed values aside; its keys are
    throwaway, so its remapped UID differs from the archive's. explain
    names the same action for each element, and diff lists what a version
    adds."""
    if "rich" not in st["arch"]:
        p.result("6 profile try agrees with the archive", False, "the file tried was not archived")
        return
    path = st["files"]["rich"]
    _, archived = st["arch"]["rich"]
    t = p.mj("profile", "try", "lab", "--profile", version, "--receiver", "deid", "--calling-aet", "PROVE", "--called-aet", "ARCHIVE", path)
    res = t["results"][0]
    keyed, equal, differ, absent_bad, compared = 0, 0, [], [], 0
    actions = {}
    for e in res["elements"]:
        if "/" in e["path"] or "[" in e["path"]:
            continue
        actions[e["path"]] = e["action"]
        tag = int(e["path"], 16)
        have = archived.get(tag)
        if not e.get("presentAfter"):
            if have is not None and tag != 0x00080005:
                absent_bad.append("%s %s: absent after the trial, %r in the archive" % (e["path"], e["keyword"], deidkit.element_text(have)))
            continue
        if have is None:
            absent_bad.append("%s %s: present after the trial, absent in the archive" % (e["path"], e["keyword"]))
            continue
        text = deidkit.element_text(have)
        if text is None or have.VR in BINARY:
            # The trial answers text values only (a number held in binary,
            # a sequence or bulk data comes back empty).
            continue
        text = text.strip(" \x00")
        compared += 1
        if e["action"] in KEYED and text != e.get("after", ""):
            # A keyed value: the trial's keys are its own.
            keyed += 1
            continue
        if text == e.get("after", "").strip(" \x00"):
            equal += 1
        else:
            differ.append("%s %s: trial %r, archive %r" % (e["path"], e["keyword"], e.get("after", "")[:50], text[:50]))
    sop_trial = next((e.get("after") for e in res["elements"] if e["path"] == "00080018"), "")
    distinct_uid = bool(sop_trial) and sop_trial != str(archived.SOPInstanceUID) and sop_trial.startswith("2.25.")
    p.result("6.1 profile try gives the archived file's values, keyed values aside, under throwaway keys",
             res["verdict"] == "TRIAL_VERDICT_PASSED" and not differ and not absent_bad and distinct_uid and equal > 20,
             "verdict %s; %d top-level elements compared: %d equal, %d keyed (UID, shifted date) and different as their keys are; "
             "%d differ; presence differs for %d; the trial's SOP Instance UID %s... is not the archive's %s...: %s"
             % (res["verdict"], compared, equal, keyed, len(differ), len(absent_bad), sop_trial[:20], str(archived.SOPInstanceUID)[:20],
                distinct_uid), differ + absent_bad)
    ex = p.mj("profile", "explain", "lab", "--profile", version, path)
    mismatch = [e["path"] for e in ex["elements"] if e["path"] in actions and e["action"] != actions[e["path"]]]
    floor = next((e for e in ex["elements"] if e["path"] == "00100010"), {})
    p.result("6.2 profile explain names the action the trial took for every element, and who decided", not mismatch and bool(floor),
             "%d elements explained, %d with another action than the trial's; Patient's Name: %s by %s (%s)"
             % (len(ex["elements"]), len(mismatch), floor.get("action"), floor.get("ruleName") or "default", floor.get("origin") or "-"),
             mismatch)
    df = p.mj("profile", "diff", "lab", "research@1", "research@2")
    added = sorted(r["to"]["name"] for r in df.get("rules", []) if r["change"] == "PROFILE_CHANGE_ADDED")
    p.result("6.3 profile diff lists the rules a version adds", added == ["keep-series", "mrn-anywhere", "mrn-study"] and not df["sameEffect"],
             "research@1 to research@2: added %s, same effect %s" % (added, df["sameEffect"]))


def wait_job(p, job, timeout=300):
    deadline = time.time() + timeout
    j = p.mj("job", "show", "lab", job)
    j = j.get("job", j)
    while j["state"] in ("JOB_STATE_QUEUED", "JOB_STATE_RUNNING") and time.time() < deadline:
        time.sleep(0.3)
        j = p.mj("job", "show", "lab", job)
        j = j.get("job", j)
    return j


def deid_job(p, *args):
    j = p.mj("deid", *args)
    j = wait_job(p, j.get("id") or j.get("job", {}).get("id"))
    fails = p.mj("job", "failures", "lab", j["id"], "--warnings", "--all")["failures"]
    return j, fails


def tree_sums(root):
    out = {}
    for path in glob.glob(os.path.join(root, "**", "*"), recursive=True):
        if os.path.isfile(path) and "/.blob-tmp/" not in path:
            with open(path, "rb") as f:
                out[os.path.relpath(path, root)] = hashlib.sha256(f.read()).hexdigest()
    return out


def basic_study(p):
    """A small study archived under basic@1 (through the route of SITE_B):
    two CT slices and an MR image, for the check and the dry run of a
    version that shifts no date. Returns its subject label and
    {SOP Instance UID: (row, data set)} of what was archived."""
    d = p.build("basic")
    study, ct_series = generate_uid(), generate_uid()
    src = p.corpus("CT_small.dcm")

    def no_institution(ds):
        # MR_small names its institution TOSHIBA, which its kept
        # Manufacturer TOSHIBA_MEC holds again: it would be held.
        del ds.InstitutionName
    files = [deidkit.rewrite(src, os.path.join(d, "ct%d.dcm" % i), study, ct_series, i + 1, patient="Basilio^Checkwell",
                             patient_id="PRV-BASIC-0001")[0] for i in range(2)]
    files.append(deidkit.rewrite(p.corpus("MR_small.dcm"), os.path.join(d, "mr.dcm"), study, generate_uid(), 3, patient="Basilio^Checkwell",
                                 patient_id="PRV-BASIC-0001", extra=no_institution)[0])
    for f in files:
        p.needles.from_file(f, "basic")
    before = p.receptions("routed")
    p.send("routed", files, aet="SITE_B")
    recs = p.settle("routed", before)
    arch = {r["uid"]: (r, p.read_archived(r)) for r in p.archived(recs)}
    held = p.held(recs)
    subjects = sorted({r["subject"] for r, _ in arch.values()})
    p.note("the study for the check under basic@1: %d archived on %s, %d held %s" % (
        len(arch), sorted({r["storage"] for r, _ in arch.values()}), len(held), [(h["reason"], h["details"]) for h in held]))
    return (subjects[0] if subjects else ""), arch


def dry_run(p, name, version, sub, arch):
    """A dry run of version over a subject: it lists exactly the MR files
    as what it would hold, and no stored byte, instance row or quarantine
    row changes."""
    before = tree_sums(os.path.join(p.roots, "lab"))
    rows_before = [tuple(r) for r in p.query("SELECT * FROM instances ORDER BY sop_instance_uid")]
    q_before = p.query("SELECT count(*) AS n FROM quarantine")[0]["n"]
    j, fails = deid_job(p, "dry-run", "lab", version, sub)
    mr = {str(ds.SOPInstanceUID) for _, ds in arch.values() if ds.get("Modality") == "MR"}
    would = {f["instanceId"] for f in fails if f["code"] == "JOB_FAILURE_CODE_WOULD_HOLD"}
    after = tree_sums(os.path.join(p.roots, "lab"))
    rows_after = [tuple(r) for r in p.query("SELECT * FROM instances ORDER BY sop_instance_uid")]
    q_after = p.query("SELECT count(*) AS n FROM quarantine")[0]["n"]
    p.result(name, would == mr and bool(mr) and before == after and rows_before == rows_after and q_before == q_after,
             "%s over %d instances: job %s, would hold %d (%s), the archived MR files (pydicom) %d, the same: %s; SHA-256 of %d stored "
             "files before and after equal: %s; instance rows equal: %s; quarantine rows %d then %d"
             % (version, len(arch), j["state"], len(would), sorted({f["detail"] for f in fails}), len(mr), would == mr, len(before),
                before == after, rows_before == rows_after, q_before, q_after))


def check(p, name, sub, arch):
    j, fails = deid_job(p, "check", "lab", sub)
    p.result(name, j["state"] == "JOB_STATE_DONE" and not fails and int(j["counts"]["instancesSent"]) == len(arch),
             "job %s: %s of %s instances passed, %d findings %s" % (j["state"], j["counts"]["instancesSent"], j["counts"]["instances"],
                                                                    len(fails), sorted({f["detail"] for f in fails})),
             [json.dumps({k: f[k] for k in ("instanceId", "code", "detail", "sopClassUid")}) for f in fails])


def check_and_dry_run(p, st, strict, basic_strict):
    """7: deid check passes what was archived; a dry run under a stricter
    version lists what it would hold and changes no byte and no row; a file
    edited on the storage to hold a removed element again is found by the
    next check. Run over the main study (a version that shifts dates) and
    over a study archived under the basic profile."""
    check(p, "7.1 deid check passes every archived instance of the main study (research@1, dates shifted)", st["subject"], st["arch"])
    dry_run(p, "7.2 a dry run of a stricter research version over the main study lists exactly the MR files and changes nothing",
            strict, st["subject"], st["arch"])
    sub, arch = basic_study(p)
    check(p, "7.3 deid check passes every archived instance of a study archived under basic@1", sub, arch)
    dry_run(p, "7.4 a dry run of a stricter basic version over it lists exactly the MR file and changes nothing", basic_strict, sub, arch)
    # The edit: an element the version removes, put back into an archived
    # file in place, as a person or a fault on the storage might.
    uid = sorted(arch)[0]
    r, _ = arch[uid]
    path = p.storage_path(r["storage"], r["storage_key"])
    with open(path, "rb") as f:
        saved = f.read()
    try:
        ds = dcmread(path)
        ds.PatientAddress = "1 Edited Street"
        ds.save_as(path, write_like_original=True)
        j, fails = deid_job(p, "check", "lab", sub)
        found = [f for f in fails if f["instanceId"] == uid]
        p.result("7.5 deid check finds an archived file edited on the storage to hold an element the version removes",
                 j["state"] != "JOB_STATE_DONE" and len(found) == 1 and len(fails) == 1,
                 "job %s; findings %d, on the edited instance %d: %s" % (j["state"], len(fails), len(found),
                                                                       [(f["code"], f["detail"]) for f in fails]))
    finally:
        with open(path, "wb") as f:
            f.write(saved)


def forward(p, st):
    """10 (optional): the main study's session sent to Orthanc by DIMSE and
    read back through Orthanc's REST API carries no planted value."""
    if not p.args.orthanc:
        p.skip("10 a de-identified session forwarded to Orthanc carries no planted value", "no --orthanc given")
        return
    u = urllib.parse.urlsplit(p.args.orthanc)
    aet, _, hostport = p.args.orthanc_dimse.partition("@")
    host, _, port = hostport.rpartition(":")

    def call(method, path):
        c = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=120)
        c.request(method, path)
        r = c.getresponse()
        data = r.read()
        c.close()
        if r.status >= 300:
            raise RuntimeError("Orthanc %s %s: HTTP %d" % (method, path, r.status))
        return data
    try:
        had = set(json.loads(call("GET", "/studies")))
    except (OSError, RuntimeError) as e:
        p.skip("10 a de-identified session forwarded to Orthanc carries no planted value", "Orthanc not reachable: %s" % e)
        return
    p.m("setting", "set", "sender.destinations", "%s ports=%s" % (host, port))
    p.m("sender", "add", "lab", "orthanc", "--type", "dimse", "--host", host, "--port", port, "--called-aet", aet,
        "--calling-aet", "NOPACS", "--enabled")
    sessions = sorted({(r["subject"], r["session"]) for r, _ in st["arch"].values()})
    added = set()
    try:
        j = p.mj("job", "create", "lab", "--sender", "orthanc", "%s/%s" % sessions[0])
        j = wait_job(p, j.get("job", j)["id"])
        added = set(json.loads(call("GET", "/studies"))) - had
        found, n = [], 0
        for s in added:
            for inst in json.loads(call("GET", "/studies/%s/instances" % s)):
                n += 1
                found += ["%s: %s" % (inst["ID"][:8], h) for h in st["needles"].search(call("GET", "/instances/%s/file" % inst["ID"]))]
        p.result("10 a de-identified session forwarded to Orthanc carries no planted value",
                 j["state"] == "JOB_STATE_DONE" and n == len(st["arch"]) and not found,
                 "job %s (%s of %s sent); Orthanc holds %d new instance(s) in %d new study(ies), each file read through its REST API: "
                 "%d planted values found" % (j["state"], j["counts"]["instancesSent"], j["counts"]["instances"], n, len(added), len(found)),
                 found)
    finally:
        for s in set(json.loads(call("GET", "/studies"))) - had:
            call("DELETE", "/studies/" + s)


def nowhere_else(p):
    """1.6: no value planted in data sent to a de-identifying receiver is in
    the tenant's or the master's database (dumped by the sqlite3 tool, and
    their files read raw), the server's log, or any file, manifest or
    sidecar of a scan a de-identifying receiver archived."""
    nd = p.needles
    found, read = [], []
    for db in (p.catalog(), os.path.join(p.data, "master.db")):
        d = subprocess.run([p.args.sqlite3, db, ".dump"], capture_output=True, timeout=300)
        if d.returncode != 0:
            found.append("sqlite3 .dump %s: exit %d" % (db, d.returncode))
        found += ["dump of %s: %s" % (os.path.basename(db), h) for h in nd.search(d.stdout)]
        read.append("%s dump (%d KiB)" % (os.path.basename(db), len(d.stdout) // 1024))
        for raw in glob.glob(db + "*"):
            with open(raw, "rb") as f:
                found += ["raw %s: %s" % (os.path.basename(raw), h) for h in nd.search(f.read())]
    with open(os.path.join(p.work, "serve.log"), "rb") as f:
        log = f.read()
    found += ["serve.log: %s" % h for h in nd.search(log)]
    scans = p.query("SELECT storage, subject_label, session_label, label FROM scans WHERE receiver NOT IN ('plain', 'flip')")
    nfiles = 0
    for s in scans:
        for kind in ("data", "meta"):
            for path in glob.glob(p.storage_path(s["storage"], "/".join([kind, s["subject_label"], s["session_label"], s["label"], "*"]))):
                nfiles += 1
                with open(path, "rb") as f:
                    found += ["%s: %s" % (os.path.relpath(path, p.roots), h) for h in nd.search(f.read())]
    p.result("1.6 no planted value is in the databases, the server's log, or any stored file of a de-identifying receiver", not found,
             "%d values in %d forms looked for in %s, the raw database files, serve.log (%d KiB), and %d stored files of %d scans: %d found"
             % (len(set(l for l, _ in nd.forms.values())), len(nd), ", ".join(read), len(log) // 1024, nfiles, len(scans), len(found)),
             found)
