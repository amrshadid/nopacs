# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "5. stored hashed and sealed".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "5. stored hashed and sealed"
# The databases and files at rest, after the clean stop.
search_n=$(wc -l < "$work/search" | tr -d ' ')
# found FILE...: the files in which any secret of the search list is found,
# searched as bytes; prints their names, never what matched.
found() { grep -laF -f "$work/search" "$@" 2> /dev/null || true; }
# where_found DUMP: which secret (by fingerprint) is on which line of a dump,
# and in which table's rows; never the line itself.
where_found() {
	local s
	while IFS= read -r s; do
		(umask 077 && printf '%s\n' "$s" > "$work/one")
		grep -naF -f "$work/one" "$1" | head -3 |
			awk -v fp="$(fingerprint "$s")" '{
				n = $0; sub(/:.*/, "", n); sub(/^[0-9]+:/, "")
				w = ""; if ($1 == "INSERT" || $1 == "COPY") { w = ($1 == "COPY") ? $2 : $3; sub(/\(.*/, "", w); w = ", rows of " w }
				print fp ": line " n w }'
	done < "$work/search"
	rm -f "$work/one"
}
"$SQLITE3" "$data/master.db" .dump > "$work/master.dump"
f=$(found "$work/master.dump")
if [ -z "$f" ] && [ -s "$work/master.dump" ]; then
	ok "5.1 no secret in the master's dump" "$search_n secrets (6 keys' secrets whole and alone, the Postgres password plain and URL-escaped, the addresses) absent from sqlite3 .dump ($(wc -c < "$work/master.dump" | tr -d ' ') bytes)"
else
	fail "5.1 no secret in the master's dump" "found in the dump" "$(where_found "$work/master.dump")"
fi
"$DOCKER" exec "$PG_CONTAINER" pg_dump -U postgres nopacs_proof_main > "$work/tenant.dump"
f=$(found "$work/tenant.dump")
if [ -z "$f" ] && [ -s "$work/tenant.dump" ]; then
	ok "5.2 no secret in the Postgres tenant's pg_dump" "absent from pg_dump of nopacs_proof_main ($(wc -c < "$work/tenant.dump" | tr -d ' ') bytes, its audit log included)"
else
	fail "5.2 no secret in the Postgres tenant's pg_dump" "found in the dump" "$(where_found "$work/tenant.dump")"
fi
nfiles=$(find "$data" -type f | wc -l | tr -d ' ')
nbytes=$(find "$data" -type f -exec cat {} + | wc -c | tr -d ' ')
f=$(found -r "$data")
if [ -z "$f" ]; then
	ok "5.3 no secret in any file under the data directory" "$nfiles files, $nbytes bytes, searched as bytes (grep -a): $(cd "$data" && find . -type f | sed 's|^\./||' | sort | tr '\n' ' ')"
else
	fail "5.3 no secret in any file under the data directory" "found in: $f"
fi
# The hash the master keeps for each key: the SHA-256 of its secret part.
missing_hash=""
for k in admin nogrant reader revoked expired tenants; do
	h=$(printf %s "$(cut -d _ -f 3 < "$work/keys/$k")" | sha256hex)
	grep -qi "X'$h'" "$work/master.dump" || missing_hash="$missing_hash $k"
done
if [ -z "$missing_hash" ]; then
	ok "5.4 each key's SHA-256 is in the master" "6 keys: the SHA-256 of each secret found as X'...' in the dump, and nothing else of it"
else
	fail "5.4 each key's SHA-256 is in the master" "missing for:$missing_hash"
fi

# The audit logs, read through the API on a restart, reconciled for section 8
# and searched here.
if start_server "$work/serve.log" "$data" --listen "127.0.0.1:$port" --tls-cert "$work/cert.pem" --tls-key "$work/key.pem" --log-level debug; then
	main_pid=$SERVER_PID
	"$PYTHON" "$helper" --work "$work" audit --sock "$sock" --cutoff "$cutoff" || true
	n_entries=$("$PYTHON" -c 'import json, sys; print(len(json.load(open(sys.argv[1]))))' "$work/audit.json" 2> /dev/null || echo 0)
	f=$(found "$work/audit.json")
	if [ -z "$f" ] && [ "$n_entries" -gt 0 ]; then
		ok "5.5 no secret in the audit log listed through the API" "$n_entries entries of every log, as JSON ($(wc -c < "$work/audit.json" | tr -d ' ') bytes)"
	else
		fail "5.5 no secret in the audit log listed through the API" "$n_entries entries; found: $f"
	fi
	stop_server "$main_pid"
else
	fail "8.1 every request the proof made is in the audit log, with its outcome" "the server did not start again" "$(tail -10 "$work/serve.log")"
fi
logs="$work/serve.log $work/copy.log $work/crash.log"
f=$(found $logs)
debug_lines=$(grep -c '"level":"DEBUG"' "$work/serve.log" || true)
if [ -z "$f" ] && [ "$debug_lines" -gt 0 ]; then
	ok "5.6 no secret in the servers' logs" "$(cat $logs | wc -l | tr -d ' ') lines, the main server's at debug level ($debug_lines debug lines), the copy's and the crashed installation's"
else
	fail "5.6 no secret in the servers' logs" "found in: $f; debug lines $debug_lines"
fi
