import { mkdirSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";

import { Code, NopacsError, nopacsInterceptors, createNopacsClient as createBrowserClient, pages } from "../src/browser.js";
import { createNopacsClient as createNodeClient } from "../src/node.js";
import { file_nopacs_v1_access, Role } from "../src/gen/nopacs/v1/access_pb.js";
import { file_nopacs_v1_audit } from "../src/gen/nopacs/v1/audit_pb.js";
import { file_nopacs_v1_audit_seal } from "../src/gen/nopacs/v1/audit_seal_pb.js";
import { file_nopacs_v1_backup } from "../src/gen/nopacs/v1/backup_pb.js";
import { file_nopacs_v1_catalog } from "../src/gen/nopacs/v1/catalog_pb.js";
import { file_nopacs_v1_correction } from "../src/gen/nopacs/v1/correction_pb.js";
import { file_nopacs_v1_data } from "../src/gen/nopacs/v1/data_pb.js";
import { file_nopacs_v1_deid } from "../src/gen/nopacs/v1/deid_pb.js";
import { file_nopacs_v1_job } from "../src/gen/nopacs/v1/job_pb.js";
import { file_nopacs_v1_metadata } from "../src/gen/nopacs/v1/metadata_pb.js";
import { file_nopacs_v1_monitor } from "../src/gen/nopacs/v1/monitor_pb.js";
import { file_nopacs_v1_profile } from "../src/gen/nopacs/v1/profile_pb.js";
import { file_nopacs_v1_quarantine } from "../src/gen/nopacs/v1/quarantine_pb.js";
import { file_nopacs_v1_receiver } from "../src/gen/nopacs/v1/receiver_pb.js";
import { file_nopacs_v1_reception } from "../src/gen/nopacs/v1/reception_pb.js";
import { file_nopacs_v1_replica } from "../src/gen/nopacs/v1/replica_pb.js";
import { file_nopacs_v1_retention } from "../src/gen/nopacs/v1/retention_pb.js";
import { file_nopacs_v1_sender } from "../src/gen/nopacs/v1/sender_pb.js";
import { file_nopacs_v1_settings, SettingSource } from "../src/gen/nopacs/v1/settings_pb.js";
import { file_nopacs_v1_storage } from "../src/gen/nopacs/v1/storage_pb.js";
import { DatabaseType, file_nopacs_v1_tenant, TenantState } from "../src/gen/nopacs/v1/tenant_pb.js";
import { freePort, Server, silentServer } from "./server.js";

// The client attribute for each service of the contract.
const SERVICE_ATTRIBUTES: Record<string, string> = {
  AccessService: "access",
  AuditService: "audit",
  AuditSealService: "auditSeal",
  BackupService: "backups",
  CatalogService: "catalog",
  CorrectionService: "corrections",
  DataService: "data",
  DeidService: "deid",
  JobService: "jobs",
  MetadataService: "metadata",
  MonitorService: "monitor",
  ProfileService: "profiles",
  QuarantineService: "quarantine",
  ReceiverService: "receivers",
  ReceptionService: "receptions",
  ReplicaService: "replicas",
  RetentionService: "retention",
  SenderService: "senders",
  SettingsService: "settings",
  StorageService: "storages",
  TenantService: "tenants",
};

const FILES = [
  file_nopacs_v1_access, file_nopacs_v1_audit, file_nopacs_v1_audit_seal, file_nopacs_v1_backup,
  file_nopacs_v1_catalog, file_nopacs_v1_correction, file_nopacs_v1_data, file_nopacs_v1_deid,
  file_nopacs_v1_job, file_nopacs_v1_metadata, file_nopacs_v1_monitor, file_nopacs_v1_profile,
  file_nopacs_v1_quarantine, file_nopacs_v1_receiver, file_nopacs_v1_reception, file_nopacs_v1_replica,
  file_nopacs_v1_retention, file_nopacs_v1_sender,
  file_nopacs_v1_settings, file_nopacs_v1_storage, file_nopacs_v1_tenant,
];
const SERVICES = FILES.flatMap((f) => f.services);

let server: Server;
let baseUrl: string;
let admin: ReturnType<typeof createNodeClient>;

beforeAll(async () => {
  server = await Server.start(inject("binary"));
  baseUrl = `http://${server.address}`;
  admin = createNodeClient({ baseUrl, key: server.key });
});

afterAll(async () => {
  await server?.close();
});

/** Calls fn and answers the NopacsError it throws; fails when it does not throw. */
async function failure(fn: () => Promise<unknown>): Promise<NopacsError> {
  try {
    await fn();
  } catch (err) {
    expect(err).toBeInstanceOf(NopacsError);
    return err as NopacsError;
  }
  throw new Error("the call did not fail");
}

describe("the contract's services", () => {
  it("has a client for every service the contract defines, and no other", () => {
    expect(SERVICES.map((s) => s.name).sort()).toEqual(Object.keys(SERVICE_ATTRIBUTES).sort());
    for (const attribute of Object.values(SERVICE_ATTRIBUTES)) {
      expect(admin).toHaveProperty(attribute);
    }
  });
});

describe("who the key says the caller is", () => {
  // The same call over each way the client can speak to the server.
  const ways = [
    ["connect over HTTP/1.1 (node)", () => createNodeClient({ baseUrl, key: server.key })],
    ["connect over h2c (node)", () => createNodeClient({ baseUrl, key: server.key, httpVersion: "2" })],
    ["grpc (node)", () => createNodeClient({ baseUrl, key: server.key, protocol: "grpc" })],
    ["grpc-web over HTTP/1.1 (node)", () => createNodeClient({ baseUrl, key: server.key, protocol: "grpc-web" })],
    ["connect over fetch (browser entry)", () => createBrowserClient({ baseUrl, key: server.key })],
  ] as const;
  for (const [name, make] of ways) {
    it(`names the key holder: ${name}`, async () => {
      const me = (await make().access.whoAmI({})).principal!;
      expect(me.caller.case).toBe("key");
      if (me.caller.case !== "key") return;
      expect(me.caller.value.holder).toBe("admin");
      expect(me.grants).toHaveLength(1);
      expect(me.grants[0]!.role).toBe(Role.ADMIN);
      expect(me.grants[0]!.scope?.kind.case).toBe("installation");
    });
  }
});

describe("failures", () => {
  it("a wrong key is Unauthenticated, with the server's message and the request id", async () => {
    const bad = `nopacs_${"1".repeat(12)}_${"2".repeat(40)}`;
    const err = await failure(() => createNodeClient({ baseUrl, key: bad }).access.whoAmI({}));
    expect(err.code).toBe(Code.Unauthenticated);
    expect(err.rawMessage).not.toBe("");
    expect(err.message).toBe(`[unauthenticated] ${err.rawMessage}`);
    expect(err.requestId).toMatch(/\S/);
  });

  it("a wrong key is Unauthenticated, with a request id, over every transport", async () => {
    const bad = `nopacs_${"1".repeat(12)}_${"2".repeat(40)}`;
    const ways = [
      ["connect over HTTP/1.1", () => createNodeClient({ baseUrl, key: bad })],
      ["connect over h2c", () => createNodeClient({ baseUrl, key: bad, httpVersion: "2" })],
      ["grpc", () => createNodeClient({ baseUrl, key: bad, protocol: "grpc" })],
      ["grpc-web", () => createNodeClient({ baseUrl, key: bad, protocol: "grpc-web" })],
      ["connect over fetch", () => createBrowserClient({ baseUrl, key: bad })],
    ] as const;
    for (const [name, make] of ways) {
      const err = await failure(() => make().access.whoAmI({}));
      expect(err.code, name).toBe(Code.Unauthenticated);
      expect(err.requestId, name).toMatch(/\S/);
    }
  });

  it("a refused server stream carries the request id", async () => {
    const bad = `nopacs_${"1".repeat(12)}_${"2".repeat(40)}`;
    const scope = { kind: { case: "installation", value: {} } } as const;
    for (const protocol of ["connect", "grpc", "grpc-web"] as const) {
      const err = await failure(async () => {
        for await (const _ of createNodeClient({ baseUrl, key: bad, protocol }).monitor.watch({ scope })) break;
      });
      expect(err.code, protocol).toBe(Code.Unauthenticated);
      expect(err.requestId, protocol).toMatch(/\S/);
    }
  });

  it("a not-found answer carries the request id over every transport", async () => {
    for (const protocol of ["connect", "grpc", "grpc-web"] as const) {
      const c = createNodeClient({ baseUrl, key: server.key, protocol });
      const err = await failure(() => c.tenants.getTenant({ name: "no-such-tenant" }));
      expect(err.code, protocol).toBe(Code.NotFound);
      expect(err.requestId, protocol).toMatch(/\S/);
    }
  });

  it("a key without a grant cannot create a tenant", async () => {
    const nobody = createNodeClient({ baseUrl, key: server.makeKey("nobody") });
    const err = await failure(() =>
      nobody.tenants.createTenant({ name: "nope", databaseType: DatabaseType.SQLITE }),
    );
    expect(err.code).toBe(Code.PermissionDenied);
    expect(err.requestId).toMatch(/\S/);
  });

  it("a connection that cannot be made is Unavailable", async () => {
    const port = await freePort();
    const err = await failure(() =>
      createNodeClient({ baseUrl: `http://127.0.0.1:${port}`, key: server.key, timeoutMs: 5000 }).access.whoAmI({}),
    );
    expect(err.code).toBe(Code.Unavailable);
    expect(err.requestId).toBe("");
    expect(err.metadata.get("nopacs-request-id")).toBeNull();
  });

  it("refuses to be made without a key", () => {
    expect(() => createNodeClient({ baseUrl, key: "" })).toThrow("a key is required");
  });
});

describe("a malformed key", () => {
  const keys = ["nopacs_a_b\nX-Evil: 1", "nopacs_a_b c", "nopacs_a_b\0", "nopacs_a_b\u00e9", " nopacs_a_b", "nopacs_a_b\n"];
  for (const key of keys) {
    it(`is refused without being repeated: ${JSON.stringify(key)}`, () => {
      for (const make of [createNodeClient, createBrowserClient]) {
        let thrown: unknown;
        try {
          make({ baseUrl, key });
        } catch (err) {
          thrown = err;
        }
        expect(thrown).toBeInstanceOf(TypeError);
        const err = thrown as TypeError;
        for (const text of [err.message, String(err), String(err.cause ?? ""), err.stack ?? ""]) {
          expect(text).not.toContain(key.trim());
          expect(text).not.toContain("X-Evil");
        }
        expect(err.cause).toBeUndefined();
      }
    });
  }
});

describe("the key in clear", () => {
  it("is not sent over plain HTTP to a host that is not this machine", () => {
    for (const baseUrl of ["http://archive.example.org:8443", "http://10.0.0.5:8443", "HTTP://192.168.1.2", "http://[2001:db8::1]:8443"]) {
      for (const make of [createNodeClient, createBrowserClient]) {
        expect(() => make({ baseUrl, key: "nopacs_a_secret" }), baseUrl).toThrow(/allowInsecure/);
        expect(() => make({ baseUrl, key: "nopacs_a_secret" }), baseUrl).not.toThrow(/nopacs_a_secret/);
        expect(() => make({ baseUrl, key: "nopacs_a_secret", allowInsecure: true }), baseUrl).not.toThrow();
      }
    }
  });

  it("may go over plain HTTP to this machine, and over HTTPS anywhere", () => {
    for (const baseUrl of ["http://127.0.0.1:1", "http://127.9.9.9:1", "http://localhost:1", "http://[::1]:8443", "https://archive.example.org"]) {
      expect(() => createNodeClient({ baseUrl, key: "nopacs_a_secret" }), baseUrl).not.toThrow();
      expect(() => createBrowserClient({ baseUrl, key: "nopacs_a_secret" }), baseUrl).not.toThrow();
    }
  });
});

describe("a call that does not finish", () => {
  let silent: Awaited<ReturnType<typeof silentServer>>;
  beforeAll(async () => {
    silent = await silentServer();
  });
  afterAll(async () => {
    await silent?.close();
  });
  const url = () => `http://127.0.0.1:${silent.port}`;
  const scope = { kind: { case: "installation", value: {} } } as const;

  const ways = [
    ["connect over HTTP/1.1", (o: object) => createNodeClient({ baseUrl: url(), key: server.key, ...o })],
    ["connect over h2c", (o: object) => createNodeClient({ baseUrl: url(), key: server.key, httpVersion: "2", ...o })],
    ["grpc", (o: object) => createNodeClient({ baseUrl: url(), key: server.key, protocol: "grpc", ...o })],
    ["grpc-web", (o: object) => createNodeClient({ baseUrl: url(), key: server.key, protocol: "grpc-web", ...o })],
    ["connect over fetch", (o: object) => createBrowserClient({ baseUrl: url(), key: server.key, ...o })],
  ] as const;

  for (const [name, make] of ways) {
    it(`a deadline is a NopacsError, unary and stream: ${name}`, async () => {
      const unary = await failure(() => make({}).access.whoAmI({}, { timeoutMs: 1 }));
      expect(unary.code).toBe(Code.DeadlineExceeded);
      expect(unary.requestId).toBe("");
      const stream = await failure(async () => {
        for await (const _ of make({}).monitor.watch({ scope }, { timeoutMs: 1 })) break;
      });
      expect(stream.code).toBe(Code.DeadlineExceeded);
      expect(stream.requestId).toBe("");
    });

    it(`an aborted call is a NopacsError, unary and stream: ${name}`, async () => {
      const abort = new AbortController();
      setTimeout(() => abort.abort(), 20);
      const unary = await failure(() => make({}).access.whoAmI({}, { signal: abort.signal }));
      expect(unary.code).toBe(Code.Canceled);
      expect(unary.requestId).toBe("");

      const again = new AbortController();
      setTimeout(() => again.abort(), 20);
      const stream = await failure(async () => {
        for await (const _ of make({}).monitor.watch({ scope }, { signal: again.signal })) break;
      });
      expect(stream.code).toBe(Code.Canceled);
      expect(stream.requestId).toBe("");
    });
  }

  it("a unary call has the configured default deadline, and a call's own replaces it", async () => {
    const started = Date.now();
    const err = await failure(() => createNodeClient({ baseUrl: url(), key: server.key, timeoutMs: 150 }).access.whoAmI({}));
    expect(err.code).toBe(Code.DeadlineExceeded);
    expect(Date.now() - started).toBeLessThan(5000);
    const own = await failure(() =>
      createNodeClient({ baseUrl: url(), key: server.key, timeoutMs: 60_000 }).access.whoAmI({}, { timeoutMs: 1 }),
    );
    expect(own.code).toBe(Code.DeadlineExceeded);
  });
});

describe("a server stream", () => {
  it("is not cut by the default deadline of unary calls", async () => {
    // The default is short here so the test is fast: a stream that the
    // default applied to would end with DeadlineExceeded after 300 ms.
    const client = createNodeClient({ baseUrl, key: server.key, timeoutMs: 300 });
    const abort = new AbortController();
    const scope = { kind: { case: "installation", value: {} } } as const;
    const seen: string[] = [];
    const error = failure(async () => {
      for await (const event of client.monitor.watch({ scope }, { signal: abort.signal })) {
        seen.push(event.$typeName);
      }
    });
    await new Promise((r) => setTimeout(r, 1200));
    abort.abort();
    const err = await error;
    expect(seen.length).toBeGreaterThan(0);
    expect(err.code).toBe(Code.Canceled);
  });

  it("has a deadline when the call passes one", async () => {
    const client = createNodeClient({ baseUrl, key: server.key });
    const scope = { kind: { case: "installation", value: {} } } as const;
    const err = await failure(async () => {
      for await (const _ of client.monitor.watch({ scope }, { timeoutMs: 300 })) {
        // Keep reading until the deadline ends the stream.
      }
    });
    expect(err.code).toBe(Code.DeadlineExceeded);
  });
});

describe("pages", () => {
  it("throws when the server repeats the token it was just sent", async () => {
    const sent: string[] = [];
    const call = async (request: { pageToken?: string }) => {
      sent.push(request.pageToken ?? "");
      return { items: ["x"], nextPageToken: "same" };
    };
    await expect(async () => {
      for await (const _ of pages(call, {}, "items")) {
        // Drain it.
      }
    }).rejects.toThrow(/items/);
    expect(sent).toEqual(["", "same"]);
  });

  it("follows tokens that change to the empty one", async () => {
    const answers: Record<string, { items: string[]; nextPageToken: string }> = {
      "": { items: ["a"], nextPageToken: "t1" },
      t1: { items: ["b"], nextPageToken: "t2" },
      t2: { items: ["c"], nextPageToken: "" },
    };
    const got: string[] = [];
    for await (const item of pages(async (r: { pageToken?: string }) => answers[r.pageToken ?? ""]!, {}, "items")) {
      got.push(item);
    }
    expect(got).toEqual(["a", "b", "c"]);
  });
});

describe("tenants", () => {
  it("are created, read, paged through by cursor at page size 1, and removed", async () => {
    const names = ["ts-a", "ts-b", "ts-c"];
    for (const name of names) {
      const made = (await admin.tenants.createTenant({ name, databaseType: DatabaseType.SQLITE })).tenant!;
      expect(made.name).toBe(name);
      expect(made.databaseType).toBe(DatabaseType.SQLITE);
      expect(made.state).toBe(TenantState.READY);
    }

    const got = (await admin.tenants.getTenant({ name: "ts-b" })).tenant!;
    expect(got.name).toBe("ts-b");
    expect(got.createTime!.seconds > 0n).toBe(true);

    const onePage = await admin.tenants.listTenants({ pageSize: 1 });
    expect(onePage.tenants).toHaveLength(1);
    expect(onePage.nextPageToken).not.toBe("");

    const request = { pageSize: 1 };
    const paged: string[] = [];
    for await (const tenant of pages(admin.tenants.listTenants, request, "tenants")) paged.push(tenant.name);
    expect([...paged].sort()).toEqual(names);
    expect(new Set(paged).size).toBe(paged.length);
    // The helper writes page tokens into copies, so a request can be reused.
    expect(request).toEqual({ pageSize: 1 });

    for (const name of names) await admin.tenants.removeTenant({ name, confirmName: name });
    const err = await failure(() => admin.tenants.getTenant({ name: "ts-b" }));
    expect(err.code).toBe(Code.NotFound);
  });
});

describe("settings", () => {
  it("are set, read back with their source, listed and unset to the default", async () => {
    const scope = { kind: { case: "installation", value: {} } } as const;
    const name = "key.default-lifetime";
    const before = (await admin.settings.getSetting({ name, scope })).setting!;
    expect(before.source).toBe(SettingSource.BUILT_IN);

    await admin.settings.setSetting({ setting: { name, scope, value: "30d" } });
    const after = (await admin.settings.getSetting({ name, scope })).setting!;
    expect(after.value).toBe("30d");
    expect(after.source).toBe(SettingSource.INSTALLATION);

    const listed = new Map<string, string>();
    for await (const s of pages(admin.settings.listSettings, { scope }, "settings")) listed.set(s.name, s.value);
    expect(listed.get(name)).toBe("30d");

    await admin.settings.unsetSetting({ name, scope });
    const again = (await admin.settings.getSetting({ name, scope })).setting!;
    expect(again.value).toBe(before.value);
    expect(again.source).toBe(SettingSource.BUILT_IN);
  });
});

describe("a tenant's storage and receiver", () => {
  it("are added, read and removed", async () => {
    const installation = { kind: { case: "installation", value: {} } } as const;
    const root = mkdtempSync(join(tmpdir(), "nopacs-ts-root."));
    mkdirSync(join(root, "archive"), { recursive: true });
    await admin.settings.setSetting({
      setting: { name: "storage.directory-roots", scope: installation, value: `fast=${root}` },
    });
    // A receiver that archives as received needs the installation to allow it.
    await admin.settings.setSetting({
      setting: { name: "receiver.allow-as-received", scope: installation, value: "true" },
    });
    await admin.tenants.createTenant({ name: "ts-r", databaseType: DatabaseType.SQLITE });
    // A role on the installation does not reach inside a tenant: the key is
    // given admin on this tenant as well.
    const me = (await admin.access.whoAmI({})).principal!;
    if (me.caller.case !== "key") throw new Error("the caller is not a key");
    await admin.access.setGrant({
      keyId: me.caller.value.id,
      scope: { kind: { case: "tenant", value: "ts-r" } },
      role: Role.ADMIN,
    });
    const port = await freePort();
    await admin.settings.setSetting({
      setting: { name: "receiver.ports", scope: { kind: { case: "tenant", value: "ts-r" } }, value: String(port) },
    });
    try {
      await admin.storages.addStorage({
        tenant: "ts-r",
        storage: { name: "main", type: { case: "directory", value: { root: "fast", path: "main" } } },
      });
      const storages: string[] = [];
      for await (const s of pages(admin.storages.listStorages, { tenant: "ts-r" }, "storages")) storages.push(s.name);
      expect(storages).toEqual(["main"]);

      await admin.receivers.addReceiver({
        tenant: "ts-r",
        receiver: {
          name: "scanner",
          storage: "main",
          enabled: false,
          type: {
            case: "dimse",
            value: {
              port,
              aeTitle: "TSTEST",
              callers: {
                case: "allowedCallers",
                value: { rules: [{ callingAeTitle: "SCANNER1" }] },
              },
            },
          },
        },
      });
      const got = (await admin.receivers.getReceiver({ tenant: "ts-r", name: "scanner" })).receiver!;
      expect(got.storage).toBe("main");
      expect(got.enabled).toBe(false);
      expect(got.type.case).toBe("dimse");
      if (got.type.case === "dimse") {
        expect(got.type.value.port).toBe(port);
        expect(got.type.value.aeTitle).toBe("TSTEST");
      }

      await admin.receivers.removeReceiver({ tenant: "ts-r", name: "scanner" });
      const err = await failure(() => admin.receivers.getReceiver({ tenant: "ts-r", name: "scanner" }));
      expect(err.code).toBe(Code.NotFound);
    } finally {
      await admin.tenants.removeTenant({ name: "ts-r", confirmName: "ts-r" });
      await admin.settings.unsetSetting({ name: "storage.directory-roots", scope: installation });
    }
  });
});

describe("services without handlers", () => {
  // api/not-served.txt lists the services the server does not serve yet.
  // When a handler arrives the service leaves the file and its own tests
  // take over.
  const listed = readFileSync(join(inject("repo"), "api", "not-served.txt"), "utf8")
    .split("\n")
    .map((l) => l.split("#")[0]!.trim().replace(/^nopacs\.v1\./, ""))
    .filter((l) => l !== "");

  // Once every service is served the list is empty; a method of a service
  // the contract does not have then stands for "not served", and the server
  // answers it as it answers any method it does not have.
  it("answers Unimplemented to a method of no service of the contract", async () => {
    const response = await fetch(`${baseUrl}/nopacs.v1.NotServedService/Nothing`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${server.key}` },
      body: "{}",
    });
    expect(response.status).toBe(501);
    expect(((await response.json()) as { code: string }).code).toBe("unimplemented");
  });

  // A line is a service, or "Service/Method" for one method of a served service.
  for (const line of listed) {
    const [service = "", only] = line.split("/");
    it(`${line} answers Unimplemented to every method`, async () => {
      const descriptor = SERVICES.find((s) => s.name === service);
      expect(descriptor, line).toBeDefined();
      const methods = descriptor!.methods.filter((m) => only === undefined || m.name === only);
      expect(methods.length, line).toBeGreaterThan(0);
      const client = admin[SERVICE_ATTRIBUTES[service]! as keyof typeof admin] as unknown as Record<
        string,
        (request: object) => unknown
      >;
      for (const method of methods) {
        const err = await failure(async () => {
          const result = client[method.localName]!({});
          if (method.methodKind === "server_streaming") {
            for await (const _ of result as AsyncIterable<unknown>) break;
          } else {
            await (result as Promise<unknown>);
          }
        });
        expect(err.code, `${service}.${method.name}`).toBe(Code.Unimplemented);
      }
    });
  }
});

describe("transports and interceptors", () => {
  it("accepts a transport the caller built", async () => {
    const { createConnectTransport } = await import("@connectrpc/connect-node");
    const transport = createConnectTransport({
      baseUrl,
      httpVersion: "1.1",
      interceptors: nopacsInterceptors(server.key),
    });
    const me = (await createBrowserClient({ transport }).access.whoAmI({})).principal!;
    expect(me.caller.case).toBe("key");
  });
});
