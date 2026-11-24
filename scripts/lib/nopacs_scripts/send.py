"""What the proof of sending (scripts/prove/sending.py) uses that is not a
step: Orthanc's REST API read with Python's standard library, a DICOM peer
of pynetdicom that answers each C-STORE with a status the proof chooses, the
studies the proof sends, and the archive read from its storage directory and
its SQLite catalog. Nothing here imports NoPACs' code.
"""

import http.client
import io
import json
import os
import shlex
import struct
import subprocess
import threading
import urllib.parse

from pydicom import dcmread
from pydicom.uid import generate_uid
from pynetdicom import AE, AllStoragePresentationContexts, evt
from pynetdicom.sop_class import Verification

from nopacs_scripts.db import query
from nopacs_scripts.paths import ROOT

CORPUS = os.path.join(ROOT, "internal", "receive", "testdata")
CT = ["ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"]

# The other files of the whole session: MR in four transfer syntaxes
# (explicit little endian, RLE, JPEG 2000 and JPEG-LS lossless), a
# secondary capture of two RLE frames, an ultrasound multi-frame of three
# JPEG baseline frames, a segmentation (an enhanced multi-frame object with
# functional groups). With the CT series that is five storage classes and
# five transfer syntaxes in one session. The names are looked for in this
# repository's corpus first, then in pydicom's.
OTHERS = ["MR_small.dcm", "MR_small_RLE.dcm", "MR_small_jp2klossless.dcm", "MR_small_jpeg_ls_lossless.dcm",
          "SC_rgb_rle_2frame.dcm", "jpeg_3frame_nobot.dcm", "liver_1frame.dcm"]


# ---- Part 10 files ----

def split_part10(b):
    """The file meta group and the data set of a Part 10 file's bytes: the
    meta group starts after the 128-byte preamble and "DICM", and its first
    element, (0002,0000), gives its length (PS3.10 7.1)."""
    if b[128:132] != b"DICM":
        raise ValueError("not a Part 10 file")
    group = struct.unpack("<I", b[140:144])[0]
    return b[132:144 + group], b[144 + group:]


def read_bytes(data):
    """A pydicom data set of a Part 10 file's bytes."""
    return dcmread(io.BytesIO(data))


# ---- the studies the proof sends ----

def corpus_path(name, pydicom_corpus):
    here = os.path.join(CORPUS, name)
    return here if os.path.exists(here) else os.path.join(pydicom_corpus, name)


def rewrite(sources, out, patient_id, patient_name, series_of=None, extra=None):
    """Writes each source as a member of one new study of the patient given:
    a new Study Instance UID, a new Series Instance UID for each series of
    the sources (each source its own series unless series_of, given the
    index of a file, groups them), and
    a new SOP Instance UID for each file, the meta group's copy of it
    included. Pixel data and transfer syntax stay as they are. Returns the
    paths written and the new Study Instance UID."""
    os.makedirs(out, exist_ok=True)
    study = generate_uid()
    series = {}
    paths = []
    for i, src in enumerate(sources):
        ds = dcmread(src)
        key = series_of(i) if series_of else src
        if key not in series:
            series[key] = generate_uid()
        ds.StudyInstanceUID = study
        ds.SeriesInstanceUID = series[key]
        ds.SOPInstanceUID = generate_uid()
        ds.file_meta.MediaStorageSOPInstanceUID = ds.SOPInstanceUID
        ds.PatientID = patient_id
        ds.PatientName = patient_name
        ds.InstanceNumber = i + 1
        if extra:
            extra(ds)
        path = os.path.join(out, "%05d.dcm" % i)
        ds.save_as(path, write_like_original=True)
        paths.append(path)
    return paths, study


def many(source, out, n, patient_id, series=1):
    """n copies of one source file in one new study of `series` series, each
    with its own SOP Instance UID: a session big enough to stop midway."""
    return rewrite([source] * n, out, patient_id, "Prove^Many", series_of=lambda i: i % series)


# ---- the archive ----

# The archived instances of a study: each with its key on the storage, which
# a row keeps only when it is not the layout's key of its scan and UID, and
# where it is inside that object.
HELD = """SELECT i.sop_instance_uid AS uid, s.subject_label AS subject, s.session_label AS session,
    CASE WHEN i.storage_key = '' THEN 'data/' || s.subject_label || '/' || s.session_label || '/' || s.label || '/'
        || i.sop_instance_uid || '.dcm' ELSE i.storage_key END AS storage_key,
    i.byte_offset AS byte_offset, i.byte_length AS byte_length
    FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE s.study_uid = ?"""


def archived(db_path, storage_root, study):
    """{SOP Instance UID: the archived file's bytes, read from the storage
    directory} for a study, and its subject and session labels."""
    out, labels = {}, None
    for r in query(db_path, HELD, study):
        with open(os.path.join(storage_root, r["storage_key"]), "rb") as f:
            f.seek(r["byte_offset"])
            out[r["uid"]] = f.read(r["byte_length"]) if r["byte_length"] else f.read()
        labels = (r["subject"], r["session"])
    return out, labels


# ---- Orthanc ----

class Orthanc:
    """The few calls of Orthanc's REST API the proof makes
    (https://orthanc.uclouvain.be/api/), and the count of the instances it
    discarded as already stored, read from its log."""

    def __init__(self, url, logs_command):
        u = urllib.parse.urlsplit(url)
        self.host, self.port = u.hostname, u.port or 80
        self.logs_command = shlex.split(logs_command) if logs_command else []
        self.log_level = None

    def request(self, method, path, body=None):
        c = http.client.HTTPConnection(self.host, self.port, timeout=120)
        c.request(method, path, body)
        r = c.getresponse()
        data = r.read()
        c.close()
        if r.status >= 300:
            raise RuntimeError("Orthanc %s %s: HTTP %d %s" % (method, path, r.status, data[:200]))
        return data

    def json(self, path):
        return json.loads(self.request("GET", path))

    def count(self):
        return int(self.json("/statistics")["CountInstances"])

    def instances(self):
        """{SOP Instance UID: Orthanc's id} of every instance Orthanc holds,
        and the number of instances it lists (two with the same UID would
        list twice)."""
        rows = self.json("/instances?expand")
        return {r["MainDicomTags"]["SOPInstanceUID"]: r["ID"] for r in rows}, len(rows)

    def file(self, oid):
        """The file as Orthanc keeps it."""
        return self.request("GET", "/instances/%s/file" % oid)

    def clear(self):
        """Deletes every study Orthanc holds: the proof starts only on an
        empty Orthanc, so every study is one it sent."""
        for s in self.json("/studies"):
            self.request("DELETE", "/studies/" + s)

    def verbose(self):
        """Orthanc logs a discarded duplicate only at its verbose level; the
        level found is restored by restore."""
        self.log_level = self.request("GET", "/tools/log-level").decode().strip()
        self.request("PUT", "/tools/log-level", b"verbose")

    def restore(self):
        if self.log_level:
            self.request("PUT", "/tools/log-level", self.log_level.encode())

    def discarded(self):
        """How many incoming instances Orthanc's log says it discarded
        because it held them already, or None without a way to read its log."""
        if not self.logs_command:
            return None
        p = subprocess.run(self.logs_command, capture_output=True, text=True, timeout=120)
        return (p.stdout + p.stderr).count("has been discarded because it is already stored")


# ---- a peer that refuses what it is told to ----

class Peer:
    """A C-STORE SCP of pynetdicom on a port of its own. statuses maps a SOP
    Instance UID to the status it answers (success otherwise); received
    lists every UID it was sent, in order, repeats included."""

    def __init__(self, port, aet="PEER"):
        self.port = port
        self.statuses = {}
        # once maps a SOP Instance UID to a status answered the first time it
        # comes only, as a peer out of resources for a moment.
        self.once = {}
        self.received = []
        self.lock = threading.Lock()
        ae = AE(ae_title=aet)
        ae.supported_contexts = AllStoragePresentationContexts
        ae.add_supported_context(Verification)
        self.server = ae.start_server(("127.0.0.1", port), block=False,
                                      evt_handlers=[(evt.EVT_C_STORE, self.store), (evt.EVT_C_ECHO, lambda e: 0x0000)])

    def store(self, event):
        uid = str(event.request.AffectedSOPInstanceUID)
        with self.lock:
            self.received.append(uid)
            if uid in self.once:
                return self.once.pop(uid)
            return self.statuses.get(uid, 0x0000)

    def stop(self):
        self.server.shutdown()
