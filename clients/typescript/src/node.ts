import {
  createConnectTransport,
  createGrpcTransport,
  createGrpcWebTransport,
} from "@connectrpc/connect-node";
import type { Transport } from "@connectrpc/connect";
import type { ClientSessionOptions, SecureClientSessionOptions } from "node:http2";
import type { RequestOptions as HttpRequestOptions } from "node:http";
import type { RequestOptions as HttpsRequestOptions } from "node:https";

import { assertSecure, nopacsInterceptors, createClients, type NopacsClients } from "./core.js";

export { NopacsError, REQUEST_ID_HEADER } from "./errors.js";
export { pages } from "./pages.js";
export { nopacsInterceptors, createClients, errorInterceptor, keyInterceptor } from "./core.js";
export type { NopacsClients } from "./core.js";
export { Code } from "@connectrpc/connect";

/** Options of {@link createNopacsClient} on Node. */
export type NopacsClientOptions =
  | {
      /** The server's address, such as `https://nopacs.example.org:8443`. */
      baseUrl: string;
      /** The API key, `nopacs_<id>_<secret>`. */
      key: string;
      /**
       * The wire protocol: `"connect"` (the default), `"grpc"` or
       * `"grpc-web"`. gRPC always uses HTTP/2 and ignores `httpVersion`.
       */
      protocol?: "connect" | "grpc" | "grpc-web";
      /**
       * The HTTP version for the Connect and gRPC-web protocols: 1.1 (the
       * default) or 2. Over plain `http://`, HTTP/2 is h2c with prior
       * knowledge, which the server accepts on a loopback address.
       */
      httpVersion?: "1.1" | "2";
      /**
       * Node's own options for the connection, such as `{ ca }` to trust a
       * private certificate authority. The type is that of `node:http`,
       * `node:https` or `node:http2`, as the HTTP version uses.
       */
      nodeOptions?: HttpRequestOptions | HttpsRequestOptions | ClientSessionOptions | SecureClientSessionOptions;
      /**
       * Milliseconds a unary call may take; 60000 when absent, 0 for no
       * limit. A server stream has no deadline unless its call passes one.
       */
      timeoutMs?: number;
      /**
       * Send the key over plain `http:` to a host that is not this machine.
       * Off by default: such an address is refused, because the key would
       * travel in clear.
       */
      allowInsecure?: boolean;
      transport?: undefined;
    }
  | {
      /**
       * A transport you built, whose interceptors include
       * `nopacsInterceptors(key)`. The other options are then not used, except
       * `timeoutMs`.
       */
      transport: Transport;
      baseUrl?: string;
      key?: string;
      protocol?: undefined;
      httpVersion?: undefined;
      nodeOptions?: undefined;
      allowInsecure?: undefined;
      /** The deadline of a unary call made without its own; see above. */
      timeoutMs?: number;
    };

/**
 * Makes one typed client per service of a NoPACs server on Node. The default
 * is the Connect protocol over HTTP/1.1; `protocol` picks gRPC or gRPC-web
 * instead. Every failure is thrown as a {@link NopacsError}.
 */
export function createNopacsClient(options: NopacsClientOptions): NopacsClients {
  const { timeoutMs } = options;
  if (options.transport) return createClients(options.transport, { timeoutMs });
  if (!options.baseUrl) throw new TypeError("baseUrl is required");
  assertSecure(options.baseUrl, options.allowInsecure);
  const protocol = options.protocol ?? "connect";
  const common = {
    baseUrl: options.baseUrl,
    interceptors: nopacsInterceptors(options.key),
  };
  // The union of Node's option types is narrowed by the HTTP version the
  // caller chose; a mismatch is the caller's, and Node reports it.
  if (protocol === "grpc") {
    return createClients(
      createGrpcTransport({
        ...common,
        nodeOptions: options.nodeOptions as SecureClientSessionOptions | undefined,
      }),
      { timeoutMs },
    );
  }
  const node =
    options.httpVersion === "2"
      ? ({ httpVersion: "2", nodeOptions: options.nodeOptions as SecureClientSessionOptions | undefined } as const)
      : ({ httpVersion: "1.1", nodeOptions: options.nodeOptions as HttpsRequestOptions | undefined } as const);
  return createClients(
    protocol === "grpc-web"
      ? createGrpcWebTransport({ ...common, ...node })
      : createConnectTransport({ ...common, ...node }),
    { timeoutMs },
  );
}
