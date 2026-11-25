"""The group of the proof of keeping the archive whole where a send job
meets the archive as it is now, not as it was when the job was made: 200
instances sent to a DICOM peer of pynetdicom, slowed so a person's
corrections land while it runs: a scan deleted before its unit begins, an
instance replaced in the unit being sent. What the peer received, by its own
count, against the job's failures and tallies.

  jobs  a 200-instance send meeting a delete and a replace
"""

import os
import time

from nopacs_scripts.dicom import sop_uids
from nopacs_scripts.interrupt import KeepingPeer, data_set
from nopacs_scripts.net import free_port
from nopacs_scripts.report import Failure
from nopacs_scripts.send import rewrite
from nopacs_scripts.sections.whole import tallies
from nopacs_scripts.sections.whole_refs import replace_by, corrected_copy
from nopacs_scripts.wholemore import failures, storescu, wait_job

SCANS, PER = 4, 50  # four series of fifty: 200 instances, four scans


def group_jobs(p):
    p.setup()
    p.m("setting", "set", "scrub.interval", "0", "--tenant", "lab")
    src = os.path.join(p.args.pydicom_corpus, "CT_small.dcm")
    files, _ = rewrite([src] * (SCANS * PER), os.path.join(p.work, "many"), "JOB-1", "Jobs^Many", series_of=lambda i: i // PER)
    n, out = storescu(p, p.raw_port, files)
    if n != len(files) or p.wait_held(len(files), timeout=150) != len(files):
        raise Failure("the scene: %d of %d answered success, %d held: %s" % (n, len(files), len(p.held()), out[-300:]))
    rows = {r["uid"]: r for r in p.held()}
    subject = next(iter(rows.values()))["subject"]
    by_file = dict(zip(sop_uids(files), files))
    peer = KeepingPeer(free_port())
    p.cleanups = [peer.stop]
    p.m("setting", "set", "sender.destinations", "127.0.0.1 ports=%d" % peer.port)
    # About 40 KB an instance at 128 KiB a second: a scan of fifty takes about fifteen seconds.
    p.m("sender", "add", "lab", "peer", "--type", "dimse", "--host", "127.0.0.1", "--port", str(peer.port), "--called-aet", "PEER",
        "--calling-aet", "NOPACS", "--max-associations", "1", "--max-bytes-per-second", "128KiB", "--enabled")
    jid = p.mj("job", "create", "lab", "--sender", "peer", subject)["job"]["id"]
    deadline = time.time() + 60
    while time.time() < deadline and not peer.bytes:
        time.sleep(0.1)
    if not peer.bytes:
        raise Failure("the peer received nothing in 60 s")
    first = next(iter(peer.bytes))
    current = rows[first]["scan"]
    scans = sorted({r["scan"] for r in rows.values()})
    # the scan to delete: one none of whose instances has reached the peer
    with peer.lock:
        got_scans = {rows[u]["scan"] for u in peer.bytes if u in rows}
    gone = [s for s in scans if s not in got_scans and s != current][-1]
    gone_uids = {u for u, r in rows.items() if r["scan"] == gone}
    dele = p.mj("correction", "delete", "lab", "%s/%s/%s" % (subject, rows[first]["session"], gone), "--reason", "consent withdrawn for one series",
                "--yes")["correction"]["id"]
    # the instance to replace: the last of the unit being sent, not yet received
    with peer.lock:
        pending = sorted((u for u, r in rows.items() if r["scan"] == current and u not in peer.bytes), key=lambda u: files.index(by_file[u]))
    if not pending:
        raise Failure("the unit %s ended before the replace could be made; slow the sender" % current)
    target = pending[-1]
    corr = replace_by(p, p.raw_port, corrected_copy(by_file[target], os.path.join(p.work, "replaced.dcm")))
    with peer.lock:
        before_commit = target in peer.bytes
    job = wait_job(p, "lab", jid, timeout=170)
    t = tallies(job)
    fails = failures(p, "lab", jid)
    received = set(peer.bytes)
    copies = sum(len(v) for v in peer.bytes.values())
    stored = {r["uid"]: os.path.join(p.storage_root(), r["storage_key"]) for r in p.held()}
    r2 = [c for c in peer.bytes.get(target, []) if target in stored and c == data_set(stored[target])]
    wrong = [u for u, cs in peer.bytes.items() if u != target and (u not in stored or any(c != data_set(stored[u]) for c in cs))]
    counts = job.get("counts", {})
    p.note("the job: %s; counts %s; tallies %s; %d failures %s" % (job["state"], counts, t, len(fails),
                                                                  sorted({(f.get("reason") or f.get("code") or str(f)[:60]) for f in fails})[:4]))
    p.result("15 the peer holds every instance but the deleted scan's, each once and each the stored bytes (pynetdicom's own record, compared with the stored object's data set)",
             received == set(rows) - gone_uids and copies == len(received) and not wrong,
             "%d of %d instances received (%d copies); none of the %d of the scan deleted before its unit; %d whose bytes differ from the stored object"
             % (len(received), len(rows) - len(gone_uids), copies, len(gone_uids & received) or len(gone_uids), len(wrong)),
             ["deleted and received: %s" % sorted(gone_uids & received)[:3], "missing: %s" % sorted(set(rows) - gone_uids - received)[:3]])
    name = "15 the instance replaced in the unit being sent reached the peer as its new revision"
    if before_commit:
        # The unit sent the instance before the replace committed: the old
        # revision was the current one when it was read, so sending it was
        # right, and this run cannot show the new revision going out. Said as
        # not shown rather than passed or failed; a strict run counts it.
        p.skip(name, "the unit sent the instance before the replace committed (%d copy), so this run shows the old revision going out, which is right then"
               % len(peer.bytes.get(target, [])))
    else:
        p.result(name, len(r2) == 1,
                 "copies equal to the .r2 data set: %d of %d (replace %s)"
                 % (len(r2), len(peer.bytes.get(target, [])), corr.get("id", "")[-8:]))
    # A job counts an instance deleted after it was made as refused, with a
    # failure DELETED_IN_ARCHIVE naming it (job.proto), and goes on.
    sent_said, refused = int(counts.get("instancesSent", 0)), int(counts.get("instancesRefused", 0))
    codes = {f.get("code", "") for f in fails}
    named = {v for f in fails for v in f.values() if isinstance(v, str) and v in rows}
    p.result("15 the job's counts and failures add up: sent, the peer's count, and refused, the deleted scan, make the 200 the job named",
             job["state"] in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL") and int(counts.get("instances", 0)) == SCANS * PER and sent_said == len(received)
             and refused == len(gone_uids) == len(fails) and codes == {"JOB_FAILURE_CODE_DELETED_IN_ARCHIVE"} and (not named or named == gone_uids),
             "state %s; instances %s = sent %d (the peer received %d) + refused %d (the scan held %d); %d failures, codes %s, naming %s; tallies %s; delete %s"
             % (job["state"].replace("JOB_STATE_", "").lower(), counts.get("instances"), sent_said, len(received), refused, len(gone_uids), len(fails),
                sorted(codes), "the deleted scan's instances" if named == gone_uids else "%d of them" % len(named), t or "none", dele[-8:]),
             ["job: %s" % {k: v for k, v in job.items() if k in ("counts", "tallies", "state")}, "a failure: %s" % (fails[:1],)])


GROUPS = {"jobs": group_jobs}
