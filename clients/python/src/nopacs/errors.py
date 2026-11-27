"""The one exception every failed call raises."""

import base64
import datetime
from typing import Dict, Iterable, Optional, Tuple

import grpc

#: The domain of every google.rpc.ErrorInfo the server answers.
DOMAIN = "nopacs"

_STATUS_DETAILS = "grpc-status-details-bin"
_ERROR_INFO = "type.googleapis.com/google.rpc.ErrorInfo"
_RETRY_INFO = "type.googleapis.com/google.rpc.RetryInfo"
# A server that names a longer delay, or one that does not decode, is not
# believed, so a hostile answer cannot park a client for days.
_MAX_RETRY_SECONDS = 24 * 60 * 60


class Error(Exception):
    """A call the server refused or could not answer.

    Attributes:
        code: the gRPC status code (a grpc.StatusCode), such as
            StatusCode.NOT_FOUND, UNAUTHENTICATED, PERMISSION_DENIED or
            UNIMPLEMENTED. The API contract says which a method can answer.
        message: the server's message, which never carries a secret.
        request_id: the id the server gave the call, for finding it in the
            audit log; empty when the server sent none (for example when the
            connection failed or the call was cancelled before an answer).
        reason: the reason of the answer's google.rpc.ErrorInfo detail, a
            fixed word to branch on: a case the server names (HELD,
            NOT_ONLINE, STORAGE_SWITCHING, TENANT_MOVED, RELABELED, ...) or
            the code's general reason (NOT_FOUND, UNAVAILABLE, ...). Empty
            when the answer carried none of the server's domain (an older
            server, or a failure before any answer). A reason this client
            does not know is one added later: treat it as its code alone.
        metadata: the ErrorInfo's facts, such as {"hold": "..."} for HELD or
            {"recall_state": "requested"} for NOT_ONLINE; empty for none.
        retry_delay: how long to wait before trying again, a
            datetime.timedelta from the google.rpc.RetryInfo detail; None
            when the server gave none, in which case waiting alone does not
            help.
    """

    def __init__(
        self,
        code: grpc.StatusCode,
        message: str,
        request_id: str = "",
        reason: str = "",
        metadata: Optional[Dict[str, str]] = None,
        retry_delay: Optional[datetime.timedelta] = None,
    ):
        super().__init__(f"{code.name}: {message}")
        self.code = code
        self.message = message
        self.request_id = request_id
        self.reason = reason
        self.metadata = dict(metadata or {})
        self.retry_delay = retry_delay

    @classmethod
    def from_rpc_error(cls, err: grpc.RpcError) -> "Error":
        """Make an Error from the exception grpc raised."""
        code = err.code() if callable(getattr(err, "code", None)) else grpc.StatusCode.UNKNOWN
        details = err.details() if callable(getattr(err, "details", None)) else str(err)
        # The server sends the id with the response's headers, so over gRPC it
        # is in the initial metadata, and in the trailers when the call ended
        # without headers. The error details are in the trailers.
        request_id = ""
        status_bin = b""
        for name in ("initial_metadata", "trailing_metadata"):
            method = getattr(err, name, None)
            for key, value in (method() if callable(method) else None) or ():
                if key == "nopacs-request-id" and not request_id:
                    request_id = value.decode("ascii", "replace") if isinstance(value, bytes) else value
                elif key == _STATUS_DETAILS and not status_bin:
                    status_bin = value if isinstance(value, bytes) else _b64(value)
        reason, metadata, retry = details_of(status_bin)
        return cls(code, details or "", request_id, reason, metadata, retry)


def details_of(status: bytes) -> Tuple[str, Dict[str, str], Optional[datetime.timedelta]]:
    """Read the reason, metadata and retry delay from an encoded google.rpc.Status.

    The value of the gRPC trailer grpc-status-details-bin (grpcio hands it
    over decoded from base64) holds the details as google.protobuf.Any
    messages. The two the server writes, google.rpc.ErrorInfo and
    google.rpc.RetryInfo, are read here from their wire form, so the client
    needs no package beyond grpcio and protobuf. An ErrorInfo of another
    domain, a malformed message and a retry delay outside 0 to 24 hours are
    ignored.
    """
    reason, metadata, retry = "", {}, None
    try:
        for num, value in _fields(status):
            if num != 3 or not isinstance(value, bytes):
                continue
            type_url, body = "", b""
            for n, v in _fields(value):
                if n == 1 and isinstance(v, bytes):
                    type_url = v.decode("utf-8")
                elif n == 2 and isinstance(v, bytes):
                    body = v
            if type_url == _ERROR_INFO and not reason:
                r, domain, md = _error_info(body)
                if domain == DOMAIN:
                    reason, metadata = r, md
            elif type_url == _RETRY_INFO and retry is None:
                retry = _retry_info(body)
    except (ValueError, UnicodeDecodeError):
        return "", {}, None
    return reason, metadata, retry


def _error_info(body: bytes) -> Tuple[str, str, Dict[str, str]]:
    """Decode google.rpc.ErrorInfo: reason 1, domain 2, metadata 3 (map entries of key 1, value 2)."""
    reason, domain, metadata = "", "", {}
    for num, value in _fields(body):
        if not isinstance(value, bytes):
            continue
        if num == 1:
            reason = value.decode("utf-8")
        elif num == 2:
            domain = value.decode("utf-8")
        elif num == 3:
            entry = {n: v for n, v in _fields(value) if isinstance(v, bytes)}
            metadata[entry.get(1, b"").decode("utf-8")] = entry.get(2, b"").decode("utf-8")
    return reason, domain, metadata


def _retry_info(body: bytes) -> Optional[datetime.timedelta]:
    """Decode google.rpc.RetryInfo: retry_delay 1, a Duration of seconds 1 and nanos 2."""
    for num, value in _fields(body):
        if num != 1 or not isinstance(value, bytes):
            continue
        seconds, nanos = 0, 0
        for n, v in _fields(value):
            if n == 1 and isinstance(v, int):
                seconds = v
            elif n == 2 and isinstance(v, int):
                nanos = v
        if not 0 <= seconds <= _MAX_RETRY_SECONDS or not 0 <= nanos < 1_000_000_000:
            return None
        delay = datetime.timedelta(seconds=seconds, microseconds=nanos // 1000)
        return delay if delay <= datetime.timedelta(seconds=_MAX_RETRY_SECONDS) else None
    return None


def _fields(data: bytes) -> Iterable[Tuple[int, object]]:
    """Yield (field number, value) of a protobuf message: an int for a varint, bytes for a length-delimited field.

    Fixed-width fields are skipped; a group or a truncated message raises ValueError.
    """
    i = 0
    while i < len(data):
        tag, i = _varint(data, i)
        num, wire = tag >> 3, tag & 7
        if wire == 0:
            value, i = _varint(data, i)
            yield num, value
        elif wire == 2:
            size, i = _varint(data, i)
            if i + size > len(data):
                raise ValueError("a field runs past the end of the message")
            yield num, data[i : i + size]
            i += size
        elif wire == 1:
            i += 8
        elif wire == 5:
            i += 4
        else:
            raise ValueError(f"wire type {wire} is not read")
    if i != len(data):
        raise ValueError("a fixed-width field runs past the end of the message")


def _varint(data: bytes, i: int) -> Tuple[int, int]:
    """Read a base-128 varint at i; answer its value and the index after it."""
    value, shift = 0, 0
    while True:
        if i >= len(data) or shift > 63:
            raise ValueError("a varint is truncated or too long")
        b = data[i]
        i += 1
        value |= (b & 0x7F) << shift
        if not b & 0x80:
            return value, i
        shift += 7


def _b64(value: str) -> bytes:
    """Decode a binary header's value given as text: base64, padded or not."""
    try:
        return base64.b64decode(value + "=" * (-len(value) % 4))
    except ValueError:
        return b""
