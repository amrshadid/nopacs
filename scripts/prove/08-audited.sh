# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "8. audited".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "8. audited"
curl -sS --unix-socket "$sock" http://local/metrics > "$work/metrics.txt"
"$PYTHON" "$helper" --work "$work" metrics --file "$work/metrics.txt" || true
cutoff=$(now)
stop_server "$main_pid"
stopped=$(grep '"msg":"server: stopped"' "$work/serve.log" | tail -1)
if [ "$STOP_STATUS" = 0 ] && printf '%s' "$stopped" | grep -q '"audit_failed":0' && printf '%s' "$stopped" | grep -q '"audit_dropped":0'; then
	ok "8.4 a clean stop exits 0 and writes every held audit entry" "SIGTERM: exit 0; its last line: $(printf '%s' "$stopped" | sed 's/.*"msg":"server: stopped",//; s/}$//')"
else
	fail "8.4 a clean stop exits 0 and writes every held audit entry" "exit $STOP_STATUS" "$stopped" "$(tail -5 "$work/serve.log")"
fi
