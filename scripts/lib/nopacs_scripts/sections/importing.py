"""The proof of importing a directory, from outside the binary.

It builds and runs the real program, sets it up with its own command line, and
imports directory trees built from the public corpus, only through its
commands (setting, tenant, storage, receiver, job import, job show, job files,
job units). Trees: one in the shape of an XNAT archive (the recipe of
docs/use/importing.md, with a map file under the root), one in a generic
pattern shape with a map sent with the job, with traps planted in them (a
link, two names of one file, files that are not DICOM, a folder wider than
import.max-dir-entries, a path over 2048 bytes, a folder named "ct:9", a file
outside the layout, a session the map lacks, a file over the largest file, the
same instance in two folders). What was archived is read back only with tools
that are not NoPACs: pydicom, hashlib, Python's sqlite3 on the tenant's
catalog, and a walk of the tree made with lstat and a descriptor per
directory. Each check prints one line, "ok" or "FAIL", with what it saw; the
run ends with a count and exits 1 when one failed.

  importing.py --go GO [--pydicom-corpus DIR] [--only tree,deid,kill]   (make prove-import)

Groups: tree (the two trees as received, every skip and count, the archive
against the tree, a second run, the roots, a reader), deid (the tree through
a de-identifying receiver, and the references of an SR that survive it) and
kill (a 600-file import killed with signal 9 and started again, against an
uninterrupted one). Each group builds its own server and finishes in under
three minutes on a laptop. It needs no container.

Needs: Go and a Python with pydicom (scripts/requirements/prove-reception.txt).
"""

import argparse
import http.client
import io
import json
import os
import shutil
import signal
import subprocess
import tempfile
import time

from pydicom import dcmread

from nopacs_scripts import importing as importkit
from nopacs_scripts.db import query
from nopacs_scripts.importing import Model
from nopacs_scripts.net import free_port
from nopacs_scripts.proc import build_binaries
from nopacs_scripts.proof import Programs, Reporting
from nopacs_scripts.report import Failure

ENDED = ("JOB_STATE_DONE", "JOB_STATE_PARTIAL", "JOB_STATE_FAILED", "JOB_STATE_CANCELED")
XNAT = r"^(?P<project>[^/]+)/arc[0-9]{3}/(?P<session>[^/]+)/SCANS/(?P<scan>[^/]+)/DICOM/[^/]+$"
GENERIC = r"^(?P<study>[^/]+)/(?P<visit>[^/]+)/series/(?P<scan>[^/]+)/[^/]+$"
DEFAULT_CORPUS = "/Users/amrshadid/Downloads/go-dicom/.vscode/refrance/pydicom-main/src/pydicom/data/test_files"


def word(name, prefix):
    return name[len(prefix):].lower() if name.startswith(prefix) else name


def counts(j):
    """The counts of an import job as plain integers and dictionaries."""
    c = j.get("importJob", {}).get("counts", {})

    def n(k):
        return int(c.get(k, 0))
    return {"found": n("found"), "handed_in": n("handedIn"), "handed_again": n("handedAgain"), "archived": n("archived"),
            "already_held": n("alreadyHeld"), "lost": n("lost"), "failed": n("failed"), "discarded": n("discarded"),
            "processing": n("processing"), "label_differs": n("labelDiffers"),
            "skipped": {word(s["code"], "IMPORT_SKIP_CODE_"): int(s["count"]) for s in c.get("skipped", [])},
            "quarantined": {word(q["reason"], "QUARANTINE_REASON_"): int(q["count"]) for q in c.get("quarantined", [])}}


def identities(c):
    """The two identities of a job's counts: found + handed again = skipped +
    handed in; handed in = archived + already held + quarantined + lost +
    failed + discarded + processing. Returns (holds, text)."""
    skipped, held = sum(c["skipped"].values()), sum(c["quarantined"].values())
    left = c["found"] + c["handed_again"]
    right = skipped + c["handed_in"]
    decided = c["archived"] + c["already_held"] + held + c["lost"] + c["failed"] + c["discarded"] + c["processing"]
    return left == right and c["handed_in"] == decided, "found %d + again %d = skipped %d + handed in %d; handed in %d = %d decided" % (
        c["found"], c["handed_again"], skipped, c["handed_in"], c["handed_in"], decided)


def summed(*cs):
    """The counts of several jobs added."""
    out = {k: 0 for k in ("found", "handed_in", "handed_again", "archived", "already_held", "lost", "failed", "discarded", "processing",
                          "label_differs")}
    out["skipped"], out["quarantined"] = {}, {}
    for c in cs:
        for k in out:
            if k in ("skipped", "quarantined"):
                for kk, v in c[k].items():
                    out[k][kk] = out[k].get(kk, 0) + v
            else:
                out[k] += c[k]
    return out


class Proof(Reporting, Programs):
    """The run: its work directory, the server it runs, and its results."""

    def __init__(self, args):
        self.args = args
        # A short directory: the local socket's path must fit the 104 bytes a Unix socket address holds on macOS.
        self.work = tempfile.mkdtemp(prefix="nopacs-imp.", dir="/tmp")
        os.chmod(self.work, 0o700)
        self.data = os.path.join(self.work, "data")
        self.bin = os.path.join(self.work, "nopacs")
        self.sbin = os.path.join(self.work, "nopacs-server")
        self.roots = os.path.join(self.work, "roots", "fast")
        self.tree = os.path.join(self.work, "tree")
        self.server = None
        self.api_port = free_port()
        self.counts = {"ok": 0, "FAIL": 0}

    # ---- the program ----

    def run(self, *args, key=None):
        if key:
            return subprocess.run([self.bin, "--server", "http://127.0.0.1:%d" % self.api_port, *args], capture_output=True, text=True,
                                  timeout=120, env=dict(os.environ, NOPACS_KEY=key))
        return subprocess.run([self.exe(*args), "--data-dir", self.data, *args], capture_output=True, text=True, timeout=120)

    def m(self, *args):
        p = self.run(*args)
        if p.returncode != 0:
            raise Failure("nopacs %s: exit %d: %s" % (" ".join(args), p.returncode, p.stderr.strip()))
        return p.stdout

    def serve(self):
        log = open(os.path.join(self.work, "serve.log"), "a")
        self.server = subprocess.Popen([self.sbin, "--data-dir", self.data, "serve", "--listen", "127.0.0.1:%d" % self.api_port],
                                       stdout=log, stderr=log)
        for _ in range(300):
            if self.server.poll() is not None:
                raise Failure("the server exited at its start; see serve.log")
            if self.run("whoami").returncode == 0:
                return
            time.sleep(0.1)
        raise Failure("the server did not answer")

    def kill9(self):
        """A crash: the server gone with signal 9, nothing flushed."""
        self.server.send_signal(signal.SIGKILL)
        self.server.wait(60)

    # ---- the archive ----

    def catalog(self, tenant):
        return os.path.join(self.data, "tenants", tenant + ".db")

    def storage(self, tenant):
        return os.path.join(self.roots, tenant, "main")

    def held(self, tenant):
        return importkit.archived(self.catalog(tenant), self.storage(tenant))

    # ---- import jobs ----

    def start(self, tenant, receiver, path, *extra):
        return self.mj("job", "import", tenant, receiver, path, *extra)["job"]["id"]

    def get(self, tenant, job):
        return self.mj("job", "show", tenant, job)["job"]

    def wait(self, tenant, job, timeout=170):
        deadline = time.time() + timeout
        j = self.get(tenant, job)
        while j["state"] not in ENDED and time.time() < deadline:
            time.sleep(0.2)
            j = self.get(tenant, job)
        return j

    def files(self, tenant, job):
        return self.mj("job", "files", tenant, job, "--all")["files"]

    def tenant(self, name, as_received=True):
        self.m("tenant", "create", name)
        self.m("setting", "set", "import.allowed-roots", "old", "--tenant", name)
        if as_received:
            self.m("setting", "set", "receiver.allow-as-received", "true", "--tenant", name)
        self.m("storage", "add", name, "main", "--type", "dir", "--root", "fast", "--path", "main")

    def xnat_receiver(self, tenant, name, mapped=True, profile=None, pattern=XNAT):
        """An import receiver in the recipe's shape: a pattern layout over the
        project, session and scan folders, one unit a session folder."""
        a = ["receiver", "add", tenant, name, "--type", "import", "--storage", "main", "--root", "old", "--layout", "pattern",
             "--pattern", pattern, "--unit-depth", "3", "--max-file-size", "128KiB", "--quiet-period", "5s", "--enabled"]
        # A receiver that de-identifies makes its labels from hashed values only: the default naming, not the path's parts.
        if not profile:
            a += ["--naming-session", "{project}_{session}", "--naming-scan", "{scan}",
                  "--naming-subject", "{project}_{subject}" if mapped else "{project}_{session}"]
        if mapped and not profile:
            a += ["--map-file", "maps/sessions.csv", "--map-key-column", "project", "--map-key-column", "label",
                  "--map-key-part", "project", "--map-key-part", "session", "--map-value-column", "subject_label", "--map-gives", "subject"]
        a += ["--profile", profile] if profile else ["--as-received"]
        self.m(*a)

    def setup(self, tenants):
        """init, serve, the roots, and the tenants given as (name, as_received)."""
        os.makedirs(self.roots)
        if self.args.binary:
            shutil.copy(self.args.binary, self.bin)
            shutil.copy(self.args.server_binary, self.sbin)
        else:
            build_binaries(self.args.go, self.bin, self.sbin)
        self.m("init")
        self.serve()
        self.m("setting", "set", "storage.directory-roots", "fast=" + self.roots)
        self.m("setting", "set", "import.roots", "old=" + self.tree)
        for name, as_received in tenants:
            self.tenant(name, as_received)


# ---- checks shared by the groups ----

def expect_counts(p, name, j, model, job):
    """A job's counts against what the tree holds, the identities, and the
    state the skips imply."""
    c, want = counts(j), model.counts(job)
    ok_id, text = identities(c)
    same = (c["found"] == want["found"] and c["handed_in"] == want["handed_in"] and c["archived"] == want["archived"]
            and c["already_held"] == want["already_held"] and c["skipped"] == want["skipped"])
    p.result(name, same and ok_id and not c["quarantined"] and c["handed_again"] == 0 and c["processing"] == 0,
             "job %s: found %d, handed in %d, archived %d, already held %d, skipped %s, quarantined %s, handed again %d; %s"
             % (j["state"].replace("JOB_STATE_", "").lower(), c["found"], c["handed_in"], c["archived"], c["already_held"],
                c["skipped"], c["quarantined"] or "none", c["handed_again"], text),
             ["wanted found %d, handed in %d, archived %d, already held %d, skipped %s" % (want["found"], want["handed_in"], want["archived"],
                                                                                        want["already_held"], want["skipped"])])
    return c


def expect_files(p, name, tenant, job, model, jobname):
    """Every skipped entry listed by job files, with its path and code."""
    got = sorted((f["path"], word(f.get("skipCode", ""), "IMPORT_SKIP_CODE_"), word(f["kind"], "IMPORT_FILE_KIND_")) for f in p.files(tenant, job))
    want = sorted((listed, e, "skipped") for _, listed, e in model.of(jobname) if e not in ("hand", "dup"))
    # A path too long to print is checked by its length.
    p.result(name, got == want, "%d entries listed, each by path with its code: %s"
             % (len(got), ", ".join(sorted({"%s %d" % (c, sum(1 for g in got if g[1] == c)) for _, c, _ in got}))),
             ["listed, not wanted: %s" % [(g[0][-60:], g[1]) for g in got if g not in want][:6],
              "wanted, not listed: %s" % [(w[0][-60:], w[1]) for w in want if w not in got][:6]])


def expect_archive(p, name, tenant, model, as_received=True):
    """The archive equal to the tree: each instance's bytes as the source's,
    pydicom element for element, and the labels the layout and map give."""
    arch = p.held(tenant)
    missing = sorted(set(model.source) - set(arch))
    extra = sorted(set(arch) - set(model.source))
    byte_diff, elem_diff, label_diff = [], [], []
    for sop, (data, subject, session, scan, _) in arch.items():
        src = model.source.get(sop)
        if not src:
            continue
        if as_received and data != open(src, "rb").read():
            byte_diff.append(sop)
        if as_received and dcmread(io.BytesIO(data)) != dcmread(src):
            elem_diff.append(sop)
        if (subject, session, scan) != model.labels[sop]:
            label_diff.append((sop[-8:], (subject, session, scan), model.labels[sop]))
    p.result(name, not (missing or extra or byte_diff or elem_diff or label_diff),
             "%d instances archived of %d in the tree, %d extra; byte-equal (SHA-256) to their files: %d; pydicom-equal: %d; labels as "
             "the layout and map give: %d" % (len(arch), len(model.source), len(extra), len(arch) - len(missing) - len(byte_diff),
                                              len(arch) - len(missing) - len(elem_diff), len(arch) - len(missing) - len(label_diff)),
             ["missing %s" % missing[:4], "extra %s" % extra[:4], "bytes differ %s" % byte_diff[:4], "elements differ %s" % elem_diff[:4],
              "labels %s" % label_diff[:4]])
    return arch


def quarantine_empty(p, tenant):
    files = p.mj("quarantine", "list", tenant, "--all").get("files", [])
    return len(files), [f.get("reason") for f in files]


# ---- group: tree ----

def group_tree(p):
    model, mapping = Model(), {}
    importkit.build_xnat(model, p.tree, p.args.pydicom_corpus, mapping)
    generic = Model()
    csv = importkit.build_generic(generic, p.tree, p.args.pydicom_corpus)
    csv_path = os.path.join(p.work, "generic-map.csv")
    with open(csv_path, "w") as f:
        f.write(csv)
    before = importkit.snapshot(p.tree)
    long_entries = [k for k in before if len(k) > 2048]
    print("     the tree: %d entries, %d files of it DICOM to be handed in, a path of %d bytes, a folder of %d entries"
          % (len(before), len(model.source) + len(generic.source), max(len(k) for k in before), importkit.WIDE), flush=True)
    p.setup([("lab", True)])
    p.m("setting", "set", "import.max-dir-entries", "1000")
    p.xnat_receiver("lab", "xnat")
    p.m("receiver", "add", "lab", "generic", "--type", "import", "--storage", "main", "--root", "old", "--layout", "pattern",
        "--pattern", GENERIC, "--unit-depth", "2", "--naming-subject", "{study}_{subject}", "--naming-session", "{study}_{visit}",
        "--naming-scan", "{scan}", "--max-file-size", "128KiB", "--quiet-period", "5s", "--as-received", "--enabled",
        "--map-key-column", "trial", "--map-key-column", "visit", "--map-key-part", "study", "--map-key-part", "visit",
        "--map-value-column", "participant", "--map-gives", "subject")
    gmap = ["--map-csv", csv_path]
    jobs = {"projA": p.start("lab", "xnat", "projA"), "projB": p.start("lab", "xnat", "projB"),
            "trial-a": p.start("lab", "generic", "trial-a", *gmap), "trial-b": p.start("lab", "generic", "trial-b", *gmap)}
    ended = {k: p.wait("lab", v) for k, v in jobs.items()}
    both = Model()
    both.entries = model.entries + generic.entries
    both.source = {**model.source, **generic.source}
    both.labels = {**model.labels, **generic.labels}
    cs = {}
    for k in ("projA", "projB", "trial-a", "trial-b"):
        cs[k] = expect_counts(p, "1 the job over %s: counts as the tree holds them, both identities hold" % k, ended[k], both, k)
    states = {k: ended[k]["state"].replace("JOB_STATE_", "").lower() for k in ended}
    p.result("1 the jobs end as their skips say", states == {"projA": "partial", "projB": "partial", "trial-a": "done", "trial-b": "done"},
             "states %s; reasons %s" % (states, {k: ended[k].get("reason", "") or "-" for k in ended}))
    for k in ("projA", "projB", "trial-a"):
        expect_files(p, "2 job files lists each skipped entry of %s with the right code" % k, "lab", jobs[k], both, k)
    total = summed(*cs.values())
    p.result("2 the skips by code across the jobs", total["skipped"] == {
        c: sum(1 for _, _, e in both.entries if e == c) for c in {e for _, _, e in both.entries if e not in ("hand", "dup")}},
             "%s" % dict(sorted(total["skipped"].items())))
    units = p.mj("job", "units", "lab", jobs["projA"], "--all")["units"]
    folders = sorted(u.get("folder", "") for u in units)
    p.result("2 job units lists the listing unit and one unit per session folder", len(units) == 1 + 3 and "projA/arc001/s1" in folders,
             "%d units: %s" % (len(units), folders))
    arch = expect_archive(p, "3 the archive read back with pydicom equals the tree's files, labelled as the layout and map give", "lab", both)
    n, reasons = quarantine_empty(p, "lab")
    p.result("3 quarantine is empty: not-DICOM files never reach it", n == 0, "%d quarantined %s" % (n, reasons))
    stored = importkit.stored_files(p.storage("lab"))
    p.result("3 the storage holds one object per instance", stored == len(arch), "%d objects, %d instances in the catalog" % (stored, len(arch)))
    shared = sorted({r[2] for r in arch.values()} & {"projA_s1", "projB_s1"})
    p.result("3 two projects that share a session label stay apart", shared == ["projA_s1", "projB_s1"], "sessions %s" % sorted({r[2] for r in arch.values()}))
    after = importkit.snapshot(p.tree)
    p.result("4 the tree is unchanged after the jobs (mode, size, mtime, hash, link targets, directories)", before == after,
             "%d entries compared; the link points to %s" % (len(after), after["projA/arc001/s1/SCANS/1/DICOM/link.dcm"][4]), importkit.diff(before, after))
    # a second run
    held_before, objects = set(arch), stored
    again = {k: p.start("lab", "xnat" if k.startswith("proj") else "generic", k, *([] if k.startswith("proj") else gmap)) for k in jobs}
    ended2 = {k: p.wait("lab", v) for k, v in again.items()}
    c2 = {k: counts(j) for k, j in ended2.items()}
    held_now = set(p.held("lab"))
    wanted = {k: both.counts(k)["handed_in"] for k in jobs}
    good = (all(c2[k]["archived"] == 0 and c2[k]["already_held"] == wanted[k] and c2[k]["handed_in"] == wanted[k] and identities(c2[k])[0] for k in jobs)
            and held_now == held_before and importkit.stored_files(p.storage("lab")) == objects)
    p.result("5 the same import run again: everything already held, nothing new archived", good,
             "already held %s of %s handed in; archived %s; catalog %d -> %d instances; storage %d -> %d objects"
             % ({k: c2[k]["already_held"] for k in jobs}, wanted, {k: c2[k]["archived"] for k in jobs}, len(held_before), len(held_now),
                objects, importkit.stored_files(p.storage("lab"))))
    p.result("5 the tree is unchanged after the second run", importkit.snapshot(p.tree) == before, "compared again")
    roots(p)
    reader(p, jobs["projA"])


def roots(p):
    """6: a root that lies in the data directory, or is it, is refused, and
    the roots stay as they were."""
    inside = os.path.join(p.data, "imports")
    os.makedirs(inside, exist_ok=True)
    start = p.m("setting", "get", "import.roots").strip()
    attempts = [("inside the data directory", "old=%s\nbad=%s" % (p.tree, inside)), ("the data directory itself", "old=%s\nbad=%s" % (p.tree, p.data)),
                ("inside a storage root", "old=%s\nbad=%s" % (p.tree, os.path.join(p.roots, "lab")))]
    refused = []
    for what, value in attempts:
        r = p.run("setting", "set", "import.roots", value)
        refused.append((what, r.returncode, (r.stderr.strip().splitlines() or [""])[-1][:110]))
    kept = p.m("setting", "get", "import.roots").strip()
    p.result("6 a root inside the data directory, or the data directory, or a storage root, is refused", all(r[1] != 0 for r in refused) and kept == start,
             "; ".join("%s: exit %d (%s)" % r for r in refused) + "; the roots unchanged: %s" % (kept == start))


def reader(p, job):
    """6: a key with the reader role sees root names and counts, never a path."""
    out = p.m("key", "create", "--holder", "proof reader", "--no-expiry")
    key = out.splitlines()[0].strip()
    p.m("grant", "set", key.split("_")[1], "--role", "reader", "--tenant", "lab")
    shown = p.run("job", "show", "lab", job, "--json", key=key)
    receiver = p.run("receiver", "show", "lab", "xnat", "--json", key=key)
    files = p.run("job", "files", "lab", job, key=key)
    units = p.run("job", "units", "lab", job, key=key)
    text = shown.stdout + receiver.stdout + files.stdout + files.stderr + units.stdout + units.stderr
    leaked = [s for s in ("projA", "projB", "arc001", "sessions.csv", p.tree, "s1/SCANS") if s in text]
    names = receiver.returncode == 0 and '"old"' in receiver.stdout
    p.result("6 a reader sees the job's counts and the root's name, and no path", shown.returncode == 0 and files.returncode != 0
             and units.returncode != 0 and not leaked and names,
             "job show exit %d; receiver show exit %d, root name shown: %s; job files exit %d; job units exit %d; paths found in any answer: %s"
             % (shown.returncode, receiver.returncode, names, files.returncode, units.returncode, leaked or "none"),
             [shown.stdout[:300], receiver.stdout[:300]])


# ---- group: deid ----

def group_deid(p):
    model, mapping = Model(), {}
    importkit.build_xnat(model, p.tree, p.args.pydicom_corpus, mapping)
    refs = importkit.build_refs(p.tree, p.args.pydicom_corpus)
    before = importkit.snapshot(p.tree)
    p.setup([("deid", False)])
    p.m("setting", "set", "import.max-dir-entries", "1000")
    doc = os.path.join(p.work, "basic.json")
    with open(doc, "w") as f:
        f.write('{"bases": ["annex-e/basic:2026d"]}')
    p.m("profile", "create", "deid", "basic", "--document", doc, "--note", "the basic profile")
    p.xnat_receiver("deid", "xnat-deid", profile="basic@1")
    p.xnat_receiver("deid", "refs-deid", mapped=False, profile="basic@1")
    jobs = {k: p.start("deid", "xnat-deid", k) for k in ("projA", "projB")}
    ended = {k: p.wait("deid", v) for k, v in jobs.items()}
    cs = {k: counts(j) for k, j in ended.items()}
    for k, c in cs.items():
        ok, text = identities(c)
        want = model.counts(k)
        # With no map, the session the map lacked is imported too.
        unmapped = want["skipped"].pop("unmapped_session", 0)
        p.result("7 the job over %s through a profile: every file has an outcome" % k, ok and c["processing"] == 0 and c["skipped"] == want["skipped"]
                 and c["handed_in"] == want["handed_in"] + unmapped, "job %s: archived %d, already held %d, quarantined %s, skipped %s; %s"
                 % (ended[k]["state"].replace("JOB_STATE_", "").lower(), c["archived"], c["already_held"], c["quarantined"] or "none",
                    c["skipped"], text))
    arch = p.held("deid")
    originals = {v for v in model.original if len(v) >= 6}
    found = {}
    for sop, (data, *_) in arch.items():
        for v in originals:
            if v.encode() in data:
                found.setdefault(v, []).append(sop[-6:])
    names = {str(dcmread(io.BytesIO(d[0])).get("PatientName", "")) for d in arch.values()}
    p.result("7 the de-identified copy carries no patient name, identifier or UID of the tree", arch and not found,
             "%d instances archived; %d original values (names, IDs, study, series and instance UIDs) searched in every byte of each: %d found; "
             "PatientName now %s" % (len(arch), len(originals), len(found), sorted(names)[:3]), ["found %s" % sorted(found)[:6]])
    held = p.mj("quarantine", "list", "deid", "--all").get("files", [])
    print("     quarantined by the profile (fail closed): %d %s" % (len(held), sorted({f.get("reason", "") for f in held})), flush=True)
    # references
    earlier = set(p.held("deid"))
    a = p.start("deid", "refs-deid", "projR/arc001/ct")
    pa = p.wait("deid", a)
    b = p.start("deid", "refs-deid", "projR/arc001/ref")
    pb = p.wait("deid", b)
    arch = p.held("deid")
    # The files these two jobs archived, told apart by class: the labels are hashed.
    new = {s: v for s, v in arch.items() if s not in earlier}
    cls = {s: str(dcmread(io.BytesIO(v[0]), stop_before_pixels=True).SOPClassUID) for s, v in new.items()}
    ct = {s: v for s, v in new.items() if cls[s] == importkit.CT_CLASS}
    srs = {s: v for s, v in new.items() if cls[s] == importkit.SR_CLASS}
    key_out = p.m("key", "create", "--holder", "proof reader", "--no-expiry")
    key = key_out.splitlines()[0].strip()
    p.m("grant", "set", key.split("_")[1], "--role", "reader", "--tenant", "deid")
    resolved, dangling = {}, {}
    for sop, (data, *_) in srs.items():
        ds = dcmread(io.BytesIO(data))
        refd = {str(e.value) for e in ds.iterall() if e.keyword == "ReferencedSOPInstanceUID"}
        (resolved if all(qido(p, key, "deid", r) for r in refd) else dangling)[sop] = refd
    good_refs = [r for refs_ in resolved.values() for r in refs_]
    original_refs = set(refs["ct"]) | {refs["lost"]}
    p.result("8 the SR's references resolve, remapped, to instances of the same tenant", len(ct) == 3 and len(srs) == 2 and len(resolved) == 1
             and len(good_refs) == 2 and not (set(good_refs) & original_refs) and set(good_refs) <= set(ct),
             "CT archived %d, SRs archived %d; the SR whose every reference QIDO-RS finds: %d (references %s, none an original UID, all among the "
             "CT's archived UIDs: %s)" % (len(ct), len(srs), len(resolved), [r[-8:] for r in good_refs], set(good_refs) <= set(ct)),
             ["jobs %s %s" % (pa["state"], pb["state"]), "quarantined %s" % sorted({f.get("reason", "") for f in
                                                                              p.mj("quarantine", "list", "deid", "--all").get("files", [])})])
    p.result("8 the SR that refers to an instance that is in neither folder is found dangling", len(dangling) == 1 and all(
        not qido(p, key, "deid", r) and r not in ct for refs_ in dangling.values() for r in refs_),
             "%d dangling SR, its reference found by no QIDO-RS search and absent from the CT's UIDs" % len(dangling))
    p.result("8 the tree is unchanged after the jobs", importkit.snapshot(p.tree) == before, "compared")


def qido(p, key, tenant, uid):
    """Whether QIDO-RS finds the instance in the tenant, by a plain HTTP call with a reader key."""
    c = http.client.HTTPConnection("127.0.0.1", p.api_port, timeout=60)
    try:
        c.request("GET", "/tenants/%s/dicom-web/instances?SOPInstanceUID=%s" % (tenant, uid),
                  headers={"Authorization": "Bearer " + key, "Accept": "application/dicom+json"})
        r = c.getresponse()
        body = r.read()
        return r.status == 200 and bool(json.loads(body or b"[]"))
    finally:
        c.close()


# ---- group: kill ----

def group_kill(p):
    tree = importkit.build_big(p.tree, p.args.pydicom_corpus)
    before = importkit.snapshot(p.tree)
    p.setup([("lab", True), ("kil", True)])
    p.m("setting", "set", "import.max-units", "1", "--tenant", "kil")
    for t in ("lab", "kil"):
        p.m("receiver", "add", t, "plain", "--type", "import", "--storage", "main", "--root", "old", "--layout", "plain",
            "--max-file-size", "128KiB", "--quiet-period", "5s", "--as-received", "--enabled")
    ctl = counts(p.wait("lab", p.start("lab", "plain", "big")))
    p.result("9 an uninterrupted import of %d files" % len(tree), ctl["archived"] == len(tree) and identities(ctl)[0] and ctl["already_held"] == 0
             and ctl["handed_again"] == 0, "archived %d, already held %d, handed again %d; %s" % (ctl["archived"], ctl["already_held"],
                                                                                              ctl["handed_again"], identities(ctl)[1]))
    job = p.start("kil", "plain", "big")
    at = 0
    deadline = time.time() + 120
    while time.time() < deadline:
        at = counts(p.get("kil", job))["handed_in"]
        if at >= 150:
            break
        time.sleep(0.05)
    p.kill9()
    mid = counts_when_down(p)
    p.serve()
    j = p.wait("kil", job)
    c = counts(j)
    arch = p.held("kil")
    equal = all(arch[s][0] == open(f, "rb").read() for s, f in tree.items() if s in arch)
    keys = ("found", "handed_in", "archived", "already_held", "skipped", "quarantined", "lost", "failed", "discarded", "processing")
    good = (c["archived"] == len(tree) and all(c[k] == ctl[k] for k in keys) and c["handed_again"] == 0 and c["already_held"] == 0
            and identities(c)[0] and j["state"] == "JOB_STATE_DONE" and set(arch) == set(tree) and equal
            and importkit.stored_files(p.storage("kil")) == len(tree))
    p.result("9 a server killed with signal 9 midway, started again: the job ends done, counts as the uninterrupted run's", good,
             "killed with %d of %d handed in (the last count read before the kill; %s after); then %s: archived %d, already held %d (a file handed "
             "twice would show here), handed again %d; catalog %d instances, storage %d objects, bytes equal to the files: %s; %s"
             % (at, len(tree), mid, j["state"].replace("JOB_STATE_", "").lower(), c["archived"], c["already_held"], c["handed_again"],
                len(arch), importkit.stored_files(p.storage("kil")), equal, identities(c)[1]),
             ["uninterrupted %s" % ctl, "killed %s" % c])
    p.result("9 the tree is unchanged by either run", importkit.snapshot(p.tree) == before, "compared")


def counts_when_down(p):
    """What the catalog says right after the kill, read by SQLite itself: the
    held instances the tenant has."""
    rows = query(p.catalog("kil"), "SELECT count(*) AS n FROM instances")
    return "%d held in the catalog" % rows[0]["n"]


GROUPS = {"tree": group_tree, "deid": group_deid, "kill": group_kill}


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--go", default="go")
    ap.add_argument("--pydicom-corpus", default=os.environ.get("PYDICOM_TEST_FILES", DEFAULT_CORPUS), help="pydicom's test files directory")
    ap.add_argument("--binary", default="", help="a built nopacs to run, in place of building the working tree's; needs --server-binary")
    ap.add_argument("--server-binary", default="", help="a built nopacs-server to run, in place of building the working tree's")
    ap.add_argument("--only", default="", help="groups to run, comma separated: tree, deid, kill (default all)")
    args = ap.parse_args(argv)
    names = [n for n in args.only.split(",") if n] or list(GROUPS)
    bad = [n for n in names if n not in GROUPS]
    if bad:
        raise Failure("no group %s; groups are %s" % (bad, ", ".join(GROUPS)))
    failed = 0
    ok = 0
    for n in names:
        p = Proof(args)
        print("== %s" % n, flush=True)
        t0 = time.time()
        try:
            GROUPS[n](p)
        finally:
            p.stop()
        print("   (%s took %.0f s)" % (n, time.time() - t0), flush=True)
        ok += p.counts["ok"]
        failed += p.counts["FAIL"]
        if p.counts["FAIL"]:
            print("the work directory is kept: %s" % p.work)
        else:
            subprocess.run(["rm", "-rf", p.work])
    print()
    print("%d ok, %d FAIL" % (ok, failed))
    if failed:
        return 1

