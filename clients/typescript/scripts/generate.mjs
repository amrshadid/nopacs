#!/usr/bin/env node
// Makes the TypeScript code of the API contract, or checks that it is current.
//
// The contract is the proto files in api/proto. This script runs buf, with
// the protoc-gen-es plugin npm installed (versions pinned in package.json), and
// writes src/gen, which is checked in and never edited by hand.
//
// Usage:
//   node scripts/generate.mjs           write src/gen again
//   node scripts/generate.mjs --check   exit 1 when src/gen differs from a
//                                       fresh generation, listing the files
//
// The output depends on the plugin's version: run it after npm ci.

import { spawnSync } from "node:child_process";
import { cpSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const gen = join(root, "src", "gen");
const check = process.argv.includes("--check");
if (process.argv.slice(2).some((a) => a !== "--check")) {
  console.error("usage: generate.mjs [--check]");
  process.exit(2);
}

/** Runs buf from the package directory; its output goes to dir. */
function generate(dir) {
  const buf = join(root, "node_modules", ".bin", "buf");
  const done = spawnSync(buf, ["generate", "--template", "buf.gen.yaml", "--output", dir], {
    cwd: root,
    stdio: ["ignore", "inherit", "inherit"],
  });
  if (done.error || done.status !== 0) {
    console.error(`buf generate failed (run npm ci first): ${done.error ?? "exit " + done.status}`);
    process.exit(1);
  }
}

/** Answers a map of relative path to bytes (as text) for every file under dir. */
function tree(dir) {
  const files = new Map();
  const walk = (d) => {
    for (const name of readdirSync(d).sort()) {
      const path = join(d, name);
      if (statSync(path).isDirectory()) walk(path);
      else files.set(relative(dir, path), readFileSync(path, "utf8"));
    }
  };
  try {
    walk(dir);
  } catch (err) {
    if (err.code !== "ENOENT") throw err;
  }
  return files;
}

const tmp = mkdtempSync(join(tmpdir(), "nopacs-ts-gen."));
try {
  // buf writes the plugin's out directory under --output.
  generate(tmp);
  const fresh = join(tmp, "src", "gen");
  const want = tree(fresh);
  if (want.size === 0) {
    console.error("buf wrote no files");
    process.exit(1);
  }
  if (check) {
    const have = tree(gen);
    const bad = [...new Set([...have.keys(), ...want.keys()])]
      .filter((k) => have.get(k) !== want.get(k))
      .sort();
    for (const k of bad) console.error(`differs from a fresh generation: ${k}`);
    if (bad.length > 0) {
      console.error("run npm run generate in clients/typescript and commit the result");
      process.exit(1);
    }
    console.log(`src/gen equals a fresh generation (${want.size} files)`);
  } else {
    rmSync(gen, { recursive: true, force: true });
    cpSync(fresh, gen, { recursive: true });
    console.log(`wrote ${want.size} files under ${gen}`);
  }
} finally {
  rmSync(tmp, { recursive: true, force: true });
}
