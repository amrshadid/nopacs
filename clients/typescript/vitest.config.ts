import { defineConfig } from "vitest/config";

// The tests start one real server for the whole run, and the server's build
// takes a while on a cold cache, so the hooks and tests get generous limits.
export default defineConfig({
  test: {
    include: ["test/**/*.test.ts"],
    globalSetup: ["test/global-setup.ts"],
    hookTimeout: 600_000,
    testTimeout: 60_000,
    // One server, one installation: files run one after another.
    fileParallelism: false,
  },
});
