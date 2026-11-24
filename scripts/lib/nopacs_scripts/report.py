"""How a check says it failed.

A check that cannot go on raises Failure with the line to print; the entry
(nopacs_scripts.entry) prints it and sets the exit status, so a function here
never ends the process itself.
"""


class Failure(BaseException):
    """A check that cannot go on. message is printed as it is, to standard
    error, or to standard output when stdout is set. It is not an Exception,
    so the handler of a step that records its own errors as a failed check
    does not take a failure that must stop the whole run."""

    def __init__(self, message, stdout=False):
        super().__init__(message)
        self.message = message
        self.stdout = stdout


def failing(prefix, stdout=False):
    """A function that raises Failure with prefix before the reason, for a
    check to name itself in what it says."""

    def fail(reason):
        raise Failure(prefix + reason, stdout=stdout)

    return fail
