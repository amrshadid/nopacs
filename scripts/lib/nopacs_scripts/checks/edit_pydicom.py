"""Check the revisions an edit of header values wrote, with pydicom.

An edit gives each instance a later revision in which only the attributes it
names changed. This check reads a new revision and the revision it
superseded with pydicom, the outside tool, and finds every difference:

    edit-pydicom.py values KEYWORD FILE...
        prints, a line a file, the keyword's value as pydicom reads it, or
        "<absent>".
    edit-pydicom.py compare OLD NEW [--set KEYWORD=VALUE]... [--remove KEYWORD]...
        prints "same" when NEW reads as OLD with only those changes: each set
        keyword holds its value, each removed one is absent, and every other
        element of the data set is equal as pydicom decodes it (sequences
        whole, Pixel Data byte for byte), as is the transfer syntax. Specific
        Character Set (0008,0005) is the old one or ISO_IR 192: go-dicom
        writes every file in UTF-8 and says so; the text it decodes is the
        same. Any other difference fails, each named by its tag, never its
        value.

The Go tests of internal/correct run it when NOPACS_CHECK_EDIT_PYTHON names
an interpreter with pydicom (make check-edit).
"""

import pydicom
from pydicom.datadict import tag_for_keyword

from nopacs_scripts.report import failing

fail = failing("check-edit: ")

# CHARSET is Specific Character Set (0008,0005).
CHARSET = 0x00080005


def values(keyword, paths):
    """The keyword's value in each file as pydicom reads it, "<absent>" for a
    file without it."""
    return [str(pydicom.dcmread(p).get(keyword, "<absent>")) for p in paths]


def differences(old, new, want, gone):
    """What tells data set new from old beyond the changes: each keyword of
    want holding its value, each of gone absent. A list of reasons, empty
    when there is none; a reason names tags and keywords, never a value."""
    named = {tag_for_keyword(k) for k in list(want) + list(gone)} | {CHARSET}
    out = []
    if old.file_meta.TransferSyntaxUID != new.file_meta.TransferSyntaxUID:
        out.append("the transfer syntax changed")
    was, now = old.get("SpecificCharacterSet", ""), new.get("SpecificCharacterSet", "")
    if now != was and now != "ISO_IR 192":
        out.append("the character set is neither the old one nor ISO_IR 192")
    for k, v in want.items():
        if str(new.get(k, "<absent>")) != v:
            out.append("%s does not hold the value set" % k)
    for k in gone:
        if k in new:
            out.append("%s is still there" % k)
    for tag in sorted({e.tag for e in old} | {e.tag for e in new}):
        if tag in named:
            continue
        a, b = old.get(tag), new.get(tag)
        if a is None or b is None:
            out.append("%s is only in the %s revision" % (tag, "old" if b is None else "new"))
        elif a.VR != b.VR or a.value != b.value:
            out.append("%s changed" % tag)
    return out


def parse_compare(argv):
    """OLD, NEW, the keywords set with their values and those removed, from
    compare's arguments."""
    if len(argv) < 2:
        fail("compare takes OLD NEW [--set KEYWORD=VALUE]... [--remove KEYWORD]...")
    old, new, rest = argv[0], argv[1], argv[2:]
    want, gone = {}, []
    while rest:
        if len(rest) < 2 or rest[0] not in ("--set", "--remove"):
            fail("after OLD NEW come --set KEYWORD=VALUE and --remove KEYWORD")
        flag, arg, rest = rest[0], rest[1], rest[2:]
        if flag == "--remove":
            gone.append(arg)
            continue
        k, sep, v = arg.partition("=")
        if not sep or not k:
            fail("--set takes KEYWORD=VALUE")
        want[k] = v
    return old, new, want, gone


def main(argv):
    """Runs the check (see the module's text); None for success."""
    if not argv or argv[0] not in ("values", "compare"):
        fail("say values KEYWORD FILE... or compare OLD NEW ...")
    if argv[0] == "values":
        if len(argv) < 3:
            fail("values takes KEYWORD FILE...")
        for v in values(argv[1], argv[2:]):
            print(v)
        return None
    old, new, want, gone = parse_compare(argv[1:])
    found = differences(pydicom.dcmread(old), pydicom.dcmread(new), want, gone)
    if found:
        fail("%s reads as more than the edit changed: %s" % (new, "; ".join(found)))
    print("same")
    return None
