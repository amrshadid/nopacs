"""Check NoPACs' frame tables against pydicom.

Two commands.

  frames-pydicom.py build <corpus dir> <out dir>

    Writes files the corpus lacks into <out dir>, each made with pydicom from
    a public corpus file, examples_ybr_color.dcm (thirty JPEG baseline
    frames, one fragment each, a Basic Offset Table):
      * jpeg_3frame_nobot.dcm: its first three frames, three fragments a
        frame, and an empty Basic Offset Table, so only the end-of-image
        markers say where a frame ends (pydicom.encaps.encapsulate with
        has_bot=False);
      * jpeg_3frame_eot.dcm: the same three frames, one fragment each, with
        an Extended Offset Table (7FE0,0001) and (7FE0,0002) and an empty
        Basic Offset Table (pydicom.encaps.encapsulate_extended).
    Each has its own SOP Instance UID. The bytes are the same on every run.

  frames-pydicom.py check <json>

    Input: a list of
      {"file": <path of a DICOM file>,
       "frames": [[[offset, length], ...], ...] or null}
    the frame table NoPACs records for the file: for each frame, the byte
    ranges of the file that joined in order are the frame as stored; null
    when NoPACs records that the frames are not byte ranges of the file.

    For an encapsulated file (PS3.5 A.4) the table must equal, range for
    range, the fragments pydicom divides each frame into
    (encaps.generate_fragmented_frames in pydicom 3, generate_pixel_data in
    pydicom 2): the offset of each fragment's content in the file, found by
    walking the items of the Pixel Data value here, and its length; and the
    bytes the ranges name must be the bytes of pydicom's fragments. For a
    native file the table must be one range a frame, frame i at the value's
    first byte plus i times Rows x Columns x Samples per Pixel x Bits
    Allocated / 8 (two thirds of that for YBR_FULL_422), and the bytes it
    names must be pydicom's PixelData cut the same way. null must be a file
    pydicom also cannot divide that way: a deflated data set, a 1-bit image
    of several frames whose frames are not whole bytes, or encapsulated
    pixel data whose division pydicom refuses or makes into another number
    of frames than Number of Frames.

Exit status 0 when every file agrees; the differences are printed otherwise.
"""
import json
import os
import struct

import pydicom
from pydicom import encaps

DEFLATED = "1.2.840.10008.1.2.1.99"
ITEM = 0xFFFEE000
DELIMITER = 0xFFFEE0DD


def pixel_offset(path):
    """The offset of the Pixel Data tag in the file: where pydicom stops."""
    with open(path, "rb") as f:
        pydicom.dcmread(f, stop_before_pixels=True)
        return f.tell()


def header_length(data, at):
    """The length of the Pixel Data element's header at offset at (PS3.5
    7.1): 12 bytes when a VR follows the tag, as Pixel Data's OB, OW, OF or
    OD does in every explicit VR syntax; 8 when its length follows the tag,
    as in Implicit VR Little Endian, or in a file whose data set is implicit
    VR whatever its meta information says (pydicom reads such a file, with a
    warning)."""
    return 12 if data[at + 4:at + 6] in (b"OB", b"OW", b"OF", b"OD") else 8


def items(data, start):
    """The (content offset, length) of each item of encapsulated pixel data
    whose value starts at start, the Basic Offset Table first, walked here
    from the item tags (PS3.5 A.4) and not by pydicom or NoPACs."""
    out = []
    at = start
    while at + 8 <= len(data):
        group, element, length = struct.unpack_from("<HHL", data, at)
        tag = group << 16 | element
        if tag == DELIMITER:
            return out
        if tag != ITEM:
            raise ValueError(f"no item tag at {at}")
        out.append((at + 8, length))
        at += 8 + length
    raise ValueError("no sequence delimiter")


def fragmented_frames(value, frames, ds):
    """pydicom's division of the value into frames, each a tuple of
    fragments, or None when pydicom refuses."""
    try:
        if hasattr(encaps, "generate_fragmented_frames"):
            eot = None
            if "ExtendedOffsetTable" in ds and "ExtendedOffsetTableLengths" in ds:
                eot = (ds.ExtendedOffsetTable, ds.ExtendedOffsetTableLengths)
            return list(encaps.generate_fragmented_frames(value, number_of_frames=frames, extended_offsets=eot))
        return list(encaps.generate_pixel_data(value, frames))
    except ValueError:
        return None


def expected_encapsulated(path, data, ds, problems):
    at = pixel_offset(path)
    start = at + header_length(data, at)
    found = items(data, start)
    fragments = found[1:]
    frames = number_of_frames(ds)
    if frames is None:
        return None
    theirs = fragmented_frames(ds.PixelData, frames, ds)
    if theirs is None or len(theirs) != frames:
        return None
    table, i = [], 0
    for frame in theirs:
        ranges = []
        for fragment in frame:
            off, length = fragments[i]
            if data[off:off + length] != fragment:
                problems.append(f"fragment {i}: the walk of the items and pydicom disagree")
            ranges.append([off, length])
            i += 1
        table.append(ranges)
    if i != len(fragments):
        problems.append(f"pydicom's frames hold {i} of {len(fragments)} fragments")
    return table


def number_of_frames(ds):
    """Number of Frames, 1 when absent, or None when pydicom cannot read it
    as a number."""
    try:
        return int(ds.get("NumberOfFrames", 1) or 1)
    except (TypeError, ValueError):
        return None


def expected_native(path, data, ds, problems):
    frames = number_of_frames(ds)
    if frames is None or any(k not in ds for k in ("Rows", "Columns", "BitsAllocated")):
        return None
    bits = int(ds.Rows) * int(ds.Columns) * int(ds.get("SamplesPerPixel", 1)) * int(ds.BitsAllocated)
    if ds.get("PhotometricInterpretation") == "YBR_FULL_422" and int(ds.get("SamplesPerPixel", 1)) == 3 and int(ds.BitsAllocated) == 8:
        bits = bits * 2 // 3
    if bits % 8 != 0 and frames > 1:
        return None
    at = pixel_offset(path)
    start = at + header_length(data, at)
    size = (bits + 7) // 8
    table = [[[start + i * size, size]] for i in range(frames)]
    raw = ds.PixelData
    for i in range(frames):
        if data[start + i * size:start + (i + 1) * size] != raw[i * size:(i + 1) * size]:
            problems.append(f"frame {i + 1}: the file's bytes are not pydicom's PixelData")
    return table


def check(entry):
    problems = []
    path = entry["file"]
    with open(path, "rb") as f:
        data = f.read()
    ds = pydicom.dcmread(path)
    syntax = ds.file_meta.TransferSyntaxUID
    if syntax == DEFLATED:
        want = None
    elif syntax.is_encapsulated if hasattr(syntax, "is_encapsulated") else syntax.is_compressed:
        want = expected_encapsulated(path, data, ds, problems)
    else:
        want = expected_native(path, data, ds, problems)
    ours = entry["frames"]
    if ours != want:
        problems.append(f"table {json.dumps(ours)[:200]} against pydicom's {json.dumps(want)[:200]}")
    if ours:
        for i, frame in enumerate(ours):
            if any(off < 0 or off + length > len(data) for off, length in frame):
                problems.append(f"frame {i + 1} lies outside the file")
    return problems, (len(want) if want else 0)


def build(corpus, out):
    src = pydicom.dcmread(os.path.join(corpus, "examples_ybr_color.dcm"))
    if hasattr(encaps, "generate_frames"):
        frames = list(encaps.generate_frames(src.PixelData, number_of_frames=int(src.NumberOfFrames)))[:3]
    else:
        frames = list(encaps.generate_pixel_data_frame(src.PixelData, int(src.NumberOfFrames)))[:3]
    made = [
        ("jpeg_3frame_nobot.dcm", "1.2.826.0.1.3680043.8.498.70001",
         encaps.encapsulate(frames, fragments_per_frame=3, has_bot=False), None),
        ("jpeg_3frame_eot.dcm", "1.2.826.0.1.3680043.8.498.70002", None, encaps.encapsulate_extended(frames)),
    ]
    for name, uid, value, extended in made:
        ds = pydicom.dcmread(os.path.join(corpus, "examples_ybr_color.dcm"))
        ds.SOPInstanceUID = uid
        ds.file_meta.MediaStorageSOPInstanceUID = uid
        ds.NumberOfFrames = len(frames)
        if extended is not None:
            value, offsets, lengths = extended
            ds.ExtendedOffsetTable = offsets
            ds["ExtendedOffsetTable"].VR = "OV"
            ds.ExtendedOffsetTableLengths = lengths
            ds["ExtendedOffsetTableLengths"].VR = "OV"
        ds.PixelData = value
        ds["PixelData"].VR = "OB"
        ds["PixelData"].is_undefined_length = True
        if hasattr(ds, "save_as") and "enforce_file_format" in ds.save_as.__code__.co_varnames:
            ds.save_as(os.path.join(out, name), enforce_file_format=True)
        else:
            ds.save_as(os.path.join(out, name), write_like_original=False)
        print(f"built {name}")
    return 0


def main(args):
    if len(args) == 3 and args[0] == "build":
        return build(args[1], args[2])
    if len(args) != 2 or args[0] != "check":
        print(__doc__)
        return 2
    with open(args[1]) as f:
        entries = json.load(f)
    bad = 0
    for e in entries:
        try:
            problems, frames = check(e)
        except Exception as err:  # a file the check itself cannot read is a difference to look at
            problems, frames = [f"the check could not read it: {type(err).__name__}: {err}"], 0
        status = "ok" if not problems else "DIFFERS"
        shown = "no ranges" if e["frames"] is None else f"{frames} frame(s)"
        print(f"{status} {os.path.basename(e['file'])} ({shown}, pydicom {pydicom.__version__})")
        for p in problems[:10]:
            print("    " + p)
        bad += bool(problems)
    print(f"{len(entries)} files, {bad} differ")
    return 1 if bad else 0

