"""The numbers of the proof of sending: how fast a session goes to Orthanc
by each kind of sender, with 1, 2 and 4 associations. sending.py runs it."""

import os
import time

import sendkit
from sendkit import CT, many


def seconds(j):
    """A job's time from its start to its end, by the times it answers."""
    def t(v):
        return time.mktime(time.strptime(v[:19], "%Y-%m-%dT%H:%M:%S")) + float("0." + (v[20:].rstrip("Z") or "0"))
    return t(j["endTime"]) - t(j["startTime"])


def bench(p, orthanc, port):
    """The numbers: one session of 400 small instances in 4 series, sent by
    each kind of sender with 1, 2 and 4 associations (requests at once)."""
    files, study = many(os.path.join(sendkit.CORPUS, CT[0]), os.path.join(p.work, "studies", "bench"), 400, "PRV-SEND-BENCH", series=4)
    held, labels = p.archive(port, files, study)
    size = sum(len(b) for b in held.values())
    for prefix, how in (("o", "DIMSE"), ("w", "STOW-RS")):
        for n in (1, 2, 4):
            orthanc.clear()
            j = p.wait_job(p.job("%s%d" % (prefix, n), "%s/%s" % labels))
            took = seconds(j)
            good = j["state"] == "JOB_STATE_DONE" and orthanc.count() == len(held)
            p.result("B %s with %d association(s): instances per second" % (how, n), good,
                     "%d instances (%.1f MiB) in %.2f s from the job's start to its end: %.0f instances/s, %.2f MiB/s; Orthanc holds %d"
                     % (len(held), size / 2**20, took, len(held) / took, size / 2**20 / took, orthanc.count()))
            p.numbers.append("%s, %d instances of %.1f KiB in 4 series, %d association(s): %.0f instances/s, %.2f MiB/s"
                             % (how, len(held), size / len(held) / 1024, n, len(held) / took, size / 2**20 / took))
    p.skip("B one large file", "a file of hundreds of MiB is left for the scale run, on a machine named for it")


