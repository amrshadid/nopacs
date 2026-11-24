"""Create a send job and follow its events until it ends.

Sends a subject (or a session or a scan of it) of a tenant to one of the
tenant's senders, then reads the tenant's event feed and prints a line for each
change of state and each report of progress of that job, until the job is done,
partial, failed or canceled.

    NOPACS_ADDRESS=127.0.0.1:8443 NOPACS_KEY=nopacs_... \\
        python follow_job.py lab1 pacs sub-0017

The key needs the operator role on the tenant. Without TLS only an address on
this machine is accepted (NOPACS_INSECURE=1 says the server has none).
"""

import os
import sys
import time
from typing import Iterator, Optional

import grpc

import nopacs
from nopacs.v1 import job_pb2, monitor_pb2, scope_pb2

# How long to wait before following the feed again after a stream ended
# without the job ending, so a server that ends streams at once is not asked
# in a tight loop.
PAUSE = 1.0

ENDED = (
    job_pb2.JOB_STATE_DONE,
    job_pb2.JOB_STATE_PARTIAL,
    job_pb2.JOB_STATE_FAILED,
    job_pb2.JOB_STATE_CANCELED,
)


def describe(job: job_pb2.Job) -> str:
    """Say in one line how far a job is."""
    c = job.counts
    answered = c.instances_sent + c.instances_warned + c.instances_refused
    return (
        f"{job_pb2.JobState.Name(job.state).removeprefix('JOB_STATE_').lower():8} "
        f"{answered} of {c.instances} instances answered, {c.instances_sent} sent, "
        f"{c.instances_refused} refused, {c.instances_remaining} remaining"
    )


def follow_job(
    client: nopacs.Client,
    tenant: str,
    sender: str,
    subject: str,
    session: str = "",
    scan: str = "",
    deadline: Optional[float] = None,
) -> Iterator[job_pb2.Job]:
    """Make a send job and yield it as it was made, then at each event of the feed
    that concerns it, the last time as it ended.

    The feed's position is taken before the job is made, so no event of the job is
    missed; an event of another job is passed over. A stream the server ends,
    cleanly or as UNAVAILABLE as it does when the stream's deadline passes, is
    followed again from the last event seen. ``deadline`` bounds each stream, in
    seconds, so a caller that must stop does.
    """
    scope = scope_pb2.Scope(tenant=tenant)
    status = client.monitor.GetStatus(monitor_pb2.GetStatusRequest(scope=scope))
    cursor = status.tenants[0].event_cursor if status.tenants else ""

    made = client.jobs.CreateJob(job_pb2.CreateJobRequest(
        tenant=tenant,
        send=job_pb2.SendJob(sender=sender, subject_label=subject, session_label=session, scan_label=scan),
    )).job
    job = made
    yield job

    types = [monitor_pb2.EVENT_TYPE_JOB_STEP, monitor_pb2.EVENT_TYPE_JOB_PROGRESS]
    while job.state not in ENDED:
        request = monitor_pb2.WatchRequest(scope=scope, cursor=cursor, types=types)
        try:
            for message in client.monitor.Watch(request, timeout=deadline):
                kind = message.WhichOneof("kind")
                if kind == "keep_alive":
                    # The position past events the filter left out.
                    cursor = message.keep_alive.cursor or cursor
                    continue
                event = message.event
                cursor = event.cursor
                payload = event.WhichOneof("payload")
                if payload == "job_step" and event.job_step.job == made.id:
                    job = job_pb2.Job(id=made.id, send=made.send, state=event.job_step.state,
                                      counts=event.job_step.counts, reason=event.job_step.reason)
                elif payload == "job_progress" and event.job_progress.job == made.id:
                    job = job_pb2.Job(id=made.id, send=made.send, state=job.state, counts=event.job_progress.counts)
                else:
                    continue
                yield job
                if job.state in ENDED:
                    return
        except nopacs.Error as err:
            # The server ends a stream at any time, and at the latest when its
            # deadline passes (30 seconds unless the call sets one), answering
            # UNAVAILABLE ("the stream ended; resume from its last cursors"); a
            # connection that dropped is UNAVAILABLE too. Both are followed on
            # from the last cursor. Any other failure is the caller's to see.
            if err.code != grpc.StatusCode.UNAVAILABLE:
                raise
        # The stream ended without the job ending: read the job, which may have
        # ended in an event the stream did not carry, and follow on.
        job = client.jobs.GetJob(job_pb2.GetJobRequest(tenant=tenant, id=made.id)).job
        if job.state not in ENDED:
            time.sleep(PAUSE)


def main(argv: list) -> int:
    """Run the example from the command line: tenant, sender, subject[/session[/scan]]."""
    if len(argv) != 4:
        print(__doc__, file=sys.stderr)
        return 2
    _, tenant, sender, target = argv
    parts = target.split("/")
    if not 1 <= len(parts) <= 3 or "" in parts:
        print("the target is <subject>[/<session>[/<scan>]]", file=sys.stderr)
        return 2
    address, key = os.environ["NOPACS_ADDRESS"], os.environ["NOPACS_KEY"]
    last = None
    with nopacs.Client(address, key, tls=not os.environ.get("NOPACS_INSECURE")) as client:
        for last in follow_job(client, tenant, sender, *parts):
            print(last.id, describe(last), flush=True)
    return 0 if last is not None and last.state == job_pb2.JOB_STATE_DONE else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
