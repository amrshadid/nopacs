"""Check a tenant's DICOMweb services with dicomweb-client and pydicom.

Usage: dicomweb-client.py <DICOMweb root URL> <file> [<file> ...]

The key is read from the environment variable NOPACS_DICOMWEB_KEY, never
from the command line, and sent as a bearer token. Each file is a DICOM
file as it was sent to the archive; together they are everything the
tenant holds.

With dicomweb-client, a client that is not NoPACs', it:

  * searches every study (QIDO-RS) and checks that the studies are exactly
    those of the files, each with its counts of series and instances;
  * searches the series of each study and the instances of each series,
    and checks they are exactly the files';
  * retrieves each instance (WADO-RS, as stored) and checks it is the file
    as pydicom reads it: the same data set and the same pixel data bytes;
  * retrieves each instance's metadata and checks it equals pydicom's
    Dataset.to_json_dict() of the file, member for member, but for Data
    Set Trailing Padding (FFFC,FFFC), which is not answered (PS3.5 7.1.3),
    Specific Character Set, which is answered as ISO_IR 192 since every
    text is UTF-8 in JSON (RFC 8259), and each BulkDataURI, which it
    retrieves with dicomweb-client and checks is pydicom's bytes (for
    encapsulated pixel data, pydicom's value and then the Sequence
    Delimitation Item, as the file holds it);
  * retrieves frame 1 of each image (and the last frame of a multi-frame
    one) as stored and checks it is pydicom's: the fragments of the frame
    for encapsulated pixel data, the frame's slice of the pixel data for
    native little endian.

Exit status 0 when everything agrees; each difference is printed.
"""
import base64
import os
import sys
from urllib.parse import urlsplit, urlunsplit

import pydicom
from dicomweb_client.api import DICOMwebClient
from pydicom import encaps

PADDING = "FFFCFFFC"
CHARSET = "00080005"
ITEM = b"\xfe\xff\x00\xe0"
DELIMITER = b"\xfe\xff\xdd\xe0\x00\x00\x00\x00"
NATIVE_LITTLE = {"1.2.840.10008.1.2", "1.2.840.10008.1.2.1"}


def their_bytes(el):
    """The bytes pydicom holds for a value, or None when it cannot say."""
    if "InlineBinary" in el:
        return base64.b64decode(el["InlineBinary"])
    if "Value" in el and len(el["Value"]) == 1 and isinstance(el["Value"][0], str):
        return el["Value"][0].encode()
    return None


def on_base(uri, base):
    """The URI with the scheme and host of the base URL.

    dicomweb-client sends a Host header without the port (it sets
    headers["Host"] to the host alone), and the server writes an absolute
    BulkDataURI with the host it was sent (PS3.18 F.2.6), so the URI names
    port 80. Only the scheme and the host are taken from the base; the path
    and the query, which name the bulk value, are the server's as written.
    """
    u, b = urlsplit(uri), urlsplit(base)
    if u.hostname != b.hostname:
        return None
    return urlunsplit((b.scheme, b.netloc, u.path, u.query, ""))


def compare(client, ours, theirs, where, problems):
    theirs = {k: v for k, v in theirs.items() if not (where == "" and k == PADDING)}
    for k in sorted(set(ours) | set(theirs)):
        at = where + k
        if k not in ours:
            problems.append(f"{at}: missing from the answer")
            continue
        if k not in theirs:
            problems.append(f"{at}: answered, pydicom has no such member")
            continue
        o, t = ours[k], theirs[k]
        if "BulkDataURI" in o:
            want = their_bytes(t)
            uri = on_base(o["BulkDataURI"], client.base_url)
            if uri is None:
                problems.append(f"{at}: the BulkDataURI {o['BulkDataURI']} names another host")
                continue
            got = client.retrieve_bulkdata(uri)
            if (len(got) == 1 and k == "7FE00010" and want is not None
                    and got[0] == want + DELIMITER and want.startswith(ITEM)):
                # Encapsulated pixel data: the URI names the value as the
                # file holds it, through its Sequence Delimitation Item
                # (PS3.5 A.4), which pydicom's value leaves out.
                continue
            if want is None or len(got) != 1 or got[0] != want:
                problems.append(f"{at}: the bulk data does not hold pydicom's bytes "
                                f"({[len(g) for g in got]} bytes, want {None if want is None else len(want)})")
            continue
        if where == "" and k == CHARSET:
            if o.get("Value") != ["ISO_IR 192"]:
                problems.append(f"{at}: Specific Character Set is {o.get('Value')}")
            continue
        if o.get("vr") != t.get("vr"):
            problems.append(f"{at}: vr {o.get('vr')} != {t.get('vr')}")
            continue
        if t.get("vr") == "SQ":
            oi, ti = o.get("Value", []), t.get("Value", [])
            if len(oi) != len(ti):
                problems.append(f"{at}: {len(oi)} items != {len(ti)}")
                continue
            for i, (a, b) in enumerate(zip(oi, ti)):
                compare(client, a, b, f"{at}[{i}].", problems)
            continue
        if o != t:
            problems.append(f"{at}: {str(o)[:120]} != {str(t)[:120]}")


def frame_bytes(ds, n):
    """Frame n of a data set as stored, by pydicom, or None when it cannot say."""
    ts = str(ds.file_meta.TransferSyntaxUID)
    count = int(getattr(ds, "NumberOfFrames", 1) or 1)
    if ds.file_meta.TransferSyntaxUID.is_compressed:
        frames = list(encaps.generate_frames(ds.PixelData, number_of_frames=count))
        return frames[n - 1]
    if ts not in NATIVE_LITTLE or ds.BitsAllocated % 8:
        return None
    size = ds.Rows * ds.Columns * ds.SamplesPerPixel * ds.BitsAllocated // 8
    return bytes(ds.PixelData[(n - 1) * size:n * size])


def uid_of(r, key):
    return r.get(key, {}).get("Value", [None])[0]


def main():
    url, paths = sys.argv[1], sys.argv[2:]
    client = DICOMwebClient(url=url, headers={"Authorization": "Bearer " + os.environ["NOPACS_DICOMWEB_KEY"]})
    files = {}
    for p in paths:
        ds = pydicom.dcmread(p)
        files[str(ds.SOPInstanceUID)] = ds
    tree = {}
    for sop, ds in files.items():
        tree.setdefault(str(ds.StudyInstanceUID), {}).setdefault(str(ds.SeriesInstanceUID), []).append(sop)

    problems = []
    studies = client.search_for_studies(get_remaining=True)
    got = sorted(uid_of(s, "0020000D") for s in studies)
    if got != sorted(tree):
        problems.append(f"studies: {got} != {sorted(tree)}")
    for s in studies:
        st = uid_of(s, "0020000D")
        if st not in tree:
            continue
        n = sum(len(v) for v in tree[st].values())
        if uid_of(s, "00201208") != n or uid_of(s, "00201206") != len(tree[st]):
            problems.append(f"study {st}: counts {uid_of(s, '00201206')}/{uid_of(s, '00201208')}")
    checked = frames = 0
    for st, series in tree.items():
        got = sorted(uid_of(s, "0020000E") for s in client.search_for_series(study_instance_uid=st))
        if got != sorted(series):
            problems.append(f"study {st}: series {got} != {sorted(series)}")
        for se, sops in series.items():
            found = client.search_for_instances(study_instance_uid=st, series_instance_uid=se)
            got = sorted(uid_of(i, "00080018") for i in found)
            if got != sorted(sops):
                problems.append(f"series {se}: instances {got} != {sorted(sops)}")
            for sop in sops:
                want = files[sop]
                ds = client.retrieve_instance(st, se, sop)
                if ds.to_json_dict() != want.to_json_dict():
                    problems.append(f"instance {sop}: the retrieved data set differs from the file's")
                if "PixelData" in want and ds.PixelData != want.PixelData:
                    problems.append(f"instance {sop}: the retrieved pixel data differs")
                meta = client.retrieve_instance_metadata(st, se, sop)
                compare(client, meta, want.to_json_dict(), "", problems)
                checked += 1
                if "PixelData" not in want:
                    continue
                count = int(getattr(want, "NumberOfFrames", 1) or 1)
                for n in sorted({1, count}):
                    expect = frame_bytes(want, n)
                    if expect is None:
                        continue
                    got = client.retrieve_instance_frames(st, se, sop, frame_numbers=[n],
                                                          media_types=(("application/octet-stream", "*"),))
                    if len(got) != 1 or got[0] != expect:
                        problems.append(f"instance {sop}: frame {n} is not pydicom's ({len(got[0]) if got else 0} bytes, want {len(expect)})")
                    frames += 1
    for p in problems:
        print(p)
    print(f"{checked} instances, {frames} frames checked; {len(problems)} differ")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
