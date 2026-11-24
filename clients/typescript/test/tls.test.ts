import { execFileSync, spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";

import { Code, NopacsError } from "../src/browser.js";
import { createNopacsClient } from "../src/node.js";
import { Server } from "./server.js";

const haveOpenssl = spawnSync("openssl", ["version"]).status === 0;

let server: Server;
let cert: string;

beforeAll(async () => {
  if (!haveOpenssl) return;
  // The server needs a certificate before it starts, and the installation's
  // directory is where one can be made.
  server = await Server.start(inject("binary"));
  cert = join(server.work, "cert.pem");
  const key = join(server.work, "key.pem");
  execFileSync("openssl", [
    "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
    "-days", "1", "-subj", "/CN=nopacs-test", "-addext", "subjectAltName=IP:127.0.0.1",
    "-keyout", key, "-out", cert,
  ], { stdio: "ignore" });
  await server.stop();
  await server.listen("--tls-cert", cert, "--tls-key", key);
});

afterAll(async () => {
  await server?.close();
});

describe.skipIf(!haveOpenssl)("TLS with a private certificate authority", () => {
  const protocols: [string, object][] = [
    ["connect over HTTP/1.1", {}],
    ["connect over HTTP/2", { httpVersion: "2" as const }],
    ["grpc", { protocol: "grpc" as const }],
    ["grpc-web", { protocol: "grpc-web" as const }],
  ];
  for (const [name, extra] of protocols) {
    it(`is trusted when the certificate is given, and refused without it: ${name}`, async () => {
      const baseUrl = `https://${server.address}`;
      const trusting = createNopacsClient({
        baseUrl, key: server.key, ...extra, nodeOptions: { ca: readFileSync(cert) },
      });
      const me = (await trusting.access.whoAmI({})).principal!;
      expect(me.caller.case === "key" && me.caller.value.holder).toBe("admin");

      const untrusting = createNopacsClient({ baseUrl, key: server.key, ...extra, timeoutMs: 10_000 });
      const err = await untrusting.access.whoAmI({}).then(
        () => { throw new Error("an untrusted certificate was accepted"); },
        (e: unknown) => e,
      );
      expect(err).toBeInstanceOf(NopacsError);
      // Connect's Node transport reports a failed handshake as Internal, with
      // the TLS library's message.
      expect((err as NopacsError).code).toBe(Code.Internal);
      expect((err as NopacsError).rawMessage).toMatch(/self-signed|certificate/);
    });
  }
});
