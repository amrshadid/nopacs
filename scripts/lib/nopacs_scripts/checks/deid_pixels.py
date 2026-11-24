"""Check images a pixel rule filled, with pydicom: an outside reading.

Usage: python3 -I deid-pixels.py <pairs.json>

pairs.json lists objects {"original", "archived", "regions", "lossy"}: the
file as received, the file archived, the rectangles [x, y, width, height]
the rule filled in every frame, and whether the original was compressed
with loss. For each pair pydicom reads both files and decodes their pixels
with its own decoders (RLE, and JPEG and JPEG-LS through pylibjpeg); dcmtk's
dcmdump reads the archived file. Every sample inside a rectangle must be
black (the lowest stored value, the highest under MONOCHROME1; for YBR zero
luminance and chroma at its midpoint; for PALETTE COLOR the index of the
palette's darkest entry, by the luminance weights of PS3.3 C.7.6.3.1.2)
and every other sample what pydicom decodes from the original. The
archived file must say Burned In Annotation (0028,0301) NO, record the
Clean Pixel Data Option's code (113101) in (0012,0064), and hold no
Extended Offset Table (7FE0,0001), which only encapsulated pixels have. A lossy original is decoded by another JPEG
decoder than the archive's, whose rounding may differ: outside the
rectangles its samples may differ by at most LOSSY_TOLERANCE, and the
largest difference is printed. Exits 1 on the first failure.
"""

import json
import shutil
import subprocess

import numpy as np
import pydicom

# The most two JPEG baseline decoders' samples differ by: ISO/IEC 10918-1
# allows each its own inverse DCT within its accuracy test (Annex A.3.3).
LOSSY_TOLERANCE = 4


def frames(ds):
    """The decoded samples, as frames by rows by columns by samples."""
    a = ds.pixel_array
    n = int(getattr(ds, "NumberOfFrames", 1) or 1)
    spp = int(ds.SamplesPerPixel)
    return a.reshape((n, int(ds.Rows), int(ds.Columns), spp))


def black(ds):
    """The stored value of black for each sample, as the archive fills."""
    stored = int(ds.BitsStored)
    pi = str(ds.PhotometricInterpretation)
    if pi == "RGB":
        return [0, 0, 0]
    if pi == "YBR_FULL":
        mid = 1 << (stored - 1)
        return [0, mid, mid]
    if pi == "PALETTE COLOR":
        return [darkest(ds)]
    if pi == "MONOCHROME1":
        return [(1 << (stored - 1)) - 1 if ds.PixelRepresentation else (1 << stored) - 1]
    return [-(1 << (stored - 1)) if ds.PixelRepresentation else 0]


def darkest(ds):
    """The index of a palette's darkest entry, from its first mapped value."""
    tables = [np.frombuffer(getattr(ds, f"{c}PaletteColorLookupTableData"), dtype="<u2").astype(np.int64)
              for c in ("Red", "Green", "Blue")]
    first = int(ds.RedPaletteColorLookupTableDescriptor[1])
    lum = 299 * tables[0] + 587 * tables[1] + 114 * tables[2]
    return first + int(np.argmin(lum))


def record(arch):
    """Why the archived file's record is wrong, or None."""
    if str(arch.get("BurnedInAnnotation", "")) != "NO":
        return f"Burned In Annotation is {arch.get('BurnedInAnnotation')!r}, not NO"
    codes = [str(i.CodeValue) for i in arch.get("DeidentificationMethodCodeSequence", [])]
    if "113101" not in codes:
        return f"the method codes {codes} lack Clean Pixel Data's 113101"
    if (0x7FE0, 0x0001) in arch:
        return "an Extended Offset Table beside native pixels"
    return None


def check(pair):
    orig = pydicom.dcmread(pair["original"])
    arch = pydicom.dcmread(pair["archived"])
    if arch.file_meta.TransferSyntaxUID.is_compressed:
        return "the archived image is still compressed"
    wrong = record(arch)
    if wrong:
        return wrong
    # pydicom 2.4's JPEG decoders give a YBR image's samples as the
    # codestream holds them, unconverted, as the archive holds them.
    b, a = frames(orig).astype(np.int64), frames(arch).astype(np.int64)
    fill = black(arch)
    if a.shape != b.shape:
        return f"shape {a.shape} archived, {b.shape} received"
    mask = np.zeros(a.shape[1:3], dtype=bool)
    for x, y, w, h in pair["regions"]:
        mask[max(y, 0):y + h, max(x, 0):x + w] = True
    inside, outside = a[:, mask, :], a[:, ~mask, :]
    for s, v in enumerate(fill):
        if not np.all(inside[..., s] == v):
            return f"sample {s} inside a rectangle is not {v}"
    diff = int(np.max(np.abs(outside - b[:, ~mask, :]))) if outside.size else 0
    limit = LOSSY_TOLERANCE if pair["lossy"] else 0
    if diff > limit:
        return f"a sample outside the rectangles differs by {diff}"
    note = f", largest difference outside {diff}" if pair["lossy"] else ""
    return None, f"{inside.size} samples black, {outside.size} as received{note}"


def main(argv):
    with open(argv[0], encoding="utf-8") as f:
        pairs = json.load(f)
    dcmdump = shutil.which("dcmdump") or "/opt/homebrew/bin/dcmdump"
    for pair in pairs:
        result = check(pair)
        if isinstance(result, str):
            print(f"FAIL {pair['archived']}: {result}")
            return 1
        dump = subprocess.run([dcmdump, "-q", pair["archived"]], capture_output=True, text=True, check=False)
        if dump.returncode != 0 or "E:" in dump.stderr:
            print(f"FAIL {pair['archived']}: dcmdump: {dump.stderr.strip()}")
            return 1
        name = "/".join(pair["archived"].rsplit("/", 3)[-3:])
        print(f"ok {name}: {result[1]}; record right; dcmdump reads it")
    print(f"{len(pairs)} images checked by pydicom {pydicom.__version__}")

