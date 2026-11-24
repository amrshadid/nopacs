"""The follow-a-job example against a real server, a real archived study and a real peer.

The study is archived with dcmtk's storescu, and sent on to dcmtk's storescp by a
job the example makes and follows; the files the peer wrote are compared, data
set for data set, with the files the archive holds. Everything is a public
corpus file: no patient data.
"""

import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import threading
import time

import pytest

import nopacs
from archive import CORPUS, STUDY, find_storescu
from conftest import REPO, Server, free_port
from nopacs.v1 import catalog_pb2, job_pb2

sys.path.insert(0, str(REPO / "clients" / "python" / "examples"))
import follow_job as example  # noqa: E402

TENANT = "sending"
STREAM_DEADLINE = 2


def find_storescp() -> str:
    """Answer the path of dcmtk's storescp, or skip the test that needs it."""
    candidates = []
    if os.environ.get("NOPACS_DCMTK"):
        candidates.append(pathlib.Path(os.environ["NOPACS_DCMTK"]) / "storescp")
    if shutil.which("storescp"):
        candidates.append(pathlib.Path(shutil.which("storescp")))
    for path in candidates:
        done = subprocess.run([str(path), "--version"], capture_output=True, text=True, timeout=60)
        if "dcmtk" in (done.stdout + done.stderr).lower():
            return str(path)
    pytest.skip("dcmtk's storescp is not installed; put it on the PATH or set NOPACS_DCMTK")


def data_set_of(path: pathlib.Path) -> bytes:
    """The data set of a DICOM file: what follows the preamble and the file meta group (PS3.10 7.1)."""
    raw = path.read_bytes()
    assert raw[128:132] == b"DICM"
    # The first element of the file meta group is its length, (0002,0000) UL.
    meta = int.from_bytes(raw[140:144], "little")
    return raw[132 + 12 + meta:]


@pytest.fixture(scope="module")
def site(server, request):
    """A server with the study archived, a peer that stores what it is sent, and a sender to it."""
    storescu, storescp = find_storescu(), find_storescp()
    work = pathlib.Path(tempfile.mkdtemp(prefix="nopacs-sd."))
    # Registered before anything is started, so a setup that fails part way still stops what it started.
    request.addfinalizer(lambda: shutil.rmtree(work, ignore_errors=True))
    # The server ends a stream whose call set no deadline after STREAM_DEADLINE
    # seconds instead of the contract's 30, as it ends every stream at its
    # deadline, so a job followed for longer crosses several streams in seconds.
    srv = Server(server.binary, work, ("--default-deadline", f"{STREAM_DEADLINE}s"))
    request.addfinalizer(srv.stop)
    roots, received = work / "roots", work / "peer"
    roots.mkdir()
    received.mkdir()
    cli = srv.cli
    cli("setting", "set", "storage.directory-roots", f"fast={roots}")
    try:
        # A receiver that archives as received needs the installation to allow it, on a server that asks.
        cli("setting", "set", "receiver.allow-as-received", "true")
    except AssertionError:
        pass
    cli("tenant", "create", TENANT)
    cli("grant", "set", srv.key.split("_")[1], "--role", "operator", "--tenant", TENANT)
    port, peer_port = free_port(), free_port()
    cli("setting", "set", "receiver.ports", str(port), "--tenant", TENANT)
    cli("setting", "set", "sender.destinations", f"localhost ports={peer_port} private-address")
    cli("storage", "add", TENANT, "main", "--type", "dir", "--root", "fast")
    cli("receiver", "add", TENANT, "scanner", "--storage", "main", "--port", str(port), "--aet", "ARCHIVE",
        "--allow-aet", "STORESCU", "--quiet-period", "5s", "--enabled")
    args = [storescu, "-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", str(port), *[str(CORPUS / f) for f in STUDY]]
    for _ in range(20):
        done = subprocess.run(args, capture_output=True, text=True, timeout=120)
        if done.returncode == 0:
            break
        time.sleep(0.5)
    else:
        raise RuntimeError(f"storescu: {done.stdout}{done.stderr}")
    peer = subprocess.Popen([storescp, "-aet", "PEER", "-od", str(received), "--write-dataset", str(peer_port)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    request.addfinalizer(peer.wait)
    request.addfinalizer(peer.terminate)
    cli("sender", "add", TENANT, "peer", "--type", "dimse", "--host", "localhost", "--port", str(peer_port),
        "--called-aet", "PEER", "--calling-aet", "NOPACS", "--enabled")
    with nopacs.Client(f"127.0.0.1:{srv.port}", srv.key, tls=False) as c:
        subject = None
        for _ in range(300):
            subjects = list(c.pages(c.catalog.ListSubjects, catalog_pb2.ListSubjectsRequest(tenant=TENANT), "subjects"))
            if sum(s.instance_count for s in subjects) == len(STUDY):
                subject = subjects[0].label
                break
            time.sleep(0.1)
        assert subject, "the study was not archived in 30 seconds"
    return {"server": srv, "subject": subject, "received": received, "stored": roots / TENANT / "data"}


def test_the_example_follows_a_job_to_its_end_and_the_peer_holds_what_was_archived(site):
    srv = site["server"]
    with nopacs.Client(f"127.0.0.1:{srv.port}", srv.key, tls=False) as c:
        updates = list(example.follow_job(c, TENANT, "peer", site["subject"], deadline=60))
    assert updates[0].state == job_pb2.JOB_STATE_QUEUED, "the job is shown as made"
    last = updates[-1]
    assert last.state == job_pb2.JOB_STATE_DONE
    assert last.counts.instances == len(STUDY) and last.counts.instances_sent == len(STUDY)
    assert last.counts.instances_refused == 0 and last.counts.instances_remaining == 0
    assert all(u.id == updates[0].id for u in updates), "the events of another job are passed over"
    assert [u.state for u in updates].count(job_pb2.JOB_STATE_DONE) == 1, "it stops at the end"

    # The peer wrote one data set for each instance, byte for byte the archived one.
    sent = {p.name.split(".", 1)[1]: p for p in site["received"].iterdir()}
    stored = {p.name.removesuffix(".dcm"): p for p in site["stored"].rglob("*.dcm")}
    assert len(sent) == len(STUDY) and sent.keys() == stored.keys()
    for uid, peer_file in sent.items():
        assert peer_file.read_bytes() == data_set_of(stored[uid])


def test_the_example_follows_a_job_for_longer_than_the_streams_deadline(site):
    """The server ends each stream as UNAVAILABLE when its deadline passes; the example follows on."""
    srv = site["server"]
    srv.cli("sender", "set", TENANT, "peer", "--enabled=false")
    # The job waits, queued, while its sender is stopped; it is started again
    # after several stream deadlines have passed.
    enable = threading.Timer(3 * STREAM_DEADLINE, srv.cli, ("sender", "set", TENANT, "peer", "--enabled"))
    started = time.monotonic()
    enable.start()
    try:
        with nopacs.Client(f"127.0.0.1:{srv.port}", srv.key, tls=False) as c:
            updates = list(example.follow_job(c, TENANT, "peer", site["subject"]))
    finally:
        enable.join()
    assert time.monotonic() - started > 3 * STREAM_DEADLINE, "it followed the job across several streams"
    assert updates[0].state == job_pb2.JOB_STATE_QUEUED
    assert updates[-1].state == job_pb2.JOB_STATE_DONE
    assert updates[-1].counts.instances_sent == len(STUDY)


def test_the_example_run_from_the_command_line_prints_each_step_and_exits_zero(site):
    srv = site["server"]
    env = dict(os.environ, NOPACS_ADDRESS=f"127.0.0.1:{srv.port}", NOPACS_KEY=srv.key, NOPACS_INSECURE="1")
    done = subprocess.run([sys.executable, str(REPO / "clients" / "python" / "examples" / "follow_job.py"),
                           TENANT, "peer", site["subject"]], env=env, capture_output=True, text=True, timeout=120,
                          cwd=REPO / "clients" / "python")
    assert done.returncode == 0, done.stderr
    lines = done.stdout.splitlines()
    assert lines[0].split()[1] == "queued"
    assert lines[-1].split()[1] == "done" and "3 of 3 instances answered" in lines[-1]
    assert srv.key not in done.stdout + done.stderr


def test_the_example_refuses_a_target_that_is_not_one(site):
    done = subprocess.run([sys.executable, str(REPO / "clients" / "python" / "examples" / "follow_job.py"),
                           TENANT, "peer", "sub//scan"], capture_output=True, text=True, timeout=60,
                          cwd=REPO / "clients" / "python")
    assert done.returncode == 2 and "the target is" in done.stderr
