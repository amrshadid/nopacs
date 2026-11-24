"""The rebuild group of the interruption harness (sections/interrupt.py): a
rebuild that replaces the catalog, killed at each of its steps with catalog
lists, QIDO-RS, downloads and a send to a DICOM peer running across the
kills; then a rebuild compare killed at each of its steps.
"""

import json
import os
import time

from nopacs_scripts.db import query
from nopacs_scripts.interrupt import (CUT, CURL_CUT, KeepingPeer, Loop, cut_out, data_set, list_subjects, qido_studies, setup, sha,
                                      studies, whole)
from nopacs_scripts.net import free_port
from nopacs_scripts.report import Failure
from nopacs_scripts.sections.whole import instance_route, row_of, tallies

REBUILD_STEPS = ["scan", "built", "activated", "dropped"]
COMPARE_STEPS = ["scan", "built", "compared", "dropped"]


def group_rebuild(p):
    setup(p)
    made = studies(p, "Rebuild", 5, ["CT_small.dcm"] * 5 + ["MR_small.dcm"])
    rows = [row_of(p, uids[0]) for _, uids in made]
    found, kept = rows[0], rows[1]
    new_subjects = sorted({r["subject"] for r in p.held()})
    old_subjects = sorted(set(new_subjects) - {found["subject"]})
    # A catalog that lacks one subject the storage holds whole: its metadata
    # is set aside, a rebuild replace (not interrupted) makes the catalog
    # without it, and the metadata is put back. The interrupted rebuild then
    # finds it, so the old and the new totals differ, and every answer of
    # the old catalog is well formed.
    meta, aside = os.path.join(p.storage_root(), "meta", found["subject"]), os.path.join(p.work, "aside-" + found["subject"])
    p.stop()
    os.rename(meta, aside)
    p.serve()
    first = p.job("lab", "rebuild", {"mode": "REBUILD_JOB_MODE_REPLACE"})
    p.stop()
    os.rename(aside, meta)
    p.serve()
    if first["state"] != "JOB_STATE_DONE" or sorted({r["subject"] for r in p.held()}) != old_subjects:
        raise Failure("the catalog without one subject could not be made: job %s, subjects %s" % (first["state"], sorted({r["subject"] for r in p.held()})))
    old_studies, new_studies = len(old_subjects), len(new_subjects)

    def subjects():
        a = list_subjects(p)
        if a is None or isinstance(a, str):
            return a
        labels, total = a
        return "old" if sorted(labels) == old_subjects and total == len(old_subjects) else \
            "new" if sorted(labels) == new_subjects and total == len(new_subjects) else ("mix", len(labels), total)

    def qido():
        n = qido_studies(p)
        return n if n is None or n == CUT else "old" if n == old_studies else "new" if n == new_studies else ("mix", n)

    def reads():
        g, k = whole(p, instance_route(found)), whole(p, instance_route(kept))
        if g is None or k is None:
            return None
        if CUT in (g, k) or g[3] in CURL_CUT or k[3] in CURL_CUT:
            return CUT
        ok_k = k[0] == 200 and sha(k[2]) == k[1].strip('"') == kept["sha256"]
        state = "new" if g[0] == 200 and sha(g[2]) == g[1].strip('"') == found["sha256"] else "old" if g[0] == 404 else ("bad", g[0])
        return state if ok_k else ("kept instance", k[0])

    # A send of each subject the old catalog holds to a DICOM peer of
    # pynetdicom, slowed to the least rate a sender allows (about 200 KB a
    # subject, three seconds each) so it runs across the kills.
    peer = KeepingPeer(free_port())
    try:
        p.m("setting", "set", "sender.destinations", "127.0.0.1 ports=%d" % peer.port)
        p.m("sender", "add", "lab", "peer", "--type", "dimse", "--host", "127.0.0.1", "--port", str(peer.port), "--called-aet", "PEER",
            "--calling-aet", "NOPACS", "--max-bytes-per-second", "64KiB", "--enabled")
        sends = [p.mj("job", "create", "lab", "--sender", "peer", s)["job"]["id"] for s in old_subjects]
        sent_uids = {r["uid"] for r in p.held() if r["subject"] in old_subjects}
        job, deaths, sends_state = rebuild_across_kills(p, subjects, qido, reads, sends)
    finally:
        peer.stop()
    for step, (killed, seen) in deaths:
        p.result("rebuild replace killed at %s: the server stopped there" % step, killed, seen)
    judge_rebuild_reads(p, *job)
    stored = {r["uid"]: os.path.join(p.storage_root(), r["storage_key"]) for r in p.held()}
    wrong = [u for u, copies in peer.bytes.items() if any(c != data_set(stored[u]) for c in copies)] if all(u in stored for u in peer.bytes) else ["unknown"]
    missing = sent_uids - set(peer.bytes)
    resent = sum(len(c) for c in peer.bytes.values()) - len(peer.bytes)
    p.result("a send to a DICOM peer (pynetdicom) running across the rebuild's kills ends done, every instance received, each copy the "
             "stored bytes", all(s == "JOB_STATE_DONE" for s in sends_state) and not missing and not wrong and resent > 0,
             "%d send jobs: %s; %d of %d instances received, %d copies, %d of them sent again after a kill (the send ran across one); "
             "data sets not equal to the stored object's: %d"
             % (len(sends), ", ".join(s.replace("JOB_STATE_", "").lower() for s in sends_state), len(sent_uids & set(peer.bytes)),
                len(sent_uids), sum(len(c) for c in peer.bytes.values()), resent, len(wrong)),
             ["missing %s" % sorted(missing)[:4], "wrong %s" % wrong[:4]])
    final_catalog(p, job[0], old_subjects, new_subjects)
    compare_across_kills(p)


def rebuild_across_kills(p, subjects, qido, reads, sends):
    """The rebuild replace made and killed at each step, with the readers
    running; returns ((job, loops), deaths, end states of the sends)."""
    job, deaths = None, []
    with Loop(subjects) as ls, Loop(qido) as qd, Loop(reads) as rd:
        time.sleep(0.5)
        for step in REBUILD_STEPS:
            p.arm("rebuild:replace:" + step)
            if job is None:
                st, a = p.call("nopacs.v1.JobService/CreateJob", {"tenant": "lab", "rebuild": {"mode": "REBUILD_JOB_MODE_REPLACE"}})
                if st != 200:
                    raise Failure("CreateJob rebuild replace: HTTP %d %s" % (st, a))
                job = a["job"]["id"]
            deaths.append((step, p.died()))
        p.restart()
        j = wait_job(p, job)
        time.sleep(1.0)
    return (j, (ls, qd, rd)), deaths, [wait_job(p, s).get("state", "") for s in sends]


def compare_across_kills(p):
    """A rebuild compare killed at each of its steps in turn, one job
    resumed after each restart: it ends done and finds the catalog the
    replace just made equal to the storages."""
    job, deaths = None, []
    for step in COMPARE_STEPS:
        p.arm("rebuild:compare:" + step)
        if job is None:
            st, a = p.call("nopacs.v1.JobService/CreateJob", {"tenant": "lab", "rebuild": {"mode": "REBUILD_JOB_MODE_COMPARE"}})
            if st != 200:
                raise Failure("CreateJob rebuild compare: HTTP %d %s" % (st, a))
            job = a["job"]["id"]
        deaths.append((step, p.died()))
    p.restart()
    j = wait_job(p, job)
    for step, (killed, seen) in deaths:
        p.result("rebuild compare killed at %s: the server stopped there" % step, killed, seen)
    t = tallies(j)
    differs = {k: v for k, v in t.items() if k.startswith("differs|")}
    p.result("rebuild compare across its kills: the job ends done and finds no difference", j.get("state") == "JOB_STATE_DONE" and not differs,
             "job %s; differences tallied: %s" % (j.get("state", "").replace("JOB_STATE_", "").lower(), differs or "none"))


def wait_job(p, jid, timeout=150):
    """Waits until a job ends; returns it as the API shows it."""
    deadline, j = time.time() + timeout, {}
    while time.time() < deadline:
        j = p.call("nopacs.v1.JobService/GetJob", {"tenant": "lab", "id": jid})[1].get("job", {})
        if j.get("state") in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL", "JOB_STATE_FAILED", "JOB_STATE_CANCELED"):
            break
        time.sleep(0.3)
    return j


def judge_rebuild_reads(p, j, loops):
    """One line per reader of the rebuild: every answer old or new, never a
    mix, never the old after the new, and answers on both sides of the kills."""
    ls, qd, rd = loops
    for label, loop in (("ListSubjects (labels and total_count)", ls), ("QIDO-RS studies", qd),
                        ("download of the subject the rebuild finds and of one it keeps", rd)):
        kept_answers, cut = cut_out(loop.answers)
        answers = [a for t, a in kept_answers]
        bad = [a for a in answers if a not in ("old", "new")]
        if cut > len(REBUILD_STEPS):
            bad.append("%d answers dropped by the server across %d kills" % (cut, len(REBUILD_STEPS)))
        first_new = answers.index("new") if "new" in answers else len(answers)
        back = [a for a in answers[first_new:] if a == "old"]
        p.result("rebuild replace across the kills: every %s answer counts the old catalog or the new, never a mix or the old after the new" % label,
                 not bad and not back and answers.count("old") > 0 and answers.count("new") > 0 and loop.away > 0,
                 "%d answers: %d old, then %d new; %d while the server was down, %d dropped by a kill; %d old after the first new"
                 % (len(answers), answers.count("old"), answers.count("new"), loop.away, cut, len(back)), [str(a) for a in bad][:12])


def final_catalog(p, j, old_subjects, new_subjects):
    """The rebuild's end: the job done, the catalog the manifests' (json)."""
    held_subjects = sorted({r["subject"] for r in p.held()})
    in_manifests = set()
    for d, _, names in os.walk(os.path.join(p.storage_root(), "meta")):
        if "manifest.json" in names:
            with open(os.path.join(d, "manifest.json")) as f:
                in_manifests |= {e["sop_instance_uid"] for e in json.load(f)["files"]}
    held = {r["uid"] for r in p.held()}
    totals = list(query(p.catalog(), "SELECT subjects, sessions, scans, instances FROM catalog_totals")[0])
    counted = [query(p.catalog(), "SELECT count(*) AS n FROM %s" % t)[0]["n"] for t in ("subjects", "sessions", "scans", "instances")]
    p.result("rebuild replace: the job ended done and the catalog in use is the one the storage's manifests hold", j.get("state") == "JOB_STATE_DONE"
             and held_subjects == new_subjects and held == in_manifests and totals == counted,
             "job %s; subjects %d (was %d); %d instances held, %d in the manifests (json); totals %s, counted %s (sqlite3)"
             % (j.get("state", "").replace("JOB_STATE_", "").lower(), len(held_subjects), len(old_subjects), len(held), len(in_manifests),
                totals, counted))
