import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";

import { pages } from "../src/browser.js";
import { createNopacsClient } from "../src/node.js";
import { JobState, type Job } from "../src/gen/nopacs/v1/job_pb.js";
import { describe as describeJob, followJob, hasEnded, main } from "../examples/follow-job.js";
import { findStorescu, STUDY } from "./archive.js";
import { freePort, Server } from "./server.js";

const TENANT = "sending";
const STREAM_DEADLINE = 2;
const storescu = findStorescu();

/** The path of dcmtk's storescp, or undefined when it is not installed. */
function findStorescp(): string | undefined {
  const candidates: string[] = [];
  if (process.env.NOPACS_DCMTK) candidates.push(join(process.env.NOPACS_DCMTK, "storescp"));
  candidates.push("storescp");
  for (const c of candidates) {
    const done = spawnSync(c, ["--version"], { encoding: "utf8", timeout: 60_000 });
    if (done.status === 0 && /dcmtk/i.test(`${done.stdout}${done.stderr}`)) return c;
  }
  return undefined;
}
const storescp = findStorescp();

/** The data set of a DICOM file: what follows the preamble and the file meta group (PS3.10 7.1). */
function dataSetOf(path: string): Buffer {
  const raw = readFileSync(path);
  expect(raw.subarray(128, 132).toString("latin1")).toBe("DICM");
  // The first element of the file meta group is its length, (0002,0000) UL.
  return raw.subarray(132 + 12 + raw.readUInt32LE(140));
}

/** Every .dcm file under a directory. */
function dcmFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n);
    return statSync(p).isDirectory() ? dcmFiles(p) : p.endsWith(".dcm") ? [p] : [];
  });
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

describe.skipIf(!storescu || !storescp)("following a send job", () => {
  let server: Server;
  let client: ReturnType<typeof createNopacsClient>;
  let peer: ChildProcess;
  let received: string;
  let roots: string;
  let subject = "";

  beforeAll(async () => {
    // The server ends a stream whose call set no deadline after STREAM_DEADLINE
    // seconds instead of the contract's 30, as it ends every stream at its
    // deadline, so a job followed for longer crosses several streams in seconds.
    server = await Server.start(inject("binary"), "--default-deadline", `${STREAM_DEADLINE}s`);
    roots = join(server.work, "roots");
    received = mkdtempSync(join(tmpdir(), "nopacs-ts-peer."));
    mkdirSync(roots);
    server.cli("setting", "set", "storage.directory-roots", `fast=${roots}`);
    try {
      // A receiver that archives as received needs the installation to allow it, on a server that asks.
      server.cli("setting", "set", "receiver.allow-as-received", "true");
    } catch {
      // A server that does not know the setting does not ask.
    }
    server.cli("tenant", "create", TENANT);
    server.cli("grant", "set", server.key.split("_")[1]!, "--role", "operator", "--tenant", TENANT);
    const port = await freePort();
    const peerPort = await freePort();
    server.cli("setting", "set", "receiver.ports", String(port), "--tenant", TENANT);
    server.cli("setting", "set", "sender.destinations", `localhost ports=${peerPort} private-address`);
    server.cli("storage", "add", TENANT, "main", "--type", "dir", "--root", "fast");
    server.cli("receiver", "add", TENANT, "scanner", "--storage", "main", "--port", String(port), "--aet", "ARCHIVE",
      "--allow-aet", "STORESCU", "--quiet-period", "5s", "--enabled");
    const files = STUDY.map((f) => join(inject("repo"), "internal", "receive", "testdata", f));
    let last = "";
    for (let i = 0; i < 20; i++) {
      const done = spawnSync(storescu!, ["-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", String(port), ...files], { encoding: "utf8", timeout: 120_000 });
      if (done.status === 0) break;
      last = `${done.stdout}${done.stderr}`;
      await sleep(500);
      if (i === 19) throw new Error(`storescu: ${last}`);
    }
    peer = spawn(storescp!, ["-aet", "PEER", "-od", received, "--write-dataset", String(peerPort)], { stdio: "ignore" });
    server.cli("sender", "add", TENANT, "peer", "--type", "dimse", "--host", "localhost", "--port", String(peerPort),
      "--called-aet", "PEER", "--calling-aet", "NOPACS", "--enabled");
    client = createNopacsClient({ baseUrl: `http://${server.address}`, key: server.key });
    for (let i = 0; i < 300 && !subject; i++) {
      let n = 0n;
      let first = "";
      for await (const s of pages(client.catalog.listSubjects, { tenant: TENANT }, "subjects")) {
        n += s.instanceCount;
        first ||= s.label;
      }
      if (n === BigInt(STUDY.length)) subject = first;
      else await sleep(100);
    }
    if (!subject) throw new Error("the study was not archived in 30 seconds");
  });

  afterAll(async () => {
    peer?.kill();
    await server?.close();
    if (received) rmSync(received, { recursive: true, force: true });
  });

  it("follows a job to its end, and the peer holds the data sets that were archived", async () => {
    const updates: Job[] = [];
    for await (const job of followJob(client, { tenant: TENANT, sender: "peer", subject })) updates.push(job);
    expect(updates[0]!.state).toBe(JobState.QUEUED);
    const last = updates.at(-1)!;
    expect(last.state).toBe(JobState.DONE);
    expect(last.counts?.instances).toBe(BigInt(STUDY.length));
    expect(last.counts?.instancesSent).toBe(BigInt(STUDY.length));
    expect(last.counts?.instancesRefused).toBe(0n);
    expect(updates.every((u) => u.id === updates[0]!.id)).toBe(true);
    expect(updates.filter(hasEnded)).toHaveLength(1);

    // The peer wrote one data set for each instance, byte for byte the archived one.
    const sent = new Map(readdirSync(received).map((n) => [n.split(".").slice(1).join("."), join(received, n)]));
    const stored = new Map(dcmFiles(join(roots, TENANT, "data")).map((p) => [p.split("/").at(-1)!.replace(/\.dcm$/, ""), p]));
    expect([...sent.keys()].sort()).toEqual([...stored.keys()].sort());
    expect(sent.size).toBe(STUDY.length);
    for (const [uid, file] of sent) expect(readFileSync(file).equals(dataSetOf(stored.get(uid)!))).toBe(true);
  });

  it("follows a job for longer than the stream's deadline, across the streams the server ends", async () => {
    server.cli("sender", "set", TENANT, "peer", "--enabled=false");
    // The job waits, queued, while its sender is stopped; it is started again
    // after several stream deadlines have passed.
    const enable = setTimeout(() => server.cli("sender", "set", TENANT, "peer", "--enabled"), 3 * STREAM_DEADLINE * 1000);
    const started = Date.now();
    const updates: Job[] = [];
    try {
      for await (const job of followJob(client, { tenant: TENANT, sender: "peer", subject })) updates.push(job);
    } finally {
      clearTimeout(enable);
    }
    expect(Date.now() - started).toBeGreaterThan(3 * STREAM_DEADLINE * 1000);
    expect(updates[0]!.state).toBe(JobState.QUEUED);
    expect(updates.at(-1)!.state).toBe(JobState.DONE);
    expect(updates.at(-1)!.counts?.instancesSent).toBe(BigInt(STUDY.length));
  }, 60_000);

  it("run as a program prints each step and ends zero; a target that is not one is refused", async () => {
    const lines: string[] = [];
    const env = { NOPACS_URL: `http://${server.address}`, NOPACS_KEY: server.key };
    expect(await main([TENANT, "peer", subject], env, (l) => lines.push(l))).toBe(0);
    expect(lines[0]).toContain("queued");
    expect(lines.at(-1)).toContain("done");
    expect(lines.at(-1)).toContain(`${STUDY.length} of ${STUDY.length} instances answered`);
    expect(lines.join("\n")).not.toContain(server.key);

    const refused: string[] = [];
    expect(await main([TENANT, "peer", "sub//scan"], env, (l) => refused.push(l))).toBe(2);
    expect(refused[0]).toContain("usage:");
  });

  it("says how far a job is in one line", () => {
    const line = describeJob({ state: JobState.RUNNING, counts: { instances: 10n, instancesSent: 4n, instancesRefused: 1n, instancesRemaining: 5n } } as never);
    expect(line).toBe("running  5 of 10 instances answered, 4 sent, 1 refused, 5 remaining");
  });
});
