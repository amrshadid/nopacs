"""A DICOM sender on pynetdicom for the listener's tests: one action per
call, its outcome returned as a dict. It is another implementation of the
protocol than the one the listener is built on, so what it sees is evidence
the listener speaks the standard, not only the library it uses.

Actions, each with the listener's host and port and the calling and called
AE titles:

  echo
  store FILE...
  propose SOPCLASS TRANSFERSYNTAX
  garbage

garbage associates, then sends a PDU of a type PS3.8 Table 9-11 does not
define on the association's socket, and waits for the listener's answer.

store sends each file from its path, which pynetdicom does without decoding
the data set (only the file meta information is read), so the bytes on the
wire are the file's own after its meta group.
"""

import time

from pydicom import dcmread
from pynetdicom import AE, evt
from pynetdicom.pdu import A_ASSOCIATE_RJ

VERIFICATION = "1.2.840.10008.1.1"
IMPLICIT_LITTLE_ENDIAN = "1.2.840.10008.1.2"


def associate(host, port, calling, called, contexts):
    """Requests an association with the (abstract syntax, transfer syntax)
    pairs in contexts and returns it, established or not, with the result,
    source and diagnostic of the A-ASSOCIATE-RJ PDU the listener sent, None
    when it sent none.

    The A-ASSOCIATE-RJ is taken from the PDU as it is decoded off the wire,
    not from what the association reports after. pynetdicom (2.1) answers an
    A-ASSOCIATE-RJ by closing its socket from its reactor thread (PS3.8 9.2,
    AE-4), while the thread that sent the request then looks at the socket to
    learn whether it connected: when the rejection arrives first, that thread
    finds the socket closed, takes it for a failed connection and aborts, and
    the association reports aborted although the RJ was received whole. What
    reached the caller is the PDU, so the PDU is what is reported."""
    ae = AE(ae_title=calling)
    ae.acse_timeout = 10
    ae.dimse_timeout = 60
    ae.network_timeout = 60
    for sop, ts in contexts:
        ae.add_requested_context(sop, ts)
    received = []

    def on_pdu(event):
        if isinstance(event.pdu, A_ASSOCIATE_RJ):
            received.append([event.pdu.result, event.pdu.source, event.pdu.reason_diagnostic])

    assoc = ae.associate(host, int(port), ae_title=called, evt_handlers=[(evt.EVT_PDU_RECV, on_pdu)])
    return assoc, (received[0] if received else None)


def outcome(assoc, rj):
    """Returns how the association request ended: "assoc" is accepted,
    rejected (with "rj", the result, source and diagnostic of the
    A-ASSOCIATE-RJ received, as associate returns it), aborted or failed."""
    out = {}
    if assoc.is_established:
        out["assoc"] = "accepted"
    elif rj is not None:
        out["assoc"] = "rejected"
        out["rj"] = rj
    elif assoc.is_aborted:
        out["assoc"] = "aborted"
    else:
        out["assoc"] = "failed"
    return out


def status_of(st):
    """Returns the Status of a DIMSE answer, None when it has none."""
    return st.Status if "Status" in st else None


def run(action, host, port, calling, called, rest):
    """Runs one action against the listener and returns its outcome as a
    dict of what the caller can print as JSON. An unknown action raises
    ValueError."""
    if action == "echo":
        assoc, rj = associate(host, port, calling, called, [(VERIFICATION, IMPLICIT_LITTLE_ENDIAN)])
        out = outcome(assoc, rj)
        if assoc.is_established:
            out["statuses"] = [status_of(assoc.send_c_echo())]
            assoc.release()
    elif action == "store":
        contexts = []
        for path in rest:
            meta = dcmread(path, stop_before_pixels=True)
            pair = (str(meta.SOPClassUID), str(meta.file_meta.TransferSyntaxUID))
            if pair not in contexts:
                contexts.append(pair)
        assoc, rj = associate(host, port, calling, called, contexts)
        out = outcome(assoc, rj)
        if assoc.is_established:
            out["statuses"] = [status_of(assoc.send_c_store(path)) for path in rest]
            assoc.release()
    elif action == "propose":
        sop, ts = rest
        assoc, rj = associate(host, port, calling, called, [(sop, ts)])
        out = outcome(assoc, rj)
        if assoc.is_established:
            out["accepted_contexts"] = [cx.abstract_syntax for cx in assoc.accepted_contexts]
            out["rejected_contexts"] = [cx.abstract_syntax for cx in assoc.rejected_contexts]
            assoc.release()
    elif action == "garbage":
        assoc, rj = associate(host, port, calling, called, [(VERIFICATION, IMPLICIT_LITTLE_ENDIAN)])
        out = outcome(assoc, rj)
        if assoc.is_established:
            # Type 0x7F, length four, four bytes.
            assoc.dul.socket.send(bytes([0x7F, 0, 0, 0, 0, 4, 0, 0, 0, 0]))
            deadline = time.monotonic() + 20
            while assoc.is_alive() and not assoc.is_aborted and time.monotonic() < deadline:
                time.sleep(0.05)
            out["after"] = "aborted" if assoc.is_aborted else ("open" if assoc.is_established else "closed")
    else:
        raise ValueError("unknown action " + action)
    return out
