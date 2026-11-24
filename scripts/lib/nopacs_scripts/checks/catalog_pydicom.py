"""Check NoPACs' catalog answers against pydicom.

Input: the path of a JSON file
  {"files": [<paths of the DICOM files as they were received>],
   "subjects": [{"label", "session_count", "instance_count", "size_bytes"}],
   "sessions": [{"subject_label", "label", "study_instance_uid", "study_date",
                 "modalities", "scan_count", "instance_count", "size_bytes"}],
   "scans": [{"subject_label", "session_label", "label", "series_instance_uid",
              "modality", "series_description", "series_number" (absent when
              the answer has none), "instance_count", "size_bytes"}],
   "sessions_by_study_date": [study UIDs, in the order ListSessions answered
                              by study date],
   "scans_by_series_description": [series UIDs, in the order ListScans
                                   answered by series description],
   "sessions_by_modality": {modality: [study UIDs ListSessions answered]},
   "scans_by_modality": {modality: [series UIDs ListScans answered]}}

Every file is read with pydicom, and what the catalog answers must equal what
pydicom reads: the subjects (one per Patient ID, the default naming rule's),
each with its sessions' studies, the sessions (one per Study Instance UID,
with the study date when it is a valid date, the modalities of its scans, each
once, in byte order), the scans (one per Series Instance UID, with modality,
series description, series number, and the instances, one per SOP Instance
UID), and the bytes of the files. Labels are the archive's own and are not
recomputed here; a session is found by its Study Instance UID and a scan by
its Series Instance UID, and a subject by the sessions it holds.

Exit status 0 when everything agrees; the differences are printed otherwise.
"""
import datetime
import json
import os

import pydicom


def text(ds, keyword):
    """A value as the catalog keeps it: text with the padding removed."""
    v = ds.get(keyword)
    if v is None:
        return ""
    return str(v).strip()


def valid_date(s):
    if len(s) != 8 or not s.isdigit():
        return False
    try:
        datetime.datetime.strptime(s, "%Y%m%d")
    except ValueError:
        return False
    return True


def read_files(paths):
    """The instances, one per SOP Instance UID: what pydicom reads of each."""
    instances = {}
    for p in paths:
        ds = pydicom.dcmread(p, stop_before_pixels=True)
        sop = text(ds, "SOPInstanceUID")
        number = None
        try:
            number = int(str(ds.get("SeriesNumber")).strip())
        except (TypeError, ValueError):
            pass
        date = text(ds, "StudyDate")
        instances[sop] = {
            "patient": text(ds, "PatientID"),
            "study": text(ds, "StudyInstanceUID"),
            "series": text(ds, "SeriesInstanceUID"),
            "date": date if valid_date(date) else "",
            "modality": text(ds, "Modality"),
            "description": text(ds, "SeriesDescription"),
            "number": number,
            "size": os.path.getsize(p),
        }
    return instances


def main(argv):
    with open(argv[0]) as f:
        got = json.load(f)
    instances = read_files(got["files"])
    problems = []

    def differ(what, ours, theirs):
        if ours != theirs:
            problems.append("%s: the catalog answers %r, pydicom reads %r" % (what, ours, theirs))

    def among(what, ours, theirs):
        if ours not in theirs:
            problems.append("%s: the catalog answers %r, pydicom reads %r" % (what, ours, sorted(theirs, key=repr)))

    # What pydicom reads, grouped as the catalog groups it.
    series = {}
    studies = {}
    patients = {}
    for sop, i in instances.items():
        s = series.setdefault(i["series"], {"study": i["study"], "sops": [], "size": 0, "first": i})
        s["sops"].append(sop)
        s["size"] += i["size"]
        st = studies.setdefault(i["study"], {"patient": i["patient"], "series": set(), "sops": 0, "size": 0, "date": i["date"]})
        st["series"].add(i["series"])
        st["sops"] += 1
        st["size"] += i["size"]
        pa = patients.setdefault(i["patient"], {"studies": set(), "sops": 0, "size": 0})
        pa["studies"].add(i["study"])
        pa["sops"] += 1
        pa["size"] += i["size"]

    # Scans.
    answered = {s["series_instance_uid"]: s for s in got["scans"]}
    differ("the scans' series", sorted(answered), sorted(series))
    for uid, s in series.items():
        a = answered.get(uid)
        if a is None:
            continue
        # A scan keeps the first value it was given, so where the files of a
        # series disagree the answer must be one of the values read.
        w = "scan " + uid
        among(w + " modality", a["modality"], {instances[o]["modality"] for o in s["sops"]})
        among(w + " series description", a["series_description"], {instances[o]["description"] for o in s["sops"]})
        among(w + " series number", a.get("series_number"), {instances[o]["number"] for o in s["sops"]})
        differ(w + " instances", a["instance_count"], len(s["sops"]))
        differ(w + " bytes", a["size_bytes"], s["size"])

    # Sessions.
    answered_sessions = {s["study_instance_uid"]: s for s in got["sessions"]}
    differ("the sessions' studies", sorted(answered_sessions), sorted(studies))
    for uid, st in studies.items():
        a = answered_sessions.get(uid)
        if a is None:
            continue
        w = "session " + uid
        mods = sorted({answered[s]["modality"] for s in st["series"] if s in answered and answered[s]["modality"]})
        differ(w + " study date", a["study_date"], st["date"])
        differ(w + " modalities", a["modalities"], mods)
        differ(w + " scans", a["scan_count"], len(st["series"]))
        differ(w + " instances", a["instance_count"], st["sops"])
        differ(w + " bytes", a["size_bytes"], st["size"])

    # A scan is in its study's session: the labels agree.
    for uid, s in series.items():
        a, se = answered.get(uid), answered_sessions.get(s["study"])
        if a and se:
            differ("scan %s places" % uid, (a["subject_label"], a["session_label"]), (se["subject_label"], se["label"]))

    # Subjects: one per Patient ID, found by the studies its sessions hold.
    by_subject = {}
    for se in got["sessions"]:
        by_subject.setdefault(se["subject_label"], set()).add(se["study_instance_uid"])
    differ("the subjects", sorted(s["label"] for s in got["subjects"]), sorted(by_subject))
    patient_of = {frozenset(p["studies"]): p for p in patients.values()}
    for s in got["subjects"]:
        studies_of = frozenset(by_subject.get(s["label"], ()))
        p = patient_of.get(studies_of)
        if p is None:
            problems.append("subject %s holds studies %s that no one Patient ID has" % (s["label"], sorted(studies_of)))
            continue
        differ("subject %s sessions" % s["label"], s["session_count"], len(p["studies"]))
        differ("subject %s instances" % s["label"], s["instance_count"], p["sops"])
        differ("subject %s bytes" % s["label"], s["size_bytes"], p["size"])

    # Orders and filters of the lists.
    dated = [(st["date"], uid) for uid, st in studies.items() if st["date"]]
    order = got["sessions_by_study_date"]
    differ("the study date order's sessions", sorted(order), sorted(uid for _, uid in dated))
    got_dates = [studies[u]["date"] for u in order if u in studies]
    differ("the study date order", got_dates, sorted(got_dates, reverse=True))
    described = [(s["first"]["description"].lower(), uid) for uid, s in series.items() if s["first"]["description"]]
    order = got["scans_by_series_description"]
    differ("the series description order's scans", sorted(order), sorted(uid for _, uid in described))
    got_desc = [series[u]["first"]["description"].lower() for u in order if u in series]
    differ("the series description order", got_desc, sorted(got_desc))
    for m, uids in got["sessions_by_modality"].items():
        want = sorted(uid for uid, st in studies.items() if any(series[x]["first"]["modality"] == m for x in st["series"]))
        differ("sessions of modality " + m, sorted(uids), want)
    for m, uids in got["scans_by_modality"].items():
        differ("scans of modality " + m, sorted(uids), sorted(u for u, s in series.items() if s["first"]["modality"] == m))

    for p in problems:
        print("DIFFER", p)
    print("%d instances, %d scans, %d sessions, %d subjects checked; %d differ" % (
        len(instances), len(series), len(studies), len(patients), len(problems)))
    return 1 if problems else 0

