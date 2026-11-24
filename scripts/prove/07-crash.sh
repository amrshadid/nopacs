# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "7. nothing half-made after a crash".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "7. nothing half-made after a crash"
# A separate installation, so the kills lose nothing the audit check of
# section 8 counts on (a killed server may lose the batched entries of its
# last moment, as the contract says).
crash=$work/k
"$sbin" init --data-dir "$crash" > /dev/null
start_server "$work/crash.log" "$crash" || { fail "7.1 after each kill the tenant is whole or absent" "the server did not start" "$(tail -20 "$work/crash.log")"; }
crash_pid=$SERVER_PID
want_pg_tables=$(printf '%s' "$pg_tables" | wc -w | tr -d ' ')
: > "$work/crash.tsv"
lo=0
hi=200
odd=""
bad_integrity=""
for i in $(seq 1 "$CRASHES"); do
	db=nopacs_proof_crash_$i
	psql_su -c "create database $db owner nopacs_proof" > /dev/null
	NOPACS_PROOF_CRASH=$(pg_dsn "$db")
	secret "$NOPACS_PROOF_CRASH" search
	# The first kills are spread over 0 to 120 ms after the command starts;
	# the rest aim between the latest kill that came before the creation
	# and the earliest that came after it, where a kill lands inside it.
	if [ "$i" -le 8 ] || [ $((hi - lo)) -le 2 ]; then
		ms=$((RANDOM % 121))
	else
		ms=$((lo + 1 + RANDOM % (hi - lo - 1)))
	fi
	NOPACS_PROOF_CRASH=$NOPACS_PROOF_CRASH NOPACS_DATA_DIR=$crash "$bin" tenant create "c$i" --db-env NOPACS_PROOF_CRASH > /dev/null 2>&1 &
	cpid=$!
	sleep "$(printf '%d.%03d' $((ms / 1000)) $((ms % 1000)))"
	kill -KILL "$crash_pid" 2> /dev/null || true
	wait "$crash_pid" 2> /dev/null || true
	untrack "$crash_pid"
	wait "$cpid" 2> /dev/null || true
	row=$("$SQLITE3" "$crash/master.db" "select state from tenants where name = 'c$i'")
	integrity=$("$SQLITE3" "$crash/master.db" 'PRAGMA integrity_check' | tr '\n' ' ')
	tables=$(psql_su -d "$db" -c "select count(*) from pg_tables where schemaname = 'public'")
	case $row in
		"") class=before; [ "$ms" -gt "$lo" ] && lo=$ms ;;
		upgrading) class=mid ;;
		ready) class=after; [ "$ms" -lt "$hi" ] && hi=$ms ;;
		*) class="odd:$row" ;;
	esac
	[ "$integrity" = "ok " ] || bad_integrity="$bad_integrity kill $i: $integrity;"
	if start_server "$work/crash.log" "$crash"; then
		crash_pid=$SERVER_PID
		cli "$crash" "" "" --json tenant list
		after=$("$PYTHON" -c 'import json, sys; t = [t for t in json.load(open(sys.argv[1]))["tenants"] if t["name"] == sys.argv[2]]; print(t[0]["state"] if t else "absent")' "$work/cli.out" "c$i" 2> /dev/null || echo "unreadable")
		rows_after=$("$SQLITE3" "$crash/master.db" "select count(*) from tenants where name = 'c$i'")
		tables_after=$(psql_su -d "$db" -c "select count(*) from pg_tables where schemaname = 'public'")
		case $after in
			TENANT_STATE_READY)
				cli "$crash" "" "" setting list --tenant "c$i"
				[ "$CLI_STATUS" = 0 ] && [ "$tables_after" = "$want_pg_tables" ] || odd="$odd kill $i: ready but setting list exit $CLI_STATUS, $tables_after tables;"
				;;
			absent) [ "$rows_after" = 0 ] || odd="$odd kill $i: not listed but $rows_after master rows;" ;;
			*) odd="$odd kill $i: $after after the restart;" ;;
		esac
	else
		after="no restart"
		odd="$odd kill $i: the server did not start again: $(tail -3 "$work/crash.log" | tr '\n' ' ');"
		break
	fi
	case $class in odd:*) odd="$odd kill $i: master row state $row right after the kill;" ;; esac
	printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$i" "$ms" "$class" "$tables" "$after" "$integrity" >> "$work/crash.tsv"
done
kills=$(wc -l < "$work/crash.tsv" | tr -d ' ')
n_before=$(awk -F '\t' '$3 == "before"' "$work/crash.tsv" | wc -l | tr -d ' ')
n_mid=$(awk -F '\t' '$3 == "mid"' "$work/crash.tsv" | wc -l | tr -d ' ')
n_after=$(awk -F '\t' '$3 == "after"' "$work/crash.tsv" | wc -l | tr -d ' ')
mid_delays=$(awk -F '\t' '$3 == "mid" { printf "%s%s", sep, $2; sep = "," }' "$work/crash.tsv")
mid_ready=$(awk -F '\t' '$3 == "mid" && $5 == "TENANT_STATE_READY"' "$work/crash.tsv" | wc -l | tr -d ' ')
mid_partial=$(awk -F '\t' '$3 == "mid" && $4 > 0' "$work/crash.tsv" | wc -l | tr -d ' ')
if [ -z "$odd" ] && [ "$kills" -ge 20 ]; then
	ok "7.1 after each kill -9 during tenant create the tenant is whole and ready, or absent" "$kills kills, $kills restarts: $(awk -F '\t' '$5 == "TENANT_STATE_READY"' "$work/crash.tsv" | wc -l | tr -d ' ') ready (setting list answers, $want_pg_tables tables), $(awk -F '\t' '$5 == "absent"' "$work/crash.tsv" | wc -l | tr -d ' ') absent (no master row), 0 other"
else
	fail "7.1 after each kill -9 during tenant create the tenant is whole and ready, or absent" "$kills kills" "$odd" "$(cat "$work/crash.tsv")"
fi
if [ -z "$bad_integrity" ] && [ "$kills" -ge 20 ]; then
	ok "7.2 the master passes PRAGMA integrity_check after every kill" "$kills of $kills: ok (sqlite3, before each restart)"
else
	fail "7.2 the master passes PRAGMA integrity_check after every kill" "$bad_integrity"
fi
if [ "$n_mid" -gt 0 ]; then
	ok "7.3 kills landed inside the creation" "$n_mid of $kills kills found the tenant's row upgrading (at $mid_delays ms; $mid_partial with some of its tables made), all $mid_ready finished ready by the next start; $n_before before the row, $n_after after it was ready"
else
	fail "7.3 kills landed inside the creation" "none of $kills kills found the row upgrading: $n_before before, $n_after after; the check proved nothing" "$(cat "$work/crash.tsv")"
fi
stop_server "$crash_pid"
