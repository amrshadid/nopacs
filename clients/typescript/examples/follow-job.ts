/**
 * Create a send job and follow its events until it ends.
 *
 * Sends a subject (or a session or a scan of it) of a tenant to one of the
 * tenant's senders, then reads the tenant's event feed and reports each change
 * of state and each report of progress of that job, until the job is done,
 * partial, failed or canceled.
 *
 *     NOPACS_URL=http://127.0.0.1:8443 NOPACS_KEY=nopacs_... \
 *       node follow-job.js lab1 pacs sub-0017
 *
 * The key needs the operator role on the tenant. In your own project import
 * the client from "nopacs/node".
 */
import { JobState, type Job } from "../src/gen/nopacs/v1/job_pb.js";
import { EventType } from "../src/gen/nopacs/v1/monitor_pb.js";
import { Code, NopacsError, createNopacsClient } from "../src/node.js";

/** How long to wait before following the feed again after a stream ended without the job ending. */
const PAUSE_MS = 1000;

/** The states a job never leaves. */
const ENDED = new Set<JobState>([JobState.DONE, JobState.PARTIAL, JobState.FAILED, JobState.CANCELED]);

/** Whether a job's state is one it never leaves. */
export const hasEnded = (job: Pick<Job, "state">): boolean => ENDED.has(job.state);

/** Where a job is, and what it sends: the label path of its target. */
export interface FollowOptions {
  tenant: string;
  sender: string;
  subject: string;
  session?: string;
  scan?: string;
  /** Aborting it stops the following; the job goes on. */
  signal?: AbortSignal;
}

/** Says in one line how far a job is. */
export function describe(job: Job): string {
  const c = job.counts;
  const answered = (c?.instancesSent ?? 0n) + (c?.instancesWarned ?? 0n) + (c?.instancesRefused ?? 0n);
  return (
    `${JobState[job.state]!.toLowerCase().padEnd(8)} ${answered} of ${c?.instances ?? 0n} instances answered, ` +
    `${c?.instancesSent ?? 0n} sent, ${c?.instancesRefused ?? 0n} refused, ${c?.instancesRemaining ?? 0n} remaining`
  );
}

/**
 * Makes a send job and yields it as it was made, then at each event of the
 * feed that concerns it, the last time as it ended.
 *
 * The feed's position is taken before the job is made, so no event of the job
 * is missed; an event of another job is passed over. A stream the server ends,
 * cleanly or as UNAVAILABLE as it does when the stream's deadline passes, is
 * followed again from the last event seen.
 */
export async function* followJob(client: ReturnType<typeof createNopacsClient>, o: FollowOptions): AsyncGenerator<Job> {
  const scope = { kind: { case: "tenant", value: o.tenant } } as const;
  const status = await client.monitor.getStatus({ scope });
  let cursor = status.tenants[0]?.eventCursor ?? "";

  const { job: made } = await client.jobs.createJob({
    tenant: o.tenant,
    kind: { case: "send", value: { sender: o.sender, subjectLabel: o.subject, sessionLabel: o.session ?? "", scanLabel: o.scan ?? "" } },
  });
  let job = made!;
  yield job;

  while (!hasEnded(job)) {
    const stream = client.monitor.watch(
      { scope, cursor, types: [EventType.JOB_STEP, EventType.JOB_PROGRESS] },
      { signal: o.signal },
    );
    try {
      for await (const message of stream) {
        if (message.kind.case === "keepAlive") {
          // The position past events the filter left out.
          cursor = message.kind.value.cursor || cursor;
          continue;
        }
        if (message.kind.case !== "event") continue;
        const event = message.kind.value;
        cursor = event.cursor;
        const p = event.payload;
        if (p.case === "jobStep" && p.value.job === made!.id) {
          job = { ...made!, state: p.value.state, counts: p.value.counts, reason: p.value.reason };
        } else if (p.case === "jobProgress" && p.value.job === made!.id) {
          job = { ...made!, state: job.state, counts: p.value.counts };
        } else {
          continue;
        }
        yield job;
        if (hasEnded(job)) return;
      }
    } catch (err) {
      // The server ends a stream at any time, and at the latest when its
      // deadline passes (30 seconds unless the call sets one), answering
      // UNAVAILABLE ("the stream ended; resume from its last cursors"); a
      // connection that dropped is UNAVAILABLE too. Both are followed on from
      // the last cursor. Any other failure, or the caller's own abort, is the
      // caller's to see.
      if (o.signal?.aborted || !(err instanceof NopacsError) || err.code !== Code.Unavailable) throw err;
    }
    // The stream ended without the job ending: read the job, which may have
    // ended in an event the stream did not carry, and follow on.
    job = (await client.jobs.getJob({ tenant: o.tenant, id: made!.id })).job!;
    // A pause, so a server that ends streams at once is not asked in a tight loop.
    if (!hasEnded(job)) await new Promise((r) => setTimeout(r, PAUSE_MS));
  }
}

/** Runs the example from the command line: tenant, sender, subject[/session[/scan]]. */
export async function main(argv: string[], env: NodeJS.ProcessEnv, out: (line: string) => void): Promise<number> {
  const [tenant, sender, target] = argv;
  const parts = (target ?? "").split("/");
  if (argv.length !== 3 || parts.length > 3 || parts.includes("")) {
    out("usage: follow-job <tenant> <sender> <subject>[/<session>[/<scan>]]");
    return 2;
  }
  const client = createNopacsClient({ baseUrl: env.NOPACS_URL!, key: env.NOPACS_KEY! });
  let last: Job | undefined;
  for await (const job of followJob(client, { tenant: tenant!, sender: sender!, subject: parts[0]!, session: parts[1], scan: parts[2] })) {
    last = job;
    out(`${job.id} ${describe(job)}`);
  }
  return last?.state === JobState.DONE ? 0 : 1;
}
