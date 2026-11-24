"""Check JPEG 2000 images whose comment segments were removed, with pydicom.

Usage: python3 -I deid-j2k-pydicom.py <manifest.json>

manifest.json is {"name": <the text planted in the comments>, "files":
[{"input", "output"}]}: each file as given to the engine, a name in a
comment of its codestream's main header and of every tile-part header,
and the same file de-identified. For each pair pydicom reads both files
and decodes their pixels with its own JPEG 2000 decoder (pylibjpeg's
OpenJPEG plugin, or Pillow's): the output must still be JPEG 2000, decode
to the very samples the input decodes to, and hold the name nowhere in its
bytes. Exits 1 on the first failure, or when no JPEG 2000 decoder is
installed for pydicom.
"""

import json
import sys

import numpy as np
import pydicom

JPEG2000 = {"1.2.840.10008.1.2.4.90", "1.2.840.10008.1.2.4.91",
            "1.2.840.10008.1.2.4.201", "1.2.840.10008.1.2.4.202", "1.2.840.10008.1.2.4.203"}


def decoder():
    """The name of the JPEG 2000 decoder pydicom has, or None."""
    try:
        import openjpeg  # noqa: F401 (pylibjpeg-openjpeg)
        return f"pylibjpeg-openjpeg {openjpeg.__version__}"
    except ImportError:
        pass
    try:
        from PIL import features
        if features.check("jpg_2000"):
            return "Pillow with OpenJPEG"
    except ImportError:
        pass
    return None


def check(pair, name):
    """Why the pair is wrong, or None."""
    with open(pair["output"], "rb") as f:
        if name in f.read():
            return "the name is in the output"
    inp = pydicom.dcmread(pair["input"])
    out = pydicom.dcmread(pair["output"])
    ts = str(out.file_meta.TransferSyntaxUID)
    if ts not in JPEG2000:
        return f"the output is in {ts}, not JPEG 2000"
    a, b = inp.pixel_array, out.pixel_array
    if a.shape != b.shape or not np.array_equal(a, b):
        return f"decoded otherwise: shape {b.shape} for {a.shape}"
    return None


def main():
    with open(sys.argv[1], encoding="utf-8") as f:
        spec = json.load(f)
    using = decoder()
    if using is None:
        print("FAIL: pydicom has no JPEG 2000 decoder (pylibjpeg-openjpeg or Pillow with OpenJPEG)")
        sys.exit(1)
    name = spec["name"].encode("latin-1")
    for pair in spec["files"]:
        wrong = check(pair, name)
        if wrong:
            print(f"FAIL {pair['output']}: {wrong}")
            sys.exit(1)
    print(f"{len(spec['files'])} files decoded alike by pydicom {pydicom.__version__} with {using}; no name left")


if __name__ == "__main__":
    main()
