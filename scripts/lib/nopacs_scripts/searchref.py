"""A reference search over DICOM files, written from the condition language's
grammar and the DICOM standard, not from the archive's code.

A search answer must equal what this module computes from the files that were
received: the files are read with pydicom, grouped as the archive groups them
(a subject by Patient ID, a session by Study Instance UID, a scan by Series
Instance UID), and a condition is parsed and evaluated here on those rows.

The functions take their input and return data; none changes a global or
reads the environment.

The reading of a scan follows the archive's published rule: a scan is
described by the file of its series with the lowest SOP Instance UID (compared
as text), so the row does not depend on the order the files arrived in.
"""

import re
import unicodedata

import pydicom
from pydicom.multival import MultiValue

# The attributes a search holds: keyword, how the value is kept (text, number,
# date, datetime), and whether the attribute may hold several values.
ATTRIBUTES = {
    "Modality": ("text", False),
    "SeriesDescription": ("text", False),
    "SeriesNumber": ("number", False),
    "ProtocolName": ("text", False),
    "StudyDescription": ("text", False),
    "BodyPartExamined": ("text", False),
    "Manufacturer": ("text", False),
    "ManufacturerModelName": ("text", False),
    "StationName": ("text", False),
    "SequenceName": ("text", False),
    "PatientSex": ("text", False),
    "PatientAge": ("text", False),
    "MagneticFieldStrength": ("number", False),
    "SliceThickness": ("number", False),
    "RepetitionTime": ("number", False),
    "EchoTime": ("number", False),
    "FlipAngle": ("number", False),
    "AcquisitionDate": ("date", False),
    "AcquisitionDateTime": ("datetime", False),
    "ImageType": ("text", True),
    "ScanningSequence": ("text", True),
    "OperatorsName": ("text", True),
}

# The attributes that have a folded form (compared with "=" and "nocase").
FOLDED = {
    "Modality", "SeriesDescription", "ProtocolName", "StudyDescription", "BodyPartExamined", "Manufacturer",
    "ManufacturerModelName", "StationName", "SequenceName", "ImageType", "ScanningSequence", "OperatorsName",
}


def fold(text):
    """The text compared without regard to case or accents: case folded in
    full (so "ß" is "ss"), accents removed, spaces at the ends trimmed."""
    decomposed = unicodedata.normalize("NFD", text.casefold())
    kept = "".join(c for c in decomposed if unicodedata.category(c) != "Mn")
    return unicodedata.normalize("NFC", kept).strip()


def normal_date(text):
    """A DA value (or the date of a DT value) as YYYYMMDD, or None."""
    if len(text) >= 8 and text[:8].isdigit():
        return text[:8]
    return None


def normal_datetime(text):
    """A DT value as YYYYMMDDHHMMSS (PS3.5 6.2): the parts it leaves out are
    the lowest the part can be, its fraction and its offset dropped. None
    for a value that is not a date-time."""
    for cut in "+-.":
        if cut in text:
            text = text[: text.index(cut)]
    if not text.isdigit() or len(text) < 4 or len(text) > 14 or len(text) % 2:
        return None
    if len(text) < 8:
        return (text + "0101")[:8] + "000000"
    return text + "0" * (14 - len(text))


def column_values(keyword, element):
    """What an element gives a search for the attribute: the values of the
    attribute's kind, each once for a multi-valued attribute, the first only
    for any other."""
    kind, multi = ATTRIBUTES[keyword]
    raw = list(element) if isinstance(element, MultiValue) else [element]
    out = []
    for v in raw:
        text = str(v).strip()
        if not text:
            continue
        if kind == "number":
            try:
                value = float(text)
            except ValueError:
                continue
        elif kind == "date":
            value = normal_date(text)
        elif kind == "datetime":
            value = normal_datetime(text)
        else:
            value = text
        if value is None or (multi and value in out):
            continue
        out.append(value)
    return out if multi else out[:1]


def read_instance(path):
    """What a file says that a search holds: its three identifiers, its
    patient and study date, and the values of every attribute."""
    ds = pydicom.dcmread(path, stop_before_pixels=True)
    values = {}
    for keyword in ATTRIBUTES:
        found = ds.get(keyword)
        values[keyword] = column_values(keyword, found) if found is not None else []
    date = str(ds.get("StudyDate", "")).strip()
    return {
        "sop": str(ds.SOPInstanceUID), "series": str(ds.SeriesInstanceUID), "study": str(ds.StudyInstanceUID),
        "patient": str(ds.get("PatientID", "")).strip(), "date": normal_date(date) if date else None, "values": values,
    }


def build_rows(paths):
    """The rows of the three levels from the files: scans (by series), with
    the values of the file of the lowest SOP Instance UID, sessions (by
    study) and subjects (by patient), each a dict, linked by `session` and
    `subject` keys on a scan and `scans` and `sessions` lists above."""
    lowest = {}
    for path in paths:
        i = read_instance(path)
        if i["series"] not in lowest or i["sop"] < lowest[i["series"]]["sop"]:
            lowest[i["series"]] = i
    subjects, sessions, scans = {}, {}, {}
    for series, i in lowest.items():
        subject = subjects.setdefault(i["patient"], {"id": i["patient"], "sessions": [], "scans": []})
        session = sessions.get(i["study"])
        if session is None:
            session = sessions[i["study"]] = {"id": i["study"], "date": i["date"], "scans": [], "subject": subject}
            subject["sessions"].append(session)
        scan = {"id": series, "values": i["values"], "session": session, "subject": subject}
        session["scans"].append(scan)
        subject["scans"].append(scan)
        scans[series] = scan
    return {"subject": subjects, "session": sessions, "scan": scans}


def key_of(level, row):
    """How an answer names a row: a scan by its Series Instance UID, a
    session by its Study Instance UID and a subject by the sorted Study
    Instance UIDs of its sessions."""
    if level == "scan" or level == "session":
        return row["id"]
    return ",".join(sorted(s["id"] for s in row["sessions"]))


# The tokens of a condition.
TOKEN = re.compile(
    r"""\s*(?:
    (?P<str>"(?:[^"\\]|\\.)*")
    |(?P<datetime>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2})?)
    |(?P<date>\d{4}-\d{2}-\d{2})
    |(?P<num>-?\d+(?:\.\d+)?)
    |(?P<op><=|>=|==|!=|=|<|>)
    |(?P<punct>[(),])
    |(?P<word>[A-Za-z_][A-Za-z0-9_.]*)
    )""",
    re.VERBOSE,
)


def tokens(text):
    """The tokens of a condition as (kind, text) pairs."""
    out, at = [], 0
    while text[at:].strip():
        m = TOKEN.match(text, at)
        if not m:
            raise ValueError("cannot read the condition at %r" % text[at:])
        kind = m.lastgroup
        out.append((kind, m.group(kind)))
        at = m.end()
    return out


class Parser:
    """A recursive-descent reader of the part of the grammar the reference
    checks cover: and, or, not, parentheses, has and every, and the leaves
    with present, absent, the comparison operators, in, between, prefix,
    like and contains."""

    def __init__(self, text):
        self.toks = tokens(text)
        self.at = 0

    def peek(self):
        return self.toks[self.at] if self.at < len(self.toks) else (None, None)

    def take(self):
        t = self.peek()
        self.at += 1
        return t

    def word(self, w):
        return self.peek() == ("word", w)

    def expect(self, kind, text=None):
        k, t = self.take()
        if k != kind or (text is not None and t != text):
            raise ValueError("expected %s %s, got %s %s" % (kind, text or "", k, t))
        return t

    def parse(self):
        node = self.expr()
        if self.peek()[0] is not None:
            raise ValueError("text after the condition: %r" % (self.peek(),))
        return node

    def expr(self):
        kids = [self.conj()]
        while self.word("or"):
            self.take()
            kids.append(self.conj())
        return kids[0] if len(kids) == 1 else ("or", kids)

    def conj(self):
        kids = [self.unary()]
        while self.word("and"):
            self.take()
            kids.append(self.unary())
        return kids[0] if len(kids) == 1 else ("and", kids)

    def unary(self):
        if self.word("not"):
            self.take()
            return ("not", self.unary())
        if self.peek() == ("punct", "("):
            self.take()
            node = self.expr()
            self.expect("punct", ")")
            return node
        if self.word("has") or self.word("every"):
            every = self.take()[1] == "every"
            level = self.expect("word")
            self.expect("punct", "(")
            child = self.expr()
            self.expect("punct", ")")
            return ("has", every, level, child)
        return self.leaf()

    def value(self):
        kind, text = self.take()
        if kind == "str":
            return ("text", text[1:-1].replace('\\"', '"').replace("\\\\", "\\"))
        if kind == "num":
            return ("number", float(text))
        if kind == "date":
            return ("date", text.replace("-", ""))
        if kind == "datetime":
            digits = text.replace("-", "").replace(":", "").replace("T", "")
            return ("datetime", digits + "0" * (14 - len(digits)))
        raise ValueError("expected a value, got %s %s" % (kind, text))

    def leaf(self):
        field = self.expect("word")
        if self.word("present") or self.word("absent"):
            return ("leaf", field, self.take()[1], False, [])
        nocase = False
        if self.word("nocase"):
            self.take()
            nocase = True
        kind, text = self.take()
        if kind == "op":
            op = text
        elif kind == "word" and text in ("in", "between", "prefix", "like", "contains"):
            op = text
        else:
            raise ValueError("expected an operator, got %s %s" % (kind, text))
        if op == "in":
            self.expect("punct", "(")
            values = [self.value()]
            while self.peek() == ("punct", ","):
                self.take()
                values.append(self.value())
            self.expect("punct", ")")
        elif op == "between":
            low = self.value()
            self.expect("word", "and")
            values = [low, self.value()]
        else:
            values = [self.value()]
        # "=" compares folded text; "contains" always does; "nocase" folds
        # the others. Only text is folded.
        is_text = values[0][0] == "text"
        folded = is_text and (nocase or op in ("=", "contains"))
        if op == "!=":
            return ("not", ("leaf", field, "=", folded, values))
        return ("leaf", field, op, folded, values)


def parse(text):
    """The tree of a condition text; None for an empty one."""
    return Parser(text).parse() if text.strip() else None


# The level each catalog field is held at.
CATALOG = {"session.date": "session"}


def field_level(field):
    return CATALOG.get(field, "scan")


def field_values(field, row):
    """The values a row has for a field."""
    if field == "session.date":
        return [row["date"]] if row["date"] else []
    return row["values"][field]


def like_match(pattern, text):
    """PS3.4 C.2.2.2.4: "*" is any run of characters, "?" one character, and
    nothing else is special."""
    rx = "".join(".*" if c == "*" else "." if c == "?" else re.escape(c) for c in pattern)
    return re.fullmatch(rx, text, re.DOTALL) is not None


def compare(op, folded, values, have):
    """Whether one value `have` of a field meets the operator with the
    condition's `values`."""
    lits = [v[1] for v in values]
    if folded:
        have, lits = fold(have), [fold(x) for x in lits]
    if op in ("=", "=="):
        return have == lits[0]
    if op == "in":
        return have in lits
    if op == "<":
        return have < lits[0]
    if op == "<=":
        return have <= lits[0]
    if op == ">":
        return have > lits[0]
    if op == ">=":
        return have >= lits[0]
    if op == "between":
        return lits[0] <= have <= lits[1]
    if op == "prefix":
        return have.startswith(lits[0])
    if op == "like":
        return like_match(lits[0], have)
    if op == "contains":
        return lits[0] in have
    raise ValueError("operator %s" % op)


def related(row, level, to):
    """The rows at level `to` that belong with a row at level `level`."""
    if to == level:
        return [row]
    if to == "subject":
        return [row["subject"]]
    if to == "session":
        return [row["session"]] if level == "scan" else row["sessions"] if level == "subject" else [row]
    # to == "scan"
    return row["scans"] if level in ("session", "subject") else [row]


def holds(node, level, row):
    """Whether a row at `level` meets the condition. An attribute a row lacks
    makes a comparison false, never an error, and "not" negates that."""
    kind = node[0]
    if kind == "and":
        return all(holds(c, level, row) for c in node[1])
    if kind == "or":
        return any(holds(c, level, row) for c in node[1])
    if kind == "not":
        return not holds(node[1], level, row)
    if kind == "has":
        _, every, to, child = node
        rows = related(row, level, to)
        verdicts = [holds(child, to, r) for r in rows]
        return bool(verdicts) and (all(verdicts) if every else any(verdicts))
    _, field, op, folded, values = node
    if field_level(field) != level:
        raise ValueError("%s is held at level %s, compared at %s" % (field, field_level(field), level))
    have = field_values(field, row)
    if op == "present":
        return len(have) > 0
    if op == "absent":
        return len(have) == 0
    return any(compare(op, folded, values, v) for v in have)


def answer(rows, level, text):
    """The rows of a level that meet a condition (all for an empty one)."""
    node = parse(text)
    return [r for r in rows[level].values() if node is None or holds(node, level, r)]


def facet(matched, field):
    """How many of the matched rows hold each value of a single-valued text
    field, the value as it is written in the file."""
    out = {}
    for r in matched:
        for v in field_values(field, r)[:1]:
            out[v] = out.get(v, 0) + 1
    return out
