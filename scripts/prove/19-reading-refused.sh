# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "19. reading is closed by default", made by scripts/prove/reading.py
# (closed) on the reading installation of section 13; then its server is
# stopped, as the last of the sections that use it.
# shellcheck shell=bash

section "19. reading is closed by default"
if [ -n "$rd_pid" ]; then
	"$PYTHON" "$reading" --work "$work" closed --sqlite3 "$SQLITE3" || true
	stop_server "$rd_pid"
	if [ "$STOP_STATUS" = 0 ]; then
		ok "19.5 the reading installation's server stops cleanly" "SIGTERM: exit 0"
	else
		fail "19.5 the reading installation's server stops cleanly" "exit $STOP_STATUS" "$(tail -5 "$work/rd-serve.log")"
	fi
	rd_pid=""
else
	fail "19.0 the reading installation is there" "section 13 could not make or serve it"
fi
