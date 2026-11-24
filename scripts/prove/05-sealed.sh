# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "5. sealed: the server with another key file".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "5. sealed: the server with another key file"
# A copy of the installation, its databases copied by sqlite3's online backup
# while the server runs, given the key file of another installation. Its run/
# holds the empty lock file a stopped server leaves, as an installation whose
# key file was swapped after it had been served would. Each tenant's own log
# is given an entry first (a read of its settings, by request id), so the
# copy can be asked for both tenants' data; the pause lets the batching
# audit writer put both entries in their logs before the copy is taken.
api sock "" nopacs.v1.SettingsService/ListSettings '{"scope":{"tenant":"lite"}}' lite
rid_lite=$API_RID
api sock "" nopacs.v1.SettingsService/ListSettings '{"scope":{"tenant":"pg"}}' pg
rid_pg=$API_RID
sleep 2
cli "$data" /nopacs.v1.AccessService/ListKeys "*" --json key list
main_keys=$(grep -c '"holder"' "$work/cli.out" || true)
copy=$work/c
mkdir -m 700 "$copy" "$copy/run" "$copy/tenants"
"$SQLITE3" "$data/master.db" ".backup '$copy/master.db'"
"$SQLITE3" "$data/tenants/lite.db" ".backup '$copy/tenants/lite.db'"
cp "$data/installation" "$copy/installation"
: > "$copy/run/serve.lock"
"$sbin" init --data-dir "$work/o" > /dev/null
cp "$work/o/key" "$copy/key"
chmod 600 "$copy/key" "$copy/installation" "$copy/master.db" "$copy/tenants/lite.db" "$copy/run/serve.lock"
# tree_sums DIR: every entry under DIR with its type, mode and, for a file,
# the SHA-256 of its bytes, one line each, in a fixed order.
tree_sums() {
	local p
	(cd "$1" && find . | LC_ALL=C sort) | while IFS= read -r p; do
		if [ -f "$1/$p" ]; then
			printf '%s file %s %s\n' "$p" "$(mode_of "$1/$p")" "$(sha256hex < "$1/$p")"
		else
			printf '%s %s %s\n' "$p" "$(test -d "$1/$p" && echo dir || echo other)" "$(mode_of "$1/$p")"
		fi
	done
}
tree_sums "$copy" > "$work/copy.before"
cport=$(free_port)
refused=0
if cmp -s "$data/key" "$copy/key"; then refused="same key file"; fi
"$sbin" serve --data-dir "$copy" --listen "127.0.0.1:$cport" --tls-cert "$work/cert.pem" --tls-key "$work/key.pem" > "$work/copy.log" 2>&1 &
cpid=$!
track "$cpid"
for i in $(seq 1 150); do kill -0 "$cpid" 2> /dev/null || break; sleep 0.1; done
# The port is tried before a server that did not exit is stopped, so a
# server that listened is seen listening.
port_status=0
curl -sS --max-time 3 -o /dev/null --cacert "$work/cert.pem" "https://127.0.0.1:$cport/grpc.health.v1.Health/Check" 2> /dev/null || port_status=$?
socket_made=no
[ -e "$copy/run/nopacs.sock" ] && socket_made=yes
if kill -0 "$cpid" 2> /dev/null; then
	kill -KILL "$cpid"
fi
wait "$cpid" 2> /dev/null || refused=$?
untrack "$cpid"
line=$(grep -v '^{' "$work/copy.log" | head -1 || true)
if [ "$refused" = 1 ] && printf '%s' "$line" | grep -q 'the key file is not the one this installation was made with; nothing was changed' &&
	[ "$socket_made" = no ] && [ "$port_status" = 7 ]; then
	ok "5.7 serve refuses another key file before it opens anything" "exit 1: '$line'; no socket made, the TLS port refuses connections (curl exit 7)"
else
	fail "5.7 serve refuses another key file before it opens anything" "exit $refused, socket made: $socket_made, curl on the port exit $port_status" "$(cat "$work/copy.log")"
fi
tree_sums "$copy" > "$work/copy.after"
if cmp -s "$work/copy.before" "$work/copy.after"; then
	ok "5.8 the refused start changed nothing in the data directory" "$(grep -c ' file ' "$work/copy.after") files and $(grep -c ' dir ' "$work/copy.after") directories: names, modes and SHA-256 of every file the same before and after"
else
	fail "5.8 the refused start changed nothing in the data directory" "the tree differs" "$(diff "$work/copy.before" "$work/copy.after")"
fi
# The installation's identity is its installation file and the master's
# copy of it; the key file only seals. The id must not be in the key file.
inst_id=$(tr -d '\n' < "$copy/installation")
"$SQLITE3" "$copy/master.db" .dump > "$work/master.backup.dump"
if [ -n "$inst_id" ] && ! grep -qF "$inst_id" "$data/key" && grep -qF "$inst_id" "$work/master.backup.dump"; then
	ok "5.9 the installation's identity is not in its key file" "the id in the installation file (${#inst_id} bytes) is in the master and not in the key file"
else
	fail "5.9 the installation's identity is not in its key file" "id ${#inst_id} bytes; in the key file: $(grep -cF "$inst_id" "$data/key"); in the master: $(grep -cF "$inst_id" "$work/master.backup.dump")"
fi
# The installation's own key file put back: the same copy starts, and both
# tenants are served. The Postgres tenant is the same database the main
# server uses; it is only read here.
cp "$data/key" "$copy/key"
chmod 600 "$copy/key"
if start_server "$work/copy.log" "$copy"; then
	copy_pid=$SERVER_PID
	cli "$copy" "" "" --json tenant list
	cp "$work/cli.out" "$work/copy-tenants.json"
	t_state() { "$PYTHON" -c 'import json, sys; t = [t for t in json.load(open(sys.argv[1]))["tenants"] if t["name"] == sys.argv[2]]; print(t[0]["state"] if t else "absent")' "$work/copy-tenants.json" "$1"; }
	pgs=$(t_state pg)
	lis=$(t_state lite)
	cli "$copy" "" "" setting list --tenant lite
	lite_cli=$CLI_STATUS
	cli "$copy" "" "" --json tenant show pg
	pg_show=$(json_get '["tenant"]["state"]' < "$work/cli.out" 2> /dev/null || echo "exit $CLI_STATUS")
	if [ "$pgs" = TENANT_STATE_READY ] && [ "$lis" = TENANT_STATE_READY ] && [ "$lite_cli" = 0 ] && [ "$pg_show" = TENANT_STATE_READY ]; then
		ok "5.10 with its own key file put back, the same copy starts and serves both tenants" "pg $pgs (tenant show: $pg_show), lite $lis (setting list --tenant lite exit 0)"
	else
		fail "5.10 with its own key file put back, the same copy starts and serves both tenants" "pg $pgs / $pg_show, lite $lis, setting list exit $lite_cli" "$(cat "$work/copy-tenants.json")" "$(tail -5 "$work/copy.log")"
	fi
	# The data the copy was made with, read back through it: the entry made
	# above in each tenant's own log, and every key. Reading the Postgres
	# tenant's log is itself recorded in that log, which is the main
	# server's too, so it is entered in the ledger like the main server's
	# requests.
	cli "$copy" "" "" --json audit list --tenant lite --limit 1000
	lite_has=$(grep -c "\"requestId\": \"$rid_lite\"" "$work/cli.out" || true)
	cli "$copy" /nopacs.v1.AuditService/ListAuditEntries pg --json audit list --tenant pg --limit 1000
	pg_has=$(grep -c "\"requestId\": \"$rid_pg\"" "$work/cli.out" || true)
	cli "$copy" "" "" --json key list
	copy_keys=$(grep -c '"holder"' "$work/cli.out" || true)
	if [ -n "$rid_lite" ] && [ -n "$rid_pg" ] && [ "$lite_has" = 1 ] && [ "$pg_has" = 1 ] && [ "$copy_keys" = "$main_keys" ] && [ "$main_keys" -ge 5 ]; then
		ok "5.11 the copy started with its own key file keeps both tenants' data" "the entry of request $rid_lite in lite's own log and of $rid_pg in pg's, read through the copy; $copy_keys keys, as many as the installation it was copied from"
	else
		fail "5.11 the copy started with its own key file keeps both tenants' data" "lite's entry found $lite_has times, pg's $pg_has (want 1 each); keys $copy_keys of $main_keys" "$(cat "$work/cli.err")"
	fi
	stop_server "$copy_pid"
else
	fail "5.10 with its own key file put back, the same copy starts and serves both tenants" "the copy did not start" "$(tail -20 "$work/copy.log")"
fi
