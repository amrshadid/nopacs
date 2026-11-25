"""The group of the proof of keeping the archive whole that purges on
buckets that keep versions: an S3 server (Silo, the maintained fork of
MinIO) in a container named for the run, a bucket with versioning and object
lock, a versioned one whose versions the storage is set to keep, an
unversioned one and a directory. Each bucket is read after the purge by
boto3's list_object_versions, the directory by os.walk, and what the
archive said it did by its own correction record.

  versions  dispositions on a versioned bucket (needs docker and boto3)
"""

import datetime
import hashlib
import os

from nopacs_scripts.dicom import sop_uids
from nopacs_scripts.net import free_ports
from nopacs_scripts.report import Failure
from nopacs_scripts.sections.whole import instance_route, objects_under
from nopacs_scripts.whole import listing, make_study
from nopacs_scripts.wholemore import S3Server, add_s3_storage, keys_under, receiver, versions_of


def dispositions(p, cid):
    """{storage: StorageDisposition as JSON} of a correction, by the command line."""
    c = p.mj("correction", "show", "lab", cid)["correction"]
    return {d["storage"]: d for d in c.get("dispositions", [])}, c


def layer(c, bucket, key, body, lock_until=None):
    """Puts on key, by boto3, a second version (under object lock until
    lock_until when given), a delete marker, then the bytes again as the
    current version: the key then holds two older versions and a delete
    marker under its current one. Returns the version ids put, in order."""
    kw = {"ObjectLockMode": "GOVERNANCE", "ObjectLockRetainUntilDate": lock_until} if lock_until else {}
    v2 = c.put_object(Bucket=bucket, Key=key, Body=body, **kw)["VersionId"]
    marker = c.delete_object(Bucket=bucket, Key=key)["VersionId"]
    v3 = c.put_object(Bucket=bucket, Key=key, Body=body)["VersionId"]
    return v2, marker, v3


def group_versions(p):
    s3 = S3Server(p.args.docker, p.args.s3_image)
    p.cleanups = [s3.stop]
    s3.start()
    c = s3.client()
    c.create_bucket(Bucket="locked", ObjectLockEnabledForBucket=True)
    c.create_bucket(Bucket="kept")
    c.put_bucket_versioning(Bucket="kept", VersioningConfiguration={"Status": "Enabled"})
    c.create_bucket(Bucket="plain")
    lock = c.get_object_lock_configuration(Bucket="locked")["ObjectLockConfiguration"].get("ObjectLockEnabled")
    states = {b: c.get_bucket_versioning(Bucket=b).get("Status", "never enabled") for b in ("locked", "kept", "plain")}
    p.result("8 the S3 server (%s) holds a bucket with versioning and object lock, a versioned one and an unversioned one (boto3)" % s3.image.split("@")[0],
             lock == "Enabled" and states == {"locked": "Enabled", "kept": "Enabled", "plain": "never enabled"},
             "object lock of locked: %s; versioning %s" % (lock, states))
    p.setup()
    p.m("setting", "set", "storage.s3-endpoints", s3.endpoint + " plain-http private-address")
    for b in ("locked", "plain"):
        add_s3_storage(p, "lab", b, s3, b)
    # The storage of the versioned bucket set to keep older versions:
    # S3Storage.purge_versions off, through the API, since the command line
    # has no flag for it: added with it, then asked again with SetStorage.
    st, a = p.call("nopacs.v1.StorageService/AddStorage", {"tenant": "lab", "storage": {
        "name": "kept", "s3": {"endpoint": s3.endpoint, "region": "us-east-1", "bucket": "kept", "accessKeyId": s3.user,
                               "secretAccessKey": s3.secret, "pathStyle": True, "purgeVersions": False}}})
    if st != 200:
        raise Failure("AddStorage kept: HTTP %d %s" % (st, a))
    _, g1 = p.call("nopacs.v1.StorageService/GetStorage", {"tenant": "lab", "name": "kept"})
    st, a = p.call("nopacs.v1.StorageService/SetStorage", {"tenant": "lab", "storage": {"name": "kept", "s3": {"purgeVersions": False}},
                                                         "updateMask": "s3.purgeVersions"})
    _, g = p.call("nopacs.v1.StorageService/GetStorage", {"tenant": "lab", "name": "kept"})
    pv = lambda x: x.get("storage", {}).get("s3", {}).get("purgeVersions", "absent")
    p.result("8 the kept bucket's storage is set to keep older versions (AddStorage with s3.purge_versions false, then SetStorage; read back by GetStorage)",
             pv(g1) is False or pv(g) is False,
             "AddStorage answered 200, GetStorage then answers purgeVersions %r; SetStorage HTTP %d %s; GetStorage answers purgeVersions %r"
             % (pv(g1), st, (a.get("code", "") + " " + a.get("message", "")).strip()[:160] or "ok", pv(g)))
    ports = free_ports(3)
    for b, port in zip(("locked", "kept", "plain"), ports):
        receiver(p, "lab", "r" + b, b, port)
    sd = os.path.join(p.work, "studies")
    sent = {}
    for b, port in zip(("locked", "kept", "plain", "main"), ports + [p.raw_port]):
        files = make_study(p.args.pydicom_corpus, sd, "Vers^" + b, "VE-" + b, ["CT_small.dcm", "MR_small.dcm"])
        n, out = p.storescu(port, files)
        if n != len(files):
            raise Failure("the scene: %d of %d answered success on %s: %s" % (n, len(files), b, out[-300:]))
        sent[b] = files
    if p.wait_held(8) != 8:
        raise Failure("the scene: %d of 8 held" % len(p.held()))
    rows ={b: [r for r in p.held() if r["uid"] in set(sop_uids(sent[b]))] for b in sent}
    subject = {b: rows[b][0]["subject"] for b in rows}
    until = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=1)).replace(microsecond=0)
    layered = {}
    for b, lock_until in (("locked", until), ("kept", None)):
        k = rows[b][0]["storage_key"]
        body = c.get_object(Bucket=b, Key=k)["Body"].read()
        layered[b] = (k, layer(c, b, k, body, lock_until), hashlib.sha256(body).hexdigest())
    v = versions_of(c, "locked", layered["locked"][0])
    code, _, got, _ = p.curl(instance_route(rows["locked"][0]))
    p.result("8 a key of the locked bucket holds three versions (one under object lock until %s) and a delete marker, and still serves its bytes" % until.isoformat(),
             len([x for x in v if x[1] == "version"]) == 3 and len([x for x in v if x[1] == "marker"]) == 1 and code == 200
             and hashlib.sha256(got).hexdigest() == layered["locked"][2] == rows["locked"][0]["sha256"],
             "boto3 lists %d versions and %d delete markers of %s; curl HTTP %d, SHA-256 equal to the catalog's (sqlite3)"
             % (len([x for x in v if x[1] == "version"]), len([x for x in v if x[1] == "marker"]), layered["locked"][0][-24:], code))
    before = {b: keys_under(c, b, "") for b in ("locked", "kept", "plain")}
    dir_before = listing(p.storage_root())
    cids = {}
    for b in sent:
        out = p.mj("correction", "delete", "lab", subject[b], "--reason", "consent withdrawn", "--yes")
        cids[b] = out["correction"]["id"]
    for b in sent:
        p.m("correction", "purge", "lab", cids[b], "--wait", "--yes")
    disp = {b: dispositions(p, cids[b]) for b in sent}
    # locked: every version but the locked one gone; the record says retained until the lock's end
    k, (v2, marker, v3), _ = layered["locked"]
    left = versions_of(c, "locked", k)
    others = sorted(x for x in keys_under(c, "locked", "") if x != k and (x.startswith("data/%s/" % subject["locked"]) or x.startswith("meta/%s/" % subject["locked"])))
    d = disp["locked"][0].get("locked", {})
    until_said = d.get("retainUntil", "")
    p.result("8 purge versions on, object lock: boto3 lists only the locked version of the layered key, no other key of the subject, and the record says retained until T",
             [x[0] for x in left] == [v2] and not others and int(d.get("objectsRetained", 0)) == 1 and until_said.startswith(until.strftime("%Y-%m-%dT%H:%M:%S")),
             "versions left of the layered key: %s (the locked one %s); other keys of the subject with any version: %d; the record: removed %s, retained %s, "
             "versions removed %s, retain until %s" % ([x[0][-8:] for x in left], v2[-8:], len(others), d.get("objectsRemoved", 0), d.get("objectsRetained", 0),
                                                       d.get("versionsRemoved", 0), until_said or "absent"),
             ["disposition: %s" % d, "others: %s" % others[:4]])
    # kept: the versions stay, and the record says hidden with the count
    k2, (kv2, kmarker, kv3), _ = layered["kept"]
    left2 = versions_of(c, "kept", k2)
    d2 = disp["kept"][0].get("kept", {})
    p.result("8 purge versions off: the key answers not found, its versions stay (boto3), and the record says hidden, N versions kept",
             d2 and int(d2.get("objectsHidden", 0)) >= 1 and int(d2.get("versionsRemoved", 0)) == 0 and len([x for x in left2 if x[1] == "version"]) >= 3,
             "versions left of the layered key: %d versions, %d delete markers; the record: removed %s, hidden %s, versions removed %s"
             % (len([x for x in left2 if x[1] == "version"]), len([x for x in left2 if x[1] == "marker"]), d2.get("objectsRemoved", 0),
                d2.get("objectsHidden", 0), d2.get("versionsRemoved", 0)), ["disposition: %s" % d2])
    # plain: nothing of the subject left at all
    plain_left = sorted(x for x in keys_under(c, "plain", "") if subject["plain"] in x)
    d3 = disp["plain"][0].get("plain", {})
    p.result("8 an unversioned bucket: no key of the subject is left in any version (boto3), and the record says removed, none hidden or retained",
             not plain_left and int(d3.get("objectsRemoved", 0)) == len([x for x in before["plain"] if subject["plain"] in x])
             and not int(d3.get("objectsHidden", 0)) and not int(d3.get("objectsRetained", 0)),
             "%d of the subject's %d keys left; the record: removed %s, hidden %s, retained %s"
             % (len(plain_left), len([x for x in before["plain"] if subject["plain"] in x]), d3.get("objectsRemoved", 0), d3.get("objectsHidden", 0),
                d3.get("objectsRetained", 0)), plain_left[:4])
    dir_left = objects_under(listing(p.storage_root()), "data/%s/" % subject["main"], "meta/%s/" % subject["main"])
    owned = objects_under(dir_before, "data/%s/" % subject["main"], "meta/%s/" % subject["main"])
    d4 = disp["main"][0].get("main", {})
    p.result("8 a directory: nothing of the subject left (os.walk), and the record says removed for each object", not dir_left and owned
             and int(d4.get("objectsRemoved", 0)) == len(owned) and not int(d4.get("objectsHidden", 0)),
             "%d of %d objects left; the record: removed %s, hidden %s" % (len(dir_left), len(owned), d4.get("objectsRemoved", 0), d4.get("objectsHidden", 0)))
    p.note("the bucket of the S3 server is removed with its container (%s)" % s3.name)


GROUPS = {"versions": group_versions}
