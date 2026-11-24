# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "6. a tenant stops alone".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "6. a tenant stops alone"
make_key tenants --no-expiry || fail "6.0 a key for both tenants" "key create failed"
cli "$data" /nopacs.v1.AccessService/SetGrant "*" grant set "$(cat "$work/keys/tenants.id")" --role reader --tenant lite
cli "$data" /nopacs.v1.AccessService/SetGrant "*" grant set "$(cat "$work/keys/tenants.id")" --role reader --tenant pg
lite_req='{"scope":{"tenant":"lite"}}'
pg_req='{"scope":{"tenant":"pg"}}'
# time_lite FILE N: N calls to the SQLite tenant over TLS, their times in FILE,
# each followed by a WhoAmI, which touches no tenant, its time in FILE.ref:
# the reference of the same moments, so a machine that is busy at that time
# is not taken for a tenant slowed by another. Prints how many calls to the
# tenant did not answer 200.
time_lite() {
	local i bad=0
	: > "$1"
	: > "$1.ref"
	for i in $(seq 1 "$2"); do
		api tls tenants nopacs.v1.SettingsService/ListSettings "$lite_req" lite
		echo "$API_TIME" >> "$1"
		[ "$API_STATUS" = 200 ] || bad=$((bad + 1))
		api tls tenants nopacs.v1.AccessService/WhoAmI '{}'
		echo "$API_TIME" >> "$1.ref"
	done
	echo "$bad"
}
tenant_state() {
	api tls admin nopacs.v1.TenantService/GetTenant "{\"name\":\"$1\"}" "$1"
	printf '%s' "$API_BODY" | json_get '["tenant"]["state"]' 2> /dev/null || echo "status $API_STATUS"
}
bad_before=$(time_lite "$work/lite.before" 30)
read -r before_med before_max <<< "$(median_max "$work/lite.before")"
api tls tenants nopacs.v1.SettingsService/ListSettings "$pg_req" pg
pg_before=$API_STATUS

# Stopped with "docker stop": the database's server goes away as on a crash
# of its machine, here and in CI (where it is the job's service container).
stopped_at=$(now)
"$DOCKER" stop -t 5 "$PG_CONTAINER" > /dev/null
state=""
for i in $(seq 1 120); do
	state=$(tenant_state pg)
	[ "$state" = TENANT_STATE_READY ] || break
	sleep 1
done
detected=$("$PYTHON" -c "import time; print('%.1f' % (time.time() - $stopped_at))")
if [ "$state" = TENANT_STATE_UNREACHABLE ]; then
	ok "6.1 the Postgres tenant becomes unreachable when its database stops" "docker stop of the container; GetTenant says $state after ${detected} s (the server checks every tenant each 30 s)"
else
	fail "6.1 the Postgres tenant becomes unreachable when its database stops" "after ${detected} s: $state" "$API_BODY"
fi
api tls tenants nopacs.v1.SettingsService/ListSettings "$pg_req" pg
pg_down_status=$API_STATUS
pg_down_time=$(awk -v t="$API_TIME" 'BEGIN { printf "%.1f", t * 1000 }')
pg_down_body=$API_BODY
if [ "$pg_down_status" = 503 ] && printf '%s' "$pg_down_body" | grep -q '"code":"unavailable"'; then
	ok "6.2 the stopped tenant is answered unavailable" "ListSettings on pg: 503 unavailable in $pg_down_time ms: $pg_down_body"
else
	fail "6.2 the stopped tenant is answered unavailable" "status $pg_down_status in $pg_down_time ms" "$pg_down_body"
fi
bad_during=$(time_lite "$work/lite.during" 30)
read -r during_med during_max <<< "$(median_max "$work/lite.during")"
read -r ref_med ref_max <<< "$(median_max "$work/lite.during.ref")"
# "Its usual time": the median no worse than twice the median before plus
# 10 ms, or than twice that of the calls touching no tenant made in between
# plus 10 ms (when the machine itself was slower then); and no call over
# half a second. All are far below the 30 s a call would wait on a dead
# database.
if [ "$bad_before" = 0 ] && [ "$bad_during" = 0 ] && [ "$pg_before" = 200 ] &&
	awk -v b="$before_med" -v r="$ref_med" -v d="$during_med" -v m="$during_max" 'BEGIN { exit !((d <= 2 * b + 10 || d <= 2 * r + 10) && m <= 500) }'; then
	ok "6.3 the SQLite tenant answers in its usual time while Postgres is down" "30 calls each over TLS: before median $before_med ms, max $before_max ms; during median $during_med ms, max $during_max ms (WhoAmI between them: median $ref_med ms)"
else
	fail "6.3 the SQLite tenant answers in its usual time while Postgres is down" "before median $before_med max $before_max ms ($bad_before not 200); during median $during_med max $during_max ms ($bad_during not 200); WhoAmI between them median $ref_med ms; pg before: $pg_before"
fi
"$DOCKER" start "$PG_CONTAINER" > /dev/null
wait_pg || true
back_at=$(now)
for i in $(seq 1 180); do
	state=$(tenant_state pg)
	[ "$state" != TENANT_STATE_READY ] || break
	sleep 1
done
back=$("$PYTHON" -c "import time; print('%.1f' % (time.time() - $back_at))")
api tls tenants nopacs.v1.SettingsService/ListSettings "$pg_req" pg
if [ "$state" = TENANT_STATE_READY ] && [ "$API_STATUS" = 200 ] && kill -0 "$main_pid" 2> /dev/null; then
	ok "6.4 the Postgres tenant is ready again without a restart" "$state ${back} s after Postgres answered; same server process ($main_pid); ListSettings on pg: 200"
else
	fail "6.4 the Postgres tenant is ready again without a restart" "after ${back} s: $state; ListSettings $API_STATUS" "$API_BODY"
fi
