import { Code, ConnectError } from "@connectrpc/connect";
import { BinaryReader, WireType } from "@bufbuild/protobuf/wire";

/** The trailer or header in which the server returns the id of a call. */
export const REQUEST_ID_HEADER = "nopacs-request-id";

/** The domain of every `google.rpc.ErrorInfo` the server answers. */
export const ERROR_DOMAIN = "nopacs";

/** The full names of the two error details the server writes (google/rpc/error_details.proto). */
const ERROR_INFO = "google.rpc.ErrorInfo";
const RETRY_INFO = "google.rpc.RetryInfo";

/**
 * The longest retry delay believed: a server that names more, or a delay
 * that does not decode, is ignored, so a hostile answer cannot park a client
 * for days.
 */
const MAX_RETRY_SECONDS = 24 * 60 * 60;

/** What the error details of an answer say beyond its code and message. */
export interface ErrorDetails {
  /**
   * The reason of the answer's `google.rpc.ErrorInfo`, a fixed word to
   * branch on: a case the server names (`HELD`, `NOT_ONLINE`,
   * `STORAGE_SWITCHING`, `TENANT_MOVED`, `RELABELED`, ...) or the code's
   * general reason (`NOT_FOUND`, `UNAVAILABLE`, ...). An empty string when
   * the answer carried none of the server's domain. A reason this client
   * does not know is one added later: treat it as its code alone.
   */
  reason: string;
  /** The ErrorInfo's facts, such as `{ hold: "..." }` for `HELD`; empty for none. */
  metadata: Record<string, string>;
  /**
   * How long to wait before trying again, in milliseconds, from the
   * `google.rpc.RetryInfo` detail; `undefined` when the server gave none, in
   * which case waiting alone does not help.
   */
  retryDelayMs: number | undefined;
}

/**
 * Reads the reason, metadata and retry delay from the details of a Connect
 * error, whichever protocol carried them. The two messages are decoded from
 * their wire form here, so the client needs no package of the `google.rpc`
 * types. An ErrorInfo of another domain, a malformed message and a delay
 * outside 0 to 24 hours are ignored.
 */
export function errorDetailsOf(err: ConnectError): ErrorDetails {
  const out: ErrorDetails = { reason: "", metadata: {}, retryDelayMs: undefined };
  for (const d of err.details) {
    if (!("type" in d)) continue;
    try {
      if (d.type === ERROR_INFO && out.reason === "") {
        const info = decodeErrorInfo(d.value);
        if (info.domain === ERROR_DOMAIN) {
          out.reason = info.reason;
          out.metadata = info.metadata;
        }
      } else if (d.type === RETRY_INFO && out.retryDelayMs === undefined) {
        out.retryDelayMs = decodeRetryInfo(d.value);
      }
    } catch {
      // A detail that does not decode says nothing.
    }
  }
  return out;
}

/** Decodes google.rpc.ErrorInfo: reason 1, domain 2, metadata 3 (map entries of key 1, value 2). */
function decodeErrorInfo(bytes: Uint8Array): { reason: string; domain: string; metadata: Record<string, string> } {
  const r = new BinaryReader(bytes);
  const out = { reason: "", domain: "", metadata: {} as Record<string, string> };
  while (r.pos < r.len) {
    const [field, wire] = r.tag();
    if (wire !== WireType.LengthDelimited) {
      r.skip(wire, field);
      continue;
    }
    if (field === 1) out.reason = r.string();
    else if (field === 2) out.domain = r.string();
    else if (field === 3) {
      const e = new BinaryReader(r.bytes());
      let key = "";
      let value = "";
      while (e.pos < e.len) {
        const [f, w] = e.tag();
        if (f === 1 && w === WireType.LengthDelimited) key = e.string();
        else if (f === 2 && w === WireType.LengthDelimited) value = e.string();
        else e.skip(w, f);
      }
      out.metadata[key] = value;
    } else r.skip(wire, field);
  }
  return out;
}

/** Decodes google.rpc.RetryInfo: retry_delay 1, a Duration of seconds 1 and nanos 2; undefined when absurd. */
function decodeRetryInfo(bytes: Uint8Array): number | undefined {
  const r = new BinaryReader(bytes);
  while (r.pos < r.len) {
    const [field, wire] = r.tag();
    if (field !== 1 || wire !== WireType.LengthDelimited) {
      r.skip(wire, field);
      continue;
    }
    const d = new BinaryReader(r.bytes());
    let seconds = 0n;
    let nanos = 0;
    while (d.pos < d.len) {
      const [f, w] = d.tag();
      if (f === 1 && w === WireType.Varint) seconds = BigInt(d.int64());
      else if (f === 2 && w === WireType.Varint) nanos = d.int32();
      else d.skip(w, f);
    }
    if (seconds < 0n || seconds > BigInt(MAX_RETRY_SECONDS) || nanos < 0 || nanos >= 1e9) return undefined;
    const ms = Number(seconds) * 1000 + Math.floor(nanos / 1e6);
    return ms <= MAX_RETRY_SECONDS * 1000 ? ms : undefined;
  }
  return undefined;
}

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
 * connection failed. `reason`, `reasonMetadata` and `retryDelayMs` are what
 * the answer's error details say ({@link ErrorDetails}); `metadata` stays the
 * Connect error's headers and trailers.
 */
export class NopacsError extends ConnectError {
  /** The `nopacs-request-id` of the call, or an empty string. */
  readonly requestId: string;
  /** The reason of the answer's `google.rpc.ErrorInfo`, or an empty string. */
  readonly reason: string;
  /** The ErrorInfo's facts; empty for none. */
  readonly reasonMetadata: Readonly<Record<string, string>>;
  /** The retry delay in milliseconds, or `undefined` when the server gave none. */
  readonly retryDelayMs: number | undefined;

  /** Makes an error from its parts; calls throw it through {@link NopacsError.wrap}. */
  constructor(
    code: Code,
    message: string,
    requestId = "",
    options?: { cause?: unknown; metadata?: HeadersInit; details?: ConnectError["details"] },
  ) {
    const metadata = new Headers(options?.metadata);
    if (requestId) metadata.set(REQUEST_ID_HEADER, requestId);
    super(message, code, metadata, undefined, options?.cause);
    // The name stays "ConnectError": Connect recognizes a subclass by it, and
    // would wrap an error with another name in a second, Unknown, error.
    this.requestId = requestId;
    if (options?.details) this.details = options.details;
    const d = errorDetailsOf(this);
    this.reason = d.reason;
    this.reasonMetadata = d.metadata;
    this.retryDelayMs = d.retryDelayMs;
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
   * one is returned as it is; a Connect error keeps its code, message,
   * metadata and details; any other failure keeps its message under the code
   * Unknown.
   */
  static wrap(err: unknown): NopacsError {
    if (err instanceof NopacsError) return err;
    const e = ConnectError.from(err);
    return new NopacsError(e.code, e.rawMessage, e.metadata.get(REQUEST_ID_HEADER) ?? "", {
      cause: err,
      metadata: e.metadata,
      details: e.details,
    });
  }
}
