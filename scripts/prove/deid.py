#!/usr/bin/env python3
"""The proof of de-identification, from outside the binary.

It builds and runs the real program, sets it up with its own command line
only (profile create, receiver add --profile, receiver route, quarantine
release, profile try, explain and diff, deid check and dry-run), and sends
it public corpus files (pydicom's test files, its character set files, this
repository's receive corpus) and files this script builds with pydicom, in
which identifying values are planted where they are known to hide: names
in every script the corpus has, identifiers, dates in written forms, UIDs,
addresses, private elements with and without a creator and as UN, names
ten sequences deep, burned-in text in the pixels, a JPEG comment segment.
What was archived, held and logged is then read only with tools that are
not NoPACs: pydicom, dcmtk's dcmdump and dcmdjpeg, Python's sqlite3 and the
sqlite3 tool, and a byte search written here. Each check prints one line,
"ok", "FAIL" or "skip", with what it saw; the run ends with the numbers it
measured and the counts, and exits 1 when one failed.

  deid.py --go GO --dcmtk DIR [--sqlite3 PATH]
          [--orthanc URL --orthanc-dimse AET@HOST:PORT]          (make prove-deid)

--only and --skip take comma separated groups (faults rules trial check crash
numbers more memory forward nowhere) to run a part of the proof in under three
minutes; the setup and the main study always run, and more also runs faults.

--orthanc adds a last step: a de-identified session sent to that Orthanc
by DIMSE and read back through its REST API (each instance's file), which
must carry no planted value; only the studies the step adds are deleted
after. Orthanc must admit the calling AE title NoPACs. Without it the step
is skipped.

Needs: Go, dcmtk, the sqlite3 tool, and a Python with pydicom 2, numpy and
pynetdicom (scripts/requirements/prove-reception.txt).
"""

import argparse
import json
import os
import platform
import shutil
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time

from pydicom import dcmread
from pynetdicom import AE

import deid_faults
import deid_more
import deid_numbers
import deid_rules
import deid_steps
import deidkit

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
QUIET = "5s"  # the least quiet period a receiver takes
ENDED_STATES = ("done", "failed", "discarded")

# The profiles the proof makes, as documents in their file form.
RESEARCH = {
    "bases": ["nopacs/longitudinal:1"],
    "description": "The proof's research profile: dates shifted per subject, burned-in bands filled.",
    "rules": [
        {"name": "refuse-flagged", "selector": {"private_element": {"creator": "PROVE REFUSE", "group": "0029", "element": "xx"}},
         "action": {"quarantine": {}}, "reason": "a file its sender flagged is not archived"},
    ],
    "pixel_rules": [
        {"name": "us-band", "when": [{"subject": "CONDITION_SUBJECT_MANUFACTURER", "equals": "PROVE US"}],
         "redact": {"rectangles": [{"x": 0, "y": 0, "width": 64, "height": 12}]}, "reason": "the band this device writes names in"},
        {"name": "outside", "when": [{"subject": "CONDITION_SUBJECT_MANUFACTURER", "equals": "PROVE OUTSIDE"}],
         "redact": {"rectangles": [{"x": 4000, "y": 0, "width": 8, "height": 8}]}, "reason": "a rectangle past every image"},
    ],
}
MRN_RULES = [
    {"name": "mrn-anywhere", "selector": {"value_pattern": {"pattern": "MRN[0-9]{6}", "search": True}},
     "action": {"redact": {"pattern": "MRN[0-9]{6}"}}, "reason": "a record number typed anywhere"},
    {"name": "mrn-study", "selector": {"keyword": "StudyDescription"}, "action": {"redact": {"pattern": "MRN[0-9]{6}"}},
     "reason": "the study description is kept but for record numbers"},
    {"name": "keep-series", "selector": {"keyword": "SeriesDescription"}, "action": {"keep": {}},
     "reason": "kept as typed, so a copy of a cut value there is found"},
]
NO_MR = {"name": "no-mr", "selector": {"keyword": "Modality"},
         "when": [{"subject": "CONDITION_SUBJECT_MODALITY", "equals": "MR"}], "action": {"quarantine": {}},
         "reason": "a stricter version that holds MR"}
BASIC = {"bases": ["annex-e/basic:2026d"]}
COHORT = {"name": "cohort-code", "selector": {"keyword": "PatientID"},
          "action": {"lookup": {"table": "cohort", "on_missing": "LOOKUP_MISSING_QUARANTINE"}},
          "reason": "the code a person chose for each subject"}


class Proof:
    """The run: its work directory, the server it runs, and its results."""

    def __init__(self, args):
        self.args = args
        # A short directory: the local socket's path must fit the 104 bytes
        # a Unix socket address holds on macOS.
        self.work = tempfile.mkdtemp(prefix="nopacs-deid.", dir="/tmp")
        os.chmod(self.work, 0o700)
        self.data = os.path.join(self.work, "data")
        self.bin = os.path.join(self.work, "nopacs")
        self.sbin = os.path.join(self.work, "nopacs-server")
        self.roots = os.path.join(self.work, "roots", "fast")
        self.files = os.path.join(self.work, "files")
        self.server = None
        self.counts = {"ok": 0, "FAIL": 0, "skip": 0}
        self.numbers = []
        self.ports = {}
        self.receivers = ()
        # Every value of the data sent only to de-identifying receivers.
        self.needles = deidkit.Needles(everywhere=True)
        self.versions = {}  # "name@version" -> the catalog's id

    # ---- results ----

    def result(self, name, ok, detail, evidence=()):
        word = "ok" if ok else "FAIL"
        self.counts[word] += 1
        print("%-4s %s: %s" % (word, name, detail), flush=True)
        if not ok:
            for e in list(evidence)[:12]:
                print("     | %s" % e, flush=True)

    def skip(self, name, why):
        self.counts["skip"] += 1
        print("skip %s: %s" % (name, why), flush=True)

    def note(self, text):
        print("     " + text, flush=True)

    # ---- the program ----

    def exe(self, *args):
        """The program a command belongs to: the commands that touch the data directory with the server stopped are nopacs-server's."""
        server = args[:1] in (("init",), ("backup",)) or args[:2] == ("tenant", "import")
        return self.sbin if server else self.bin

    def run(self, *args, stdin=None, timeout=180):
        return subprocess.run([self.exe(*args), "--data-dir", self.data, *args], capture_output=True, text=True, timeout=timeout, input=stdin)

    def m(self, *args, stdin=None):
        """Runs the command line on the local socket and returns its output."""
        p = self.run(*args, stdin=stdin)
        if p.returncode != 0:
            raise SystemExit("nopacs %s: exit %d: %s" % (" ".join(args), p.returncode, p.stderr.strip()))
        return p.stdout

    def mj(self, *args):
        return json.loads(self.m(*args, "--json"))

    def serve(self):
        log = open(os.path.join(self.work, "serve.log"), "a")
        self.server = subprocess.Popen([self.sbin, "--data-dir", self.data, "serve"], stdout=log, stderr=log)
        for _ in range(300):
            if self.server.poll() is not None:
                raise SystemExit("the server exited at its start; see serve.log")
            if self.run("whoami").returncode == 0:
                return
            time.sleep(0.1)
        raise SystemExit("the server did not answer")

    def kill(self):
        """Ends the server with signal 9, as a crash does."""
        self.server.send_signal(signal.SIGKILL)
        self.server.wait(60)

    def stop(self):
        if self.server and self.server.poll() is None:
            self.server.terminate()
            try:
                self.server.wait(60)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait()

    def profile(self, name, doc, note):
        """Stores a document as the next version of a profile; returns
        NAME@VERSION."""
        path = os.path.join(self.work, "%s-%d.json" % (name, len(self.versions) + 1))
        with open(path, "w") as f:
            json.dump(doc, f)
        v = self.mj("profile", "create", "lab", name, "--document", path, "--note", note)
        ver = v.get("version", v)
        ref = "%s@%d" % (ver["name"], ver["version"])
        self.versions[ref] = self.query("SELECT id FROM profile_versions WHERE name = ? AND version = ?",
                                        ver["name"], ver["version"])[0]["id"]
        return ref

    def setting(self, name, value):
        self.m("setting", "set", name, value, "--tenant", "lab")
        time.sleep(2)  # a setting reaches the pipeline within a second or two

    def wait_up(self, receiver, timeout=30):
        deadline = time.time() + timeout
        while time.time() < deadline:
            r = self.mj("receiver", "show", "lab", receiver)
            if r.get("receiver", r).get("health", {}).get("state") == "RECEIVER_STATE_UP":
                return
            time.sleep(0.1)
        raise SystemExit("the receiver %s did not come up" % receiver)

    # ---- outside tools ----

    def dcmtk(self, name):
        return os.path.join(self.args.dcmtk, name) if self.args.dcmtk else name

    def dcmdump(self, path):
        """dcmdump's exit code, its output, and the lines it warned or
        failed with."""
        p = subprocess.run([self.dcmtk("dcmdump"), "+E", path], capture_output=True, text=True, errors="replace", timeout=60)
        bad = [line for line in (p.stdout + p.stderr).splitlines() if line.startswith(("W:", "E:"))]
        return p.returncode, p.stdout, bad

    def send(self, receiver, files, aet="PROVE", on_each=None):
        """Sends files with pynetdicom, each from its path; returns each
        status (None for one not answered)."""
        ae = AE(ae_title=aet)
        ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 60
        contexts = []
        for f in files:
            meta = dcmread(f, stop_before_pixels=True, force=True)
            pair = (str(meta.SOPClassUID), str(meta.file_meta.TransferSyntaxUID))
            if pair not in contexts:
                contexts.append(pair)
                ae.add_requested_context(*pair)
        assoc = ae.associate("127.0.0.1", self.ports[receiver], ae_title="ARCHIVE")
        statuses = []
        if not assoc.is_established:
            return statuses
        try:
            for i, f in enumerate(files):
                st = assoc.send_c_store(f)
                statuses.append(st.Status if "Status" in st else None)
                if on_each:
                    on_each(i)
            assoc.release()
        except Exception:  # noqa: BLE001 - the server went away mid association: a crash
            statuses.append(None)
        return statuses

    # ---- the archive, read by SQLite itself ----

    def catalog(self):
        return os.path.join(self.data, "tenants", "lab.db")

    def query(self, sql, *params):
        db = sqlite3.connect("file:%s?mode=ro" % self.catalog(), uri=True, timeout=30)
        db.row_factory = sqlite3.Row
        try:
            return db.execute(sql, params).fetchall()
        finally:
            db.close()

    def receptions(self, receiver):
        return {r["id"] for r in self.query("SELECT id FROM receptions WHERE receiver = ?", receiver)}

    def settle(self, receiver, before, timeout=180):
        """Waits until the receiver has receptions not in before and every
        one of them has ended, and none has appeared for two seconds;
        returns their ids."""
        deadline = time.time() + timeout
        stable_since, last = None, None
        while time.time() < deadline:
            rows = [r for r in self.query("SELECT id, ended FROM receptions WHERE receiver = ?", receiver) if r["id"] not in before]
            ids = frozenset(r["id"] for r in rows)
            if rows and all(r["ended"] for r in rows):
                if ids != last:
                    stable_since, last = time.time(), ids
                elif time.time() - stable_since >= 2:
                    return set(ids)
            else:
                last = None
            time.sleep(0.2)
        raise SystemExit("the receptions of %s did not end in %d s" % (receiver, timeout))

    def archived(self, receptions):
        return deidkit.instances_of(self.query, sorted(receptions))

    def held(self, receptions):
        if not receptions:
            return []
        marks = ",".join("?" * len(receptions))
        return self.query("SELECT * FROM quarantine WHERE reception_id IN (%s)" % marks, *sorted(receptions))

    def storage_path(self, storage, key=""):
        return os.path.join(self.roots, "lab", storage, key)

    def incoming_path(self, row):
        return os.path.join(self.data, "incoming", "lab", row["incoming"])

    def read_archived(self, row):
        return dcmread(self.storage_path(row["storage"], row["storage_key"]))

    def build(self, name):
        d = os.path.join(self.files, name)
        os.makedirs(d, exist_ok=True)
        return d

    def corpus(self, name):
        """A public corpus file: this repository's receive corpus first, then
        pydicom's test files and its character set files beside them."""
        here = self.args.pydicom_corpus
        for d in (os.path.join(ROOT, "internal", "receive", "testdata"), here, os.path.join(os.path.dirname(here), "charset_files")):
            path = os.path.join(d, name)
            if os.path.exists(path):
                return path
        raise SystemExit("%s is in neither this repository's corpus nor %s; --pydicom-corpus names pydicom's whole test_files "
                         "directory (a pydicom installed by pip holds only part of it)" % (name, here))


def free_ports(n):
    socks = [socket.socket() for _ in range(n)]
    for s in socks:
        s.bind(("127.0.0.1", 0))
    ports = [s.getsockname()[1] for s in socks]
    for s in socks:
        s.close()
    return ports


def machine():
    try:
        cpu = subprocess.run(["sysctl", "-n", "machdep.cpu.brand_string"], capture_output=True, text=True).stdout.strip()
        mem = int(subprocess.run(["sysctl", "-n", "hw.memsize"], capture_output=True, text=True).stdout.strip()) / 2**30
        return "%s, %.0f GiB, %s %s" % (cpu, mem, platform.system(), platform.release())
    except (OSError, ValueError):
        return "%s %s %s" % (platform.system(), platform.release(), platform.machine())


RECEIVERS = ("deid", "mrn", "routed", "flip", "plain", "bystudy", "cohort")


def setup(p):
    """The installation, made with the program's own commands: a tenant on
    SQLite with three storages on a directory, six profile versions and
    seven receivers."""
    p.receivers = RECEIVERS
    ports = free_ports(len(RECEIVERS))
    p.ports = dict(zip(RECEIVERS, ports))
    p.m("init")
    p.serve()
    os.makedirs(p.roots)
    p.m("setting", "set", "storage.directory-roots", "fast=" + p.roots)
    p.m("tenant", "create", "lab")
    p.m("setting", "set", "receiver.ports", ",".join(str(x) for x in ports), "--tenant", "lab")
    p.m("setting", "set", "receiver.allow-as-received", "true", "--tenant", "lab")
    for s in ("main", "a", "b"):
        p.m("storage", "add", "lab", s, "--type", "dir", "--root", "fast", "--path", s)
    research1 = p.profile("research", RESEARCH, "dates shifted, burned-in bands filled")
    research2 = p.profile("research", dict(RESEARCH, rules=RESEARCH["rules"] + MRN_RULES), "record numbers cut")
    research3 = p.profile("research", dict(RESEARCH, rules=RESEARCH["rules"] + [NO_MR]), "stricter: holds MR")
    basic1 = p.profile("basic", BASIC, "the basic profile alone")
    basic2 = p.profile("basic", dict(BASIC, rules=[NO_MR]), "stricter: holds MR")
    research4 = p.profile("research", dict(RESEARCH, rules=RESEARCH["rules"] + [COHORT]), "subjects coded from a table")
    common = ["--aet", "ARCHIVE", "--quiet-period", QUIET, "--enabled"]
    p.m("receiver", "add", "lab", "deid", "--storage", "main", "--port", str(p.ports["deid"]), "--allow-aet", "PROVE",
        "--profile", research1, *common)
    p.m("receiver", "add", "lab", "mrn", "--storage", "main", "--port", str(p.ports["mrn"]), "--allow-aet", "PROVE",
        "--profile", research2, *common)
    p.m("receiver", "add", "lab", "routed", "--storage", "main", "--port", str(p.ports["routed"]), "--allow-aet", "SITE_A",
        "--allow-aet", "SITE_B", "--allow-aet", "OTHER", "--profile", research1, *common)
    p.m("receiver", "add", "lab", "flip", "--storage", "main", "--port", str(p.ports["flip"]), "--allow-aet", "PROVE",
        "--as-received", "--aet", "ARCHIVE", "--quiet-period", "20s", "--enabled")
    p.m("receiver", "add", "lab", "bystudy", "--storage", "main", "--port", str(p.ports["bystudy"]), "--allow-aet", "PROVE",
        "--profile", research1, "--naming-subject", "sub-{#StudyInstanceUID}", "--naming-session", "ses-{#StudyInstanceUID}",
        "--naming-scan", "scan-{#SeriesInstanceUID}", *common)
    p.m("receiver", "add", "lab", "cohort", "--storage", "main", "--port", str(p.ports["cohort"]), "--allow-aet", "PROVE",
        "--profile", research4, *common)
    p.m("receiver", "add", "lab", "plain", "--storage", "main", "--port", str(p.ports["plain"]), "--allow-aet", "PROVE",
        "--as-received", *common)
    p.m("receiver", "route", "add", "lab", "routed", "site-a", "--storage", "a", "--equals", "calling-aet=SITE_A",
        "--profile", research1)
    p.m("receiver", "route", "add", "lab", "routed", "site-b", "--storage", "b", "--equals", "calling-aet=SITE_B",
        "--profile", basic1)
    p.m("receiver", "route", "default", "lab", "routed", "quarantine")
    for r in RECEIVERS:
        p.wait_up(r)
    p.note("the tenant: storages main, a, b; versions %s; receivers %s; routes of routed:\n%s"
           % (", ".join("%s (id %d)" % kv for kv in p.versions.items()), ", ".join(RECEIVERS),
              "\n".join("       " + line for line in p.m("receiver", "route", "list", "lab", "routed").rstrip().splitlines())))
    return research1, research2, research3, basic1, basic2


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--go", default="go")
    ap.add_argument("--dcmtk", default="")
    ap.add_argument("--sqlite3", default="sqlite3")
    ap.add_argument("--pydicom-corpus", default=os.environ.get("PYDICOM_TEST_FILES", ""),
                    help="pydicom's test_files directory (default: the installed pydicom's)")
    ap.add_argument("--only", default="", help="run only these groups, comma separated: faults rules trial check crash numbers more "
                                               "memory forward nowhere (the setup and the main study always run)")
    ap.add_argument("--skip", default="", help="leave out these groups, comma separated")
    ap.add_argument("--orthanc", default="", help="Orthanc's REST API, such as http://127.0.0.1:8042, for the optional last step")
    ap.add_argument("--orthanc-dimse", default="", help="Orthanc's DIMSE service, AET@HOST:PORT")
    args = ap.parse_args()
    if not args.pydicom_corpus:
        from pydicom.data import get_testdata_file
        args.pydicom_corpus = os.path.dirname(get_testdata_file("MR_small.dcm"))
    p = Proof(args)
    try:
        subprocess.run([args.go, "build", "-o", p.bin, os.path.join(ROOT, "cmd", "nopacs")], check=True)
        subprocess.run([args.go, "build", "-o", p.sbin, os.path.join(ROOT, "cmd", "nopacs-server")], check=True)
        versions = setup(p)
        study = deid_steps.no_value_survives(p)
        # Groups run in this order; --only keeps some, --skip drops some.
        # faults is what more needs, so it always runs when more does.
        faults = {}
        groups = [
            ("faults", lambda: faults.update(deid_faults.fail_closed(p))),
            ("rules", lambda: (deid_rules.pixel_rules(p, study), deid_rules.routing(p), deid_rules.patterns(p))),
            ("trial", lambda: deid_steps.trial_agrees(p, study, versions[0])),
            ("check", lambda: deid_steps.check_and_dry_run(p, study, versions[2], versions[4])),
            ("crash", lambda: deid_faults.crash(p)),
            ("numbers", lambda: deid_numbers.bench(p)),
            ("more", lambda: deid_more.run(p, study, faults)),
            ("memory", lambda: deid_more.check_memory(p)),
            ("forward", lambda: deid_steps.forward(p, study)),
            ("nowhere", lambda: deid_steps.nowhere_else(p)),
        ]
        known = [g for g, _ in groups]
        chosen = [g for g in args.only.split(",") if g] or known
        dropped = [g for g in args.skip.split(",") if g]
        bad = [g for g in chosen + dropped if g not in known]
        if bad:
            raise SystemExit("unknown group %s; the groups are %s" % (bad, ", ".join(known)))
        if "more" in chosen and "more" not in dropped:
            chosen.append("faults")
        for name, fn in groups:
            if name in chosen and name not in dropped:
                began = time.time()
                fn()
                p.note("[group %s took %.0f s]" % (name, time.time() - began))
    finally:
        p.stop()
    print()
    print("The numbers, on %s:" % machine())
    for n in p.numbers:
        print("  " + n)
    print("%d ok, %d FAIL, %d skipped" % (p.counts["ok"], p.counts["FAIL"], p.counts["skip"]))
    if p.counts["FAIL"]:
        print("the work directory is kept: %s" % p.work)
        sys.exit(1)
    shutil.rmtree(p.work, ignore_errors=True)


if __name__ == "__main__":
    main()
