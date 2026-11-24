"""The numbers of the proof of de-identification: files per second through
the archive step of a receiver that de-identifies and of one that archives
as received, for copies of the same study, and the cost of a pixel rule
per frame. deid.py runs it."""

import os
import time

from pydicom.uid import generate_uid

from nopacs_scripts import deid as deidkit
from nopacs_scripts.report import Failure


def timed(p, receiver, files):
    """Sends files to a receiver and times its archive step from the
    catalog: from the moment its reception enters the state its archive
    step runs in (its step time then) to its end. Returns (seconds,
    archived)."""
    before = p.receptions(receiver)
    p.send(receiver, files)
    start = None
    deadline = time.time() + 600
    while time.time() < deadline:
        rows = [r for r in p.query("SELECT id, state, step_at, ended, ended_at FROM receptions WHERE receiver = ?", receiver)
                if r["id"] not in before]
        if rows and start is None and rows[0]["state"] == "identified":
            start = rows[0]["step_at"]
        if rows and rows[0]["ended"]:
            if start is None:
                start = rows[0]["step_at"]
            recs = p.settle(receiver, before)
            return (rows[0]["ended_at"] - start) / 1e6, len(p.archived(recs))
        time.sleep(0.01)
    raise Failure("the reception of %s did not end" % receiver)


def bench(p, n=300, rounds=2):
    """9: n copies of one CT slice through each receiver, then five 512x512
    cine images of 30 frames with and without a pixel rule; each in turns,
    rounds times, the best of each kept, since other work on the machine
    only slows a run."""
    src = p.corpus("CT_small.dcm")
    rates = {"plain": [], "deid": []}
    for r in range(rounds):
        for receiver in ("plain", "deid"):
            d = p.build("bench-%s-%d" % (receiver, r))
            study, series = generate_uid(), generate_uid()

            def fresh(ds):
                ds.FrameOfReferenceUID = generate_uid()
                if receiver == "plain":
                    deidkit.foreign(ds)
            files = [deidkit.rewrite(src, os.path.join(d, "%04d.dcm" % i), study, series, i + 1,
                                     patient="Benchley^" + ("Plainus" if receiver == "plain" else "Archivus"),
                                     patient_id="PRV-BENCH-%s-%d" % (receiver.upper(), r), extra=fresh)[0] for i in range(n)]
            if receiver == "deid":
                p.needles.from_file(files[0], "bench")
            size = sum(os.path.getsize(f) for f in files)
            secs, got = timed(p, receiver, files)
            rates[receiver].append(got / secs)
            p.result("9 %d CT slices through %s (%s), round %d: files per second in the archive step" % (
                n, receiver, "as received" if receiver == "plain" else "research@1", r + 1), got == n,
                "%d archived of %.1f MiB in %.2f s: %.0f files/s" % (got, size / 2**20, secs, got / secs))
    plain, deid = max(rates["plain"]), max(rates["deid"])
    p.numbers.append("CT slices of 128x128x16 bits, %d per study, best of %d: as received %.0f files/s, de-identified "
                     "(nopacs/longitudinal:1) %.0f files/s, de-identification costs %.0f%% of the archive step's speed"
                     % (n, rounds, plain, deid, 100 * (1 - deid / plain)))
    frames = 30
    times = {"filled": [], "untouched": []}
    for r in range(rounds):
        for kind, manufacturer, burned in (("filled", "PROVE US", "YES"), ("untouched", "PROVE PLAIN", "NO")):
            d = p.build("pixels-%s-%d" % (kind, r))
            study = generate_uid()
            files = [deidkit.burned_in(os.path.join(d, "%d.dcm" % i), study, generate_uid(), i + 1, frames=frames, rows=512, cols=512,
                                       manufacturer=manufacturer, burned=burned,
                                       patient="Pixelby^" + ("Fillmore" if kind == "filled" else "Untoucha"),
                                       patient_id="PRV-PIXEL-%s-%d" % (kind.upper(), r)) for i in range(5)]
            p.needles.from_file(files[0], "pixels")
            secs, got = timed(p, "deid", files)
            times[kind].append(secs)
            p.result("9 five 512x512 cine images of %d frames, %s by a pixel rule, round %d: time of the archive step"
                     % (frames, kind, r + 1), got == 5, "%d archived in %.2f s" % (got, secs))
    filled, untouched = min(times["filled"]), min(times["untouched"])
    per = (filled - untouched) / (5 * frames) * 1000
    p.numbers.append("pixel rule on 512x512 8-bit frames, best of %d: %.2f ms a frame more than the same images untouched (%d frames "
                     "in %.2f s against %.2f s)" % (rounds, per, 5 * frames, filled, untouched))
