# nopacs

The TypeScript client of a NoPACs archive's API. Its code is generated from the
protobuf contract in `api/proto` with `@bufbuild/protoc-gen-es`, so it has every
service and message the contract has; a thin layer on top opens the
connection, sends the key and turns failures into one error class.

It speaks the Connect protocol by default, which works from a browser over
plain HTTP/1.1 with `fetch`. On Node it can also speak gRPC or gRPC-web.

## Install

The package is compiled when it is packed, so install the packed file, not
the folder (a folder install would have no compiled code to import):

```sh
cd clients/typescript
npm ci --ignore-scripts
npm pack                                  # writes nopacs-0.0.0.tgz here
cd /path/to/your/project
npm install /path/to/clients/typescript/nopacs-0.0.0.tgz
```

It needs Node 22 or later (the Connect package for Node does), or a browser.
Its dependencies are the Connect packages and `@bufbuild/protobuf`.

## Use in a browser

```ts
import { createNopacsClient, pages } from "nopacs";

const nopacs = createNopacsClient({ baseUrl: "https://nopacs.example.org:8443", key });

const me = await nopacs.access.whoAmI({});
for await (const tenant of pages(nopacs.tenants.listTenants, { pageSize: 50 }, "tenants")) {
  console.log(tenant.name);
}
```

A browser on another origin can call the server only if the server, or a proxy
in front of it, answers the browser's cross-origin (CORS) check; NoPACs' server
does not add CORS headers itself. The key is a secret: do not put one in a page
that others can read.

## Use on Node

```ts
import { NopacsError, Code, createNopacsClient } from "nopacs/node";
import { readFileSync } from "node:fs";

const nopacs = createNopacsClient({
  baseUrl: "https://nopacs.example.org:8443",
  key,
  protocol: "grpc", // "connect" (the default), "grpc" or "grpc-web"
  nodeOptions: { ca: readFileSync("ca.pem") }, // for a private certificate authority
});

try {
  await nopacs.tenants.getTenant({ name: "pilot" });
} catch (err) {
  if (err instanceof NopacsError && err.code === Code.NotFound) {
    console.log("no such tenant; request id", err.requestId);
  } else {
    throw err;
  }
}
```

## What it offers

- `createNopacsClient({ baseUrl, key, transport?, timeoutMs?, allowInsecure? })` returns one
  client per service: `access`, `audit`, `auditSeal`, `backups`, `catalog`,
  `corrections`, `data`, `jobs`,
  `metadata`, `monitor`, `profiles`, `quarantine`, `receivers`, `receptions`,
  `replicas`, `retention`, `senders`, `settings`, `storages`, `tenants`. Each method takes the request (a plain object of its
  fields) and returns the response message; a server-streaming method returns
  an async iterable. `key` (`nopacs_<id>_<secret>`) is sent with every call as
  `Authorization: Bearer <key>`. A key with a space, a line break or any other
  character a header cannot carry makes `createNopacsClient` throw a
  `TypeError` that does not repeat it.
- `timeoutMs` is the deadline of a unary call: 60000 by default, 0 for none,
  and a call's own `{ timeoutMs }` option replaces it. A server stream
  (`monitor.watch`) has no deadline unless its call passes one, because it is
  meant to stay open.
- A plain `http:` address that is not this machine (`localhost`, 127.0.0.0/8,
  `::1`) is refused, because the key would travel in clear; pass
  `allowInsecure: true` only on a network you trust.
  On Node, `protocol`, `httpVersion` (`"1.1"` or `"2"`) and `nodeOptions` choose
  how to connect.
- `pages(call, request, field)` yields the items of a list call across all its
  pages, following `nextPageToken` until it is empty. The request is not
  changed. A server that answers the token it was just sent makes it throw
  instead of looping.
- Every failed call throws `NopacsError`, including a deadline
  (`Code.DeadlineExceeded`) and a call cancelled with an `AbortSignal`
  (`Code.Canceled`), for unary calls and at the failing message of a stream. It
  is a Connect error with `code` (a `Code`),
  `rawMessage` and `requestId` (the id to look for in the audit log; empty when
  the server sent none, for example when the connection failed). A handshake
  that fails certificate checks has the code `Internal` and the TLS library's
  message.
- To bring your own transport, build it with `nopacsInterceptors(key)` among
  its interceptors and pass it as `transport`.
- Messages, enums and service descriptors are in the generated modules, for
  example `import { DatabaseType } from "nopacs/gen/tenant_pb"`. The
  contract's comments say what each method needs and answers.

The entry `nopacs` imports no Node module, so a bundler can use it for a
browser; `nopacs/node` is for Node.

## Develop

The generated code is in `src/gen`, checked in and never edited by hand.

```sh
cd clients/typescript
npm ci --ignore-scripts
npm run generate         # make it again
npm run generate:check   # fail if it is out of date
npm run typecheck
npm test                 # needs Go and, for the TLS test, openssl
```

The tests build the `nopacs` binary, start it on a temporary installation and
call it with a key made on that installation, over each protocol. The reading
tests also send a small public CT study (from `internal/receive/testdata`) with
dcmtk's `storescu`, found through `NOPACS_DCMTK` or the `PATH`, and skip when it
is missing; no other DICOM data is used. `npm run build` writes the compiled package to `dist/`, which is
not checked in.
