import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { freePort, Server } from "./server.js";

/** The tenant the study is archived in. */
export const TENANT = "reading";

/** Two series: ct2n holds two instances, ct5n one. */
export const STUDY = ["ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm"];

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** The path of dcmtk's storescu, or undefined when it is not installed. */
export function findStorescu(): string | undefined {
  const candidates: string[] = [];
  if (process.env.NOPACS_DCMTK) candidates.push(join(process.env.NOPACS_DCMTK, "storescu"));
  candidates.push("storescu");
  for (const c of candidates) {
    const done = spawnSync(c, ["--version"], { encoding: "utf8", timeout: 60_000 });
    if (done.status === 0 && /dcmtk/i.test(`${done.stdout}${done.stderr}`)) return c;
  }
  return undefined;
}

/** SHA-256 of bytes in lowercase hexadecimal. */
export function sha256(b: Uint8Array): string {
  return createHash("sha256").update(b).digest("hex");
}

/**
 * Starts a server with a tenant, a directory storage and a receiver, and
 * sends the public CT study to it with dcmtk's storescu, so the files the
 * client reads are the ones a real reception archived. The key may read the
 * tenant. Nothing here is patient data.
 */
export async function startArchive(binary: string, repo: string, storescu: string): Promise<Server> {
  const server = await Server.start(binary);
  const roots = join(server.work, "roots");
  mkdirSync(roots);
  server.cli("setting", "set", "storage.directory-roots", `fast=${roots}`);
  // A receiver that archives as received needs the installation to allow it.
  server.cli("setting", "set", "receiver.allow-as-received", "true");
  server.cli("tenant", "create", TENANT);
  server.cli("grant", "set", server.key.split("_")[1]!, "--role", "reader", "--tenant", TENANT);
  const port = await freePort();
  server.cli("setting", "set", "receiver.ports", String(port), "--tenant", TENANT);
  server.cli("storage", "add", TENANT, "main", "--type", "dir", "--root", "fast");
  server.cli("receiver", "add", TENANT, "scanner", "--storage", "main", "--port", String(port), "--aet", "ARCHIVE",
    "--allow-aet", "STORESCU", "--quiet-period", "5s", "--enabled");
  const files = STUDY.map((f) => join(repo, "internal", "receive", "testdata", f));
  for (const f of files) if (!existsSync(f)) throw new Error(`missing corpus file ${f}`);
  let last = "";
  for (let i = 0; i < 20; i++) {
    const done = spawnSync(storescu, ["-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", String(port), ...files], { encoding: "utf8", timeout: 120_000 });
    if (done.status === 0) return server;
    last = `${done.stdout}${done.stderr}`;
    await sleep(500);
  }
  await server.close();
  throw new Error(`storescu: ${last}`);
}

/** Reads a file's bytes. */
export function read(path: string): Uint8Array {
  return readFileSync(path);
}
