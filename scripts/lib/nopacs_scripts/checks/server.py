"""The server checked by Python's grpcio, a gRPC implementation that is not Go.

Usage: server.py PROTO_DIR SOCKET TLS_ADDRESS CA_FILE NO_GRANT_KEY ADMIN_KEY METRICS_FILE

It compiles the contract with grpcio-tools into a scratch directory, then
calls the server run by scripts/check/server.sh:

  - over TLS: ListKeys with no key, a malformed key, an unknown key and a key
    with no grant, each of which must be refused with the code and message
    the contract fixes, and the same call with an admin key, which must
    succeed; every answer must carry a Nopacs-Request-Id;
  - over the local socket (unix: address): WhoAmI as the local operator,
    which must succeed, and with an unknown key, which must be refused;
  - the standard health service over TLS without a key, as raw bytes (an
    empty HealthCheckRequest; SERVING is field 1 = 1), so no health package
    is needed;
  - the text in METRICS_FILE (a scrape of /metrics) through the Prometheus
    Python client's parser;
  - when NOPACS_CHECK_TENANTS is 1 (the real program, not the stand-in):
    CreateTenant over TLS with no key and with a key with no grant, each
    refused, then with the admin key, which must make the tenant ready, and
    ListTenants, which must list it to the admin key and nothing to the key
    with no grant; then the first and the third use of
    docs/api-walkthrough-reception.md, call by call, on that tenant (see
    check_reception), with the directory root NOPACS_CHECK_ROOT names,
    which the installation's setting storage.directory-roots lists as
    "fast".

It prints one line per check and exits 1 on the first failure.
"""

import importlib
import os
import sys
import tempfile

import grpc
from grpc_tools import protoc
from prometheus_client.parser import text_string_to_metric_families

from nopacs_scripts.report import failing

UNAUTHENTICATED = "the request carries no valid key"
DENIED = "the key may not do this"
UNKNOWN_KEY = "nopacs_111111111111_1111111111111111111111111111111111111111111"


fail = failing("FAIL: ", stdout=True)


def compile_contract(proto_dir, out_dir):
    files = [os.path.join("nopacs", "v1", f) for f in sorted(os.listdir(os.path.join(proto_dir, "nopacs", "v1")))]
    import grpc_tools

    include = os.path.join(os.path.dirname(grpc_tools.__file__), "_proto")
    args = ["protoc", "-I" + proto_dir, "-I" + include, "--python_out=" + out_dir, "--grpc_python_out=" + out_dir] + files
    if protoc.main(args) != 0:
        fail("grpcio-tools could not compile the contract")
    sys.path.insert(0, out_dir)


def call(stub_method, request, key=None):
    metadata = [("authorization", "Bearer " + key)] if key else []
    try:
        _, rpc = stub_method.with_call(request, metadata=metadata, timeout=10)
        return grpc.StatusCode.OK, "", dict(rpc.initial_metadata())
    except grpc.RpcError as e:
        md = dict(e.initial_metadata() or []) | dict(e.trailing_metadata() or [])
        return e.code(), e.details(), md


def expect(name, got, code, message):
    status, details, md = got
    if status != code or (message is not None and details != message):
        fail(f"{name}: got {status} {details!r}, want {code} {message!r}")
    if not md.get("nopacs-request-id"):
        fail(f"{name}: no nopacs-request-id")
    print(f"ok   {name}: {status.name} {details!r} request-id={md['nopacs-request-id']}")


def check_tenants(tls_addr, creds, no_grant, admin):
    tenant_pb2 = importlib.import_module("nopacs.v1.tenant_pb2")
    tenant_grpc = importlib.import_module("nopacs.v1.tenant_pb2_grpc")
    channel = grpc.secure_channel(tls_addr, creds)
    stub = tenant_grpc.TenantServiceStub(channel)
    create = tenant_pb2.CreateTenantRequest(name="py1", database_type=tenant_pb2.DATABASE_TYPE_SQLITE)
    expect("tls CreateTenant with no key", call(stub.CreateTenant, create), grpc.StatusCode.UNAUTHENTICATED, UNAUTHENTICATED)
    expect("tls CreateTenant with a key with no grant", call(stub.CreateTenant, create, no_grant), grpc.StatusCode.PERMISSION_DENIED, DENIED)
    try:
        made = stub.CreateTenant(create, metadata=[("authorization", "Bearer " + admin)], timeout=10)
    except grpc.RpcError as e:
        fail(f"tls CreateTenant with the admin key: {e.code()} {e.details()!r}")
    if made.tenant.state != tenant_pb2.TENANT_STATE_READY:
        fail(f"tls CreateTenant with the admin key: {made}")
    print("ok   tls CreateTenant with an admin key: py1 ready")
    listed = stub.ListTenants(tenant_pb2.ListTenantsRequest(), metadata=[("authorization", "Bearer " + admin)], timeout=10)
    if "py1" not in [t.name for t in listed.tenants]:
        fail(f"tls ListTenants with the admin key: {listed}")
    print(f"ok   tls ListTenants with an admin key: {[t.name for t in listed.tenants]}")
    none = stub.ListTenants(tenant_pb2.ListTenantsRequest(), metadata=[("authorization", "Bearer " + no_grant)], timeout=10)
    if list(none.tenants):
        fail(f"tls ListTenants with a key with no grant: {none}")
    print("ok   tls ListTenants with a key with no grant: none")
    channel.close()


def ok(name, detail=""):
    print(f"ok   {name}{': ' + detail if detail else ''}")


def must(name, method, request, key=None):
    """Calls method and returns its answer, failing the check on an error."""
    metadata = [("authorization", "Bearer " + key)] if key else []
    try:
        return method(request, metadata=metadata, timeout=30)
    except grpc.RpcError as e:
        fail(f"{name}: {e.code()} {e.details()!r}")


def refused(name, method, request, key, code, message=None):
    """Fails the check unless method answers code, with message if given."""
    expect(name, call(method, request, key), code, message)


def check_reception(tls_addr, creds, socket, admin, root):
    """The first and third uses of the API walk-through, on the tenant py1.

    Use 1: the installation's admin allots ports to the tenant (its own
    admin is refused, and ports another tenant holds are a failed
    precondition); the tenant's admin adds a directory storage under the
    root and tests it, adds the receiver of the walk-through and reads the
    defaults it was given, changes its quiet period with a mask, and is
    refused a port outside the allotment, a whole type in a mask and a
    second storage on the same directory; the two are listed, then
    removed. Use 3, on a tenant with nothing quarantined (nothing can be
    without the reception pipeline): the held files are listed (none), a
    file is not found, and every refusal the walk-through names holds: the
    header and the UID filters for a reader, a release in no mode, a
    rejection with no reason, a release of a file that is not there.
    """
    pb = {n: importlib.import_module(f"nopacs.v1.{n}_pb2") for n in ("access", "scope", "settings", "storage", "receiver", "reception", "quarantine")}
    grpc_mod = {n: importlib.import_module(f"nopacs.v1.{n}_pb2_grpc") for n in ("access", "settings", "storage", "receiver", "reception", "quarantine", "tenant")}
    from google.protobuf import duration_pb2, field_mask_pb2

    local = grpc.insecure_channel("unix:" + socket)
    acc = grpc_mod["access"].AccessServiceStub(local)
    tenants = grpc_mod["tenant"].TenantServiceStub(local)
    tenant_pb2 = importlib.import_module("nopacs.v1.tenant_pb2")
    must("socket CreateTenant py2", tenants.CreateTenant, tenant_pb2.CreateTenantRequest(name="py2", database_type=tenant_pb2.DATABASE_TYPE_SQLITE))

    def key_on(tenant, role, holder):
        made = must("socket CreateKey " + holder, acc.CreateKey, pb["access"].CreateKeyRequest(holder=holder, never_expire=True))
        scope = pb["scope"].Scope(tenant=tenant)
        must("socket SetGrant " + holder, acc.SetGrant, pb["access"].SetGrantRequest(key_id=made.key.id, scope=scope, role=role))
        return made.secret

    lab_admin = key_on("py1", pb["access"].ROLE_ADMIN, "py1 admin")
    lab_reader = key_on("py1", pb["access"].ROLE_READER, "py1 reader")

    channel = grpc.secure_channel(tls_addr, creds)
    settings = grpc_mod["settings"].SettingsServiceStub(channel)
    storages = grpc_mod["storage"].StorageServiceStub(channel)
    receivers = grpc_mod["receiver"].ReceiverServiceStub(channel)
    receptions = grpc_mod["reception"].ReceptionServiceStub(channel)
    quarantine = grpc_mod["quarantine"].QuarantineServiceStub(channel)
    S, R, Q, E = pb["storage"], pb["receiver"], pb["quarantine"], pb["reception"]

    def setting(name, tenant, value):
        scope = pb["scope"].Scope(tenant=tenant) if tenant else pb["scope"].Scope(installation=pb["scope"].Installation())
        return pb["settings"].SetSettingRequest(setting=pb["settings"].Setting(name=name, scope=scope, value=value))

    # Use 1. What the installation allows: the root is set by the script;
    # the ports are the installation admin's to allot, on the tenant.
    ports = setting("receiver.ports", "py1", "11112,11120-11129")
    refused("use 1: receiver.ports by the tenant's own admin", settings.SetSetting, ports, lab_admin, grpc.StatusCode.PERMISSION_DENIED, DENIED)
    must("use 1: receiver.ports by the installation's admin", settings.SetSetting, ports, admin)
    ok("use 1: receiver.ports set on py1 by the installation's admin", "11112,11120-11129")
    must("use 1: receiver.allow-as-received by the installation's admin", settings.SetSetting, setting("receiver.allow-as-received", "py1", "true"), admin)
    refused("use 1: py2's ports over py1's", settings.SetSetting, setting("receiver.ports", "py2", "11125-11130"), admin, grpc.StatusCode.FAILED_PRECONDITION)
    refused("use 1: receiver.ports on the installation", settings.SetSetting, setting("receiver.ports", "", "11112"), admin, grpc.StatusCode.INVALID_ARGUMENT)

    # Add a storage and test it.
    main_storage = S.Storage(name="main", directory=S.DirectoryStorage(root="fast", path="mri"))
    added = must("use 1: AddStorage", storages.AddStorage, S.AddStorageRequest(tenant="py1", storage=main_storage), lab_admin)
    made = os.path.join(root, "py1", "mri")
    mode = os.stat(made).st_mode & 0o777
    if added.storage.directory.path != "mri" or mode != 0o700:
        fail(f"use 1: AddStorage: {added} with the directory's mode {oct(mode)}")
    ok("use 1: AddStorage main", f"root fast, path mri, the directory made with mode {oct(mode)}")
    refused("use 1: AddStorage on the same directory in other case", storages.AddStorage,
            S.AddStorageRequest(tenant="py1", storage=S.Storage(name="copy", directory=S.DirectoryStorage(root="fast", path="MRI"))), lab_admin, grpc.StatusCode.FAILED_PRECONDITION)
    refused("use 1: AddStorage by a reader", storages.AddStorage, S.AddStorageRequest(tenant="py1", storage=main_storage), lab_reader, grpc.StatusCode.PERMISSION_DENIED, DENIED)
    tested = must("use 1: TestStorage", storages.TestStorage, S.TestStorageRequest(tenant="py1", name="main"), lab_admin)
    steps = [(S.StorageTestStep.Name(r.step), S.StorageTestOutcome.Name(r.outcome)) for r in tested.results]
    if not tested.passed or [o for _, o in steps] != ["STORAGE_TEST_OUTCOME_PASSED"] * 4:
        fail(f"use 1: TestStorage: {tested}")
    ok("use 1: TestStorage", ", ".join(s.replace("STORAGE_TEST_STEP_", "") for s, _ in steps) + " passed")

    # Add a receiver.
    rule = R.CallerRule(calling_ae_title="PRISMA1", address_range="10.20.0.0/24")
    scanner = R.Receiver(name="scanner1", enabled=True, storage="main",
                         dimse=R.DimseReceiver(port=11112, ae_title="NOPACS", allowed_callers=R.AllowedCallers(rules=[rule])))
    got = must("use 1: AddReceiver", receivers.AddReceiver, R.AddReceiverRequest(tenant="py1", receiver=scanner), lab_admin).receiver
    if got.quiet_period.seconds != 60 or got.max_open_time.seconds != 3600 or got.max_file_bytes != 4294967296 or got.HasField("naming"):
        fail(f"use 1: AddReceiver defaults: {got}")
    ok("use 1: AddReceiver scanner1", f"quietPeriod 60s, maxOpenTime 3600s, maxFileBytes 4294967296, health {R.ReceiverState.Name(got.health.state)}")
    outside = R.Receiver()
    outside.CopyFrom(scanner)
    outside.name, outside.dimse.port = "scanner2", 104
    refused("use 1: a port outside receiver.ports", receivers.AddReceiver, R.AddReceiverRequest(tenant="py1", receiver=outside), lab_admin, grpc.StatusCode.FAILED_PRECONDITION)
    any_false = R.Receiver()
    any_false.CopyFrom(scanner)
    any_false.name, any_false.dimse.port = "scanner3", 11120
    any_false.dimse.any_caller = False
    refused("use 1: anyCaller false", receivers.AddReceiver, R.AddReceiverRequest(tenant="py1", receiver=any_false), lab_admin, grpc.StatusCode.INVALID_ARGUMENT)

    # Change the quiet period.
    change = R.SetReceiverRequest(tenant="py1", receiver=R.Receiver(name="scanner1", quiet_period=duration_pb2.Duration(seconds=300)),
                                  update_mask=field_mask_pb2.FieldMask(paths=["quiet_period"]))
    changed = must("use 1: SetReceiver quietPeriod", receivers.SetReceiver, change, lab_admin).receiver
    if changed.quiet_period.seconds != 300 or changed.dimse.port != 11112:
        fail(f"use 1: SetReceiver: {changed}")
    ok("use 1: SetReceiver with the mask quietPeriod", "300s, the port kept")
    whole = R.SetReceiverRequest(tenant="py1", receiver=R.Receiver(name="scanner1"), update_mask=field_mask_pb2.FieldMask(paths=["dimse"]))
    refused("use 1: a mask naming dimse whole", receivers.SetReceiver, whole, lab_admin, grpc.StatusCode.INVALID_ARGUMENT)
    star = R.SetReceiverRequest(tenant="py1", receiver=R.Receiver(name="scanner1"), update_mask=field_mask_pb2.FieldMask(paths=["*"]))
    refused("use 1: the mask *", receivers.SetReceiver, star, lab_admin, grpc.StatusCode.INVALID_ARGUMENT)

    listed = must("use 1: ListReceivers", receivers.ListReceivers, R.ListReceiversRequest(tenant="py1"), lab_reader)
    stored = must("use 1: ListStorages", storages.ListStorages, S.ListStoragesRequest(tenant="py1"), lab_reader)
    if [r.name for r in listed.receivers] != ["scanner1"] or [s.name for s in stored.storages] != ["main"]:
        fail(f"use 1: the lists: {listed} {stored}")
    ok("use 1: ListReceivers and ListStorages by a reader", "scanner1, main")
    refused("use 1: RemoveStorage while a receiver names it", storages.RemoveStorage, S.RemoveStorageRequest(tenant="py1", name="main"), lab_admin, grpc.StatusCode.FAILED_PRECONDITION)
    must("use 1: RemoveReceiver", receivers.RemoveReceiver, R.RemoveReceiverRequest(tenant="py1", name="scanner1"), lab_admin)
    refused("use 1: RemoveReceiver again", receivers.RemoveReceiver, R.RemoveReceiverRequest(tenant="py1", name="scanner1"), lab_admin, grpc.StatusCode.NOT_FOUND)
    must("use 1: RemoveStorage", storages.RemoveStorage, S.RemoveStorageRequest(tenant="py1", name="main"), lab_admin)
    ok("use 1: RemoveReceiver, then RemoveStorage", "a repeat is NOT_FOUND")

    # Use 3, on a tenant with nothing quarantined.
    held = must("use 3: ListQuarantinedFiles held", quarantine.ListQuarantinedFiles, Q.ListQuarantinedFilesRequest(tenant="py1", state=Q.QUARANTINE_STATE_HELD), lab_reader)
    if list(held.files):
        fail(f"use 3: held files: {held}")
    ok("use 3: ListQuarantinedFiles held, by a reader", "none")
    refused("use 3: GetQuarantinedFile of no file", quarantine.GetQuarantinedFile, Q.GetQuarantinedFileRequest(tenant="py1", id="q-19d2"), lab_reader, grpc.StatusCode.NOT_FOUND)
    refused("use 3: the header for a reader", quarantine.GetQuarantinedFile, Q.GetQuarantinedFileRequest(tenant="py1", id="q-19d2", include_header=True), lab_reader, grpc.StatusCode.PERMISSION_DENIED, DENIED)
    refused("use 3: a UID filter for a reader", quarantine.ListQuarantinedFiles, Q.ListQuarantinedFilesRequest(tenant="py1", sop_instance_uid="2.25.987654321"), lab_reader, grpc.StatusCode.PERMISSION_DENIED, DENIED)
    refused("use 3: a release in no mode", quarantine.ReleaseQuarantinedFile, Q.ReleaseQuarantinedFileRequest(tenant="py1", id="q-19d2"), lab_admin, grpc.StatusCode.INVALID_ARGUMENT)
    refused("use 3: a release of no file", quarantine.ReleaseQuarantinedFile, Q.ReleaseQuarantinedFileRequest(tenant="py1", id="q-19d2", mode=Q.RELEASE_MODE_AS_IS), lab_admin, grpc.StatusCode.NOT_FOUND)
    refused("use 3: a release by a reader", quarantine.ReleaseQuarantinedFile, Q.ReleaseQuarantinedFileRequest(tenant="py1", id="q-19d2", mode=Q.RELEASE_MODE_AS_IS), lab_reader, grpc.StatusCode.PERMISSION_DENIED, DENIED)
    refused("use 3: a rejection with no reason", quarantine.RejectQuarantinedFile, Q.RejectQuarantinedFileRequest(tenant="py1", id="q-19d2"), lab_admin, grpc.StatusCode.INVALID_ARGUMENT)
    refused("use 3: a reception by study for a reader", receptions.ListReceptions, E.ListReceptionsRequest(tenant="py1", study_instance_uid="2.25.1234567890123456789"), lab_reader, grpc.StatusCode.PERMISSION_DENIED, DENIED)
    none = must("use 3: ListReceptions", receptions.ListReceptions, E.ListReceptionsRequest(tenant="py1"), lab_reader)
    if list(none.receptions):
        fail(f"use 3: receptions: {none}")
    ok("use 3: ListReceptions by a reader", "none")
    channel.close()
    local.close()


def main(argv):
    if len(argv) != 7:
        fail(__doc__)
    proto_dir, socket, tls_addr, ca_file, no_grant, admin, metrics_file = argv
    out = tempfile.mkdtemp(prefix="nopacs-py-")
    compile_contract(proto_dir, out)
    access_pb2 = importlib.import_module("nopacs.v1.access_pb2")
    access_grpc = importlib.import_module("nopacs.v1.access_pb2_grpc")

    with open(ca_file, "rb") as f:
        creds = grpc.ssl_channel_credentials(root_certificates=f.read())
    tls = grpc.secure_channel(tls_addr, creds)
    stub = access_grpc.AccessServiceStub(tls)
    req = access_pb2.ListKeysRequest()
    expect("tls no key", call(stub.ListKeys, req), grpc.StatusCode.UNAUTHENTICATED, UNAUTHENTICATED)
    try:
        _, rpc = stub.ListKeys.with_call(req, metadata=[("authorization", "garbage")], timeout=10)
        fail("tls malformed key was served")
    except grpc.RpcError as e:
        expect("tls malformed key", (e.code(), e.details(), dict(e.initial_metadata() or [])), grpc.StatusCode.UNAUTHENTICATED, UNAUTHENTICATED)
    expect("tls unknown key", call(stub.ListKeys, req, UNKNOWN_KEY), grpc.StatusCode.UNAUTHENTICATED, UNAUTHENTICATED)
    expect("tls key with no grant", call(stub.ListKeys, req, no_grant), grpc.StatusCode.PERMISSION_DENIED, DENIED)
    expect("tls admin key", call(stub.ListKeys, req, admin), grpc.StatusCode.OK, "")

    health = tls.unary_unary("/grpc.health.v1.Health/Check", request_serializer=None, response_deserializer=None)
    answer = health(b"", timeout=10)
    if answer != b"\x08\x01":
        fail(f"health: {answer!r}, want SERVING (08 01)")
    print("ok   tls health without a key: SERVING")
    tls.close()

    if os.environ.get("NOPACS_CHECK_TENANTS") == "1":
        check_tenants(tls_addr, creds, no_grant, admin)
        check_reception(tls_addr, creds, socket, admin, os.environ["NOPACS_CHECK_ROOT"])

    local = grpc.insecure_channel("unix:" + socket)
    lstub = access_grpc.AccessServiceStub(local)
    status, details, md = call(lstub.WhoAmI, access_pb2.WhoAmIRequest())
    expect("socket local operator", (status, details, md), grpc.StatusCode.OK, "")
    expect("socket unknown key", call(lstub.WhoAmI, access_pb2.WhoAmIRequest(), UNKNOWN_KEY), grpc.StatusCode.UNAUTHENTICATED, UNAUTHENTICATED)
    local.close()

    with open(metrics_file) as f:
        text = f.read()
    families = {fam.name: fam for fam in text_string_to_metric_families(text)}
    for name in ("nopacs_requests", "nopacs_request_duration_seconds", "nopacs_requests_in_flight",
                 "nopacs_audit_entries_written", "nopacs_audit_entries_failed",
                 "nopacs_audit_entries_dropped_refusals", "nopacs_audit_entries_dropped_records",
                 "nopacs_audit_entries_dropped_writes", "nopacs_audit_entries_rerouted", "nopacs_audit_entries_notices",
                 "nopacs_key_last_use_marks_written", "nopacs_key_last_use_marks_dropped", "nopacs_key_last_use_marks_failed"):
        if name not in families:
            fail(f"metrics: no family {name} in {sorted(families)}")
    samples = sum(len(f.samples) for f in families.values())
    print(f"ok   /metrics parsed by prometheus_client: {len(families)} families, {samples} samples")

