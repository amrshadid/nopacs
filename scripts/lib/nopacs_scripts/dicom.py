"""Small readings of DICOM files and DICOM JSON by pydicom, the outside tool
every proof judges the archive with."""

import base64

from pydicom import dcmread


def their_bytes(el):
    """The bytes pydicom holds for a DICOM JSON value (an element of its
    to_json_dict), or None when it cannot say."""
    if "InlineBinary" in el:
        return base64.b64decode(el["InlineBinary"])
    if "Value" in el and len(el["Value"]) == 1 and isinstance(el["Value"][0], str):
        return el["Value"][0].encode()
    return None


def sop_uids(paths):
    """The SOP Instance UIDs of the files, in order, read by pydicom without
    their pixel data."""
    return [str(dcmread(p, stop_before_pixels=True).SOPInstanceUID) for p in paths]
