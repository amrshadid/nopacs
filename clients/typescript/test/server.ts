import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { closeSync, existsSync, mkdirSync, mkdtempSync, openSync, readFileSync, rmSync } from "node:fs";
import { createServer, type Socket } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

/**
 * Answers a loopback port nobody listens on. It is for a port a test hands
 * to a thing that cannot report its own, and for a connection that must be
 * refused; the server picks its own port.
 */
export function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.once("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address() as { port: number };
      s.close(() => resolve(port));
    });
  });
}

/**
 * A loopback TCP server that accepts connections and never answers, so a call
 * to it can end only by its deadline or by being cancelled.
 */
export async function silentServer(): Promise<{ port: number; close: () => Promise<void> }> {
  const sockets = new Set<Socket>();
  const s = createServer((c) => {
    sockets.add(c);
    c.on("error", () => {});
    c.on("close", () => sockets.delete(c));
  });
  await new Promise<void>((resolve) => s.listen(0, "127.0.0.1", resolve));
  const { port } = s.address() as { port: number };
  return {
    port,
    close: () =>
      new Promise<void>((resolve) => {
        for (const c of sockets) c.destroy();
        s.close(() => resolve());
      }),
  };
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** The port in the server's log line for its network listener; 0 until the line is there. */
function loggedPort(log: string): number {
  for (const line of log.split("\n")) {
    try {
      const entry = JSON.parse(line) as { msg?: string; address?: string };
      if (entry.msg === "server: listening on the network" && entry.address) {
        return Number(entry.address.slice(entry.address.lastIndexOf(":") + 1));
      }
    } catch {
      // A line that is not JSON is not the one.
    }
  }
  return 0;
}

/**
 * A running server on a temporary installation, with an admin key for it.
 * Everything is synthetic: a new installation, a key made on its local
 * socket, no DICOM data.
 */
export class Server {
  readonly work: string;
  readonly data: string;
  readonly env: NodeJS.ProcessEnv;
  port = 0;
  key = "";
  private process?: ChildProcess;

  private constructor(readonly binary: string) {
    this.work = mkdtempSync(join(existsSync("/tmp") ? "/tmp" : tmpdir(), "nopacs-ts-srv."));
    this.data = join(this.work, "d");
    mkdirSync(this.data, { mode: 0o700 });
    this.env = { ...process.env, NOPACS_DATA_DIR: this.data };
  }

  /** Initializes an installation, starts the server on a loopback port it picks and makes the admin key. */
  static async start(binary: string, ...extra: string[]): Promise<Server> {
    const server = new Server(binary);
    server.serverCli("init");
    await server.listen(...extra);
    server.key = server.makeKey("admin", true);
    return server;
  }

  /** Runs a nopacs command on the installation's local socket and answers its standard output. */
  cli(...args: string[]): string {
    const done = spawnSync(this.binary, args, { env: this.env, encoding: "utf8", timeout: 60_000 });
    if (done.status !== 0) throw new Error(`nopacs ${args.join(" ")}: ${done.stderr}`);
    return done.stdout;
  }

  /** Runs a nopacs-server command, built beside the nopacs binary, and answers its standard output. */
  serverCli(...args: string[]): string {
    const done = spawnSync(join(dirname(this.binary), "nopacs-server"), args, {
      env: this.env,
      encoding: "utf8",
      timeout: 60_000,
    });
    if (done.status !== 0) throw new Error(`nopacs-server ${args.join(" ")}: ${done.stderr}`);
    return done.stdout;
  }

  /** Makes a key on the local socket, and gives it admin on the installation if asked. */
  makeKey(holder: string, admin = false): string {
    const key = this.cli("key", "create", "--holder", holder, "--no-expiry").split("\n")[0]!.trim();
    if (!key.startsWith("nopacs_")) throw new Error("the first line of key create is the key");
    if (admin) this.cli("grant", "set", key.split("_")[1]!, "--role", "admin", "--installation");
    return key;
  }

  /** The address of the network listener, `127.0.0.1:<port>`. */
  get address(): string {
    return `127.0.0.1:${this.port}`;
  }

  /**
   * Starts the server on a loopback port it picks, with extra serve flags,
   * waits until it answers, and sets `port` to the one it logged.
   */
  async listen(...extra: string[]): Promise<void> {
    const logPath = join(this.work, "serve.log");
    const log = openSync(logPath, "w");
    const child = spawn(join(dirname(this.binary), "nopacs-server"), ["serve", "--listen", "127.0.0.1:0", ...extra], {
      env: this.env,
      stdio: ["ignore", log, log],
    });
    closeSync(log);
    this.process = child;
    const sock = join(this.data, "run", "nopacs.sock");
    for (let i = 0; i < 300; i++) {
      if (child.exitCode !== null) throw new Error(`nopacs serve exited: ${readFileSync(logPath, "utf8")}`);
      const port = loggedPort(readFileSync(logPath, "utf8"));
      if (port > 0 && existsSync(sock)) {
        try {
          this.cli("whoami");
          this.port = port;
          return;
        } catch {
          // The socket exists before the server answers on it.
        }
      }
      await sleep(50);
    }
    child.kill("SIGKILL");
    throw new Error("nopacs serve did not answer in 15 seconds");
  }

  /** Stops the server the way an operator does, and waits for it. */
  async stop(): Promise<void> {
    const child = this.process;
    if (!child || child.exitCode !== null) return;
    const exited = new Promise((resolve) => child.once("exit", resolve));
    child.kill("SIGTERM");
    await exited;
  }

  /** Stops the server and removes the installation. */
  async close(): Promise<void> {
    await this.stop();
    rmSync(this.work, { recursive: true, force: true });
  }
}
