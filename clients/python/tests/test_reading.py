"""The catalog, metadata and data services against a real server holding a real study."""

import hashlib
import json

import pytest

import nopacs
from archive import STUDY, TENANT, Archive, CORPUS
from nopacs.v1 import catalog_pb2, data_pb2, metadata_pb2


@pytest.fixture(scope="module")
def archive(server):
    """The study, sent to a server of its own and archived."""
    a = Archive(server.binary)
    yield a
    a.close()


@pytest.fixture
def reader(archive):
    """A client holding the installation admin key, which may read the tenant."""
    with nopacs.Client(f"127.0.0.1:{archive.server.port}", archive.server.key, tls=False) as c:
        archive.wait_archived(c)
        yield c


def scans_of(reader):
    """Walk the catalog from the subject to each scan: (subject, session, scan) messages."""
    out = []
    for sub in reader.pages(reader.catalog.ListSubjects, catalog_pb2.ListSubjectsRequest(tenant=TENANT), "subjects"):
        for ses in reader.pages(reader.catalog.ListSessions, catalog_pb2.ListSessionsRequest(tenant=TENANT, subject_label=sub.label), "sessions"):
            for scan in reader.pages(reader.catalog.ListScans, catalog_pb2.ListScansRequest(
                    tenant=TENANT, subject_label=sub.label, session_label=ses.label), "scans"):
                out.append((sub, ses, scan))
    return out


def test_the_catalog_lists_the_study_as_subject_session_scans(reader):
    """One subject, one session, two scans with the instance counts the study has."""
    walked = scans_of(reader)
    assert sorted(scan.instance_count for _, _, scan in walked) == [1, 2]
    sub, ses, _ = walked[0]
    assert sub.session_count == 1 and sub.instance_count == 3
    assert ses.scan_count == 2 and "CT" in ses.modalities
    page = reader.catalog.ListSubjects(catalog_pb2.ListSubjectsRequest(tenant=TENANT, page_size=1))
    assert len(page.subjects) == 1 and page.next_page_token == ""
    assert not page.HasField("total_count") or page.total_count == 1
    for _, _, scan in walked:
        assert scan.modality == "CT" and len(scan.manifest_sha256) == 64


def test_scan_metadata_is_dicom_json_that_pydicom_agrees_with(reader):
    """Each instance's header names the SOP Instance UID and modality its file holds."""
    pydicom = pytest.importorskip("pydicom")
    uids = {pydicom.dcmread(CORPUS / f, stop_before_pixels=True).SOPInstanceUID for f in STUDY}
    seen = set()
    for sub, ses, scan in scans_of(reader):
        res = reader.metadata.GetScanMetadata(metadata_pb2.GetScanMetadataRequest(
            tenant=TENANT, subject_label=sub.label, session_label=ses.label, scan_label=scan.label))
        assert res.manifest_sha256 == scan.manifest_sha256
        assert len(res.instances) == scan.instance_count
        for inst in res.instances:
            header = json.loads(inst.dicom_json)
            assert header["00080060"]["Value"] == ["CT"]
            assert header["00080018"]["Value"][0] == inst.sop_instance_uid
            seen.add(inst.sop_instance_uid)
    assert seen == uids


def collect(stream):
    """Read a ReadFiles stream into [(announcement, bytes)] and the scan markers."""
    files, markers, cur, chunks = [], [], None, []
    for msg in stream:
        part = msg.WhichOneof("part")
        if part == "scan":
            markers.append(msg.scan)
        elif part == "file":
            if cur is not None:
                files.append((cur, b"".join(chunks)))
            cur, chunks = msg.file, []
        else:
            assert msg.chunk.offset == sum(len(c) for c in chunks)
            chunks.append(msg.chunk.data)
    if cur is not None:
        files.append((cur, b"".join(chunks)))
    return files, markers


def test_read_files_streams_the_archived_bytes_checked_by_their_checksums(reader, archive):
    """Every file's SHA-256 is the announced one and the one of the file on the storage."""
    sub, _, _ = scans_of(reader)[0]
    files, markers = collect(reader.data.ReadFiles(data_pb2.ReadFilesRequest(tenant=TENANT, subject_label=sub.label)))
    assert len(files) == len(STUDY) and len(markers) == 2
    got = []
    for ann, body in files:
        assert len(body) == ann.length
        assert hashlib.sha256(body).hexdigest() == ann.sha256
        got.append(ann.sha256)
    # What the server streamed is what lies in the directory storage.
    on_disk = sorted(hashlib.sha256(f.read_bytes()).hexdigest() for f in archive.stored.rglob("*.dcm"))
    assert sorted(got) == on_disk
    assert {m.manifest_sha256 for m in markers} == {scan.manifest_sha256 for _, _, scan in scans_of(reader)}


def test_streamed_files_are_the_instances_that_were_sent(reader):
    """pydicom reads each streamed file, and its pixels are those of the file sent."""
    pydicom = pytest.importorskip("pydicom")
    import io

    sent = {}
    for f in STUDY:
        ds = pydicom.dcmread(CORPUS / f)
        sent[ds.SOPInstanceUID] = bytes(ds.PixelData)
    sub, _, _ = scans_of(reader)[0]
    files, _ = collect(reader.data.ReadFiles(data_pb2.ReadFilesRequest(tenant=TENANT, subject_label=sub.label)))
    got = {}
    for _, body in files:
        ds = pydicom.dcmread(io.BytesIO(body))
        got[ds.SOPInstanceUID] = bytes(ds.PixelData)
    assert got == sent


def test_a_stream_resumes_after_the_last_whole_instance_and_announces_alone(reader):
    """start_after continues where a stream stopped; announce_only sends no bytes."""
    sub, _, _ = scans_of(reader)[0]
    everything, _ = collect(reader.data.ReadFiles(data_pb2.ReadFilesRequest(tenant=TENANT, subject_label=sub.label)))
    first = everything[0][0]
    rest, _ = collect(reader.data.ReadFiles(data_pb2.ReadFilesRequest(
        tenant=TENANT, subject_label=sub.label,
        start_after=data_pb2.ReadPosition(session_label=first.session_label, scan_label=first.scan_label, instance_id=first.instance_id))))
    assert [a.instance_id for a, _ in rest] == [a.instance_id for a, _ in everything[1:]]
    only = list(reader.data.ReadFiles(data_pb2.ReadFilesRequest(tenant=TENANT, subject_label=sub.label, announce_only=True)))
    assert all(m.WhichOneof("part") != "chunk" for m in only)
    assert sum(m.WhichOneof("part") == "file" for m in only) == len(STUDY)


def test_a_scan_that_does_not_exist_is_not_found(reader):
    """The error is a nopacs.Error with NOT_FOUND."""
    import grpc

    with pytest.raises(nopacs.Error) as caught:
        reader.catalog.GetScan(catalog_pb2.GetScanRequest(tenant=TENANT, subject_label="nope", session_label="nope", scan_label="nope"))
    assert caught.value.code == grpc.StatusCode.NOT_FOUND
