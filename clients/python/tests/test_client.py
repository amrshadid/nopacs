"""The client against a real server: what it sends, what it returns, how it fails."""

import grpc
import pytest

import nopacs
from conftest import free_port
from nopacs.v1 import access_pb2, monitor_pb2, receiver_pb2, scope_pb2, settings_pb2, storage_pb2, tenant_pb2

# The names of the contract's services and the client attribute for each.
SERVICE_ATTRIBUTES = {
    "AccessService": "access",
    "AuditService": "audit",
    "AuditSealService": "audit_seal",
    "BackupService": "backups",
    "CatalogService": "catalog",
    "CorrectionService": "corrections",
    "DataService": "data",
    "DeidService": "deid",
    "JobService": "jobs",
    "MetadataService": "metadata",
    "MonitorService": "monitor",
    "ProfileService": "profiles",
    "QuarantineService": "quarantine",
    "ReceiverService": "receivers",
    "ReceptionService": "receptions",
    "ReplicaService": "replicas",
    "RetentionService": "retention",
    "SenderService": "senders",
    "SettingsService": "settings",
    "StorageService": "storages",
    "TenantService": "tenants",
}


def test_every_service_has_a_stub(client):
    """A service added to the contract and left out of Client fails here."""
    from nopacs.v1 import (
        access_pb2, audit_pb2, audit_seal_pb2, backup_pb2, catalog_pb2, correction_pb2, data_pb2,
        deid_pb2, job_pb2, metadata_pb2, monitor_pb2, profile_pb2, quarantine_pb2, receiver_pb2,
        reception_pb2, replica_pb2, retention_pb2, sender_pb2, settings_pb2, storage_pb2, tenant_pb2,
    )
    defined = set()
    for module in (access_pb2, audit_pb2, audit_seal_pb2, backup_pb2, catalog_pb2, correction_pb2,
                   data_pb2, deid_pb2, job_pb2, metadata_pb2, monitor_pb2, profile_pb2,
                   quarantine_pb2, receiver_pb2, reception_pb2, replica_pb2, retention_pb2,
                   sender_pb2, settings_pb2, storage_pb2, tenant_pb2):
        defined |= set(module.DESCRIPTOR.services_by_name)
    assert defined == set(SERVICE_ATTRIBUTES)
    for attribute in SERVICE_ATTRIBUTES.values():
        assert hasattr(client, attribute)


def test_whoami_names_the_key_holder(client):
    """The key reaches the server as a bearer credential and is recognized."""
    me = client.access.WhoAmI(access_pb2.WhoAmIRequest()).principal
    assert me.WhichOneof("caller") == "key"
    assert me.key.holder == "admin"
    assert len(me.grants) == 1
    assert me.grants[0].role == access_pb2.ROLE_ADMIN
    assert me.grants[0].scope.WhichOneof("kind") == "installation"


def test_a_wrong_key_is_unauthenticated(server):
    """The one exception carries the code and the server's message."""
    bad = "nopacs_" + "1" * 12 + "_" + "2" * 40
    with nopacs.Client(f"127.0.0.1:{server.port}", bad, tls=False) as c:
        with pytest.raises(nopacs.Error) as caught:
            c.access.WhoAmI(access_pb2.WhoAmIRequest())
    assert caught.value.code == grpc.StatusCode.UNAUTHENTICATED
    assert caught.value.message
    assert caught.value.request_id
    # Refused before the call is routed, the answer still carries its reason.
    assert caught.value.reason == "UNAUTHENTICATED"
    assert caught.value.metadata == {}
    assert caught.value.retry_delay is None


def test_an_error_carries_the_reason_of_its_error_info(client, server):
    """Every answer carries a google.rpc.ErrorInfo; the Error exposes its reason and metadata."""
    with pytest.raises(nopacs.Error) as caught:
        client.tenants.GetTenant(tenant_pb2.GetTenantRequest(name="no-such-tenant"))
    assert caught.value.code == grpc.StatusCode.NOT_FOUND
    assert caught.value.reason == "NOT_FOUND"
    assert caught.value.metadata == {}
    assert caught.value.retry_delay is None
    key = server.make_key("nobody")
    with nopacs.Client(f"127.0.0.1:{server.port}", key, tls=False) as c:
        with pytest.raises(nopacs.Error) as caught:
            c.tenants.CreateTenant(tenant_pb2.CreateTenantRequest(name="nope", database_type=tenant_pb2.DATABASE_TYPE_SQLITE))
    assert caught.value.reason == "PERMISSION_DENIED"


def _field(num: int, payload: bytes) -> bytes:
    """Encode one length-delimited protobuf field (num below 16, payload below 128 bytes)."""
    assert num < 16 and len(payload) < 128
    return bytes([num << 3 | 2, len(payload)]) + payload


def _status(*details: bytes) -> bytes:
    """Encode a google.rpc.Status of code 9 holding each detail as it is (already an Any)."""
    return bytes([0x08, 9]) + _field(2, b"m") + b"".join(_field(3, d) for d in details)


def _any(type_name: str, value: bytes) -> bytes:
    """Encode a google.protobuf.Any of a google.rpc message."""
    return _field(1, b"type.googleapis.com/google.rpc." + type_name.encode()) + _field(2, value)


# google.rpc.ErrorInfo(reason "HELD", domain "nopacs", metadata {hold: 01jh, b: x}) and
# google.rpc.RetryInfo(retry_delay 30s 500ns), as googleapis-common-protos serializes them.
HELD = bytes.fromhex("0a0448454c4412066e6f706163731a060a01621201781a0c0a04686f6c64120430316a68")
RETRY_30S = bytes.fromhex("0a05081e10f403")


def test_the_details_are_read_from_the_status_trailer():
    """grpc-status-details-bin gives the reason, the metadata and the retry delay; a foreign domain, a malformed
    message and an absurd delay are ignored."""
    import datetime

    status = _status(_any("ErrorInfo", HELD), _any("RetryInfo", RETRY_30S))
    err = nopacs.Error.from_rpc_error(_RpcError(trailing=(("grpc-status-details-bin", status),)))
    assert (err.reason, err.metadata) == ("HELD", {"hold": "01jh", "b": "x"})
    assert err.retry_delay == datetime.timedelta(seconds=30)
    foreign = _field(1, b"HELD") + _field(2, b"example.com")
    assert nopacs.Error.from_rpc_error(_RpcError(trailing=(("grpc-status-details-bin", _status(_any("ErrorInfo", foreign))),))).reason == ""
    assert nopacs.Error.from_rpc_error(_RpcError(trailing=(("grpc-status-details-bin", b"\x1a\x7f\x0a"),))).reason == ""
    years = _field(1, bytes([0x08, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01]))
    assert nopacs.Error.from_rpc_error(_RpcError(trailing=(("grpc-status-details-bin", _status(_any("RetryInfo", years))),))).retry_delay is None
    assert nopacs.Error.from_rpc_error(_RpcError()).reason == ""


def test_a_refused_stream_carries_the_request_id(server):
    """A server stream that the server refuses raises nopacs.Error with the call's id."""
    bad = "nopacs_" + "1" * 12 + "_" + "2" * 40
    request = monitor_pb2.WatchRequest(scope=scope_pb2.Scope(installation=scope_pb2.Installation()))
    with nopacs.Client(f"127.0.0.1:{server.port}", bad, tls=False) as c:
        with pytest.raises(nopacs.Error) as caught:
            next(iter(c.monitor.Watch(request)))
    assert caught.value.code == grpc.StatusCode.UNAUTHENTICATED
    assert caught.value.request_id


def test_a_key_without_a_grant_is_refused(server):
    """A key that holds no role cannot list tenants."""
    key = server.make_key("nobody")
    with nopacs.Client(f"127.0.0.1:{server.port}", key, tls=False) as c:
        with pytest.raises(nopacs.Error) as caught:
            c.tenants.CreateTenant(tenant_pb2.CreateTenantRequest(name="nope", database_type=tenant_pb2.DATABASE_TYPE_SQLITE))
    assert caught.value.code == grpc.StatusCode.PERMISSION_DENIED
    assert caught.value.request_id


def test_tenant_lifecycle_and_paging(client):
    """Tenants are created, read, paged through by cursor, and removed."""
    names = ["pytest-a", "pytest-b", "pytest-c"]
    for name in names:
        made = client.tenants.CreateTenant(tenant_pb2.CreateTenantRequest(name=name, database_type=tenant_pb2.DATABASE_TYPE_SQLITE)).tenant
        assert made.name == name
        assert made.database_type == tenant_pb2.DATABASE_TYPE_SQLITE
        assert made.state == tenant_pb2.TENANT_STATE_READY

    got = client.tenants.GetTenant(tenant_pb2.GetTenantRequest(name="pytest-b")).tenant
    assert got.name == "pytest-b"
    assert got.create_time.seconds > 0

    one_page = client.tenants.ListTenants(tenant_pb2.ListTenantsRequest(page_size=1))
    assert len(one_page.tenants) == 1
    assert one_page.next_page_token

    paged = [t.name for t in client.pages(
        client.tenants.ListTenants, tenant_pb2.ListTenantsRequest(page_size=1), "tenants")]
    assert sorted(paged) == names
    assert len(paged) == len(set(paged))

    for name in names:
        client.tenants.RemoveTenant(tenant_pb2.RemoveTenantRequest(name=name, confirm_name=name))
    with pytest.raises(nopacs.Error) as caught:
        client.tenants.GetTenant(tenant_pb2.GetTenantRequest(name="pytest-b"))
    assert caught.value.code == grpc.StatusCode.NOT_FOUND
    assert caught.value.request_id


def test_pages_leaves_the_request_unchanged(client):
    """The helper writes page tokens into a copy, so a request can be reused."""
    request = tenant_pb2.ListTenantsRequest(page_size=1)
    for name in ("pytest-d", "pytest-e"):
        client.tenants.CreateTenant(tenant_pb2.CreateTenantRequest(name=name, database_type=tenant_pb2.DATABASE_TYPE_SQLITE))
    try:
        assert len(list(client.pages(client.tenants.ListTenants, request, "tenants"))) >= 2
        assert request.page_token == ""
    finally:
        for name in ("pytest-d", "pytest-e"):
            client.tenants.RemoveTenant(tenant_pb2.RemoveTenantRequest(name=name, confirm_name=name))


def test_settings_round_trip(client):
    """A setting is set, read back with its source, and unset to its default."""
    scope = scope_pb2.Scope(installation=scope_pb2.Installation())
    name = "key.default-lifetime"
    before = client.settings.GetSetting(settings_pb2.GetSettingRequest(name=name, scope=scope)).setting
    assert before.source == settings_pb2.SETTING_SOURCE_BUILT_IN

    client.settings.SetSetting(settings_pb2.SetSettingRequest(
        setting=settings_pb2.Setting(name=name, scope=scope, value="30d")))
    after = client.settings.GetSetting(settings_pb2.GetSettingRequest(name=name, scope=scope)).setting
    assert after.value == "30d"
    assert after.source == settings_pb2.SETTING_SOURCE_INSTALLATION

    listed = {s.name: s.value for s in client.pages(
        client.settings.ListSettings, settings_pb2.ListSettingsRequest(scope=scope), "settings")}
    assert listed[name] == "30d"

    client.settings.UnsetSetting(settings_pb2.UnsetSettingRequest(name=name, scope=scope))
    again = client.settings.GetSetting(settings_pb2.GetSettingRequest(name=name, scope=scope)).setting
    assert again.value == before.value
    assert again.source == settings_pb2.SETTING_SOURCE_BUILT_IN


def test_storage_and_receiver_of_a_tenant(client, server, tmp_path):
    """A storage on a named root and a receiver that uses it are added, read and removed."""
    scope = scope_pb2.Scope(installation=scope_pb2.Installation())
    root = tmp_path / "archive"
    root.mkdir()
    client.settings.SetSetting(settings_pb2.SetSettingRequest(setting=settings_pb2.Setting(
        name="storage.directory-roots", scope=scope, value=f"fast={root}")))
    # A receiver that archives as received needs the installation to allow it.
    client.settings.SetSetting(settings_pb2.SetSettingRequest(setting=settings_pb2.Setting(
        name="receiver.allow-as-received", scope=scope, value="true")))
    client.tenants.CreateTenant(tenant_pb2.CreateTenantRequest(name="pytest-r", database_type=tenant_pb2.DATABASE_TYPE_SQLITE))
    # A role on the installation does not reach inside a tenant: the key is
    # given admin on this tenant as well.
    key_id = client.access.WhoAmI(access_pb2.WhoAmIRequest()).principal.key.id
    client.access.SetGrant(access_pb2.SetGrantRequest(
        key_id=key_id, scope=scope_pb2.Scope(tenant="pytest-r"), role=access_pb2.ROLE_ADMIN))
    port = free_port()
    client.settings.SetSetting(settings_pb2.SetSettingRequest(setting=settings_pb2.Setting(
        name="receiver.ports", scope=scope_pb2.Scope(tenant="pytest-r"), value=str(port))))
    try:
        client.storages.AddStorage(storage_pb2.AddStorageRequest(
            tenant="pytest-r",
            storage=storage_pb2.Storage(name="main", directory=storage_pb2.DirectoryStorage(root="fast", path="main"))))
        listed = [s.name for s in client.pages(
            client.storages.ListStorages, storage_pb2.ListStoragesRequest(tenant="pytest-r"), "storages")]
        assert listed == ["main"]

        client.receivers.AddReceiver(receiver_pb2.AddReceiverRequest(
            tenant="pytest-r",
            receiver=receiver_pb2.Receiver(
                name="scanner", storage="main", enabled=False,
                dimse=receiver_pb2.DimseReceiver(port=port, ae_title="PYTEST", allowed_callers=receiver_pb2.AllowedCallers(
                    rules=[receiver_pb2.CallerRule(calling_ae_title="SCANNER1")])))))
        got = client.receivers.GetReceiver(receiver_pb2.GetReceiverRequest(tenant="pytest-r", name="scanner")).receiver
        assert got.storage == "main"
        assert got.dimse.port == port
        assert got.dimse.ae_title == "PYTEST"
        assert not got.enabled

        client.receivers.RemoveReceiver(receiver_pb2.RemoveReceiverRequest(tenant="pytest-r", name="scanner"))
        with pytest.raises(nopacs.Error) as caught:
            client.receivers.GetReceiver(receiver_pb2.GetReceiverRequest(tenant="pytest-r", name="scanner"))
        assert caught.value.code == grpc.StatusCode.NOT_FOUND
    finally:
        client.tenants.RemoveTenant(tenant_pb2.RemoveTenantRequest(name="pytest-r", confirm_name="pytest-r"))
        client.settings.UnsetSetting(settings_pb2.UnsetSettingRequest(name="storage.directory-roots", scope=scope))


class _Page:
    """A fake list response: the items of one page and the token for the next."""

    def __init__(self, tenants, next_page_token):
        self.tenants = tenants
        self.next_page_token = next_page_token


def test_pages_stops_on_a_token_the_server_repeats():
    """A server that answers the token it was just sent would loop the helper forever."""
    sent = []

    def call(request):
        sent.append(request.page_token)
        return _Page(["x"], "same")

    with pytest.raises(RuntimeError, match="tenants"):
        list(nopacs.Client.pages(call, tenant_pb2.ListTenantsRequest(), "tenants"))
    assert sent == ["", "same"]


def test_pages_follows_distinct_tokens_to_the_end():
    """Tokens that change are followed until the empty one."""
    answers = {"": _Page(["a"], "t1"), "t1": _Page(["b"], "t2"), "t2": _Page(["c"], "")}
    got = list(nopacs.Client.pages(
        lambda request: answers[request.page_token], tenant_pb2.ListTenantsRequest(), "tenants"))
    assert got == ["a", "b", "c"]


class _RpcError(grpc.RpcError):
    """A failure with only the metadata a test gives it."""

    def __init__(self, initial=(), trailing=()):
        self._initial, self._trailing = initial, trailing

    def code(self):
        return grpc.StatusCode.INTERNAL

    def details(self):
        return "boom"

    def initial_metadata(self):
        return self._initial

    def trailing_metadata(self):
        return self._trailing


def test_the_request_id_is_read_from_the_headers_or_the_trailers():
    """The server sends the id with the headers; a call that ended without them carries it in the trailers."""
    assert nopacs.Error.from_rpc_error(_RpcError(initial=(("nopacs-request-id", "h1"),))).request_id == "h1"
    assert nopacs.Error.from_rpc_error(_RpcError(trailing=(("nopacs-request-id", "t1"),))).request_id == "t1"
    assert nopacs.Error.from_rpc_error(_RpcError()).request_id == ""


SECRET_KEYS = ["nopacs_a_b\nX-Evil: 1", "nopacs_a_b c", "nopacs_a_b\x00", "nopacs_a_b\u00e9", " nopacs_a_b", "nopacs_a_b\n"]


@pytest.mark.parametrize("key", SECRET_KEYS)
def test_a_malformed_key_is_refused_without_echoing_it(key):
    """A key a header cannot carry is refused when the client is made, and never repeated."""
    with pytest.raises(ValueError) as caught:
        nopacs.Client("127.0.0.1:1", key, tls=False)
    for text in (str(caught.value), repr(caught.value), *map(str, caught.value.args)):
        assert key not in text
        assert key.strip() not in text
    assert caught.value.__cause__ is None
    assert caught.value.__context__ is None


@pytest.mark.parametrize("address", ["127.0.0.1:50051", "127.1.2.3:1", "localhost:1", "[::1]:8443", "unix:/tmp/x.sock"])
def test_a_loopback_server_may_be_reached_without_tls(address):
    """tls=False is accepted for this machine's own addresses."""
    nopacs.Client(address, "nopacs_a_b", tls=False).close()


@pytest.mark.parametrize("address", ["archive.example.org:8443", "10.0.0.5:8443", "192.168.1.2:1", "[2001:db8::1]:8443"])
def test_the_key_is_not_sent_in_clear_to_another_host(address):
    """tls=False for a server that is not on this machine is refused, with the way to allow it."""
    with pytest.raises(ValueError, match="allow_insecure") as caught:
        nopacs.Client(address, "nopacs_a_secret", tls=False)
    assert "nopacs_a_secret" not in str(caught.value)
    nopacs.Client(address, "nopacs_a_secret", tls=False, allow_insecure=True).close()


def not_served_services():
    """The services api/not-served.txt lists as having no handler yet."""
    from conftest import REPO
    lines = (REPO / "api" / "not-served.txt").read_text().splitlines()
    return [l.split("#")[0].strip().removeprefix("nopacs.v1.") for l in lines if l.split("#")[0].strip()]


def test_a_method_of_no_service_answers_unimplemented(client):
    """A method of no service of the contract answers UNIMPLEMENTED.

    Once every service is served, api/not-served.txt is empty and the test
    below has nothing to cover; this stands in for "a method not served".
    """
    call = client._channel.unary_unary("/nopacs.v1.NotServedService/Nothing")
    with pytest.raises(grpc.RpcError) as caught:
        call(b"")
    assert caught.value.code() == grpc.StatusCode.UNIMPLEMENTED


@pytest.mark.parametrize("service", not_served_services())
def test_a_service_without_handlers_answers_unimplemented(client, service):
    """Every method of a service the server does not serve yet answers UNIMPLEMENTED.

    When a handler arrives, the service leaves api/not-served.txt and this
    test no longer covers it; its own tests take over.
    """
    import importlib
    # A line is a service, or "Service/Method" for one method of a served service.
    service, _, only = service.partition("/")
    module = importlib.import_module(f"nopacs.v1.{service.removesuffix('Service').lower()}_pb2")
    descriptor = module.DESCRIPTOR.services_by_name[service]
    stub = getattr(client, SERVICE_ATTRIBUTES[service])
    methods = [m for m in descriptor.methods if not only or m.name == only]
    assert methods
    for method in methods:
        request = method.input_type._concrete_class()
        with pytest.raises(nopacs.Error) as caught:
            result = getattr(stub, method.name)(request)
            if method.server_streaming:
                next(iter(result))
        assert caught.value.code == grpc.StatusCode.UNIMPLEMENTED, method.full_name
