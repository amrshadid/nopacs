# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "18. the Python client against the real server", made
# by scripts/prove/reading.py (client) on the reading
# installation of section 13.
# shellcheck shell=bash

section "18. the Python client against the real server"
if [ -n "$rd_pid" ]; then
	"$PYTHON" "$reading" --work "$work" client || true
else
	fail "18.0 the reading installation is there" "section 13 could not make or serve it"
fi
