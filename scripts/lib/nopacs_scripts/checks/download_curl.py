"""Check NoPACs' plain HTTP download of instances with curl and hashlib.

Input: the path of a JSON file
  {"base": <the server's URL, http://host:port>,
   "tenant": <the tenant's name>,
   "headers": <a file of request headers for curl's -H @file, such as the
               key; optional>,
   "socket": <the path of the server's local socket, which curl reaches
              with --unix-socket and which needs no key; optional>,
   "manifests": [<path of a scan's manifest, as the storage holds it>, ...]}

Each manifest is read here as the storage holds it (its JSON form), and for
every file it lists, the download at
/tenants/<tenant>/instances/<subject>/<session>/<scan>/<SOP Instance UID>,
the labels read from the file's key, must give:

  * the whole file: 200, Content-Length its length, ETag its SHA-256 in
    quotes, and a body of that length whose SHA-256, computed here with
    hashlib, is the manifest's;
  * a download resumed in two ranges (curl --range): 206 and 206, the
    second with If-Range set to the ETag, and the two put together have the
    manifest's SHA-256;
  * a download cut short and resumed by curl itself (curl -C -): the file
    curl completes has the manifest's SHA-256;
  * a range past the end: 416 with Content-Range: bytes */<length>;
  * If-Range with another tag: the whole file, 200;
  * HEAD: 200 and Content-Length its length, no body.

Exit status 0 when every file agrees; the differences are printed otherwise.
"""
import hashlib
import json
import os
import subprocess
import tempfile

CURL = os.environ.get("CURL", "curl")


def reach(cfg):
    """The arguments that reach the server: its socket and the headers file."""
    out = []
    if cfg.get("socket"):
        out += ["--unix-socket", cfg["socket"]]
    if cfg.get("headers"):
        out += ["-H", "@" + cfg["headers"]]
    return out


def curl(work, cfg, url, *args):
    """Run curl and return (status, response headers by lower-case name, body)."""
    hdr = os.path.join(work, "headers.out")
    body = os.path.join(work, "body.out")
    for p in (hdr, body):
        if os.path.exists(p):
            os.remove(p)
    cmd = [CURL, "-sS", *reach(cfg), "-D", hdr, "-o", body, "-w", "%{http_code}", *args, url]
    out = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if out.returncode != 0:
        raise RuntimeError("curl %s: exit %d: %s" % (" ".join(args), out.returncode, out.stderr.strip()))
    fields = {}
    with open(hdr, encoding="latin-1") as f:
        for line in f:
            if ":" in line:
                k, v = line.split(":", 1)
                fields[k.strip().lower()] = v.strip()
    data = b""
    if os.path.exists(body):
        with open(body, "rb") as f:
            data = f.read()
    return int(out.stdout.strip() or 0), fields, data


def sha(data):
    return hashlib.sha256(data).hexdigest()


def check_file(work, cfg, entry, problems):
    parts = entry["key"].split("/")
    if len(parts) != 5 or parts[0] != "data" or not parts[4].endswith(".dcm"):
        problems.append("%s: not a key of the data tree" % entry["key"])
        return
    _, subject, session, scan, name = parts
    uid = name[: -len(".dcm")]
    if uid != entry["sop_instance_uid"]:
        problems.append("%s: the key does not end in the file's UID" % entry["key"])
        return
    url = "%s/tenants/%s/instances/%s/%s/%s/%s" % (cfg["base"], cfg["tenant"], subject, session, scan, uid)
    length, want = entry["length"], entry["sha256"]
    tag = '"%s"' % want
    where = "%s/%s/%s/<instance>" % (subject, session, scan)

    def expect(what, cond):
        if not cond:
            problems.append("%s: %s" % (where, what))

    status, h, body = curl(work, cfg, url)
    expect("whole: status %d" % status, status == 200)
    expect("whole: Content-Length %s, the manifest has %d" % (h.get("content-length"), length), h.get("content-length") == str(length))
    expect("whole: ETag %s" % h.get("etag"), h.get("etag") == tag)
    expect("whole: %d bytes with SHA-256 %s, the manifest has %d and %s" % (len(body), sha(body), length, want),
           len(body) == length and sha(body) == want)
    expect("whole: Content-Type %s" % h.get("content-type"), h.get("content-type") == "application/dicom")

    cut = max(1, length // 3)
    s1, h1, p1 = curl(work, cfg, url, "--range", "0-%d" % (cut - 1))
    s2, h2, p2 = curl(work, cfg, url, "--range", "%d-" % cut, "-H", "If-Range: " + h1.get("etag", ""))
    expect("resumed: statuses %d and %d" % (s1, s2), s1 == 206 and s2 == 206)
    expect("resumed: Content-Range %s and %s" % (h1.get("content-range"), h2.get("content-range")),
           h1.get("content-range") == "bytes 0-%d/%d" % (cut - 1, length)
           and h2.get("content-range") == "bytes %d-%d/%d" % (cut, length - 1, length))
    expect("resumed: the two ranges have SHA-256 %s" % sha(p1 + p2), sha(p1 + p2) == want)

    partial = os.path.join(work, "partial.dcm")
    with open(partial, "wb") as f:
        f.write(p1)
    out = subprocess.run([CURL, "-sS", *reach(cfg), "-C", "-", "-o", partial, "-w", "%{http_code}", url],
                         capture_output=True, text=True, check=False)
    with open(partial, "rb") as f:
        resumed = f.read()
    expect("curl -C -: exit %d, status %s, SHA-256 %s" % (out.returncode, out.stdout, sha(resumed)),
           out.returncode == 0 and out.stdout.strip() == "206" and sha(resumed) == want)

    s, h, _ = curl(work, cfg, url, "--range", "%d-" % length)
    expect("past the end: %d %s" % (s, h.get("content-range")), s == 416 and h.get("content-range") == "bytes */%d" % length)

    s, _, body = curl(work, cfg, url, "--range", "%d-" % cut, "-H", 'If-Range: "%s"' % ("0" * 64))
    expect("If-Range with another tag: %d, %d bytes" % (s, len(body)), s == 200 and sha(body) == want)

    s, h, body = curl(work, cfg, url, "-I")
    expect("HEAD: %d %s, %d bytes" % (s, h.get("content-length"), len(body)),
           s == 200 and h.get("content-length") == str(length) and h.get("etag") == tag)


def main(argv):
    with open(argv[0], encoding="utf-8") as f:
        cfg = json.load(f)
    problems, files = [], 0
    with tempfile.TemporaryDirectory() as work:
        for path in cfg["manifests"]:
            with open(path, encoding="utf-8") as f:
                manifest = json.load(f)
            for entry in manifest["files"]:
                files += 1
                check_file(work, cfg, entry, problems)
    for p in problems:
        print(p)
    print("%d files checked with curl and hashlib against %d manifests, %d differences" % (files, len(cfg["manifests"]), len(problems)))
    return 1 if problems or files == 0 else 0

