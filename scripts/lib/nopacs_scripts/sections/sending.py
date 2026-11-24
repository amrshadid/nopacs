"""The proof of sending, from outside the binary.

It builds and runs the real program, sets it up with its own command line,
archives public and synthetic studies sent by dcmtk's storescu, and sends
them on with the program's senders and jobs to an Orthanc server, by DIMSE
(C-STORE) and by STOW-RS at its /dicom-web. What arrived is read back only
with tools that are not NoPACs: Orthanc's REST API (each instance's file as
Orthanc keeps it), pydicom, Python's sqlite3 on the tenant's catalog, and a
pynetdicom peer that answers each C-STORE with a status the proof chooses.
Each check prints one line, "ok", "FAIL" or "skip", with what it saw; the
run ends with the numbers it measured and the counts, and exits 1 when one
failed.

  sending.py --go GO --dcmtk DIR --orthanc URL --orthanc-dimse AET@HOST:PORT
             [--orthanc-logs COMMAND] [--many N]
             [--postgres URL --psql COMMAND]                    (make prove-sending)

URL is Orthanc's REST API, such as http://127.0.0.1:8042, without
authentication; its DICOMweb plugin must be on, and its DIMSE port must
admit the calling AE title NOPACS from this machine. Orthanc must hold
nothing when the run starts: every study in it is deleted between the
checks and at the end. An Orthanc made for it:

  docker run -d --name orthanc -p 127.0.0.1:8042:8042 -p 127.0.0.1:4242:4242 \\
      -e DICOM_WEB_PLUGIN_ENABLED=true -e ORTHANC__AUTHENTICATION_ENABLED=false \\
      orthancteam/orthanc

--orthanc-logs is a command that prints Orthanc's log, such as "docker logs
orthanc": an instance sent twice is discarded by Orthanc and seen only
there, so without it the checks of how many were sent twice are skipped.
The proof sets Orthanc's log level to verbose for the run and puts it back.
--many is the size of the session stopped midway (400 by default).
--postgres (postgres://USER:PASSWORD@HOST:PORT/DB) and --psql (a command that
runs psql against that server, such as "docker exec -i CONTAINER psql -U
postgres") add a tenant on Postgres, in a database the proof makes and drops,
whose send is killed with signal 9 midway; without them that check is skipped.
openssl makes the throwaway authority of the check over TLS.

Needs: Go, dcmtk's storescu (pynetdicom installs a program of the same
name), and a Python with pydicom and pynetdicom
(scripts/requirements/prove-reception.txt).
"""

import argparse
import json
import os
import signal
import subprocess
import tempfile
import time

from pydicom import dcmread

from nopacs_scripts import send as sendkit
from nopacs_scripts.db import query
from nopacs_scripts.net import free_ports
from nopacs_scripts.proc import build_binaries
from nopacs_scripts.proof import Programs, Reporting
from nopacs_scripts.proof import machine
from nopacs_scripts.report import Failure
from nopacs_scripts.sections import sending_faults
from nopacs_scripts.sections import sending_numbers
from nopacs_scripts.send import CT, OTHERS, Orthanc, Peer, archived, corpus_path, many, read_bytes, rewrite, split_part10

QUIET = "5s"  # the least quiet period a receiver takes
ENDED = ("JOB_STATE_DONE", "JOB_STATE_PARTIAL", "JOB_STATE_FAILED", "JOB_STATE_CANCELED")
# The storescu option that proposes a compressed transfer syntax, by its UID:
# RLE, JPEG baseline, JPEG 2000 lossless and JPEG-LS lossless.
PROPOSE = {"1.2.840.10008.1.2.5": "-xr", "1.2.840.10008.1.2.4.50": "-xy", "1.2.840.10008.1.2.4.90": "-xv",
           "1.2.840.10008.1.2.4.80": "-xt"}
MAX_BATCH = 64  # the most instances whose answers one progress write records


class Proof(Reporting, Programs):
    """The run: its work directory, the server it runs, and its results."""

    def __init__(self, args):
        self.args = args
        # A short directory: the local socket's path must fit the 104 bytes
        # a Unix socket address holds on macOS.
        self.work = tempfile.mkdtemp(prefix="nopacs-send.", dir="/tmp")
        os.chmod(self.work, 0o700)
        self.data = os.path.join(self.work, "data")
        self.bin = os.path.join(self.work, "nopacs")
        self.sbin = os.path.join(self.work, "nopacs-server")
        self.roots = os.path.join(self.work, "roots", "fast")
        self.server = None
        self.api_port = 0  # the server's network listener, for the calls made with a key
        self.counts = {"ok": 0, "FAIL": 0, "skip": 0}
        self.numbers = []

    # ---- the program ----

    def run(self, *args, stdin=None):
        return subprocess.run([self.exe(*args), "--data-dir", self.data, *args], capture_output=True, text=True, timeout=120, input=stdin)

    def m(self, *args, stdin=None):
        """Runs the command line on the local socket and returns its output."""
        p = self.run(*args, stdin=stdin)
        if p.returncode != 0:
            raise Failure("nopacs %s: exit %d: %s" % (" ".join(args), p.returncode, p.stderr.strip()))
        return p.stdout

    def serve(self):
        log = open(os.path.join(self.work, "serve.log"), "a")
        listen = ["--listen", "127.0.0.1:%d" % self.api_port] if self.api_port else []
        self.server = subprocess.Popen([self.sbin, "--data-dir", self.data, "serve", *listen], stdout=log, stderr=log)
        for _ in range(300):
            if self.server.poll() is not None:
                raise Failure("the server exited at its start; see serve.log")
            if self.run("whoami").returncode == 0:
                return
            time.sleep(0.1)
        raise Failure("the server did not answer")

    def end(self, sig):
        """Ends the server with sig: SIGTERM as a person stops it, SIGKILL as
        a crash does."""
        self.server.send_signal(sig)
        self.server.wait(120)

    def counts_of(self, j):
        return counts(j)

    # ---- the archive ----

    def catalog(self):
        return os.path.join(self.data, "tenants", "lab.db")

    def storage(self):
        return os.path.join(self.roots, "lab", "main")

    def storescu(self, port, files):
        """Sends files with dcmtk's storescu; returns how many were answered
        success. storescu proposes the uncompressed transfer syntaxes alone
        unless told to propose another, and cannot convert a compressed file
        to them, so the files of each compressed syntax go in an
        association that proposes their own; -R proposes only the SOP
        classes the files need (the segmentation's is not among storescu's
        defaults)."""
        tool = os.path.join(self.args.dcmtk, "storescu") if self.args.dcmtk else "storescu"
        groups = {}
        for f in files:
            ts = str(dcmread(f, stop_before_pixels=True).file_meta.TransferSyntaxUID)
            groups.setdefault(PROPOSE.get(ts, ""), []).append(f)
        out = ""
        for option, part in sorted(groups.items()):
            p = subprocess.run([tool, "-v", "-R", *([option] if option else []), "-aet", "STORESCU", "-aec", "ARCHIVE",
                                "127.0.0.1", str(port), *part], capture_output=True, text=True, timeout=600)
            out += p.stdout + p.stderr
        self.storescu_out = out
        return out.count("Received Store Response (Success)")

    def archive(self, port, files, study):
        """Sends files to a receiver and waits until their reception has
        ended; returns the archived files of the study and its labels."""
        n = self.storescu(port, files)
        if n != len(files):
            said = [line for line in self.storescu_out.splitlines() if line[:2] == "E:" or "Response" in line or "Sending file" in line]
            raise Failure("storescu: %d of %d files answered success:\n%s" % (n, len(files), "\n".join(said[-40:])))
        deadline = time.time() + 180
        while time.time() < deadline:
            rows = query(self.catalog(), "SELECT state, ended FROM receptions WHERE study_uid = ?", study)
            if rows and all(r["ended"] for r in rows):
                break
            time.sleep(0.2)
        held, labels = archived(self.catalog(), self.storage(), study)
        if len(held) != len(files):
            raise Failure("the study %s: %d of %d files archived" % (study, len(held), len(files)))
        return held, labels

    # ---- jobs ----

    def job(self, sender, target):
        return self.mj("job", "create", "lab", "--sender", sender, target)["job"]["id"]

    def retry(self, job):
        """Retries an ended job; returns the new job's id, which the ended
        job names."""
        ended = self.mj("job", "retry", "lab", job)["job"]
        new = ended["retryJob"] if ended["id"] == job else ended["id"]
        if not new or new == job:
            raise Failure("job retry %s named no new job: %s" % (job, json.dumps(ended)))
        return new

    def get(self, job):
        return self.mj("job", "show", "lab", job)["job"]

    def wait_job(self, job, timeout=300, until=ENDED):
        deadline = time.time() + timeout
        j = self.get(job)
        while j["state"] not in until and time.time() < deadline:
            time.sleep(0.1)
            j = self.get(job)
        return j


def counts(j):
    c = j["counts"]
    return "%s sent, %s warned, %s refused of %s" % (c["instancesSent"], c["instancesWarned"], c["instancesRefused"], c["instances"])


def compare(p, orthanc, held):
    """Each archived instance against Orthanc's copy: the data sets after
    the meta group byte for byte, and pydicom's reading element for element.
    Returns (missing, bytes differ, elements differ, whole files equal,
    instances Orthanc lists)."""
    have, listed = orthanc.instances()
    missing, byte_diff, elem_diff, whole = [], [], [], 0
    for uid, mine in held.items():
        if uid not in have:
            missing.append(uid)
            continue
        theirs = orthanc.file(have[uid])
        whole += theirs == mine
        if split_part10(theirs)[1] != split_part10(mine)[1]:
            byte_diff.append(uid)
        if read_bytes(theirs) != read_bytes(mine):
            elem_diff.append(uid)
    return missing, byte_diff, elem_diff, whole, listed


def send_session(p, orthanc, name, sender, held, labels):
    """A whole session sent by one sender arrives in Orthanc complete and
    equal to what is archived."""
    orthanc.clear()
    t0 = time.time()
    job = p.job(sender, "%s/%s" % labels)
    j = p.wait_job(job)
    wall = time.time() - t0
    missing, byte_diff, elem_diff, whole, listed = compare(p, orthanc, held)
    good = j["state"] == "JOB_STATE_DONE" and not missing and not byte_diff and not elem_diff and listed == len(held)
    p.result(name, good, "job %s (%s) in %.2f s; Orthanc holds %d of %d instances (lists %d); data sets byte-equal to the "
             "archived: %d; pydicom-equal: %d; whole files equal, meta group included: %d"
             % (j["state"], counts(j), wall, len(held) - len(missing), len(held), listed, len(held) - len(missing) - len(byte_diff),
                len(held) - len(missing) - len(elem_diff), whole),
             ["missing: %s" % missing[:5], "bytes differ: %s" % byte_diff[:5], "elements differ: %s" % elem_diff[:5]])
    return j


# ---- the steps ----

def whole_session(p, orthanc, port):
    """1 and 2: a session of five storage classes and five transfer syntaxes,
    archived from storescu, sent by DIMSE and then by STOW-RS."""
    series = [str(dcmread(os.path.join(sendkit.CORPUS, f), stop_before_pixels=True).SeriesInstanceUID) for f in CT]
    sources = [os.path.join(sendkit.CORPUS, f) for f in CT] + [corpus_path(f, p.args.pydicom_corpus) for f in OTHERS]
    files, study = rewrite(sources, os.path.join(p.work, "studies", "whole"), "PRV-SEND-1", "Prove^Sending",
                           series_of=lambda i: series[i] if i < len(series) else i)
    held, labels = p.archive(port, files, study)
    kinds = {(str(d.SOPClassUID), str(d.file_meta.TransferSyntaxUID)) for d in (dcmread(f, stop_before_pixels=True) for f in files)}
    print("     the session %s/%s: %d instances, %d storage classes, %d transfer syntaxes, %d series"
          % (labels[0], labels[1], len(held), len({k[0] for k in kinds}), len({k[1] for k in kinds}),
             len({str(dcmread(f, stop_before_pixels=True).SeriesInstanceUID) for f in files})), flush=True)
    send_session(p, orthanc, "1 a whole session sent by DIMSE arrives in Orthanc complete and equal", "orthanc", held, labels)
    send_session(p, orthanc, "2 a whole session sent by STOW-RS arrives in Orthanc complete and equal", "web", held, labels)


def restart(p, orthanc, port):
    """3: a send stopped midway by SIGTERM, then by SIGKILL, ends done after
    the restart with every instance in Orthanc once and at most one batch
    sent twice."""
    files, study = many(os.path.join(sendkit.CORPUS, CT[0]), os.path.join(p.work, "studies", "many"), p.args.many, "PRV-SEND-MANY")
    held, labels = p.archive(port, files, study)
    for sig, name in ((signal.SIGTERM, "SIGTERM"), (signal.SIGKILL, "SIGKILL")):
        orthanc.clear()
        before = orthanc.discarded()
        job = p.job("slow", "%s/%s" % labels)
        deadline = time.time() + 120
        while orthanc.count() < len(held) // 3 and time.time() < deadline:
            time.sleep(0.05)
        at_stop = orthanc.count()
        p.end(sig)
        p.serve()
        j = p.wait_job(job, timeout=300)
        have, listed = orthanc.instances()
        missing = [u for u in held if u not in have]
        after = orthanc.discarded()
        twice = None if before is None else after - before
        good = j["state"] == "JOB_STATE_DONE" and not missing and listed == len(held) and (twice is None or twice <= MAX_BATCH)
        p.result("3 a send stopped by %s midway ends done after the restart, each instance once" % name, good,
                 "stopped with %d of %d in Orthanc; job %s (%s); Orthanc holds %d distinct SOP Instance UIDs, lists %d, misses %d; "
                 "sent twice (Orthanc's log, discarded as already stored): %s, at most %d allowed (one batch)"
                 % (at_stop, len(held), j["state"], counts(j), len(have), listed, len(missing),
                    "not read (no --orthanc-logs)" if twice is None else twice, MAX_BATCH), ["missing: %s" % missing[:5]])
    if orthanc.discarded() is None:
        p.skip("3 how many were sent twice", "no --orthanc-logs: Orthanc discards a repeat and says so only in its log")
    return held, labels


def forwarding(p, orthanc, port):
    """4: a receiver that forwards sends each reception it archives on."""
    orthanc.clear()
    sources = [os.path.join(sendkit.CORPUS, f) for f in CT]
    files, study = rewrite(sources, os.path.join(p.work, "studies", "fwd"), "PRV-SEND-FWD", "Prove^Forward")
    held, labels = p.archive(port, files, study)
    deadline = time.time() + 120
    jobs = []
    while time.time() < deadline:
        jobs = [j for j in p.mj("job", "list", "lab")["jobs"] if j.get("send", {}).get("receiver") == "fwd"]
        if jobs and all(j["state"] in ENDED for j in jobs):
            break
        time.sleep(0.2)
    missing, byte_diff, elem_diff, _, listed = compare(p, orthanc, held)
    good = len(jobs) == 1 and jobs[0]["state"] == "JOB_STATE_DONE" and not missing and not byte_diff and not elem_diff
    p.result("4 a receiver with --forward-to sends each archived reception on", good,
             "%d forward job(s) made for the reception %s: %s; Orthanc holds %d of %d, data sets byte-equal: %d"
             % (len(jobs), jobs[0]["send"]["reception"] if jobs else "-", ", ".join("%s (%s)" % (j["state"], counts(j)) for j in jobs),
                len(held) - len(missing), len(held), len(held) - len(missing) - len(byte_diff)))
    return held, labels


def refusal(p, peer, held, labels):
    """5: instances a peer refuses are listed with its status, the job ends
    partial, and a retry after the peer is fixed sends only those."""
    uids = sorted(held)
    chosen = {uids[0]: 0xC000, uids[1]: 0xC000, uids[2]: 0x0122, uids[3]: 0xB000}
    peer.statuses = dict(chosen)
    peer.received = []
    j = p.wait_job(p.job("peer", "%s/%s" % labels))
    fails = p.mj("job", "failures", "lab", j["id"], "--warnings", "--all")["failures"]
    text = p.m("job", "failures", "lab", j["id"], "--warnings")
    print("     nopacs job failures --warnings:\n" + "\n".join("       " + line for line in text.rstrip().splitlines()), flush=True)
    by_status = sorted(json.dumps({k: v for k, v in f.items() if k in ("code", "status", "peerStatus")}, sort_keys=True) for f in fails)
    refused = int(j["counts"]["instancesRefused"])
    good = (j["state"] == "JOB_STATE_PARTIAL" and refused == 3 and int(j["counts"]["instancesWarned"]) == 1 and len(fails) == 4
            and "c000" in text.lower() and "0122" in text and "b000" in text.lower())
    p.result("5 instances the peer refuses are listed with its status and the job ends partial", good,
             "job %s (%s); the peer answered C000 twice, 0122 once, B000 once; failures listed: %d %s"
             % (j["state"], counts(j), len(fails), by_status), [json.dumps(f) for f in fails])
    peer.statuses = {}
    peer.received = []
    r = p.retry(j["id"])
    rj = p.wait_job(r)
    want = sorted(u for u, s in chosen.items() if s != 0xB000)
    good = rj["state"] == "JOB_STATE_DONE" and sorted(peer.received) == want
    p.result("5 after the peer is fixed, job retry sends exactly the refused instances", good,
             "retry job %s (%s); the peer received %d instances, the refused %d: %s"
             % (rj["state"], counts(rj), len(peer.received), len(want), "the same" if sorted(peer.received) == want else "differ"))


def cancel_retry(p, orthanc, held, labels):
    """6: a running job canceled, then retried: everything ends in Orthanc once."""
    orthanc.clear()
    before = orthanc.discarded()
    job = p.job("slow", "%s/%s" % labels)
    deadline = time.time() + 120
    while orthanc.count() < len(held) // 4 and time.time() < deadline:
        time.sleep(0.05)
    p.m("job", "cancel", "lab", job)
    j = p.wait_job(job, timeout=60)
    at_cancel = orthanc.count()
    time.sleep(3)
    settled = orthanc.count()
    rid = p.retry(job)
    rj = p.wait_job(rid, timeout=300)
    have, listed = orthanc.instances()
    missing = [u for u in held if u not in have]
    twice = None if before is None else orthanc.discarded() - before
    good = (j["state"] == "JOB_STATE_CANCELED" and settled - at_cancel <= MAX_BATCH and rj["state"] == "JOB_STATE_DONE"
            and not missing and listed == len(held) and (twice is None or twice <= MAX_BATCH))
    p.result("6 a canceled job stops, and its retry sends the rest: each instance once", good,
             "canceled: %s (%s), Orthanc had %d, %d more arrived after (within one batch); retry %s (%s); Orthanc holds %d of %d, "
             "lists %d; sent twice: %s" % (j["state"], counts(j), at_cancel, settled - at_cancel, rj["state"], counts(rj), len(have),
                                         len(held), listed, "not read" if twice is None else twice))


def deidentified(p, orthanc, port):
    """7: what a de-identifying receiver archives is sent as archived and
    carries no original name, identifier, date or UID."""
    doc = os.path.join(p.work, "basic.json")
    with open(doc, "w") as f:
        f.write('{"bases": ["annex-e/basic:2026d"]}')
    made = p.run("profile", "create", "lab", "basic", "--document", doc, "--note", "the basic profile")
    if made.returncode != 0:
        p.skip("7 de-identified data is sent as archived", "the profile could not be made (ProfileService not served?): exit %d: %s"
               % (made.returncode, made.stderr.strip()[:200]))
        return
    p.m("receiver", "add", "lab", "deid", "--storage", "main", "--port", str(port), "--aet", "ARCHIVE", "--quiet-period", QUIET,
        "--allow-aet", "STORESCU", "--profile", "basic@1", "--enabled")
    wait_up(p, "deid")

    def mark(ds):
        ds.PatientBirthDate = "19610723"
        ds.StudyDate = ds.SeriesDate = ds.ContentDate = "20240317"
        ds.AccessionNumber = "PRVACC77"
    # The five CINE MODE files of the CT study. The two scouts are kept out:
    # the basic profile removes their Series Description "Scout", and the
    # check for removed values found again quarantines them for the word in
    # Scan Options "SCOUT MODE", as a de-identification that fails closed does.
    sources = [os.path.join(sendkit.CORPUS, f) for f in CT if f.startswith("ct5n")]
    files, study = rewrite(sources, os.path.join(p.work, "studies", "deid"), "PRV-SEND-DEID-0001", "Proofington^Sendalina", extra=mark)
    if p.storescu(port, files) != len(files):
        raise Failure("storescu: the de-identifying receiver did not take every file")
    deadline = time.time() + 180
    held = {}
    while time.time() < deadline:
        studies = query(p.catalog(), "SELECT DISTINCT s.study_uid FROM scans s JOIN instances i ON i.scan_id = s.id "
                                             "WHERE i.profile_version_id IS NOT NULL")
        if studies:
            held, labels = archived(p.catalog(), p.storage(), studies[0]["study_uid"])
            if len(held) == len(files):
                break
        time.sleep(0.5)
    if len(held) != len(files):
        p.result("7 a de-identifying receiver archives the study", False, "%d of %d archived" % (len(held), len(files)))
        return
    # Every value of the originals that names the patient, a date or an
    # object: each name, date and UID, the patient's ID and the accession
    # number. A UID of the standard (a class or a syntax) is the same in
    # every file and is not looked for; a value under 6 characters could be
    # found by chance.
    originals = set()
    for f in files:
        for e in dcmread(f).iterall():
            named = e.VR in ("PN", "DA", "DT", "UI") or e.keyword in ("PatientID", "AccessionNumber")
            v = str(e.value) if e.value is not None else ""
            if named and len(v) >= 6 and not v.startswith("1.2.840.10008."):
                originals.add(v)
    orthanc.clear()
    j = p.wait_job(p.job("orthanc", "%s/%s" % labels))
    missing, byte_diff, elem_diff, _, _ = compare(p, orthanc, held)
    have, _ = orthanc.instances()
    found = []
    for oid in have.values():
        b = orthanc.file(oid)
        found += [v for v in originals if v.encode() in b]
    sample = read_bytes(orthanc.file(next(iter(have.values())))) if have else None
    good = j["state"] == "JOB_STATE_DONE" and not missing and not byte_diff and not found and len(have) == len(files)
    p.result("7 de-identified data is sent as archived and carries no original value", good,
             "job %s (%s); Orthanc holds %d, byte-equal to the archived: %d; %d original values (names, IDs, dates, UIDs) looked "
             "for in every file Orthanc holds: %d found; one file reads PatientName=%r PatientID=%r StudyDate=%r"
             % (j["state"], counts(j), len(have), len(have) - len(byte_diff), len(originals), len(found),
                str(sample.get("PatientName", "")) if sample else "-", sample.get("PatientID", "") if sample else "-",
                sample.get("StudyDate", "") if sample else "-"), ["found: %s" % sorted(set(found))[:8]])


def wait_up(p, receiver, timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        r = p.mj("receiver", "show", "lab", receiver)
        if r.get("receiver", r).get("health", {}).get("state") == "RECEIVER_STATE_UP":
            return
        time.sleep(0.1)
    raise Failure("the receiver %s did not come up" % receiver)


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--go", default="go")
    ap.add_argument("--dcmtk", default="")
    ap.add_argument("--orthanc", required=True, help="Orthanc's REST API, such as http://127.0.0.1:8042")
    ap.add_argument("--orthanc-dimse", required=True, help="Orthanc's DIMSE service, AET@HOST:PORT")
    ap.add_argument("--orthanc-logs", default="", help='a command that prints Orthanc\'s log, such as "docker logs orthanc"')
    ap.add_argument("--pydicom-corpus", default=os.environ.get("PYDICOM_TEST_FILES", ""),
                    help="pydicom's test files, for the files this repository's corpus lacks (default: the installed pydicom's)")
    ap.add_argument("--many", type=int, default=400)
    ap.add_argument("--postgres", default="", help="a Postgres server's address, postgres://USER:PASSWORD@HOST:PORT/DB, for a tenant on Postgres")
    ap.add_argument("--psql", default="", help='the command that runs psql against it, such as "docker exec -i CONTAINER psql -U postgres"')
    args = ap.parse_args(argv)
    if not args.pydicom_corpus:
        from pydicom.data import get_testdata_file
        args.pydicom_corpus = os.path.dirname(get_testdata_file("MR_small.dcm"))
    aet, _, hostport = args.orthanc_dimse.partition("@")
    o_host, _, o_port = hostport.rpartition(":")
    orthanc = Orthanc(args.orthanc, args.orthanc_logs)
    if orthanc.count() != 0:
        raise Failure("Orthanc at %s holds %d instances; the proof deletes every study in it, so it starts only on an empty one"
                         % (args.orthanc, orthanc.count()))
    p = Proof(args)
    peer = None
    try:
        build_binaries(args.go, p.bin, p.sbin)
        orthanc.verbose()
        scan_port, fwd_port, deid_port, peer_port, tls_port, later_port, dmg_port, pg_port, p.api_port = free_ports(9)
        rest = args.orthanc.split("://", 1)[1].split("/", 1)[0]
        rest_port = rest.rpartition(":")[2] or "80"
        p.m("init")
        p.serve()
        os.makedirs(p.roots)
        p.m("setting", "set", "storage.directory-roots", "fast=" + p.roots)
        p.m("tenant", "create", "lab")
        p.m("setting", "set", "receiver.ports", "%d,%d,%d" % (scan_port, fwd_port, deid_port), "--tenant", "lab")
        # A server started again (by p.serve) listens on the network too, for
        # the calls a key makes.
        p.end(signal.SIGTERM)
        p.serve()
        p.m("setting", "set", "receiver.allow-as-received", "true", "--tenant", "lab")
        p.m("storage", "add", "lab", "main", "--type", "dir", "--root", "fast", "--path", "main")
        local = "%d,%d,%d,%d" % (peer_port, tls_port, later_port, dmg_port)
        p.m("setting", "set", "sender.destinations", ("%s ports=%s,%s,%s" % (o_host, o_port, rest_port, local)) if o_host == "127.0.0.1"
            else "%s ports=%s,%s\n127.0.0.1 ports=%s" % (o_host, o_port, rest_port, local))
        p.m("setting", "set", "sender.allow-plain-http", "true")
        dimse = ["--type", "dimse", "--host", o_host, "--port", o_port, "--called-aet", aet, "--calling-aet", "NOPACS", "--enabled"]
        p.m("sender", "add", "lab", "orthanc", *dimse)
        p.m("sender", "add", "lab", "slow", *dimse, "--max-associations", "1", "--max-bytes-per-second", "64KiB")
        p.m("sender", "add", "lab", "web", "--type", "stow", "--url", args.orthanc.rstrip("/") + "/dicom-web",
            "--authentication", "none", "--enabled")
        for n in (1, 2, 4):
            p.m("sender", "add", "lab", "o%d" % n, *dimse, "--max-associations", str(n))
            p.m("sender", "add", "lab", "w%d" % n, "--type", "stow", "--url", args.orthanc.rstrip("/") + "/dicom-web",
                "--authentication", "none", "--max-associations", str(n), "--enabled")
        # A stow sender with a bearer token (Orthanc ignores it), whose
        # credential is searched for everywhere at the end.
        token = "prove-token-%d-not-a-real-secret" % os.getpid()
        token_file = os.path.join(p.work, "token")
        with open(os.open(token_file, os.O_WRONLY | os.O_CREAT, 0o600), "w") as f:
            f.write(token + "\n")
        p.m("sender", "add", "lab", "webauth", "--type", "stow", "--url", args.orthanc.rstrip("/") + "/dicom-web",
            "--authentication", "bearer", "--credential-file", token_file, "--enabled")
        p.m("sender", "add", "lab", "peer", "--type", "dimse", "--host", "127.0.0.1", "--port", str(peer_port),
            "--called-aet", "PEER", "--calling-aet", "NOPACS", "--enabled")
        for s in ("orthanc", "web"):
            print("     nopacs sender test lab %s: %s" % (s, p.m("sender", "test", "lab", s).strip().splitlines()[-1]), flush=True)
        p.m("receiver", "add", "lab", "scanner", "--storage", "main", "--port", str(scan_port), "--aet", "ARCHIVE",
            "--quiet-period", QUIET, "--allow-aet", "STORESCU", "--as-received", "--enabled")
        p.m("receiver", "add", "lab", "fwd", "--storage", "main", "--port", str(fwd_port), "--aet", "ARCHIVE",
            "--quiet-period", QUIET, "--allow-aet", "STORESCU", "--as-received", "--forward-to", "orthanc", "--enabled")
        wait_up(p, "scanner")
        wait_up(p, "fwd")
        peer = Peer(peer_port)

        whole_session(p, orthanc, scan_port)
        sending_numbers.bench(p, orthanc, scan_port)
        held, labels = restart(p, orthanc, scan_port)
        fwd_held, fwd_labels = forwarding(p, orthanc, fwd_port)
        refusal(p, peer, fwd_held, fwd_labels)
        cancel_retry(p, orthanc, held, labels)
        deidentified(p, orthanc, deid_port)
        orthanc.clear()
        sending_faults.over_tls(p, tls_port, fwd_held, fwd_labels)
        sending_faults.down_then_up(p, later_port, fwd_held, fwd_labels)
        sending_faults.damaged(p, dmg_port, scan_port)
        sending_faults.resources_then_retry(p, peer, fwd_held, fwd_labels)
        orthanc.clear()
        j = p.wait_job(p.job("webauth", "%s/%s" % fwd_labels))
        p.result("12 a stow sender with a bearer token sends", j["state"] == "JOB_STATE_DONE", "job %s (%s)" % (j["state"], counts(j)))
        sending_faults.secrets(p, token)
        sending_faults.refusals(p, p.api_port, fwd_labels)
        sending_faults.on_postgres(p, orthanc, pg_port, aet, o_host, o_port)
    finally:
        p.stop()
        if peer:
            peer.stop()
        try:
            orthanc.clear()
            orthanc.restore()
        except (OSError, RuntimeError) as e:
            print("Orthanc could not be cleared: %s" % e)
    print()
    print("The numbers, on %s, to Orthanc %s in %s:" % (machine(), orthanc.json("/system").get("Version", "?"), args.orthanc))
    for n in p.numbers:
        print("  " + n)
    print("%d ok, %d FAIL, %d skipped" % (p.counts["ok"], p.counts["FAIL"], p.counts["skip"]))
    if p.counts["FAIL"]:
        print("the work directory is kept: %s" % p.work)
        return 1
    subprocess.run(["rm", "-rf", p.work])

