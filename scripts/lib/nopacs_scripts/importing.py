"""What the proof of importing (scripts/prove/importing.py) uses that is not a
step: the directory trees it builds from the public corpus, the traps planted
in them, a walk of a tree that opens each directory by its descriptor (so a
path longer than the system's limit can be read), and the archive read from
its storage directory and its SQLite catalog. Nothing here imports NoPACs'
code.

Each tree builder records in a Model what it planted: for every entry the walk
will judge, the path the job lists it under and what must become of it, either
"hand" (handed in and archived), "dup" (handed in, already held), or the name
of a skip code.
"""

import hashlib
import os
import stat

from pydicom import dcmread
from pydicom.dataset import Dataset, FileMetaDataset
from pydicom.sequence import Sequence
from pydicom.uid import ExplicitVRLittleEndian, generate_uid

from nopacs_scripts.db import query

WIDE = 1001  # entries in the too-wide directory: one more than the least import.max-dir-entries, 1000
LEVELS = 10  # nested folders of the too-long path
NAME = 240  # bytes of each folder name of the too-long path; 9 of them pass 2048 bytes
MAX_FILE = 128 * 1024  # the receivers' largest file; the corpus files used are all smaller
SR_CLASS = "1.2.840.10008.5.1.4.1.1.88.33"  # Comprehensive SR Storage
CT_CLASS = "1.2.840.10008.5.1.4.1.1.2"


class Model:
    """What a tree holds: entries (job, listed path, expectation) and, for
    each file that is archived, its source, labels and the study it is in."""

    def __init__(self):
        self.entries = []
        self.source = {}  # SOP Instance UID -> path of the file the archive must equal
        self.labels = {}  # SOP Instance UID -> (subject, session, scan) its path and map give
        self.original = set()  # every value of the sources that names a patient or an object

    def add(self, job, listed, expect):
        self.entries.append((job, listed, expect))

    def jobs(self):
        return sorted({e[0] for e in self.entries})

    def of(self, job):
        return [e for e in self.entries if e[0] == job]

    def counts(self, job):
        """found, handed in, archived, already held and skipped by code that
        the job must report."""
        rows = self.of(job)
        skipped = {}
        for _, _, e in rows:
            if e not in ("hand", "dup"):
                skipped[e] = skipped.get(e, 0) + 1
        hand = sum(e == "hand" for _, _, e in rows)
        dup = sum(e == "dup" for _, _, e in rows)
        return {"found": len(rows), "handed_in": hand + dup, "archived": hand, "already_held": dup, "skipped": skipped}


def write(model, src, dest, study, series, name, pid, number, labels=None, pad=0):
    """Writes src as an instance of the study and series given, with a new
    SOP Instance UID and the patient given; pad adds a private element of
    that many bytes. Returns the SOP Instance UID."""
    ds = dcmread(src)
    ds.StudyInstanceUID = study
    ds.SeriesInstanceUID = series
    ds.SOPInstanceUID = generate_uid()
    ds.file_meta.MediaStorageSOPInstanceUID = ds.SOPInstanceUID
    ds.PatientName = name
    ds.PatientID = pid
    ds.InstanceNumber = number
    if pad:
        ds.add_new(0x00110010, "LO", "PROVE IMPORT")
        ds.add_new(0x00111001, "OB", b"\x00" * pad)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    ds.save_as(dest, write_like_original=True)
    model.original.update([name, pid, study, series, str(ds.SOPInstanceUID)])
    if labels:
        model.source[str(ds.SOPInstanceUID)] = dest
        model.labels[str(ds.SOPInstanceUID)] = labels
    return str(ds.SOPInstanceUID)


def put(path, data):
    """Writes a plain file."""
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)


def build_xnat(model, root, corpus, mapping):
    """The XNAT-shaped trees projA and projB with their traps; mapping gets
    {(project, session): subject} for the map file. The shape is the public
    description of an XNAT archive's folders; nothing here is XNAT's code."""
    ct, mr, rle, jpg = (os.path.join(corpus, n) for n in ("CT_small.dcm", "MR_small.dcm", "MR_small_RLE.dcm", "JPEG-lossy.dcm"))

    def series(project, session, scan, files, name, pid, study):
        sd = generate_uid()
        label = ("%s_%s" % (project, mapping[(project, session)]), "%s_%s" % (project, session), scan)
        out = []
        for i, src in enumerate(files):
            rel = "%s/arc001/%s/SCANS/%s/DICOM/%05d.dcm" % (project, session, scan, i)
            sop = write(model, src, os.path.join(root, rel), study, sd, name, pid, i + 1, labels=label)
            model.add(project, rel, "hand")
            out.append((rel, sop))
        return out

    mapping.update({("projA", "s1"): "P-ALPHA", ("projA", "s2"): "P-BETA", ("projB", "s1"): "P-GAMMA"})
    s1, s2, s4 = generate_uid(), generate_uid(), generate_uid()
    first = series("projA", "s1", "1", [ct, ct, ct], "Treeman^Alpha", "TREE-A1", s1)
    series("projA", "s1", "2", [mr, mr], "Treeman^Alpha", "TREE-A1", s1)
    base = "projA/arc001/s1"
    # The same instance in a second folder of the session: handed in, already held.
    put(os.path.join(root, base, "SCANS/9/DICOM/dup.dcm"), open(os.path.join(root, first[0][0]), "rb").read())
    model.add("projA", base + "/SCANS/9/DICOM/dup.dcm", "dup")
    # Not DICOM, in a DICOM folder: counted not_part10 and never quarantined.
    put(os.path.join(root, base, "SCANS/1/DICOM/s1_catalog.xml"), b"<catalog><entry/></catalog>\n")
    put(os.path.join(root, base, "SCANS/1/DICOM/junk.txt"), b"not dicom\n")
    put(os.path.join(root, base, "SCANS/1/DICOM/no_meta.dcm"), open(os.path.join(corpus, "no_meta.dcm"), "rb").read())
    for n in ("s1_catalog.xml", "junk.txt", "no_meta.dcm"):
        model.add("projA", "%s/SCANS/1/DICOM/%s" % (base, n), "not_part10")
    # A symbolic link and two names of one file: never followed, never handed in.
    os.symlink("../../2/DICOM/00000.dcm", os.path.join(root, base, "SCANS/1/DICOM/link.dcm"))
    put(os.path.join(root, base, "SCANS/1/DICOM/hard_a.dcm"), open(mr, "rb").read())
    os.link(os.path.join(root, base, "SCANS/1/DICOM/hard_a.dcm"), os.path.join(root, base, "SCANS/1/DICOM/hard_b.dcm"))
    for n in ("link.dcm", "hard_a.dcm", "hard_b.dcm"):
        model.add("projA", "%s/SCANS/1/DICOM/%s" % (base, n), "not_regular")
    # A valid DICOM file outside the layout, and a folder of resources.
    put(os.path.join(root, base, "notes.dcm"), open(ct, "rb").read())
    put(os.path.join(root, base, "RESOURCES/NIFTI/copy.dcm"), open(mr, "rb").read())
    model.add("projA", base + "/notes.dcm", "outside_layout")
    model.add("projA", base + "/RESOURCES/NIFTI/copy.dcm", "outside_layout")
    put(os.path.join(root, base, "RESOURCES/NIFTI/brain.nii.gz"), b"\x1f\x8b" + os.urandom(64))
    model.add("projA", base + "/RESOURCES/NIFTI/brain.nii.gz", "outside_layout")
    # A DICOM file past the receivers' largest file.
    write(model, ct, os.path.join(root, base, "SCANS/1/DICOM/zbig.dcm"), s1, generate_uid(), "Treeman^Alpha", "TREE-A1", 99, pad=MAX_FILE + 8192)
    model.add("projA", base + "/SCANS/1/DICOM/zbig.dcm", "too_large")
    # A folder whose name carries a colon: not a valid segment, not entered.
    put(os.path.join(root, base, "SCANS/ct:9/DICOM/x.dcm"), open(mr, "rb").read())
    model.add("projA", base + "/SCANS", "bad_name")
    # A second session of the project, with compressed data.
    sd = generate_uid()
    for i, src in enumerate([mr, mr]):
        rel = "projA/arc001/s2/SCANS/1/DICOM/%05d.dcm" % i
        sop = write(model, src, os.path.join(root, rel), s2, sd, "Treeman^Beta", "TREE-B1", i + 1,
                    labels=("projA_P-BETA", "projA_s2", "1"))
        model.add("projA", rel, "hand")
    for scan, src in (("3", rle), ("4", jpg)):
        rel = "projA/arc001/s2/SCANS/%s/DICOM/00000.dcm" % scan
        write(model, src, os.path.join(root, rel), s2, generate_uid(), "Treeman^Beta", "TREE-B1", 5,
              labels=("projA_P-BETA", "projA_s2", scan))
        model.add("projA", rel, "hand")
    # A session the map does not list: skipped, not revived.
    s3, sd = generate_uid(), generate_uid()
    for i in range(2):
        rel = "projA/arc001/s3/SCANS/1/DICOM/%05d.dcm" % i
        write(model, mr, os.path.join(root, rel), s3, sd, "Treeman^Delta", "TREE-DELTA", i + 1)
        model.add("projA", rel, "unmapped_session")
    # The other project shares the session label s1.
    sd = generate_uid()
    for i in range(2):
        rel = "projB/arc001/s1/SCANS/1/DICOM/%05d.dcm" % i
        write(model, ct, os.path.join(root, rel), s4, sd, "Treeman^Gamma", "TREE-G1", i + 1, labels=("projB_P-GAMMA", "projB_s1", "1"))
        model.add("projB", rel, "hand")
    # A directory with more entries than import.max-dir-entries (set to its least).
    folder = os.path.join(root, "projB/arc001/wide/SCANS/1/DICOM")
    os.makedirs(folder)
    for i in range(WIDE):
        open(os.path.join(folder, "f%05d.txt" % i), "wb").close()
    model.add("projB", "projB/arc001/wide/SCANS/1/DICOM", "directory_too_wide")
    # A path longer than 2048 bytes, made by descriptors since no path of that length can be written whole.
    fd = os.open(os.path.join(root, "projB/arc001/long"), os.O_RDONLY | os.O_DIRECTORY) if os.path.isdir(os.path.join(root, "projB/arc001/long")) \
        else None
    if fd is None:
        os.makedirs(os.path.join(root, "projB/arc001/long"))
        fd = os.open(os.path.join(root, "projB/arc001/long"), os.O_RDONLY | os.O_DIRECTORY)
    listed = "projB/arc001/long"
    for level in range(LEVELS):
        name = ("d%d" % level).ljust(NAME, "x")
        os.mkdir(name, dir_fd=fd)
        nfd = os.open(name, os.O_RDONLY | os.O_DIRECTORY, dir_fd=fd)
        os.close(fd)
        fd = nfd
        if len(listed) + 1 + NAME <= 2048:
            listed += "/" + name
    ffd = os.open("deep.dcm", os.O_WRONLY | os.O_CREAT, 0o644, dir_fd=fd)
    os.write(ffd, open(ct, "rb").read())
    os.close(ffd)
    os.close(fd)
    model.add("projB", listed, "path_too_long")
    put(os.path.join(root, "maps/sessions.csv"),
        ("project,label,subject_label,ignored\n" + "".join("%s,%s,%s,x\n" % (p, s, v) for (p, s), v in sorted(mapping.items()))).encode())


def build_generic(model, root, corpus):
    """The tree <study>/<visit>/series/<scan>/<file>, with its own map given
    with the job; returns the map as CSV text."""
    ct, mr = (os.path.join(corpus, n) for n in ("CT_small.dcm", "MR_small.dcm"))
    plan = [("trial-a", "visit1", "3", ct, 3, "Generic^One", "GEN-1", "P-017"),
            ("trial-a", "visit2", "3", mr, 2, "Generic^One", "GEN-1", "P-017"),
            ("trial-b", "visit1", "4", mr, 2, "Generic^Two", "GEN-2", "P-018")]
    rows = "trial,visit,participant\n"
    for study, visit, scan, src, n, name, pid, who in plan:
        st, sd = generate_uid(), generate_uid()
        rows += "%s,%s,%s\n" % (study, visit, who)
        for i in range(n):
            rel = "%s/%s/series/%s/%04d.dcm" % (study, visit, scan, i + 1)
            write(model, src, os.path.join(root, rel), st, sd, name, pid, i + 1, labels=("%s_%s" % (study, who), "%s_%s" % (study, visit), scan))
            model.add(study, rel, "hand")
    put(os.path.join(root, "trial-a/visit3/series/1/0001.dcm"), open(mr, "rb").read())
    model.add("trial-a", "trial-a/visit3/series/1/0001.dcm", "unmapped_session")
    return rows


def build_big(root, corpus, groups=6, per=100):
    """groups folders of per instances each of one study, for a job long
    enough to be killed midway. Returns the SOP Instance UIDs and their files."""
    mr = os.path.join(corpus, "MR_small.dcm")
    model, out = Model(), {}
    for g in range(groups):
        st, sd = generate_uid(), generate_uid()
        for i in range(per):
            rel = "big/g%d/%04d.dcm" % (g, i)
            out[write(model, mr, os.path.join(root, rel), st, sd, "Bigman^%d" % g, "BIG-%d" % g, i + 1)] = os.path.join(root, rel)
    return out


def sr(path, name, pid, evidence, dangling=False):
    """A Comprehensive SR of its own study that refers to evidence, a list of
    (study UID, series UID, SOP Instance UID), and returns its SOP Instance UID.
    Each reference is in the evidence sequence and in an image content item."""
    ds = Dataset()
    ds.file_meta = FileMetaDataset()
    ds.file_meta.TransferSyntaxUID = ExplicitVRLittleEndian
    ds.file_meta.MediaStorageSOPClassUID = SR_CLASS
    ds.SOPClassUID = SR_CLASS
    ds.SOPInstanceUID = generate_uid()
    ds.file_meta.MediaStorageSOPInstanceUID = ds.SOPInstanceUID
    ds.file_meta.ImplementationClassUID = generate_uid()
    ds.StudyInstanceUID, ds.SeriesInstanceUID = generate_uid(), generate_uid()
    ds.Modality = "SR"
    ds.PatientName, ds.PatientID = name, pid
    ds.StudyDate = ds.ContentDate = "20240102"
    ds.StudyTime = ds.ContentTime = "101500"
    ds.InstanceNumber, ds.SeriesNumber = 1, 90
    ds.ValueType, ds.ContinuityOfContent, ds.CompletionFlag, ds.VerificationFlag = "CONTAINER", "SEPARATE", "COMPLETE", "UNVERIFIED"
    code = Dataset()
    code.CodeValue, code.CodingSchemeDesignator, code.CodeMeaning = "11528-7", "LN", "Radiology Report"
    ds.ConceptNameCodeSequence = Sequence([code])
    ds.PerformedProcedureCodeSequence = Sequence()
    evi = []
    items = []
    for study, series, sop in evidence:
        ref = Dataset()
        ref.ReferencedSOPClassUID, ref.ReferencedSOPInstanceUID = CT_CLASS, sop
        s = Dataset()
        s.SeriesInstanceUID = series
        s.ReferencedSOPSequence = Sequence([ref])
        e = Dataset()
        e.StudyInstanceUID = study
        e.ReferencedSeriesSequence = Sequence([s])
        evi.append(e)
        item = Dataset()
        item.RelationshipType, item.ValueType = "CONTAINS", "IMAGE"
        r2 = Dataset()
        r2.ReferencedSOPClassUID, r2.ReferencedSOPInstanceUID = CT_CLASS, sop
        item.ReferencedSOPSequence = Sequence([r2])
        items.append(item)
    ds.CurrentRequestedProcedureEvidenceSequence = Sequence(evi)
    ds.ContentSequence = Sequence(items)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    ds.save_as(path, write_like_original=False)
    return str(ds.SOPInstanceUID)


def build_refs(root, corpus):
    """projR: a CT series in one session folder, and in another folder an SR
    of its own study that refers to two of its images and an SR that refers
    to an instance that is in neither folder. Returns the CT's (study,
    series, SOP UIDs), the SR's UID, the dangling SR's UID, the dangling
    reference, and every original value."""
    model = Model()
    study, series = generate_uid(), generate_uid()
    ct = os.path.join(corpus, "CT_small.dcm")
    sops = [write(model, ct, os.path.join(root, "projR/arc001/ct/SCANS/1/DICOM/%04d.dcm" % i), study, series, "Refman^Pat", "REF-1", i + 1)
            for i in range(3)]
    good = sr(os.path.join(root, "projR/arc001/ref/SCANS/5/DICOM/good.dcm"), "Refman^Pat", "REF-1",
              [(study, series, sops[0]), (study, series, sops[2])])
    lost = generate_uid()
    dangling = sr(os.path.join(root, "projR/arc001/ref/SCANS/6/DICOM/dangling.dcm"), "Refman^Pat", "REF-1", [(study, series, lost)])
    model.original.update([study, series, good, dangling, lost, *sops])
    return {"study": study, "series": series, "ct": sops, "good": good, "dangling": dangling, "lost": lost, "original": model.original}


# ---- a walk by descriptors ----

def snapshot(root):
    """{path: (kind, mode, size, mtime_ns, link target or SHA-256)} of every
    entry under root, taken with lstat and a descriptor per directory, so a path of
    any length is read and nothing is followed."""
    out = {}

    def walk(fd, prefix):
        with os.scandir(fd) as it:
            entries = list(it)
        for e in entries:
            rel = prefix + e.name
            st = e.stat(follow_symlinks=False)
            if stat.S_ISLNK(st.st_mode):
                out[rel] = ("link", st.st_mode, st.st_size, st.st_mtime_ns, os.readlink(e.name, dir_fd=fd))
            elif stat.S_ISDIR(st.st_mode):
                out[rel + "/"] = ("dir", st.st_mode, 0, st.st_mtime_ns, "")
                sub = os.open(e.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                try:
                    walk(sub, rel + "/")
                finally:
                    os.close(sub)
            else:
                h = hashlib.sha256()
                f = os.open(e.name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=fd)
                try:
                    while True:
                        b = os.read(f, 1 << 20)
                        if not b:
                            break
                        h.update(b)
                finally:
                    os.close(f)
                out[rel] = ("file", st.st_mode, st.st_size, st.st_mtime_ns, h.hexdigest(), st.st_nlink)

    fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    try:
        walk(fd, "")
    finally:
        os.close(fd)
    return out


def diff(before, after):
    """The paths that changed, appeared or went between two snapshots."""
    out = ["gone: %s" % k[:80] for k in before if k not in after]
    out += ["new: %s" % k[:80] for k in after if k not in before]
    out += ["changed: %s" % k[:80] for k in before if k in after and before[k] != after[k]]
    return out


# ---- the archive ----

# Each held instance with its labels and where its bytes are on the storage:
# a row keeps its key only when it is not the layout's key of its scan and UID.
HELD = """SELECT i.sop_instance_uid AS uid, s.study_uid AS study, s.subject_label AS subject, s.session_label AS session,
    s.label AS scan,
    CASE WHEN i.storage_key = '' THEN 'data/' || s.subject_label || '/' || s.session_label || '/' || s.label || '/'
        || i.sop_instance_uid || '.dcm' ELSE i.storage_key END AS storage_key,
    i.byte_offset AS byte_offset, i.byte_length AS byte_length
    FROM instances AS i JOIN scans AS s ON s.id = i.scan_id"""


def archived(db_path, storage_root):
    """{SOP Instance UID: (the archived file's bytes read from the storage
    directory, subject, session, scan, study UID)} for every held instance."""
    out = {}
    for r in query(db_path, HELD):
        with open(os.path.join(storage_root, r["storage_key"]), "rb") as f:
            f.seek(r["byte_offset"])
            data = f.read(r["byte_length"]) if r["byte_length"] else f.read()
        out[r["uid"]] = (data, r["subject"], r["session"], r["scan"], r["study"])
    return out


def stored_files(storage_root):
    """How many instance objects the storage directory holds."""
    n = 0
    for _, _, names in os.walk(storage_root):
        n += sum(1 for x in names if x.endswith(".dcm"))
    return n
