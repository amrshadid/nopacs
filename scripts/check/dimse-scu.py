#!/usr/bin/env python3
"""A DICOM sender for the DIMSE listener's tests: one action per run, its
outcome printed as one line of JSON.

    dimse-scu.py echo HOST PORT CALLING CALLED
    dimse-scu.py store HOST PORT CALLING CALLED FILE...
    dimse-scu.py propose HOST PORT CALLING CALLED SOPCLASS TRANSFERSYNTAX
    dimse-scu.py garbage HOST PORT CALLING CALLED

The work is in nopacs_scripts.scu; this file only starts it.
"""

import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import scu  # noqa: E402


def main(argv):
    action, host, port, calling, called = argv[1:6]
    try:
        out = scu.run(action, host, port, calling, called, argv[6:])
    except ValueError as err:
        raise SystemExit(str(err))
    print(json.dumps(out))


if __name__ == "__main__":
    main(sys.argv)
