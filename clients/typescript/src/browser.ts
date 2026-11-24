import { createConnectTransport } from "@connectrpc/connect-web";
import type { Transport } from "@connectrpc/connect";

import { assertSecure, nopacsInterceptors, createClients, type NopacsClients } from "./core.js";

export { NopacsError, REQUEST_ID_HEADER } from "./errors.js";
export { pages } from "./pages.js";
export { nopacsInterceptors, createClients, errorInterceptor, keyInterceptor } from "./core.js";
export type { NopacsClients } from "./core.js";
export { Code } from "@connectrpc/connect";

/** Options of {@link createNopacsClient} for a browser or any runtime with fetch. */
export type NopacsClientOptions =
  | {
      /** The server's address, such as `https://nopacs.example.org:8443`. */
      baseUrl: string;
      /** The API key, `nopacs_<id>_<secret>`. */
      key: string;
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
       * `nopacsInterceptors(key)`. `baseUrl` and `key` are then not used.
       */
      transport: Transport;
      baseUrl?: string;
      key?: string;
      allowInsecure?: undefined;
      /** The deadline of a unary call made without its own; see above. */
      timeoutMs?: number;
    };

/**
 * Makes one typed client per service of a NoPACs server, over the Connect
 * protocol with `fetch`, which works from a browser over plain HTTP/1.1.
 * Every failure is thrown as a {@link NopacsError}. For gRPC or gRPC-web from
 * Node, use `nopacs/node`.
 *
 * A browser may call a server on another origin only if the server (or a
 * proxy in front of it) answers the browser's cross-origin check.
 */
export function createNopacsClient(options: NopacsClientOptions): NopacsClients {
  const { timeoutMs } = options;
  if (options.transport) return createClients(options.transport, { timeoutMs });
  if (!options.baseUrl) throw new TypeError("baseUrl is required");
  assertSecure(options.baseUrl, options.allowInsecure);
  return createClients(
    createConnectTransport({ baseUrl: options.baseUrl, interceptors: nopacsInterceptors(options.key) }),
    { timeoutMs },
  );
}
