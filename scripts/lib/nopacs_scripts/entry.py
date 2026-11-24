"""What an entry script does with a main(): run it and turn what it returns
or raises into the exit status. The only place here that knows the process
ends."""

import sys

from nopacs_scripts.report import Failure


def run(main, argv):
    """Calls main(argv) and returns the exit status for sys.exit: main's
    return value (None is success), or 1 after printing a Failure's message."""
    try:
        status = main(argv)
    except Failure as failure:
        print(failure.message, file=sys.stdout if failure.stdout else sys.stderr)
        return 1
    return status or 0
