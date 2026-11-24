"""The channel to a NoPACs server and the stubs of its services."""

import ipaddress
import re
from collections.abc import Callable, Iterable, Iterator
from typing import Any, Optional

import grpc

from nopacs.v1 import (
    access_pb2_grpc,
    audit_pb2_grpc,
    audit_seal_pb2_grpc,
    backup_pb2_grpc,
    catalog_pb2_grpc,
    correction_pb2_grpc,
    data_pb2_grpc,
    deid_pb2_grpc,
    job_pb2_grpc,
    metadata_pb2_grpc,
    monitor_pb2_grpc,
    profile_pb2_grpc,
    quarantine_pb2_grpc,
    receiver_pb2_grpc,
    reception_pb2_grpc,
    replica_pb2_grpc,
    retention_pb2_grpc,
    sender_pb2_grpc,
    settings_pb2_grpc,
    storage_pb2_grpc,
    tenant_pb2_grpc,
)
from nopacs.errors import Error

# A key is the visible ASCII characters, with no space: anything else cannot
# be an HTTP header value, and would be echoed back by the transport's errors.
_KEY = re.compile(r"[\x21-\x7e]+")


def _is_local(address: str) -> bool:
    """Say whether an address names this machine: loopback or a Unix socket."""
    if address.startswith(("unix:", "unix-abstract:")):
        return True
    host = address.rsplit(":", 1)[0] if not address.endswith("]") else address
    host = host.strip("[]")
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


class _KeyInterceptor(
    grpc.UnaryUnaryClientInterceptor,
    grpc.UnaryStreamClientInterceptor,
    grpc.StreamUnaryClientInterceptor,
    grpc.StreamStreamClientInterceptor,
):
    """Adds the key to every call as the bearer credential the server reads.

    Call credentials of grpc are refused on a channel without TLS, and the
    server also serves loopback without it, so the header is added here.
    """

    def __init__(self, key: str):
        self._header = ("authorization", "Bearer " + key)

    def _with_key(self, details):
        metadata = list(details.metadata or ()) + [self._header]
        return details._replace(metadata=metadata)

    def intercept_unary_unary(self, continuation, details, request):
        return continuation(self._with_key(details), request)

    def intercept_unary_stream(self, continuation, details, request):
        return continuation(self._with_key(details), request)

    def intercept_stream_unary(self, continuation, details, request_iterator):
        return continuation(self._with_key(details), request_iterator)

    def intercept_stream_stream(self, continuation, details, request_iterator):
        return continuation(self._with_key(details), request_iterator)


class _Stub:
    """A generated stub whose methods raise nopacs.Error instead of grpc's.

    Attribute access answers the stub's method, wrapped; everything else
    (names, arguments, the request and response messages) is the generated
    stub's own, so the API contract documents it.
    """

    def __init__(self, stub: Any, timeout: Optional[float]):
        self._stub = stub
        self._timeout = timeout

    def __getattr__(self, name: str) -> Callable:
        method = getattr(self._stub, name)
        if name.startswith("_") or not callable(method):
            raise AttributeError(name)
        streams = isinstance(method, grpc.UnaryStreamMultiCallable)

        def call(request, **kwargs):
            if not streams:
                kwargs.setdefault("timeout", self._timeout)
            try:
                result = method(request, **kwargs)
            except grpc.RpcError as err:
                raise Error.from_rpc_error(err) from None
            return _raising(result) if streams else result

        call.__name__ = name
        return call


def _raising(stream: Iterable) -> Iterator:
    """Yield a server stream's messages, turning its failure into nopacs.Error."""
    try:
        yield from stream
    except grpc.RpcError as err:
        raise Error.from_rpc_error(err) from None


class Client:
    """A connection to one NoPACs server, with a stub for each service.

    A unary call returns the response message; a server-streaming call
    returns an iterator of messages. A failure of either raises nopacs.Error.
    Messages and requests are the generated classes in nopacs.v1 (for example
    nopacs.v1.tenant_pb2.ListTenantsRequest).

    Args:
        address: "host:port" of the server's network listener.
        key: the API key, as "nopacs_<id>_<secret>".
        tls: speak TLS (the default). False is for a loopback server started
            without a certificate; the key then travels in clear, so any other
            address is refused unless allow_insecure is True.
        root_certificates: PEM bytes of the authorities to trust instead of
            grpcio's bundled roots (not the operating system's store), for a
            server with a private certificate.
        timeout: seconds a unary call may take; None for no limit. A server
            stream has no deadline unless you pass timeout= to its call.
        allow_insecure: send the key in clear to a server that is not on this
            machine (tls=False). Off by default; use it only on a network
            you trust.

    Raises:
        ValueError: the key is empty or has a character a header cannot carry
            (the message never repeats the key), or tls=False for an address
            that is not this machine without allow_insecure.
    """

    def __init__(
        self,
        address: str,
        key: str,
        tls: bool = True,
        root_certificates: Optional[bytes] = None,
        timeout: Optional[float] = 60.0,
        allow_insecure: bool = False,
    ):
        if not key:
            raise ValueError("a key is required")
        if not _KEY.fullmatch(key):
            raise ValueError("the key has a character that is not visible ASCII (no spaces or line breaks)")
        if tls:
            channel = grpc.secure_channel(address, grpc.ssl_channel_credentials(root_certificates))
        else:
            if root_certificates is not None:
                raise ValueError("root_certificates need tls=True")
            if not allow_insecure and not _is_local(address):
                raise ValueError(
                    f"tls=False would send the key in clear to {address}, which is not this machine; "
                    "use TLS, or pass allow_insecure=True"
                )
            channel = grpc.insecure_channel(address)
        self._channel = grpc.intercept_channel(channel, _KeyInterceptor(key))
        self._raw_channel = channel

        def stub(module, name):
            return _Stub(getattr(module, name)(self._channel), timeout)

        self.access = stub(access_pb2_grpc, "AccessServiceStub")
        self.audit = stub(audit_pb2_grpc, "AuditServiceStub")
        self.audit_seal = stub(audit_seal_pb2_grpc, "AuditSealServiceStub")
        self.backups = stub(backup_pb2_grpc, "BackupServiceStub")
        self.catalog = stub(catalog_pb2_grpc, "CatalogServiceStub")
        self.corrections = stub(correction_pb2_grpc, "CorrectionServiceStub")
        self.data = stub(data_pb2_grpc, "DataServiceStub")
        self.deid = stub(deid_pb2_grpc, "DeidServiceStub")
        self.jobs = stub(job_pb2_grpc, "JobServiceStub")
        self.metadata = stub(metadata_pb2_grpc, "MetadataServiceStub")
        self.monitor = stub(monitor_pb2_grpc, "MonitorServiceStub")
        self.profiles = stub(profile_pb2_grpc, "ProfileServiceStub")
        self.quarantine = stub(quarantine_pb2_grpc, "QuarantineServiceStub")
        self.receivers = stub(receiver_pb2_grpc, "ReceiverServiceStub")
        self.receptions = stub(reception_pb2_grpc, "ReceptionServiceStub")
        self.replicas = stub(replica_pb2_grpc, "ReplicaServiceStub")
        self.retention = stub(retention_pb2_grpc, "RetentionServiceStub")
        self.senders = stub(sender_pb2_grpc, "SenderServiceStub")
        self.settings = stub(settings_pb2_grpc, "SettingsServiceStub")
        self.storages = stub(storage_pb2_grpc, "StorageServiceStub")
        self.tenants = stub(tenant_pb2_grpc, "TenantServiceStub")

    def close(self) -> None:
        """Close the connection."""
        self._raw_channel.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    @staticmethod
    def pages(call: Callable, request, field: str) -> Iterator[Any]:
        """Yield every item of a list call, page after page.

        Calls `call(request)`, yields the items of the response's repeated
        field `field`, and calls again with the response's next_page_token
        until the server answers an empty one; a token equal to the one just sent
        raises RuntimeError rather than looping. Set page_size on the request
        to choose the page. The request is not changed.

        Example: client.pages(client.tenants.ListTenants, ListTenantsRequest(), "tenants")
        """
        request = type(request).FromString(request.SerializeToString())
        while True:
            response = call(request)
            yield from getattr(response, field)
            if not response.next_page_token:
                return
            if response.next_page_token == request.page_token:
                raise RuntimeError(f"the server repeated next_page_token while listing {field}")
            request.page_token = response.next_page_token
