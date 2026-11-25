"""The group of the proof of keeping the archive whole that checks what a
cold tier and a long retention will rely on: an object the store answers as
held in a tier it cannot read at once (InvalidObjectState, as S3 answers a
GET of an archived object), a grace period changed after a delete, and two
data sets whose file dates lie (one shifted by the profile, one ten years
back) purged by the server's clock alone.

The cold object is made by the archive-tier proxy (nopacs_scripts.tools.s3_stub)
in front of the S3 server of the versions group (Silo in a container named for
the run): it passes every request on, but answers a GET of one key as S3
answers one of an object in an archive class
(docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html,
InvalidObjectState, 403).

  room  not online, a grace changed after a delete, one clock (needs docker)
"""

import datetime
import os
import time

from pydicom import dcmread

from nopacs_scripts.net import free_port
from nopacs_scripts.report import Failure
from nopacs_scripts.sections.whole import instance_route, tallies
from nopacs_scripts.tools.s3_stub import ArchiveProxy
from nopacs_scripts.whole import make_study
from nopacs_scripts.wholemore import S3Server, SECRET_ENV, receiver


def purge_time(p, cid):
    c = p.mj("correction", "show", "lab", cid)["correction"]
    return c.get("purgeTime", ""), c


def ts(text):
    return datetime.datetime.fromisoformat(text.replace("Z", "+00:00"))


def group_room(p):
    p.setup()
    p.m("setting", "set", "scrub.interval", "0", "--tenant", "lab")
    # ---- an object not online ----
    s3 = S3Server(p.args.docker, p.args.s3_image)
    p.cleanups = [s3.stop]
    s3.start()
    s3.client().create_bucket(Bucket="cold")
    cold_port = free_port()
    proxy = ArchiveProxy(s3.port)
    p.cleanups.append(proxy.stop)
    endpoint = proxy.endpoint
    p.m("setting", "set", "storage.s3-endpoints", endpoint + " plain-http private-address")
    os.environ[SECRET_ENV] = s3.secret
    p.m("storage", "add", "lab", "cold", "--type", "s3", "--endpoint", endpoint, "--region", "us-east-1", "--bucket", "cold",
        "--access-key-id", s3.user, "--credential-env", SECRET_ENV, "--path-style")
    receiver(p, "lab", "rcold", "cold", cold_port)
    files = make_study(p.args.pydicom_corpus, os.path.join(p.work, "studies"), "Cold^Eight", "CO-8", ["CT_small.dcm", "MR_small.dcm"])
    n, _ = p.storescu(cold_port, files)
    if n != 2 or p.wait_held(2) != 2:
        raise Failure("the cold scene: %d of 2 answered success, %d held" % (n, len(p.held())))
    rows = p.held()
    target, other = rows[0], rows[1]
    proxy.archive("/cold/" + target["storage_key"])
    code, _, body, _ = p.curl(instance_route(target))
    code2, _, body2, _ = p.curl(instance_route(other))
    p.result("17 an object the store answers InvalidObjectState: the read answers unavailable, not_online, and the other instance is served (curl)",
             code == 503 and b"not_online" in body and code2 == 200, "the cold one HTTP %d %s; the other HTTP %d; the proxy refused %d GETs"
             % (code, body.decode("latin-1"), code2, proxy.refused))
    st, a = p.call("nopacs.v1.JobService/CreateJob", {"tenant": "lab", "scrub": {"storage": "cold"}})
    job, deadline = a.get("job", {}), datetime.datetime.now() + datetime.timedelta(seconds=90)
    while datetime.datetime.now() < deadline and job.get("state") not in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL", "JOB_STATE_FAILED", "JOB_STATE_CANCELED"):
        time.sleep(0.5)
        _, a = p.call("nopacs.v1.JobService/GetJob", {"tenant": "lab", "id": job.get("id", "")})
        job = a.get("job", job)
    t = tallies(job)
    found = p.findings()
    p.result("17 a scrub tallies it not online and makes no finding of it", t.get("unreadable|not_online") == 1 and not found,
             "job %s after at most 90 s; tallies %s; findings %s; the proxy refused %d GETs; the job: %s"
             % (job.get("state", "").replace("JOB_STATE_", "").lower(), t, [(f["kind"], f["key"][-20:]) for f in found], proxy.refused,
                {k: v for k, v in job.items() if k in ("attempts", "lastError", "error", "nextAttemptTime", "counts")}))
    if job.get("state") not in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL", "JOB_STATE_FAILED", "JOB_STATE_CANCELED"):
        p.call("nopacs.v1.JobService/CancelJob", {"tenant": "lab", "id": job.get("id", "")})
    # ---- a grace period changed after a delete ----
    sd = os.path.join(p.work, "studies")
    a = make_study(p.args.pydicom_corpus, sd, "Grace^Nine", "GR-9", ["CT_small.dcm"])
    p.storescu(p.raw_port, a)
    p.wait_held(3)
    subj = [r for r in p.held() if r["uid"] == str(dcmread(a[0], stop_before_pixels=True).SOPInstanceUID)][0]["subject"]
    first = p.mj("correction", "delete", "lab", subj, "--reason", "consent withdrawn", "--yes")["correction"]
    t1, _ = purge_time(p, first["id"])
    p.m("setting", "set", "delete.grace", "1d", "--tenant", "lab")
    t2, shown = purge_time(p, first["id"])
    made = ts(shown.get("createTime") or shown.get("commitTime"))
    p.result("17 delete.grace changed after a delete keeps the purge time it fixed (correction show)", t1 and t1 == t2 and abs((ts(t1) - made).total_seconds() - 7 * 86400) < 5,
             "purge time %s before the change and %s after it; %.2f days after the delete was made" % (t1, t2, (ts(t2) - made).total_seconds() / 86400))
    # ---- one clock ----
    doc = os.path.join(p.work, "shift.json")
    with open(doc, "w") as f:
        f.write('{"bases": ["annex-e/basic:2026d"], "rules": [{"name": "shift-dates", "selector": {"keyword": "StudyDate"}, "action": {"shift": {}}, '
                '"reason": "keep the intervals, not the dates"}]}')
    p.m("profile", "create", "lab", "shift", "--document", doc, "--note", "dates shifted")
    p.m("receiver", "set", "lab", "deid", "--profile", "shift@1")
    shifted = make_study(p.args.pydicom_corpus, sd, "Clock^Ten", "CL-10", ["CT_small.dcm"])
    old = make_study(p.args.pydicom_corpus, sd, "Clock^Eleven", "CL-11", ["MR_small.dcm"])
    ds = dcmread(old[0])
    ds.StudyDate = ds.SeriesDate = ds.ContentDate = (datetime.date.today() - datetime.timedelta(days=3653)).strftime("%Y%m%d")
    ds.save_as(old[0], write_like_original=False)
    before = {r["uid"] for r in p.held()}
    p.storescu(p.deid_port, shifted)
    p.storescu(p.raw_port, old)
    p.wait_held(len(before) + 2)
    held = {r["uid"]: r for r in p.held()}
    u_old = str(dcmread(old[0], stop_before_pixels=True).SOPInstanceUID)
    new = [r for u, r in held.items() if u not in before and u != u_old]
    if len(new) != 1 or u_old not in held:
        raise Failure("the clock scene: held %s" % sorted((r["subject"], r["uid"][-6:]) for r in held.values()))
    archived = dcmread(os.path.join(p.storage_root(), new[0]["storage_key"]), stop_before_pixels=True)
    sent = dcmread(shifted[0], stop_before_pixels=True)
    cids = {}
    for name, r in (("shifted", new[0]), ("ten years back", held[u_old])):
        cids[name] = p.mj("correction", "delete", "lab", r["subject"], "--reason", "consent withdrawn", "--yes")["correction"]["id"]
    times = {k: purge_time(p, v) for k, v in cids.items()}
    spans = {k: (ts(t) - ts(c.get("createTime") or c.get("commitTime"))).total_seconds() / 86400 for k, (t, c) in times.items()}
    p.result("17 a study date shifted by the profile and one ten years back purge on one clock: each purge time is the delete's time plus the grace (correction show)",
             archived.StudyDate != sent.StudyDate and all(abs(v - 1) < 0.001 for v in spans.values())
             and abs((ts(times["shifted"][0]) - ts(times["ten years back"][0])).total_seconds()) < 60,
             "study dates as archived: %s (sent %s) and %s; purge times %s and %s, each %.4f and %.4f days after its delete"
             % (archived.StudyDate, sent.StudyDate, ds.StudyDate, times["shifted"][0], times["ten years back"][0], spans["shifted"], spans["ten years back"]))


GROUPS = {"room": group_room}
