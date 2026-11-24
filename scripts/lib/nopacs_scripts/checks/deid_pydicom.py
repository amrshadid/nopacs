"""Check de-identified files against their originals with pydicom.

Input: the path of a JSON file:
  {"keywords": [<DICOM keywords whose values must be nowhere in a result>],
   "files": [{"original": <path of a file as received>,
              "deidentified": <path of the same file de-identified>,
              "needles": <path of a JSON list of the values the engine
                          removed or replaced: {"kind": "name" | "date" |
                          "uid" | "text", "value": ..., and "edge": "word"
                          for one matched only as a whole word, "ident"
                          for an identifier of words}>}]}

The values searched for in each result, its needles, come from three
sources, so a mistake in one is caught by another:

  - the engine's own needles for the file;
  - pydicom's reading of the original: the value of every element named by
    a keyword, and of every element PS3.15 Table E.1-1 removes or replaces
    whatever a profile says (FLOOR below: the names, identifiers, birth
    date and time, addresses and telephone numbers of persons), and of
    every element of VR PN, at any depth of sequences;
  - the values of the original's private elements that are a person's
    name, which the engine never keeps whatever a profile says: one of VR
    PN, and one of unknown VR (every private element of an implicit VR
    file) that has a name's form (a "^" between letters), when it is not
    the whole value of a standard element of the original. Other private
    values are not searched for: a private element's meaning is not known,
    its values are mostly a vendor's settings, and the engine makes no
    needle of them unless they are names.

  - every value inside a sequence covered whole (WHOLE below): Other
    Patient IDs Sequence and Person Identification Code Sequence at any
    depth whatever a profile says, and the identification and issuer
    sequences when their keyword is given (the profile removes them); but
    a number, a coded string, a time, and the attributes that name a coding
    scheme (CODING below), as the engine.

Only values that can identify by themselves are needles, as the engine's
(internal/deid/engine/needles.go): names, identifiers, dates and UIDs. The
keywords given are expected to be such attributes; a time, a value of a
numeric VR, a coded string or a description is not one. Where this script
differs from the engine: it reads no GPS coordinate and no attribute its
data dictionary does not know unless a keyword names it, and no value a
rule hashed; those come only from the engine's needles.

Each needle is searched for in the written forms the engine's clean
removes (internal/deid/engine/forms.go): a person's name as DOE^JOHN, JOHN
DOE, DOE JOHN, DOE, JOHN, DOE,JOHN, JOHN PAUL DOE, and each part, every form
holding a Latin part of one or two letters only as a whole word (LE^AN is
not in "SCAN LEFT"); a full date as 20240315, 2024-03-15, 2024/03/15,
2024.03.15, 15/03/2024, 03/15/2024, 15.03.2024 and 15-03-2024; as 15032024
and 03152024 with no digit beside them; and, only where they stand alone as
a date (no ASCII letter or digit beside them, nor a date's separator with a
digit beyond it), without leading zeros (3/5/2024, 2024-3-5), with a
two-digit year (15/03/24, "15 Mar 24"), with the month's English name, its
three letters and those with a dot, the day as an ordinal ("15th March
2024", "Mar. 15, 2024", "15-Mar-2024", "15MAR2024"), in Chinese and
Japanese (2024年3月15日, 2024年03月15日) and Korean (2024년 3월 15일,
"2024. 3. 15."). A date of only a year, or a year and month (a DT of 4 or 6
digits, or a month or day of 00), only at that precision, standing alone:
2024, 2024年, or 202403, 2024-03, 03/2024, 3/2024, "March 2024", 2024年3月; a
letter of another script beside it does not join it ("1957年" is the year
1957). Letters in any case. A needle that is a number is matched only where
no digit is next to it, so it is not found inside a longer number (a
remapped UID is a long run of digits); a full date may be followed by
digits, as in a date and time. An identifier of words (a letter at an
end, no digit, no letter of a script written without spaces) that is not an
attribute identifying the patient (FLOOR, PATIENT_IDS, or inside a sequence
covered whole) is matched on a letter side only where no letter and no "_"
is beside it, as the engine's (EdgeIdentifier): an institution TOSHIBA is
not found in the maker's name TOSHIBA_MEC. Every other identifier is
matched anywhere but next to a digit on a digit side: a Patient ID
AB778812 is found in MRNAB778812, an institution 東京中央病院 in
東京中央病院放射線科. The side file names the engine's word edge "ident";
when one form is both, it is matched anywhere.

Two searches of each result:

  - every element's value as pydicom decodes it, the file meta information,
    private elements and every item of every sequence included, Pixel Data
    excepted;
  - the raw bytes of the whole file, Pixel Data's value cut out, for each
    form encoded in UTF-8 and in the original's character set (its
    Specific Character Set, as Python's codecs write it), for what a parser
    could miss.

Text is compared in Unicode Normalization Form C (unicodedata.normalize
with "NFC"), forms and decoded values alike, so a name written with
combining marks ("u" and U+0308) is the name written composed ("ü"); the
raw bytes, which nothing normalized, are searched for each form composed
and decomposed (NFD).

Values shorter than 4 characters are not searched for (they match inside
ordinary text), and nor is one character of a script written without
spaces (Han, kana, Hangul), whose two or more characters are searched
for; UIDs the standard registers are not needles. What is skipped is
counted and printed, never passed over silently. The result must also say
Patient Identity Removed (0012,0062) YES. A result pydicom cannot read is
reported as refused, never passed.

Exit status 0 when no needle is found in any result and at least one was
searched for; 1 otherwise. A finding prints the original, the form and
where it was found (the values are synthetic or public test data).
"""
import json
import re
import unicodedata

import pydicom
from pydicom.charset import convert_encodings
from pydicom.datadict import dictionary_VR
from pydicom.uid import UID

MIN_LENGTH = 4
PIXEL_TAGS = {0x7FE00008, 0x7FE00009, 0x7FE00010}

# The attributes PS3.15 Table E.1-1 removes or replaces in the basic
# profile and no option keeps, searched for whatever a profile decided.
FLOOR = {
    "PatientName", "PatientID", "OtherPatientIDs", "OtherPatientNames",
    "OtherPatientIDsSequence", "PatientBirthName", "PatientBirthDate",
    "PatientBirthTime", "PatientAddress", "PatientMotherBirthName",
    "PatientTelephoneNumbers", "PatientTelecomInformation", "PersonAddress",
    "PersonTelephoneNumbers", "PersonTelecomInformation",
    "ReferringPhysicianAddress", "ReferringPhysicianTelephoneNumbers",
    "CountryOfResidence", "RegionOfResidence", "MedicalRecordLocator",
    "PatientInstitutionResidence", "PersonIdentificationCodeSequence",
}


# The sequences whose every value is a needle when removed
# (internal/deid/engine/needles.go, wholeTags); the first two are the
# floor's and are searched whatever a profile decided.
# The identifiers, besides FLOOR, that identify the patient or the
# patient's encounter, order or study, never matched as whole words (the
# engine's patientIdentifierTags).
PATIENT_IDS = {
    "AccessionNumber", "IssuerOfPatientID", "IssuerOfClinicalTrialProtocolID",
    "IssuerOfClinicalTrialSiteID", "ClinicalTrialSubjectID",
    "IssuerOfClinicalTrialSubjectID", "ClinicalTrialSubjectReadingID",
    "IssuerOfClinicalTrialSubjectReadingID", "IssuerOfClinicalTrialTimePointID",
    "IssuerOfClinicalTrialSeriesID", "StudyID", "StudyIDIssuer", "AdmissionID",
    "IssuerOfAdmissionID", "ServiceEpisodeID", "IssuerOfServiceEpisodeID",
    "ScheduledProcedureStepID", "PerformedProcedureStepID",
    "SpecimenAccessionNumber", "RequestedProcedureID", "PlacerOrderNumberImagingServiceRequest",
    "FillerOrderNumberImagingServiceRequest", "ResultsIDIssuer",
    "InterpretationIDIssuer",
}

WHOLE_FLOOR = {"OtherPatientIDsSequence", "PersonIdentificationCodeSequence"}
WHOLE = WHOLE_FLOOR | {
    "InstitutionCodeSequence", "ReferringPhysicianIdentificationSequence",
    "ConsultingPhysicianIdentificationSequence",
    "PhysiciansOfRecordIdentificationSequence",
    "PerformingPhysicianIdentificationSequence",
    "PhysiciansReadingStudyIdentificationSequence",
    "OperatorIdentificationSequence",
    "ScheduledPerformingPhysicianIdentificationSequence",
    "IntendedRecipientsOfResultsIdentificationSequence",
    "VerifyingObserverIdentificationCodeSequence",
    "VerbalSourceIdentifierCodeSequenceTrial",
    "ContentCreatorIdentificationCodeSequence",
    "IssuerOfAdmissionIDSequence", "IssuerOfServiceEpisodeIDSequence",
    "IssuerOfTheContainerIdentifierSequence",
    "IssuerOfTheSpecimenIdentifierSequence",
}

# The attributes that name a coding scheme or a context group, the same in
# every file that uses it: no needle inside a sequence covered whole.
CODING = {0x00080102, 0x00080103, 0x00080105, 0x00080106, 0x00080107,
          0x0008010B, 0x0008010D, 0x0008010F, 0x00080117}

# The VRs of a value inside a sequence covered whole that is a needle.
WHOLE_VRS = {"AE", "LO", "LT", "SH", "ST", "UC", "UR", "UT", "PN", "DA",
             "DT", "UI", "UN"}


def nfc(s):
    """s in Unicode Normalization Form C, the one form text is compared in."""
    return unicodedata.normalize("NFC", s)


def words(s):
    """Whether an identifier is words: a letter at an end, no digit, no
    letter of a script written without spaces."""
    s = s.strip(" \x00")
    return bool(s) and (s[0].isalpha() or s[-1].isalpha()) and not any(c.isdigit() for c in s) and not unspaced(s)


def unspaced(s):
    """Whether s holds a letter of a script written without spaces."""
    for ch in s:
        name = unicodedata.name(ch, "")
        if name.startswith(("CJK", "HIRAGANA", "KATAKANA", "HANGUL")):
            return True
    return False


class Needles:
    """The forms searched for, and what was skipped."""

    def __init__(self):
        self.forms = {}  # lowered form -> (needle, kind)
        self.skipped = 0

    def add_form(self, form, needle, kind):
        form = nfc(form.strip(" \x00"))
        if not form:
            return
        if unspaced(form):
            if len(form) < 2:
                self.skipped += 1
                return
        elif len(form) < MIN_LENGTH:
            self.skipped += 1
            return
        key = form.lower()
        # A form that is a word for one source and not for another is
        # matched anywhere: the stricter search.
        if key not in self.forms or (self.forms[key][1] == "ident" and kind != "ident"):
            self.forms[key] = (needle, kind)

    def add(self, kind, value):
        value = value.strip(" \x00")
        if not value:
            return
        if kind == "uid":
            try:
                if UID(value).is_valid and UID(value).name != value:
                    return
            except Exception:  # not a UID pydicom knows how to name
                pass
        if kind == "date":
            forms, partial = date_forms(value)
            if not partial:
                self.add_form(value, value, "date")
            for f, edge in forms:
                self.add_form(f, value, edge)
            return
        if kind == "name":
            for f, word in name_forms(value):
                self.add_form(f, value, "word" if word else "name")
            return
        self.add_form(value, value, kind)


MIN_PART = 3


def short_part(w):
    """A Latin word of a name shorter than three letters, found inside
    ordinary words ("LE" in "LEFT"): every form holding one is matched only
    as a whole word."""
    return not unspaced(w) and len(w) < MIN_PART


def name_forms(value):
    """The forms of a person's name as (form, whole word only) pairs."""
    out = []

    def add(text, words):
        out.append((text, any(short_part(w) for w in words)))

    every = []
    for group in value.split("="):
        parts = [p.strip() for p in group.split("^") if p.strip()]
        words = [w for p in parts for w in p.split()]
        every += words
        add(group, words)
        for p in parts:
            add(p, p.split())
            if len(p.split()) > 1:
                for w in p.split():
                    add(w, [w])
        if len(parts) >= 2:
            fam, given = parts[0], parts[1]
            for f in (f"{given} {fam}", f"{fam} {given}", f"{fam}, {given}", f"{fam},{given}"):
                add(f, fam.split() + given.split())
            add(" ".join(parts), words)
        if len(parts) >= 3:
            add(f"{parts[1]} {parts[2]} {parts[0]}", parts[1].split() + parts[2].split() + parts[0].split())
    add(value, every)
    return out


MONTHS = ["January", "February", "March", "April", "May", "June", "July",
          "August", "September", "October", "November", "December"]


def ordinal(d):
    if 11 <= d <= 13:
        return "th"
    return {1: "st", 2: "nd", 3: "rd"}.get(d % 10, "th")


def year_forms(y):
    return [(y, "bounded"), (y + "年", "bounded"), (y + "년", "bounded")]


def month_forms(y, mp):
    mo = int(mp)
    if not 1 <= mo <= 12:
        return year_forms(y)
    mm, name = str(mo), MONTHS[mo - 1]
    out = [y + mp, f"{y}-{mp}", f"{y}/{mp}", f"{y}.{mp}", f"{mp}/{y}", f"{mp}.{y}", f"{mp}-{y}",
           f"{y}-{mm}", f"{mm}/{y}", f"{name} {y}", f"{name[:3]} {y}", f"{name[:3]}. {y}",
           f"{y}年{mm}月", f"{y}年{mp}月", f"{y}년 {mm}월", f"{y}년{mm}월"]
    return [(f, "bounded") for f in out]


def date_forms(v):
    """The written forms of a DA or DT value, as (form, edge) pairs (edge:
    "date" anywhere, a full date a time may follow; "bounded" standing
    alone as a date; "digits" with no digit beside it), and whether it is a
    date of only a year or a year and month, whose value is then no form of
    its own. A month or day of 00, or out of range, is unknown: the forms of
    the precision the value has are given. These are the engine's forms
    (internal/deid/engine/forms.go)."""
    m = re.match(r"^(\d{4})(\d{2})?(\d{2})?", v)
    if not m:
        return [], False
    digits = len(m.group(0))
    if len(v) > digits and v[digits].isdigit():
        return [], False
    y = m.group(1)
    if digits == 4:
        return year_forms(y), True
    if digits == 6:
        return month_forms(y, m.group(2)), True
    mp, dp = m.group(2), m.group(3)
    out = [(f, "date") for f in (y + mp + dp, f"{y}-{mp}-{dp}", f"{y}/{mp}/{dp}", f"{y}.{mp}.{dp}",
                                 f"{dp}/{mp}/{y}", f"{mp}/{dp}/{y}", f"{dp}.{mp}.{y}", f"{dp}-{mp}-{y}")]
    mo, di = int(mp), int(dp)
    if not 1 <= mo <= 12 or not 1 <= di <= 31:
        return out + month_forms(y, mp), False
    out += [(dp + mp + y, "digits"), (mp + dp + y, "digits")]
    mm, dd, yy, name = str(mo), str(di), y[2:], MONTHS[mo - 1]
    bounded = [f"{y}-{mm}-{dd}", f"{y}/{mm}/{dd}", f"{y}.{mm}.{dd}", f"{dd}/{mm}/{y}", f"{mm}/{dd}/{y}",
               f"{dd}.{mm}.{y}", f"{dd}-{mm}-{y}", f"{dp}/{mp}/{yy}", f"{mp}/{dp}/{yy}", f"{dp}.{mp}.{yy}",
               f"{dp}-{mp}-{yy}", f"{dd}/{mm}/{yy}", f"{mm}/{dd}/{yy}", f"{dd}.{mm}.{yy}",
               f"{y}年{mm}月{dd}日", f"{y}年{mp}月{dp}日", f"{y}년 {mm}월 {dd}일", f"{y}년{mm}월{dd}일",
               f"{y}. {mm}. {dd}.", f"{y}. {mp}. {dp}."]
    abbr = name[:3]
    for day in (dp, dd, dd + ordinal(di)):
        for n in (name, abbr, abbr + "."):
            bounded += [f"{day} {n} {y}", f"{n} {day}, {y}", f"{n} {day} {y}"]
        bounded.append(f"{day} {abbr} {yy}")
    for day in (dp, dd):
        bounded += [f"{day}-{abbr}-{y}", f"{day}{abbr}{y}"]
    return out + [(f, "bounded") for f in bounded], False


def values_of(elem):
    """The text forms of an element's value."""
    v = elem.value
    items = list(v) if isinstance(v, (list, pydicom.multival.MultiValue)) else [v]
    out = []
    for x in items:
        if x is None:
            continue
        if isinstance(x, bytes):
            out.append(x.decode("latin-1"))
        else:
            out.append(str(x))
    if len(items) > 1:
        out.append("\\".join(str(x) for x in items))
    return out


NAME_FORM = re.compile(r"^[^\W\d_][^\d\\]*\^[^\d\\]*[^\W\d_]")


def kind_of(elem, value=""):
    """The kind of an element's value: by the data dictionary's VR for a
    standard element, whatever VR the file wrote it with (a person's name
    written as LO is a name); a private value of unknown VR is a name when
    it has a name's form."""
    vr = elem.VR
    if not elem.tag.is_private:
        try:
            vr = dictionary_VR(elem.tag)
        except KeyError:
            pass
    elif vr == "UN" and NAME_FORM.match(value.strip(" \x00")):
        return "name"
    if "PN" in vr.split(" or "):
        return "name"
    return {"DA": "date", "DT": "date", "UI": "uid"}.get(vr, "text")


def gather(orig, keywords, needles):
    """Needles pydicom reads from the original."""
    standard_values = set()
    private = []
    # Elements inside a sequence that identifies the patient, which are
    # never matched as whole words.
    inside = set()
    for elem in orig.iterall():
        if elem.VR == "SQ" and (elem.keyword in FLOOR or elem.keyword in WHOLE or elem.keyword in PATIENT_IDS):
            for item in elem.value:
                inside.update(id(e) for e in item.iterall())
    for elem in orig.iterall():
        if elem.VR == "SQ" or elem.tag in PIXEL_TAGS:
            continue
        try:
            vals = values_of(elem)
        except Exception:  # a value pydicom cannot convert
            continue
        if elem.tag.is_private:
            if not elem.tag.is_private_creator and elem.VR in ("PN", "UN"):
                private.extend((kind_of(elem, s), s) for s in vals)
            continue
        standard_values.update(nfc(s.strip(" \x00")).lower() for s in vals)
        if elem.keyword in keywords or elem.keyword in FLOOR or kind_of(elem) == "name":
            patient = elem.keyword in FLOOR or elem.keyword in PATIENT_IDS or id(elem) in inside
            for s in vals:
                kind = kind_of(elem)
                if kind == "text" and not patient and words(s):
                    kind = "ident"
                needles.add(kind, s)
    for elem in orig.iterall():
        if elem.VR != "SQ" or not (elem.keyword in WHOLE_FLOOR or (elem.keyword in WHOLE and elem.keyword in keywords)):
            continue
        for item in elem.value:
            for inner in item.iterall():
                if inner.VR not in WHOLE_VRS or inner.tag in CODING or inner.tag.is_private_creator:
                    continue
                try:
                    vals = values_of(inner)
                except Exception:  # a value pydicom cannot convert
                    continue
                for v in vals:
                    if inner.VR == "UN" and not v.strip(" \x00").isprintable():
                        continue
                    needles.add(kind_of(inner, v), v)
    for kind, s in private:
        s = s.strip(" \x00")
        if kind != "name" or not s.isprintable() or nfc(s).lower() in standard_values:
            continue
        needles.add("name", s)


def texts(ds):
    """Every element's value as text, with where it is."""
    out = []
    elems = list(ds.file_meta.iterall()) if hasattr(ds, "file_meta") else []
    elems += list(ds.iterall())
    for elem in elems:
        if elem.tag in PIXEL_TAGS or elem.VR == "SQ":
            continue
        try:
            for s in values_of(elem):
                out.append((str(elem.tag), nfc(s).lower()))
        except Exception:  # a value pydicom cannot convert is read as bytes
            out.append((str(elem.tag), repr(getattr(elem, "value", b"")).lower()))
    return out


def pattern(form, kind):
    """A regular expression for a form (text or bytes): a form that begins
    with a digit must not follow one, and one that ends with a digit must
    not be followed by one, but for a full date, which a time may follow; an
    identifier of words' side that is a letter must have no letter or "_"
    beside it; a
    bounded form must have no ASCII letter or digit beside it, nor a date's
    separator with a digit beyond it."""
    as_bytes = isinstance(form, bytes)
    text = form.decode("latin-1") if as_bytes else form
    body = re.escape(form)
    if kind == "word":
        if as_bytes:
            return re.compile(rb"(?<![0-9A-Za-z])" + body + rb"(?![0-9A-Za-z])")
        return re.compile(r"(?<![^\W_])" + body + r"(?![^\W_])")
    if kind == "digits":
        if as_bytes:
            return re.compile(rb"(?<![0-9])" + body + rb"(?![0-9])")
        return re.compile(r"(?<![0-9])" + body + r"(?![0-9])")
    if kind == "bounded":
        if as_bytes:
            return re.compile(rb"(?<![0-9A-Za-z])(?<![0-9][-/.])" + body + rb"(?![0-9A-Za-z])(?![-/.][0-9])")
        return re.compile(r"(?<![0-9A-Za-z])(?<![0-9][-/.])" + body + r"(?![0-9A-Za-z])(?![-/.][0-9])")
    # In bytes only an ASCII letter is known for one: another byte may be
    # part of a character of another set, so that side errs toward finding.
    def letter(c):
        return c.isalpha() and (c.isascii() or not as_bytes)
    if kind == "ident" and letter(text[:1]):
        body = (rb"(?<![A-Za-z_])" if as_bytes else r"(?<![^\W\d])") + body
    elif text[:1].isdigit():
        body = (rb"(?<![0-9])" if as_bytes else r"(?<![0-9])") + body
    if kind == "ident" and letter(text[-1:]):
        body = body + (rb"(?![A-Za-z_])" if as_bytes else r"(?![^\W\d])")
    elif text[-1:].isdigit() and kind != "date":
        body = body + (rb"(?![0-9])" if as_bytes else r"(?![0-9])")
    return re.compile(body)


def raw_without_pixels(path, ds):
    with open(path, "rb") as f:
        raw = f.read()
    for t in PIXEL_TAGS:
        if t in ds:
            v = ds[t].value
            if isinstance(v, bytes) and v:
                i = raw.rfind(v)
                if i >= 0:
                    raw = raw[:i] + raw[i + len(v):]
    return raw.lower()


def codecs_of(orig):
    try:
        cs = orig.get("SpecificCharacterSet", "ISO_IR 6")
        cs = list(cs) if isinstance(cs, pydicom.multival.MultiValue) else [cs]
        return [c for c in set(convert_encodings(cs)) if c not in ("utf-8", "ascii")]
    except Exception:
        return ["latin-1"]


def encoded(form, codec):
    """form in codec, its escape sequences removed (ISO 2022)."""
    try:
        b = form.encode(codec)
    except Exception:
        return None
    b = re.sub(rb"\x1b[\x24\x28\x29\x2d][\x40-\x7e]?[\x40-\x7e]", b"", b)
    return b if len(b) >= MIN_LENGTH else None


def check_pair(pair, keywords):
    """Search one result; return the forms searched, skipped and found."""
    bad = 0
    orig = pydicom.dcmread(pair["original"], force=True)
    out = pydicom.dcmread(pair["deidentified"])
    if str(out.get("PatientIdentityRemoved", "")) != "YES":
        print(f"{pair['deidentified']}: Patient Identity Removed is not YES")
        bad += 1
    needles = Needles()
    with open(pair["needles"], encoding="utf-8") as f:
        for n in json.load(f):
            edge = n.get("edge")
            needles.add(edge if edge in ("word", "ident") else n["kind"], n["value"])
    gather(orig, keywords, needles)
    hay = texts(out)
    raw = raw_without_pixels(pair["deidentified"], out)
    codecs = codecs_of(orig)
    for form, (needle, kind) in sorted(needles.forms.items()):
        rx = pattern(form, kind)
        hits = [where for where, s in hay if rx.search(s)]
        if not hits:
            raws = set()
            for variant in (nfc(form), nfc(form.upper())):
                raws.add(variant.encode("utf-8").lower())
                raws.add(unicodedata.normalize("NFD", variant).encode("utf-8").lower())
                raws.update(e.lower() for e in (encoded(variant, c) for c in codecs) if e)
            hits = ["raw bytes" for b in sorted(raws) if pattern(b, kind).search(raw)]
        if hits:
            print(f"{pair['original']}: {needle!r} found as {form!r} in {hits[0]} of the de-identified file")
            bad += 1
    return len(needles.forms), needles.skipped, bad


def main(argv):
    with open(argv[0], encoding="utf-8") as f:
        spec = json.load(f)
    keywords = set(spec["keywords"])
    searched = skipped = bad = 0
    for pair in spec["files"]:
        try:
            n, s, b = check_pair(pair, keywords)
        except Exception as e:  # a file pydicom cannot read is no pass
            print(f"{pair['original']}: refused, a file of the pair cannot be read ({type(e).__name__})")
            bad += 1
            continue
        searched += n
        skipped += s
        bad += b
    print(f"{len(spec['files'])} files, {searched} forms searched for, {skipped} skipped as too short, {bad} found")
    if bad or searched == 0:
        return 1

