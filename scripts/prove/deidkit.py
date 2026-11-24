"""What the proof of de-identification (scripts/prove/deid.py) uses that is
not a step: the files it sends, built with pydicom from public corpus files
and from nothing, each with identifying values planted where they are known
to hide; the values it then looks for, in the written forms people use; and
the search itself, over bytes, done here and not by NoPACs. Nothing here
imports NoPACs' code.
"""

import os
import re
import struct

import numpy as np
from pydicom import dcmread
from pydicom.dataset import Dataset, FileMetaDataset
from pydicom.encaps import encapsulate, generate_pixel_data_frame
from pydicom.sequence import Sequence
from pydicom.uid import UID_dictionary, ExplicitVRLittleEndian, ImplicitVRLittleEndian, RLELossless, generate_uid

US_MULTIFRAME = "1.2.840.10008.5.1.4.1.1.3.1"
MONTHS = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October",
          "November", "December"]
# The band of a burned-in image where its text is drawn, and the rectangle
# the profile's pixel rule fills: x, y, width, height from the top left.
BAND = (0, 0, 64, 12)

# The values planted in the synthetic files of the main study. Each is
# looked for afterwards in every place the archive keeps anything.
PATIENT = "Proofington^Deidra^Quilla"
PATIENT_ID = "PRV-DEID-0001"
PLANTED = {
    "OtherPatientNames": "Proofington^Dee",
    "OtherPatientIDs": "PRVOTHER7731",
    "IssuerOfPatientID": "PROVEHAM ISSUER",
    "PatientBirthDate": "19610723",
    "PatientAddress": "42 Quarantine Lane, Testville",
    "PatientTelephoneNumbers": "+1-555-0142",
    "AccessionNumber": "PRVACC4242",
    "InstitutionName": "Proveham General Infirmary",
    "InstitutionAddress": "7 Infirmary Road, Proveham",
    "ReferringPhysicianName": "Doctorly^Referra",
    "PerformingPhysicianName": "Scanwell^Operia",
    "OperatorsName": "Techman^Opaline",
    "StudyID": "PRVSTUDY88",
    "StudyDate": "20240317",
    "SeriesDate": "20240317",
    "AcquisitionDate": "20240317",
    "ContentDate": "20240317",
}
# Free text a person typed, holding the patient's name and the study's date
# in written forms: the profile's clean action must take them out.
TYPED = {
    "StudyDescription": "Follow-up: Deidra Proofington, seen 17 March 2024 (2024-03-17)",
    "SeriesDescription": "PROOFINGTON, DEIDRA axial 17/03/2024",
    "ImageComments": "Quilla Proofington scanned March 17, 2024; birth 1961-07-23",
}
PRIVATE_CREATOR = "PROVE PRIVATE"
PRIVATE_NAME = "Privatus Proofington"
DEEP_NAME = "Deepwell^Tenlevel"
SEQ_ID = "PRVSEQID9921"
UN_TEXT = "Unseen Proofington"
COM_TEXT = b"PROVE COMMENT Proofington Deidra 20240317"


# Words the corpus files use as stand-ins for a name ("Last^First",
# "Last Name^First Name", "operator"): common words, which a database or a
# log holds for other reasons, so they are not looked for.
PLACEHOLDERS = {"last", "first", "name", "names", "text", "here", "operator", "anonymous", "test", "phantom", "patient",
                "doctor", "firstname", "lastname", "middle", "unknown", "none", "first name", "last name", "observer",
                "verifying", "verifying observer"}


# The UIDs of public vocabularies (PS3.16 Table 8-1): the same in every
# file that uses them, so kept, and not looked for. Any other vocabulary
# UID is local, mostly under an institution's root, and must not survive.
PUBLIC_VOCABULARY = {"2.16.840.1.113883.6.96": "SNOMED CT", "2.16.840.1.113883.6.1": "LOINC",
                     "2.16.840.1.113883.6.8": "UCUM", "2.16.840.1.113883.6.256": "RadLex"}
SNOMED = "2.16.840.1.113883.6.96"
LOCAL_SCHEME = "1.2.826.0.1.3680043.8.498.4242"


# ---- building files ----

def meta_for(ds, sop_class, ts=ExplicitVRLittleEndian):
    """A new file meta group for ds (PS3.10 7.1)."""
    fm = FileMetaDataset()
    fm.MediaStorageSOPClassUID = sop_class
    fm.MediaStorageSOPInstanceUID = ds.SOPInstanceUID
    fm.TransferSyntaxUID = ts
    ds.file_meta = fm
    ds.is_little_endian = True
    ds.is_implicit_VR = ts == ImplicitVRLittleEndian


def save(ds, path):
    ds.save_as(path, write_like_original=False)
    return path


def plant(ds):
    """Sets every planted value, the typed free text, a private block with a
    name, a private element whose creator is missing, a name and an ID
    inside sequences, and a person's name ten sequences deep."""
    ds.PatientName = PATIENT
    ds.PatientID = PATIENT_ID
    for k, v in PLANTED.items():
        setattr(ds, k, v)
    for k, v in TYPED.items():
        setattr(ds, k, v)
    ds.SpecificCharacterSet = "ISO_IR 100"
    block = ds.private_block(0x0011, PRIVATE_CREATOR, create=True)
    block.add_new(0x10, "LO", PRIVATE_NAME)
    item = Dataset()
    item.add_new((0x0011, 0x0010), "LO", PRIVATE_CREATOR)
    item.add_new((0x0011, 0x1012), "PN", "Itemized^Proofington")
    block.add_new(0x11, "SQ", Sequence([item]))
    # A private element in a group with no creator at all (PS3.5 7.8.1
    # says it must have one; a file is never trusted to be well formed).
    ds.add_new((0x0013, 0x1010), "LO", PATIENT_ID)
    other = Dataset()
    other.PatientID = SEQ_ID
    other.IssuerOfPatientID = "PROVEHAM ISSUER"
    ds.OtherPatientIDsSequence = Sequence([other])
    req = Dataset()
    req.RequestedProcedureID = "PRVREQ5150"
    req.ScheduledPerformingPhysicianName = "Schedulus^Proofington"
    ds.RequestAttributesSequence = Sequence([req])
    # Ten levels of a standard sequence, the last item holding a name.
    inner = Dataset()
    inner.PersonName = DEEP_NAME
    for _ in range(9):
        outer = Dataset()
        outer.ContentSequence = Sequence([inner])
        inner = outer
    ds.ContentSequence = Sequence([inner])
    # A public coding scheme and a local one, under a root of this script's.
    schemes = []
    for designator, uid in (("SCT", SNOMED), ("99PROVE", LOCAL_SCHEME)):
        item = Dataset()
        item.CodingSchemeDesignator = designator
        item.CodingSchemeUID = uid
        schemes.append(item)
    ds.CodingSchemeIdentificationSequence = Sequence(schemes)


def rewrite(src, path, study, series, n, patient=PATIENT, patient_id=PATIENT_ID, extra=None, keep_names=False):
    """A copy of src as instance n of the study and series given: new SOP
    Instance UID, the patient's ID set (and the name, unless keep_names,
    for the files whose own names are the test), Burned In Annotation NO
    so an image's names are what is tested, not its pixels. Returns the
    path and the new SOP Instance UID."""
    ds = dcmread(src, force=True)
    if "TransferSyntaxUID" not in ds.file_meta:
        # A file without a meta group, or one that does not name its syntax:
        # name the one pydicom read it in (PS3.10 7.1).
        ds.file_meta.TransferSyntaxUID = ImplicitVRLittleEndian if ds.is_implicit_VR else ExplicitVRLittleEndian
    ds.StudyInstanceUID = study
    ds.SeriesInstanceUID = series
    ds.SOPInstanceUID = generate_uid()
    if "SOPClassUID" not in ds:
        ds.SOPClassUID = "1.2.840.10008.5.1.4.1.1.7"
    ds.file_meta.MediaStorageSOPInstanceUID = ds.SOPInstanceUID
    ds.file_meta.MediaStorageSOPClassUID = ds.SOPClassUID
    ds.PatientID = patient_id
    if not keep_names:
        ds.PatientName = patient
    ds.InstanceNumber = n
    if "PixelData" in ds:
        ds.BurnedInAnnotation = "NO"
    if extra:
        extra(ds)
    ds.save_as(path, write_like_original=False)
    return path, str(ds.SOPInstanceUID)


def burned_in(path, study, series, n, frames=3, rows=64, cols=64, manufacturer="PROVE US", rle=False, burned="YES",
              patient=PATIENT, patient_id=PATIENT_ID, extra_frames=0, modality="US"):
    """An ultrasound multi-frame image drawn by this script: a gradient that
    is never zero, with text-like strokes of 255 in the top band of every
    frame (different in each), its Burned In Annotation YES. extra_frames
    adds the bytes of frames the header does not state."""
    ds = Dataset()
    ds.SOPClassUID = US_MULTIFRAME
    ds.SOPInstanceUID = generate_uid()
    ds.StudyInstanceUID = study
    ds.SeriesInstanceUID = series
    ds.Modality = modality
    ds.Manufacturer = manufacturer
    ds.PatientName = patient
    ds.PatientID = patient_id
    ds.StudyDate = ds.ContentDate = "20240317"
    ds.InstanceNumber = n
    ds.BurnedInAnnotation = burned
    ds.SamplesPerPixel = 1
    ds.PhotometricInterpretation = "MONOCHROME2"
    ds.Rows, ds.Columns = rows, cols
    ds.NumberOfFrames = frames
    ds.FrameIncrementPointer = (0x0018, 0x1063)
    ds.FrameTime = 33.3
    ds.BitsAllocated = ds.BitsStored = 8
    ds.HighBit = 7
    ds.PixelRepresentation = 0
    ds.PixelData = pixels(frames + extra_frames, rows, cols).tobytes()
    meta_for(ds, US_MULTIFRAME)
    if rle:
        ds.compress(RLELossless)
    return save(ds, path)


def pixels(frames, rows, cols):
    """The frames burned_in draws: values 20 to 219 outside the strokes."""
    y, x = np.mgrid[0:rows, 0:cols]
    out = np.empty((frames, rows, cols), dtype=np.uint8)
    for f in range(frames):
        img = ((x + y + 7 * f) % 200 + 20).astype(np.uint8)
        for c in range(4, min(cols, 60), 6):
            img[2:10, c + (f % 3)] = 255
            img[2 + (c + f) % 8, c:c + 4] = 255
        out[f] = img
    return out


def with_comment(src, path, study, series, n):
    """A JPEG baseline image from the corpus with a JPEG comment segment
    (COM, PS3.5 A.4 and ISO 10918-1 B.2.4.5) holding a name and a date put
    right after the start of each frame, and Burned In Annotation NO."""
    ds = dcmread(src)
    frames = list(generate_pixel_data_frame(ds.PixelData, int(ds.get("NumberOfFrames", 1) or 1)))
    seg = b"\xff\xfe" + struct.pack(">H", len(COM_TEXT) + 2) + COM_TEXT
    ds.PixelData = encapsulate([f[:2] + seg + f[2:] for f in frames])
    ds["PixelData"].is_undefined_length = True
    ds.StudyInstanceUID = study
    ds.SeriesInstanceUID = series
    ds.SOPInstanceUID = generate_uid()
    ds.file_meta.MediaStorageSOPInstanceUID = ds.SOPInstanceUID
    ds.PatientName = PATIENT
    ds.PatientID = PATIENT_ID
    ds.InstanceNumber = n
    ds.BurnedInAnnotation = "NO"
    ds.save_as(path, write_like_original=False)
    return path


def jpeg_markers(frame):
    """The markers of a JPEG stream's header, up to its start of scan."""
    out, i = [], 2
    while i + 4 <= len(frame) and frame[i] == 0xFF:
        m = frame[i + 1]
        out.append(m)
        if m == 0xDA:
            break
        i += 2 + struct.unpack(">H", frame[i + 2:i + 4])[0]
    return out


def implicit_private(path, study, series, n):
    """A file in Implicit VR Little Endian whose private elements therefore
    have no VR the file states (PS3.5 6.2.2: a reader gives them UN), one
    holding a name as text."""
    ds = Dataset()
    ds.SOPClassUID = "1.2.840.10008.5.1.4.1.1.7"
    ds.SOPInstanceUID = generate_uid()
    ds.StudyInstanceUID = study
    ds.SeriesInstanceUID = series
    ds.Modality = "OT"
    ds.PatientName = PATIENT
    ds.PatientID = PATIENT_ID
    ds.StudyDate = "20240317"
    ds.InstanceNumber = n
    ds.add_new((0x0019, 0x0010), "LO", "PROVE IMPLICIT")
    ds.add_new((0x0019, 0x1001), "UN", UN_TEXT.encode())
    meta_for(ds, ds.SOPClassUID, ImplicitVRLittleEndian)
    return save(ds, path)


# ---- the values looked for ----

def registered(uid):
    return uid in UID_dictionary or uid.startswith("1.2.840.10008.") or uid in PUBLIC_VOCABULARY


class Needles:
    """The values the archive must not hold, each with the forms it is
    looked for in: ASCII text matched without case and, for a name part,
    as a whole word; text of other scripts byte for byte in UTF-8 and in
    the file's own character set; dates in their written forms with no
    digit beside them; UIDs as written."""

    def __init__(self, everywhere=False):
        self.forms = {}  # bytes form -> (label, whole word)
        # everywhere: the set looked for across the whole installation, in
        # the databases, the log and every stored file at once. It leaves
        # out what from_file finds of dates and institutions: a date
        # shifted in one file can fall on a date another file was sent
        # with, and an institution named for its scanner's maker is a word
        # another file keeps in Manufacturer. Each file is searched for its
        # own dates and institution on its own.
        self.everywhere = everywhere

    def add_text(self, label, text, word=False, codecs=("utf-8",)):
        text = text.strip(" \x00")
        if (len(text) < 4 and text.isascii()) or text.lower() in PLACEHOLDERS:
            return
        for codec in codecs:
            try:
                b = text.encode(codec)
            except (LookupError, UnicodeEncodeError):
                continue
            if len(b) < 4:
                # Fewer bytes than that turn up by chance in pixel data and
                # in a database's pages (one CJK character is two or three).
                continue
            self.forms[b.lower() if text.isascii() else b] = (label, word and text.isascii())

    def add_name(self, label, pn, codecs=("utf-8",), raw=None):
        """A person's name: the whole value, each group and part, and the
        orders people write a family and given name in."""
        self.add_text(label, pn, codecs=codecs)
        for group in pn.split("="):
            parts = [p for p in group.split("^") if p.strip()]
            for p in parts:
                for w in p.split():
                    self.add_text(label, w, word=True, codecs=codecs)
            if len(parts) >= 2:
                fam, giv = parts[0], parts[1]
                for t in (giv + " " + fam, fam + " " + giv, fam + ", " + giv, fam + "," + giv, "^".join(parts)):
                    self.add_text(label, t, codecs=codecs)
        if raw:
            # The value as the file wrote it, escapes of ISO 2022 and all.
            for part in re.split(rb"[=^]", raw):
                if len(part.strip()) >= 4 and not part.isascii():
                    self.forms[part] = (label, False)

    def add_date(self, label, d):
        """A date of eight digits in the written forms people use."""
        if not re.fullmatch(r"\d{8}", d) or d.strip("0") == "":
            return
        y, m, dd = d[:4], d[4:6], d[6:]
        mi, di = int(m), int(dd)
        if not (1 <= mi <= 12 and 1 <= di <= 31):
            return
        name = MONTHS[mi - 1]
        for t in (d, "%s-%s-%s" % (y, m, dd), "%s/%s/%s" % (dd, m, y), "%s/%s/%s" % (m, dd, y), "%s.%s.%s" % (dd, m, y),
                  "%d %s %s" % (di, name, y), "%s %d, %s" % (name, di, y), "%s/%s/%s" % (y, m, dd)):
            self.forms[t.lower().encode()] = (label + " date", "digits")

    def add_uid(self, label, uid):
        uid = uid.strip(" \x00")
        if len(uid) >= 10 and not registered(uid):
            self.forms[uid.encode()] = (label + " uid", "uid")

    def from_file(self, path, label=None):
        """Every identifying value of a file as received: each person's name
        at any depth, the patient's identifiers and the identifying text
        attributes, every date, and every UID outside the registry."""
        ds = dcmread(path, force=True)
        label = label or os.path.basename(path)
        cs = ds.get("SpecificCharacterSet", "")
        cs = [cs] if isinstance(cs, str) else list(cs)
        codecs = ["utf-8"] + [c for c in (charset_codec(x) for x in cs) if c]
        ident = ("PatientID", "OtherPatientIDs", "IssuerOfPatientID", "AccessionNumber", "InstitutionName", "InstitutionAddress",
                 "PatientAddress", "PatientTelephoneNumbers", "StudyID", "RequestedProcedureID", "MilitaryRank", "EthnicGroup")
        for e in walk(ds):
            if e.VR == "PN" and e.value:
                raw = e.value.original_string if hasattr(e.value, "original_string") else None
                for v in (e.value if e.VM > 1 else [e.value]):
                    self.add_name(label + " " + (e.keyword or str(e.tag)), str(v), codecs, raw if isinstance(raw, bytes) else None)
            elif e.VR in ("DA", "DT") and self.everywhere:
                continue
            elif e.keyword in ("InstitutionName", "InstitutionAddress") and self.everywhere:
                continue
            elif e.VR == "DA" and e.value:
                for v in (e.value if e.VM > 1 else [e.value]):
                    self.add_date(label + " " + e.keyword, str(v))
            elif e.VR == "DT" and e.value:
                self.add_date(label + " " + e.keyword, str(e.value)[:8])
            elif e.VR == "UI" and e.value and e.tag.group != 0x0002:
                for v in (e.value if e.VM > 1 else [e.value]):
                    self.add_uid(label + " " + e.keyword, str(v))
            elif e.keyword in ident and e.value:
                # An identifier of words alone (letters and spaces, such as an
                # institution's name) is taken as whole words; one with a digit
                # or a sign anywhere, and every patient identifier, anywhere.
                v = str(e.value).strip()
                words = bool(re.fullmatch(r"[^\W\d_]+( [^\W\d_]+)*", v)) and not e.keyword.startswith(("Patient", "OtherPatient"))
                self.add_text(label + " " + e.keyword, v, word=words, codecs=codecs)

    def search(self, data):
        """The labels of the needles found in data, with the form found."""
        low = data.lower()
        hits = []
        for form, (label, mode) in self.forms.items():
            hay = low if form.isascii() else data
            start = 0
            while True:
                i = hay.find(form, start)
                if i < 0:
                    break
                start = i + 1
                before = hay[i - 1:i] if i else b""
                after = hay[i + len(form):i + len(form) + 1]
                if mode is True and (before.isalnum() or after.isalnum() or before == b"_" or after == b"_"):
                    continue
                if mode == "digits" and (before.isdigit() or after.isdigit()):
                    continue
                if mode == "uid" and (after.isdigit() or after == b"." or before.isdigit() or before == b"."):
                    # Only the UID whole: the start of a longer UID under the
                    # same root is another UID.
                    continue
                hits.append("%s (%r)" % (label, form[:40]))
                break
        return hits

    def __len__(self):
        return len(self.forms)


def walk(ds):
    """Every element of ds at any depth, the file meta excluded."""
    for e in ds:
        yield e
        if e.VR == "SQ" and e.value:
            for item in e.value:
                yield from walk(item)


def charset_codec(term):
    """Python's codec for a Specific Character Set defined term."""
    from pydicom.charset import python_encoding
    return python_encoding.get(term.strip())


# ---- the archive ----

def instances_of(query, receptions):
    """The archived instances of the receptions given, with where each is."""
    if not receptions:
        return []
    marks = ",".join("?" * len(receptions))
    return query("""SELECT i.sop_instance_uid AS uid, i.reception_id AS reception, i.profile_version_id AS version,
        s.subject_label AS subject, s.session_label AS session, s.label AS scan, s.study_uid AS study, s.storage AS storage,
        CASE WHEN i.storage_key = '' THEN 'data/' || s.subject_label || '/' || s.session_label || '/' || s.label || '/'
            || i.sop_instance_uid || '.dcm' ELSE i.storage_key END AS storage_key
        FROM instances AS i JOIN scans AS s ON s.id = i.scan_id
        WHERE i.reception_id IN (%s)""" % marks, *receptions)


def frames_of(ds):
    """The decoded frames of an image as an array of (frames, rows, cols)."""
    a = ds.pixel_array
    n = int(ds.get("NumberOfFrames", 1) or 1)
    return a.reshape((n, int(ds.Rows), int(ds.Columns)) + a.shape[3 if n > 1 else 2:])


def pixel_rule_holds(original, archived, band=BAND):
    """Compares an archived image with the one sent, frame by frame: every
    pixel inside the band filled with 0, every pixel outside it unchanged.
    Returns (frames, inside zero, outside equal)."""
    a, b = frames_of(original), frames_of(archived)
    if a.shape != b.shape:
        return 0, False, False
    x, y, w, h = band
    inside = b[:, y:y + h, x:x + w]
    mask = np.ones(a.shape[1:3], dtype=bool)
    mask[y:y + h, x:x + w] = False
    return a.shape[0], bool((inside == 0).all()), bool((a[:, mask] == b[:, mask]).all())


def sop_of(path):
    return str(dcmread(path, stop_before_pixels=True, force=True).SOPInstanceUID)


def private_tag(ds, creator, group, element):
    """The tag a private element of a creator has in ds, or None."""
    for e in ds:
        if e.tag.group == group and 0x10 <= e.tag.element <= 0xFF and str(e.value).strip() == creator:
            return (group << 16) | (e.tag.element << 8) | element
    return None


def element_text(e):
    """An element's value written as the trial run writes it: multiple
    values joined by a backslash; None for a value with no text form."""
    if e.VR in ("SQ", "OB", "OW", "OD", "OF", "OL", "OV", "UN") or e.value is None:
        return None
    if e.VM > 1:
        return "\\".join(str(v) for v in e.value)
    return str(e.value)


def foreign(ds):
    """Gives a copy sent to a receiver that archives as received dates and
    UIDs of its own, so the values the archive legitimately keeps from it
    are never among those looked for in what was de-identified."""
    keep = {0x0020000D, 0x0020000E, 0x00080018}
    for e in walk(ds):
        if e.VR == "DA" and e.value:
            e.value = "19990505"
        elif e.VR == "DT" and e.value:
            e.value = "19990505120000"
        elif e.VR == "UI" and e.value and e.tag not in keep and e.VM == 1 and not registered(str(e.value)):
            e.value = generate_uid()
