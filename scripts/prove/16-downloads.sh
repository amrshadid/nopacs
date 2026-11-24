# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "16. downloads whole, in ranges, and resumed", made
# by scripts/prove/reading.py (downloads) on the reading
# installation of section 13.
# shellcheck shell=bash

section "16. downloads whole, in ranges, and resumed"
if [ -n "$rd_pid" ]; then
	"$PYTHON" "$reading" --work "$work" downloads || true
else
	fail "16.0 the reading installation is there" "section 13 could not make or serve it"
fi
