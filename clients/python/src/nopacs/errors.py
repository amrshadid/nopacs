"""The one exception every failed call raises."""

import grpc


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
    """

    def __init__(self, code: grpc.StatusCode, message: str, request_id: str = ""):
        super().__init__(f"{code.name}: {message}")
        self.code = code
        self.message = message
        self.request_id = request_id

    @classmethod
    def from_rpc_error(cls, err: grpc.RpcError) -> "Error":
        """Make an Error from the exception grpc raised."""
        code = err.code() if callable(getattr(err, "code", None)) else grpc.StatusCode.UNKNOWN
        details = err.details() if callable(getattr(err, "details", None)) else str(err)
        # The server sends the id with the response's headers, so over gRPC it
        # is in the initial metadata, and in the trailers when the call ended
        # without headers.
        request_id = ""
        for name in ("initial_metadata", "trailing_metadata"):
            method = getattr(err, name, None)
            for key, value in (method() if callable(method) else None) or ():
                if key == "nopacs-request-id" and not request_id:
                    request_id = value.decode("ascii", "replace") if isinstance(value, bytes) else value
        return cls(code, details or "", request_id)
