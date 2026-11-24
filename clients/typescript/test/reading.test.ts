import { readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";

import { Code, NopacsError, pages } from "../src/browser.js";
import { createNopacsClient } from "../src/node.js";
import type { ArchivedFile, ReadFilesResponse, ScanMarker } from "../src/gen/nopacs/v1/data_pb.js";
import { findStorescu, read, sha256, startArchive, STUDY, TENANT } from "./archive.js";
import type { Server } from "./server.js";

// Needs dcmtk's storescu to send the study; without it the file is skipped.
const storescu = findStorescu();

/** Every .dcm file under a directory. */
function dcmFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n);
    return statSync(p).isDirectory() ? dcmFiles(p) : p.endsWith(".dcm") ? [p] : [];
  });
}

describe.skipIf(!storescu)("reading an archived study", () => {
  let server: Server;
  let reader: ReturnType<typeof createNopacsClient>;

  beforeAll(async () => {
    server = await startArchive(inject("binary"), inject("repo"), storescu!);
    reader = createNopacsClient({ baseUrl: `http://${server.address}`, key: server.key });
    // The reception ends after its quiet period; wait until the catalog holds every instance.
    for (let i = 0; i < 300; i++) {
      let n = 0n;
      for await (const s of pages(reader.catalog.listSubjects, { tenant: TENANT }, "subjects")) n += s.instanceCount;
      if (n === BigInt(STUDY.length)) return;
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error("the study was not archived in 30 seconds");
  });

  afterAll(async () => {
    await server?.close();
  });

  /** The subject, session and scans of the catalog. */
  async function walk() {
    const out = [];
    for await (const sub of pages(reader.catalog.listSubjects, { tenant: TENANT }, "subjects")) {
      for await (const ses of pages(reader.catalog.listSessions, { tenant: TENANT, subjectLabel: sub.label }, "sessions")) {
        for await (const scan of pages(reader.catalog.listScans, { tenant: TENANT, subjectLabel: sub.label, sessionLabel: ses.label }, "scans")) {
          out.push({ sub, ses, scan });
        }
      }
    }
    return out;
  }

  /** Reads a ReadFiles stream into announcements with their bytes, and the scan markers. */
  async function collect(stream: AsyncIterable<ReadFilesResponse>) {
    const files: { file: ArchivedFile; body: Uint8Array }[] = [];
    const markers: ScanMarker[] = [];
    let cur: ArchivedFile | undefined;
    let chunks: Uint8Array[] = [];
    const finish = () => {
      if (cur) files.push({ file: cur, body: Buffer.concat(chunks) });
    };
    for await (const m of stream) {
      if (m.part.case === "scan") markers.push(m.part.value);
      else if (m.part.case === "file") {
        finish();
        cur = m.part.value;
        chunks = [];
      } else if (m.part.case === "chunk") {
        expect(m.part.value.offset).toBe(BigInt(chunks.reduce((n, c) => n + c.length, 0)));
        chunks.push(m.part.value.data);
      }
    }
    finish();
    return { files, markers };
  }

  it("lists the study as one subject with one session and two scans", async () => {
    const walked = await walk();
    expect(walked.map((w) => Number(w.scan.instanceCount)).sort()).toEqual([1, 2]);
    expect(walked[0]!.sub.sessionCount).toBe(1n);
    expect(walked[0]!.sub.instanceCount).toBe(3n);
    expect(walked[0]!.ses.modalities).toContain("CT");
    for (const w of walked) expect(w.scan.manifestSha256).toHaveLength(64);
  });

  it("answers each scan's headers as DICOM JSON that names the instances it holds", async () => {
    const seen = new Set<string>();
    for (const { sub, ses, scan } of await walk()) {
      const res = await reader.metadata.getScanMetadata({ tenant: TENANT, subjectLabel: sub.label, sessionLabel: ses.label, scanLabel: scan.label });
      expect(res.manifestSha256).toBe(scan.manifestSha256);
      expect(res.instances).toHaveLength(Number(scan.instanceCount));
      for (const inst of res.instances) {
        const header = JSON.parse(inst.dicomJson) as Record<string, { Value: string[] }>;
        expect(header["00080060"]!.Value).toEqual(["CT"]);
        expect(header["00080018"]!.Value[0]).toBe(inst.sopInstanceUid);
        seen.add(inst.sopInstanceUid);
      }
    }
    expect(seen.size).toBe(STUDY.length);
  });

  it("streams the archived bytes with the checksums the server announced, and they are what lies on the storage", async () => {
    const { sub } = (await walk())[0]!;
    const { files, markers } = await collect(reader.data.readFiles({ tenant: TENANT, subjectLabel: sub.label }));
    expect(files).toHaveLength(STUDY.length);
    expect(markers).toHaveLength(2);
    for (const { file, body } of files) {
      expect(BigInt(body.length)).toBe(file.length);
      expect(sha256(body)).toBe(file.sha256);
    }
    const onDisk = dcmFiles(join(server.work, "roots", TENANT, "data")).map((f) => sha256(read(f))).sort();
    expect(files.map((f) => f.file.sha256).sort()).toEqual(onDisk);
  });

  it("resumes a stream after the last whole instance, and announces without bytes", async () => {
    const { sub } = (await walk())[0]!;
    const all = (await collect(reader.data.readFiles({ tenant: TENANT, subjectLabel: sub.label }))).files;
    const first = all[0]!.file;
    const rest = await collect(reader.data.readFiles({
      tenant: TENANT, subjectLabel: sub.label,
      startAfter: { sessionLabel: first.sessionLabel, scanLabel: first.scanLabel, instanceId: first.instanceId },
    }));
    expect(rest.files.map((f) => f.file.instanceId)).toEqual(all.slice(1).map((f) => f.file.instanceId));
    let announced = 0;
    for await (const m of reader.data.readFiles({ tenant: TENANT, subjectLabel: sub.label, announceOnly: true })) {
      expect(m.part.case).not.toBe("chunk");
      if (m.part.case === "file") announced++;
    }
    expect(announced).toBe(STUDY.length);
  });

  it("answers a scan that does not exist as not found", async () => {
    try {
      await reader.catalog.getScan({ tenant: TENANT, subjectLabel: "nope", sessionLabel: "nope", scanLabel: "nope" });
    } catch (err) {
      expect(err).toBeInstanceOf(NopacsError);
      expect((err as NopacsError).code).toBe(Code.NotFound);
      return;
    }
    throw new Error("the call did not fail");
  });
});
