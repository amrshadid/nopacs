# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "14. the catalog equals pydicom's reading", made
# by scripts/prove/reading.py (catalog) on the reading
# installation of section 13.
# shellcheck shell=bash

section "14. the catalog equals pydicom's reading"
if [ -n "$rd_pid" ]; then
	"$PYTHON" "$reading" --work "$work" catalog || true
else
	fail "14.0 the reading installation is there" "section 13 could not make or serve it"
fi
