"""A DICOM sender for the listener's tests, on pynetdicom: one action per
run, its outcome printed as one line of JSON. It is another implementation
of the protocol than the one the listener is built on, so what it sees is
evidence the listener speaks the standard, not only the library it uses.

  echo HOST PORT CALLING CALLED
  store HOST PORT CALLING CALLED FILE...
  propose HOST PORT CALLING CALLED SOPCLASS TRANSFERSYNTAX
  garbage HOST PORT CALLING CALLED

garbage associates, then sends a PDU of a type PS3.8 Table 9-11 does not
define on the association's socket, and waits for the listener's answer.

store sends each file from its path, which pynetdicom does without decoding
the data set (only the file meta information is read), so the bytes on the
wire are the file's own after its meta group.
"""

import json
import sys
import time

from pydicom import dcmread
from pynetdicom import AE


def associate(host, port, calling, called, contexts):
    ae = AE(ae_title=calling)
    ae.acse_timeout = 10
    ae.dimse_timeout = 60
    ae.network_timeout = 60
    for sop, ts in contexts:
        ae.add_requested_context(sop, ts)
    return ae.associate(host, int(port), ae_title=called)


def outcome(assoc):
    out = {}
    if assoc.is_established:
        out["assoc"] = "accepted"
    elif assoc.is_rejected:
        out["assoc"] = "rejected"
        out["rj"] = [assoc.acceptor.primitive.result, assoc.acceptor.primitive.result_source,
                     assoc.acceptor.primitive.diagnostic]
    elif assoc.is_aborted:
        out["assoc"] = "aborted"
    else:
        out["assoc"] = "failed"
    return out


def main(argv):
    action, host, port, calling, called = argv[1:6]
    rest = argv[6:]
    if action == "echo":
        assoc = associate(host, port, calling, called, [("1.2.840.10008.1.1", "1.2.840.10008.1.2")])
        out = outcome(assoc)
        if assoc.is_established:
            st = assoc.send_c_echo()
            out["statuses"] = [st.Status if "Status" in st else None]
            assoc.release()
    elif action == "store":
        contexts = []
        for path in rest:
            meta = dcmread(path, stop_before_pixels=True)
            pair = (str(meta.SOPClassUID), str(meta.file_meta.TransferSyntaxUID))
            if pair not in contexts:
                contexts.append(pair)
        assoc = associate(host, port, calling, called, contexts)
        out = outcome(assoc)
        if assoc.is_established:
            out["statuses"] = []
            for path in rest:
                st = assoc.send_c_store(path)
                out["statuses"].append(st.Status if "Status" in st else None)
            assoc.release()
    elif action == "propose":
        sop, ts = rest
        assoc = associate(host, port, calling, called, [(sop, ts)])
        out = outcome(assoc)
        if assoc.is_established:
            out["accepted_contexts"] = [cx.abstract_syntax for cx in assoc.accepted_contexts]
            out["rejected_contexts"] = [cx.abstract_syntax for cx in assoc.rejected_contexts]
            assoc.release()
    elif action == "garbage":
        assoc = associate(host, port, calling, called, [("1.2.840.10008.1.1", "1.2.840.10008.1.2")])
        out = outcome(assoc)
        if assoc.is_established:
            # Type 0x7F, length four, four bytes.
            assoc.dul.socket.send(bytes([0x7F, 0, 0, 0, 0, 4, 0, 0, 0, 0]))
            deadline = time.monotonic() + 20
            while assoc.is_alive() and not assoc.is_aborted and time.monotonic() < deadline:
                time.sleep(0.05)
            out["after"] = "aborted" if assoc.is_aborted else ("open" if assoc.is_established else "closed")
    else:
        raise SystemExit("unknown action " + action)
    print(json.dumps(out))


if __name__ == "__main__":
    main(sys.argv)
