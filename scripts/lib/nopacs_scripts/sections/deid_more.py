"""The steps of the proof of de-identification that judge what a person does
around the profile: the forward lookup from a Patient ID to its subject,
refused by GET and kept out of every log; lookup codes imported and then
written into an archived file; a version's described method record against
the archived file's; a trial run with no server; a release under a named
version; and a file too large for de-identification's memory held, then
released once the setting is raised. deid.py runs them."""

import hashlib
import json
import os
import subprocess
import time
import urllib.parse

from pydicom import dcmread
from pydicom.dataset import Dataset
from pydicom.sequence import Sequence
from pydicom.uid import generate_uid

from nopacs_scripts import deid as deidkit
from nopacs_scripts.sections import deid_forms
from nopacs_scripts.sections.deid_faults import held_by_name, release
from nopacs_scripts.sections.deid_steps import BINARY, KEYED
from nopacs_scripts.send import split_part10

GET_VALUE = "PRV-GET-LEAK-7731"
# The long names of the file that needs the check's memory: the forms
# searched for are made of them, each a few times their length.
NAMES = 16


def forward_lookup(p, st):
    """11.1 and 11.2: deid code answers, from a Patient ID read on standard
    input, the subject label the storage holds; the value is in neither the
    audit log nor the server's log; the same request by GET is refused, and
    its value is logged nowhere either."""
    out = p.run("deid", "code", "lab", "--patient-id", "-", "--json", stdin=deidkit.PATIENT_ID + "\n")
    res = json.loads(out.stdout) if out.returncode == 0 else {}
    subj = [s for s in res.get("subjects", []) if s.get("receiver") == "deid"]
    label = subj[0].get("subjectLabel") if subj else None
    on_storage = bool(label) and os.path.isdir(p.storage_path("main", "data/" + label))
    audit = p.run("audit", "list", "--tenant", "lab", "--json").stdout
    entries = json.loads(audit).get("entries", []) if audit.strip().startswith("{") else []
    code_entries = [e for e in entries if "DeidService/Code" in json.dumps(e)]
    with open(os.path.join(p.work, "serve.log")) as f:
        log = f.read()
    leaked = [w for w, text in (("audit log", audit), ("server log", log)) if deidkit.PATIENT_ID in text]
    p.result("11.1 deid code gives the subject label on the storage path, and the Patient ID is in neither log",
             out.returncode == 0 and label == st["subject"] and subj[0].get("held") and on_storage and code_entries and not leaked,
             "exit %d; receiver deid: subject %s, held %s (the main study's subject %s); data/%s on storage main: %s; audit entries of "
             "the call: %d; the value found in: %s" % (out.returncode, label, subj[0].get("held") if subj else "-", st["subject"], label,
                                                      on_storage, len(code_entries), leaked or "neither"),
             [out.stderr.strip()[:200]])
    p.needles.add_text("value sent by GET", GET_VALUE)
    message = json.dumps({"tenant": "lab", "attribute": "PatientID", "value": GET_VALUE})
    url = "http://localhost/nopacs.v1.DeidService/Code?encoding=json&message=" + urllib.parse.quote(message)
    sock = os.path.join(p.data, "run", "nopacs.sock")
    c = subprocess.run(["curl", "-s", "-o", "-", "-w", "\n%{http_code}", "--unix-socket", sock, url], capture_output=True, text=True,
                       timeout=60)
    body, _, status = c.stdout.rpartition("\n")
    time.sleep(1)
    with open(os.path.join(p.work, "serve.log")) as f:
        log = f.read()
    audit = p.run("audit", "list", "--tenant", "lab", "--json").stdout
    code = res.get("code", "")
    p.result("11.2 DeidService/Code by GET is refused, and the value it carried is logged nowhere",
             status not in ("200", "") and status.isdigit() and (not code or code not in body) and GET_VALUE not in log
             and GET_VALUE not in audit, "curl GET over the local socket: HTTP %s, body %r; the value in the server's log: %s, in the "
             "audit log: %s" % (status, body[:120], GET_VALUE in log, GET_VALUE in audit))


def lookup_codes(p):
    """11.3: codes imported into a lookup table are written into the
    archived file by a lookup rule; a value the table lacks holds its file;
    deid code answers the imported code."""
    d = p.build("cohort")
    csv = os.path.join(p.work, "cohort.csv")
    with open(csv, "w") as f:
        f.write("value,code\nPRV-COHORT-0001,COHORT_017\n")
    imp = p.mj("profile", "lookup", "import", "lab", "cohort", csv)
    src = p.corpus("CT_small.dcm")
    known = deidkit.rewrite(src, os.path.join(d, "known.dcm"), generate_uid(), generate_uid(), 1, patient="Cohorta^Varnish",
                            patient_id="PRV-COHORT-0001")[0]
    unknown = deidkit.rewrite(src, os.path.join(d, "unknown.dcm"), generate_uid(), generate_uid(), 1, patient="Cohorta^Absentia",
                              patient_id="PRV-COHORT-0002")[0]
    for f in (known, unknown):
        p.needles.from_file(f, "cohort")
    before = p.receptions("cohort")
    st = p.send("cohort", [known, unknown])
    recs = p.settle("cohort", before)
    rows = p.archived(recs)
    held = held_by_name(p, recs, {"known": known, "unknown": unknown})
    ds = p.read_archived(rows[0]) if len(rows) == 1 else None
    h = held.get("unknown")
    det = json.loads(h["details"]) if h else {}
    look = p.run("deid", "code", "lab", "--patient-id", "-", "--json", stdin="PRV-COHORT-0001\n")
    lookups = json.loads(look.stdout).get("lookups", []) if look.returncode == 0 else []
    p.result("11.3 an imported lookup code is written into the archived file; a value the table lacks holds its file",
             st == [0, 0] and imp.get("added") == 1 and ds is not None and str(ds.PatientID) == "COHORT_017" and h is not None
             and h["reason"] == "deid_failed" and det.get("cause") == "lookup_missing"
             and any(x.get("table") == "cohort" and x.get("code") == "COHORT_017" for x in lookups),
             "import: added %s; archived %d, its Patient ID (pydicom) %r; the other held as %s %s; deid code's lookups %s"
             % (imp.get("added"), len(rows), str(ds.PatientID) if ds is not None else "-", h["reason"] if h else "-", json.dumps(det),
                lookups))


def described_method(p, st):
    """11.4: profile describe's method record is what pydicom reads in an
    archived file that no pixel rule changed."""
    dsc = p.mj("profile", "describe", "lab", "research@1")
    m = dsc.get("method", {})
    _, ds = st["arch"]["rich"]
    values = list(ds.DeidentificationMethod) if ds.get("DeidentificationMethod") else []
    codes = [(str(i.CodeValue), str(i.CodingSchemeDesignator), str(i.CodeMeaning)) for i in ds.DeidentificationMethodCodeSequence]
    want = [(c.get("value"), c.get("scheme"), c.get("meaning")) for c in m.get("codes", [])]
    p.result("11.4 profile describe's method record equals (0012,0062), (0012,0063) and (0012,0064) of an archived file",
             m.get("methodValues") == values and want == codes and m.get("identityRemoved") == ds.PatientIdentityRemoved,
             "describe: %d values, %d codes, identity removed %r; rich's file: %d values, %d codes, %r; values equal %s, codes equal %s"
             % (len(m.get("methodValues", [])), len(want), m.get("identityRemoved"), len(values), len(codes), ds.PatientIdentityRemoved,
                m.get("methodValues") == values, want == codes), ["describe %s" % m.get("methodValues"), "file %s" % values])


def local_trial(p, st):
    """11.5: profile try --local, with the server stopped, gives the archived
    file's values, keyed values aside."""
    doc = os.path.join(p.work, "research-1-export.json")
    # The export as --json writes it: the version's answer, so the local
    # trial records the version's name, as a receiver does.
    with open(doc, "w") as f:
        f.write(p.m("profile", "export", "lab", "research@1", "--json"))
    p.stop()
    try:
        alive = p.run("whoami").returncode == 0
        out = subprocess.run([p.bin, "profile", "try", "--local", "--document", doc, st["files"]["rich"], "--json"], capture_output=True,
                             text=True, timeout=180)
    finally:
        p.serve()
        for r in p.receivers:
            p.wait_up(r)
    res = json.loads(out.stdout)["results"][0] if out.returncode == 0 else {}
    _, archived = st["arch"]["rich"]
    equal, keyed, differ = 0, 0, []
    for e in res.get("elements", []):
        if "/" in e["path"] or "[" in e["path"] or not e.get("presentAfter"):
            continue
        have = archived.get(int(e["path"], 16))
        text = deidkit.element_text(have) if have is not None else None
        if text is None or have.VR in BINARY:
            continue
        text = text.strip(" \x00")
        if text == e.get("after", "").strip(" \x00"):
            equal += 1
        elif e["action"] in KEYED:
            keyed += 1
        else:
            differ.append("%s %s: local %r, archive %r" % (e["path"], e["keyword"], e.get("after", "")[:40], text[:40]))
    p.result("11.5 profile try --local, the server stopped, gives the archived file's values, keyed values aside",
             not alive and out.returncode == 0 and res.get("verdict") == "TRIAL_VERDICT_PASSED" and not differ and equal > 20,
             "server answering: %s; exit %d, verdict %s; %d elements equal, %d keyed, %d differ"
             % (alive, out.returncode, res.get("verdict"), equal, keyed, len(differ)), differ + [out.stderr.strip()[:200]])


def release_with_version(p, faults):
    """11.6: a held file released naming another version is de-identified
    by that version."""
    h = faults.get("refused")
    if not h:
        p.result("11.6 quarantine release --profile de-identifies by the version named", False, "no held record to release")
        return
    recs, said = release(p, "deid", h["id"], "--profile", "basic@1")
    rows = p.archived(recs)
    ds = p.read_archived(rows[0]) if rows else None
    method = list(ds.DeidentificationMethod) if ds is not None and ds.get("DeidentificationMethod") else []
    p.result("11.6 quarantine release --profile basic@1 de-identifies the held file by basic@1",
             len(rows) == 1 and "basic@1" in method and rows[0]["version"] == p.versions["basic@1"],
             "the file held by research@1's refuse-flagged rule; release: %s; archived %d, (0012,0063) %s, catalog version id %s "
             "(basic@1 is %d)" % (said, len(rows), method[:3], rows[0]["version"] if rows else "-", p.versions["basic@1"]))


def dataset_sum(path):
    with open(path, "rb") as f:
        return hashlib.sha256(split_part10(f.read())[1]).hexdigest()


def check_memory(p):
    """11.7: with deid.memory at its least, 256 MiB, a file whose check needs
    more is held as check_too_large with its bytes; raised to 2 GiB, its
    release archives it. The check reads pixel data as a stream, so what it
    needs is set by the longest text and by the forms it searches for: a 5
    MiB Text Value (UT) in TIS 620, which the archive writes as 15 MiB of
    UTF-8 (under the 16 MiB a value may hold), and long names whose forms
    the search builds automata of."""
    d = p.build("memory")

    def heavy(ds):
        ds.SpecificCharacterSet = "ISO_IR 166"
        # 0xA1 is the Thai letter KO KAI in TIS 620.
        ds.add_new((0x0040, 0xA160), "UT", b"\xa1" * (5 << 20))
        items = []
        for i in range(NAMES):
            item = Dataset()
            item.PatientID = "Mem%dx%s" % (i, "q" * 40000)
            items.append(item)
        ds.OtherPatientIDsSequence = Sequence(items)
    path = deidkit.rewrite(p.corpus("CT_small.dcm"), os.path.join(d, "large.dcm"), generate_uid(), generate_uid(), 1,
                           patient="Memoria^Toolarge", patient_id="PRV-MEMORY-0001", extra=heavy)[0]
    p.needles.from_file(path, "memory")
    size = os.path.getsize(path)
    p.m("setting", "set", "deid.memory", "256MiB")
    time.sleep(2)
    try:
        before = p.receptions("deid")
        st = p.send("deid", [path])
        recs = p.settle("deid", before, timeout=600)
        h = held_by_name(p, recs, {"large": path}).get("large")
        det = json.loads(h["details"]) if h else {}
        same = bool(h) and h["content_kept"] and dataset_sum(p.incoming_path(h)) == dataset_sum(path)
        p.result("11.7 a file of %d MiB under deid.memory 256MiB is held as check_too_large, its bytes kept" % (size >> 20),
                 st == [0] and h is not None and h["reason"] == "deid_failed" and det.get("cause") == "check_too_large" and same,
                 "status %s; record %s %s; data set's SHA-256 equal to the file sent: %s" % (st, h["reason"] if h else "-", json.dumps(det), same))
    finally:
        p.m("setting", "set", "deid.memory", "2GiB")
        time.sleep(2)
    if not h:
        return
    recs, said = release(p, "deid", h["id"])
    rows = p.archived(recs)
    ds = dcmread(p.storage_path(rows[0]["storage"], rows[0]["storage_key"]), stop_before_pixels=True) if rows else None
    p.result("11.7 released once deid.memory is raised to 2GiB: archived de-identified",
             len(rows) == 1 and ds.PatientIdentityRemoved == "YES",
             "release: %s; archived %d, Patient Identity Removed %s" % (said, len(rows), ds.get("PatientIdentityRemoved") if ds else "-"))
    os.remove(path)


def method_word(p):
    """11.8: a patient whose family name is a word of the method record the
    server writes into every file ("NOPACS", in (0012,0063)) is archived,
    not held: the check after looks for the names a file was sent with, not
    in text the server wrote itself."""
    d = p.build("nopacs-name")
    path = deidkit.rewrite(p.corpus("CT_small.dcm"), os.path.join(d, "nopacs.dcm"), generate_uid(), generate_uid(), 1,
                           patient="NoPACs^Maria", patient_id="PRV-NOPACS-0001")[0]
    before = p.receptions("deid")
    st = p.send("deid", [path])
    recs = p.settle("deid", before)
    rows = p.archived(recs)
    held = p.held(recs)
    p.result("11.8 a patient named NoPACs^Maria is archived, not held for the word NoPACs the server writes in (0012,0063)",
             st == [0] and len(rows) == 1 and not held,
             "status %s; archived %d; held %s" % (st, len(rows), [(h["reason"], h["details"]) for h in held]))


def run(p, st, faults):
    forward_lookup(p, st)
    lookup_codes(p)
    described_method(p, st)
    local_trial(p, st)
    release_with_version(p, faults)
    method_word(p)
    deid_forms.run(p)
