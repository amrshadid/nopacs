# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "4. refused everywhere, the same way".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "4. refused everywhere, the same way"
make_key nogrant --no-expiry || fail "4.0 the keys of the refusals" "key create nogrant failed" "$(cat "$work/cli.err")"
make_key reader --no-expiry || fail "4.0 the keys of the refusals" "key create reader failed" "$(cat "$work/cli.err")"
cli "$data" /nopacs.v1.AccessService/SetGrant "*" grant set "$(cat "$work/keys/reader.id")" --role reader --tenant lite
make_key revoked --no-expiry || fail "4.0 the keys of the refusals" "key create revoked failed" "$(cat "$work/cli.err")"
cli "$data" /nopacs.v1.AccessService/RevokeKey "*" key revoke "$(cat "$work/keys/revoked.id")"
revoke_status=$CLI_STATUS
# The shortest expiry the command takes: its lengths are a whole number of
# s, m, h, d or w, so one second (the server takes any positive duration).
make_key expired --expires 1s || fail "4.0 the keys of the refusals" "key create --expires 1s failed" "$(cat "$work/cli.err")"
expired_made=$(date +%s)
cli "$data" /nopacs.v1.AccessService/GetKey "*" --json key show "$(cat "$work/keys/expired.id")"
life=$("$PYTHON" -c '
import json, sys, datetime
k = json.load(open(sys.argv[1]))["key"]
p = lambda s: datetime.datetime.strptime(s[:19], "%Y-%m-%dT%H:%M:%S").timestamp() + float("0" + s[19:].rstrip("Z"))
print("%.3f" % (p(k["expireTime"]) - p(k["createTime"])))' "$work/cli.out" 2> /dev/null || echo "?")
while [ $(($(date +%s) - expired_made)) -lt 2 ]; do sleep 0.2; done
"$PYTHON" -c '
import json, sys
keys = {}
for name in sys.argv[2:]:
    keys[name] = {"key": open("%s/keys/%s" % (sys.argv[1], name)).read().strip(), "id": open("%s/keys/%s.id" % (sys.argv[1], name)).read().strip()}
import os
fd = os.open(sys.argv[1] + "/keys.json", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
os.write(fd, json.dumps(keys).encode())' "$work" admin nogrant reader revoked expired
if [ "$revoke_status" = 0 ] && [ "$life" = 1.000 ]; then
	ok "4.0 the keys of the refusals" "no grant; reader of lite only; revoked (key revoke exit 0); expired: --expires 1s, the shortest the command takes, expire minus create = ${life} s, used 2 s after it was made"
else
	fail "4.0 the keys of the refusals" "revoke exit $revoke_status, expired key lifetime $life s" "$(cat "$work/cli.err")"
fi
"$PYTHON" "$helper" --work "$work" refusals --mode matrix --proto "$root/api/proto" --not-served "$root/api/not-served.txt" \
	--addr "127.0.0.1:$port" --ca "$work/cert.pem" --grpcurl "$GRPCURL" --pg-tenant pg --missing-tenant nosuch \
	--data-dir "$data" --lite-tenant lite || true
