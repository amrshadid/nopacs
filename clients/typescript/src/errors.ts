import { Code, ConnectError } from "@connectrpc/connect";

/** The trailer or header in which the server returns the id of a call. */
export const REQUEST_ID_HEADER = "nopacs-request-id";

/**
 * A call the server refused or could not answer, the one error every call of
 * the client throws.
 *
 * It is a Connect error, so `code` is the status code (`Code.NotFound`,
 * `Code.Unauthenticated`, `Code.PermissionDenied`, `Code.Unimplemented`, ...;
 * the API contract says which a method can answer) and `rawMessage` is the
 * server's message, which never carries a secret, without the code's prefix.
 * `requestId` is the id the server gave the call, for finding it in the audit
 * log; it is an empty string when the server sent none, for example when the
 * connection failed.
 */
export class NopacsError extends ConnectError {
  /** The `nopacs-request-id` of the call, or an empty string. */
  readonly requestId: string;

  /** Makes an error from its parts; calls throw it through {@link NopacsError.wrap}. */
  constructor(code: Code, message: string, requestId = "", options?: { cause?: unknown; metadata?: HeadersInit }) {
    const metadata = new Headers(options?.metadata);
    if (requestId) metadata.set(REQUEST_ID_HEADER, requestId);
    super(message, code, metadata, undefined, options?.cause);
    // The name stays "ConnectError": Connect recognizes a subclass by it, and
    // would wrap an error with another name in a second, Unknown, error.
    this.requestId = requestId;
  }

  /**
   * `instanceof NopacsError` follows the prototype chain. ConnectError's own
   * check accepts any Connect error by its shape, which would make every
   * Connect error pass for a NopacsError.
   */
  static override [Symbol.hasInstance](value: unknown): boolean {
    return Function.prototype[Symbol.hasInstance].call(this, value);
  }

  /**
   * Turns anything a call threw into a NopacsError. An error that already is
   * one is returned as it is; a Connect error keeps its code, message and
   * metadata; any other failure keeps its message under the code Unknown.
   */
  static wrap(err: unknown): NopacsError {
    if (err instanceof NopacsError) return err;
    const e = ConnectError.from(err);
    return new NopacsError(e.code, e.rawMessage, e.metadata.get(REQUEST_ID_HEADER) ?? "", {
      cause: err,
      metadata: e.metadata,
    });
  }
}
