#!/usr/bin/env bash
# The server checked by three clients that are not NoPACs: curl (JSON over
# HTTP/1.1), grpcurl (gRPC, Go) and Python's grpcio (gRPC, C core), over TLS
# on loopback and over the local socket, against two real server processes:
# one running the stand-in handlers (internal/testkit/cmd/standin), and
# the real server program (./cmd/nopacs-server) with the real handlers. Against the real
# one each client also creates and lists a tenant with an admin key, and a
# key with no grant is refused the creation and lists no tenant.
#
# Each client must get the same refusal for no key, a malformed key, an
# unknown key and a key with no grant, and success for an admin key over
# TLS and for the local operator on the socket. Every answer must carry a
# Nopacs-Request-Id. /metrics must parse with the Prometheus Python
# client. Against the real program, grpcurl and Python also walk the first
# and third uses of docs/api/reception-walkthrough.md (a tenant set up to
# receive: its ports, a directory storage tested, a receiver added and
# changed; and the review of quarantine, on a tenant with nothing held, with
# every refusal it names), each on a tenant of its own, under a directory
# root the script makes and sets as the installation's.
#
# Needs: go, curl, openssl, grpcurl (GRPCURL=...), and a Python (PYTHON=...)
# with scripts/requirements/check-server.txt installed.
#
#   make check-server PYTHON=/path/to/venv/bin/python GRPCURL=grpcurl
set -euo pipefail

GO=${GO:-go}
PYTHON=${PYTHON:-python3}
GRPCURL=${GRPCURL:-grpcurl}
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d /tmp/nopacs-check.XXXXXX)
server_pid=
cleanup() {
	if [ -n "$server_pid" ]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
	rm -rf "$work"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; [ -f "$work/serve.log" ] && tail -20 "$work/serve.log" >&2; exit 1; }
ok() { echo "ok   $*"; }

"$GO" build -o "$work/standin" "$root/internal/testkit/cmd/standin"
"$GO" build -o "$work/nopacs-server" "$root/cmd/nopacs-server"

# A certificate for 127.0.0.1, made for this run only.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
	-subj /CN=nopacs-check -addext subjectAltName=IP:127.0.0.1 \
	-keyout "$work/key.pem" -out "$work/cert.pem" 2> /dev/null

# check BINARY PORT REAL: every check against a fresh installation served by
# BINARY on PORT; REAL is 1 for the real program, whose handlers also serve
# tenants.
check() {
bin=$1 port=$2 real=$3
data=$work/d$port
"$bin" init --data-dir "$data" > /dev/null
"$bin" serve --data-dir "$data" --listen "127.0.0.1:$port" \
	--tls-cert "$work/cert.pem" --tls-key "$work/key.pem" > "$work/serve.log" 2>&1 &
server_pid=$!
sock=$data/run/nopacs.sock
for _ in $(seq 100); do [ -S "$sock" ] && break; sleep 0.1; done
[ -S "$sock" ] || fail "the server did not open its socket"
tls=https://127.0.0.1:$port

# post URL PROCEDURE BODY [curl options...]: prints "status body".
post() {
	local url=$1 proc=$2 body=$3
	shift 3
	curl -sS -o "$work/body" -D "$work/headers" -w '%{http_code}' -H 'Content-Type: application/json' \
		"$@" -d "$body" "$url$proc" > "$work/status"
	grep -qi '^nopacs-request-id: [a-z2-7]\{16\}' "$work/headers" || fail "$proc: no request id"
	echo "$(cat "$work/status") $(cat "$work/body")"
}
viatls() { post "$tls" "$@" --cacert "$work/cert.pem"; }
viasock() { post http://local "$@" --unix-socket "$sock"; }
json() { "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)'"$1"')'; }

# Two keys, made by the local operator on the socket: one with no grant, one
# admin on the installation.
out=$(viasock /nopacs.v1.AccessService/CreateKey '{"holder":"no grant","neverExpire":true}')
[ "${out%% *}" = 200 ] || fail "CreateKey: $out"
nogrant=$(echo "${out#* }" | json '["secret"]')
out=$(viasock /nopacs.v1.AccessService/CreateKey '{"holder":"admin","neverExpire":true}')
admin=$(echo "${out#* }" | json '["secret"]')
adminid=$(echo "${out#* }" | json '["key"]["id"]')
out=$(viasock /nopacs.v1.AccessService/SetGrant "{\"keyId\":\"$adminid\",\"scope\":{\"installation\":{}},\"role\":\"ROLE_ADMIN\"}")
[ "${out%% *}" = 200 ] || fail "SetGrant: $out"
unknown=nopacs_111111111111_1111111111111111111111111111111111111111111

unauth='401 {"code":"unauthenticated","message":"the request carries no valid key"}'
denied='403 {"code":"permission_denied","message":"the key may not do this"}'

echo "== curl"
lk=/nopacs.v1.AccessService/ListKeys
[ "$(viatls $lk '{}')" = "$unauth" ] || fail "curl tls no key: $(viatls $lk '{}')"; ok "curl tls no key: $unauth"
[ "$(viatls $lk '{}' -H 'Authorization: Bearer junk')" = "$unauth" ] || fail "curl tls malformed key"; ok "curl tls malformed key: same"
[ "$(viatls $lk '{}' -H "Authorization: Bearer $unknown")" = "$unauth" ] || fail "curl tls unknown key"; ok "curl tls unknown key: same"
[ "$(viatls $lk '{}' -H "Authorization: Bearer $nogrant")" = "$denied" ] || fail "curl tls no grant"; ok "curl tls key with no grant: $denied"
out=$(viatls $lk '{}' -H "Authorization: Bearer $admin"); [ "${out%% *}" = 200 ] || fail "curl tls admin: $out"; ok "curl tls admin key: 200"
out=$(viasock /nopacs.v1.AccessService/WhoAmI '{}'); [ "${out%% *}" = 200 ] || fail "curl socket: $out"; ok "curl socket local operator: $out"
[ "$(viasock $lk '{}' -H "Authorization: Bearer $unknown")" = "$unauth" ] || fail "curl socket unknown key"; ok "curl socket unknown key: same refusal"
out=$(viatls /grpc.health.v1.Health/Check '{}'); [ "$out" = '200 {"status":"SERVING"}' ] || fail "curl health: $out"; ok "curl tls health without a key: $out"
if [ "$real" = 1 ]; then
	# The installation's root of directory storages, which every client's
	# tenant uses below: set by the local operator, as only the
	# installation's admin may.
	mkdir -p "$work/roots/fast"
	roots=$("$PYTHON" -c 'import json,sys; print(json.dumps({"setting": {"name": "storage.directory-roots", "scope": {"installation": {}}, "value": "fast=" + sys.argv[1]}}))' "$work/roots/fast")
	out=$(viasock /nopacs.v1.SettingsService/SetSetting "$roots"); [ "${out%% *}" = 200 ] || fail "curl socket set the roots: $out"; ok "curl socket storage.directory-roots set: fast"
	ct=/nopacs.v1.TenantService/CreateTenant
	lt=/nopacs.v1.TenantService/ListTenants
	[ "$(viatls $ct '{"name":"curl1","databaseType":"DATABASE_TYPE_SQLITE"}')" = "$unauth" ] || fail "curl tls create, no key"; ok "curl tls CreateTenant with no key: $unauth"
	[ "$(viatls $ct '{"name":"curl1","databaseType":"DATABASE_TYPE_SQLITE"}' -H "Authorization: Bearer $nogrant")" = "$denied" ] || fail "curl tls create, no grant"; ok "curl tls CreateTenant with a key with no grant: $denied"
	out=$(viatls $ct '{"name":"curl1","databaseType":"DATABASE_TYPE_SQLITE"}' -H "Authorization: Bearer $admin")
	[ "${out%% *}" = 200 ] && [ "$(echo "${out#* }" | json '["tenant"]["state"]')" = TENANT_STATE_READY ] || fail "curl tls create, admin: $out"; ok "curl tls CreateTenant with an admin key: ready"
	out=$(viatls $lt '{}' -H "Authorization: Bearer $admin")
	echo "${out#* }" | grep -q '"name":"curl1"' || fail "curl tls list, admin: $out"; ok "curl tls ListTenants with an admin key: curl1 listed"
	out=$(viatls $lt '{}' -H "Authorization: Bearer $nogrant"); [ "$out" = '200 {}' ] || fail "curl tls list, no grant: $out"; ok "curl tls ListTenants with a key with no grant: none"
fi
curl -sS --unix-socket "$sock" http://local/metrics > "$work/metrics"
grep -q '^nopacs_requests_total' "$work/metrics" || fail "metrics"
if [ "$real" = 1 ]; then
	grep -q '^nopacs_tenants{state="ready"} 1$' "$work/metrics" || fail "metrics: the tenant made above is not counted ready"; ok "metrics: nopacs_tenants{state=\"ready\"} 1"
fi

echo "== grpcurl"
# grpcurl 1.9.2's -unix flag dialed TCP when tried; the gRPC target form
# unix:///path is used for the socket instead.
gc() {
	"$GRPCURL" -import-path "$root/api/proto" -proto nopacs/v1/access.proto "$@" 2>&1 || true
}
gtls() { gc -cacert "$work/cert.pem" "$@"; }
want_unauth='Code: Unauthenticated Message: the request carries no valid key'
want_denied='Code: PermissionDenied Message: the key may not do this'
flat() { tr -s ' \n' ' ' | sed 's/^ //; s/ $//; s/ERROR: //'; }
r=$(gtls -d '{}' "127.0.0.1:$port" nopacs.v1.AccessService/ListKeys | flat); [ "$r" = "$want_unauth" ] || fail "grpcurl tls no key: $r"; ok "grpcurl tls no key: $r"
r=$(gtls -H 'authorization: Bearer junk' -d '{}' "127.0.0.1:$port" nopacs.v1.AccessService/ListKeys | flat); [ "$r" = "$want_unauth" ] || fail "grpcurl malformed: $r"; ok "grpcurl tls malformed key: same"
r=$(gtls -H "authorization: Bearer $unknown" -d '{}' "127.0.0.1:$port" nopacs.v1.AccessService/ListKeys | flat); [ "$r" = "$want_unauth" ] || fail "grpcurl unknown: $r"; ok "grpcurl tls unknown key: same"
r=$(gtls -H "authorization: Bearer $nogrant" -d '{}' "127.0.0.1:$port" nopacs.v1.AccessService/ListKeys | flat); [ "$r" = "$want_denied" ] || fail "grpcurl no grant: $r"; ok "grpcurl tls key with no grant: $r"
r=$(gtls -v -H "authorization: Bearer $admin" -d '{}' "127.0.0.1:$port" nopacs.v1.AccessService/ListKeys)
echo "$r" | grep -q '"keys"' || fail "grpcurl admin: $r"
echo "$r" | grep -qi 'nopacs-request-id' || fail "grpcurl admin: no request id"; ok "grpcurl tls admin key: keys listed, with a request id"
r=$(gc -plaintext -d '{}' "unix://$sock" nopacs.v1.AccessService/WhoAmI | flat); echo "$r" | grep -q localOperator || fail "grpcurl socket: $r"; ok "grpcurl socket local operator: $r"
r=$(gc -plaintext -H "authorization: Bearer $unknown" -d '{}' "unix://$sock" nopacs.v1.AccessService/WhoAmI | flat); [ "$r" = "$want_unauth" ] || fail "grpcurl socket unknown: $r"; ok "grpcurl socket unknown key: same refusal"
if [ "$real" = 1 ]; then
	gt() { "$GRPCURL" -import-path "$root/api/proto" -proto nopacs/v1/tenant.proto -cacert "$work/cert.pem" "$@" 2>&1 || true; }
	r=$(gt -d '{"name":"grpc1","databaseType":"DATABASE_TYPE_SQLITE"}' "127.0.0.1:$port" nopacs.v1.TenantService/CreateTenant | flat); [ "$r" = "$want_unauth" ] || fail "grpcurl create, no key: $r"; ok "grpcurl tls CreateTenant with no key: $r"
	r=$(gt -H "authorization: Bearer $nogrant" -d '{"name":"grpc1","databaseType":"DATABASE_TYPE_SQLITE"}' "127.0.0.1:$port" nopacs.v1.TenantService/CreateTenant | flat); [ "$r" = "$want_denied" ] || fail "grpcurl create, no grant: $r"; ok "grpcurl tls CreateTenant with a key with no grant: $r"
	r=$(gt -H "authorization: Bearer $admin" -d '{"name":"grpc1","databaseType":"DATABASE_TYPE_SQLITE"}' "127.0.0.1:$port" nopacs.v1.TenantService/CreateTenant)
	echo "$r" | grep -q TENANT_STATE_READY || fail "grpcurl create, admin: $r"; ok "grpcurl tls CreateTenant with an admin key: ready"
	r=$(gt -H "authorization: Bearer $admin" -d '{}' "127.0.0.1:$port" nopacs.v1.TenantService/ListTenants)
	echo "$r" | grep -q '"grpc1"' || fail "grpcurl list, admin: $r"; ok "grpcurl tls ListTenants with an admin key: grpc1 listed"

	# The first use of the walk-through on grpc1, with the bodies it shows,
	# by the installation's admin and then the local operator (an admin on
	# every tenant); and the refusals of the third, on a tenant with
	# nothing quarantined.
	gw() {
		"$GRPCURL" -import-path "$root/api/proto" -proto nopacs/v1/settings.proto -proto nopacs/v1/storage.proto \
			-proto nopacs/v1/receiver.proto -proto nopacs/v1/quarantine.proto -proto nopacs/v1/reception.proto "$@" 2>&1 || true
	}
	gwa() { gw -cacert "$work/cert.pem" -H "authorization: Bearer $admin" "$@"; }
	gwl() { gw -plaintext "$@"; }
	r=$(gwa -d '{"setting":{"name":"receiver.ports","scope":{"tenant":"grpc1"},"value":"11200,11210-11219"}}' "127.0.0.1:$port" nopacs.v1.SettingsService/SetSetting)
	echo "$r" | grep -q '"11200,11210-11219"' || fail "grpcurl receiver.ports: $r"; ok "grpcurl tls receiver.ports set on grpc1 by the installation's admin"
	r=$(gwa -d '{"setting":{"name":"receiver.allow-as-received","scope":{"tenant":"grpc1"},"value":"true"}}' "127.0.0.1:$port" nopacs.v1.SettingsService/SetSetting)
	echo "$r" | grep -q '"true"' || fail "grpcurl receiver.allow-as-received: $r"; ok "grpcurl tls receiver.allow-as-received set on grpc1 by the installation's admin"
	r=$(gwa -d '{"tenant":"grpc1","storage":{"name":"main","directory":{"root":"fast","path":"mri"}}}' "127.0.0.1:$port" nopacs.v1.StorageService/AddStorage | flat)
	[ "$r" = "$want_denied" ] || fail "grpcurl AddStorage by the installation's admin: $r"; ok "grpcurl tls AddStorage by the installation's admin, no role inside the tenant: $r"
	r=$(gwl -d '{"tenant":"grpc1","storage":{"name":"main","directory":{"root":"fast","path":"mri"}}}' "unix://$sock" nopacs.v1.StorageService/AddStorage)
	echo "$r" | grep -q '"root": "fast"' || fail "grpcurl AddStorage: $r"
	[ -d "$work/roots/fast/grpc1/mri" ] || fail "grpcurl AddStorage made no directory"; ok "grpcurl socket AddStorage main: the directory made"
	r=$(gwl -d '{"tenant":"grpc1","name":"main"}' "unix://$sock" nopacs.v1.StorageService/TestStorage)
	echo "$r" | grep -q '"passed": true' && [ "$(echo "$r" | grep -c STORAGE_TEST_OUTCOME_PASSED)" = 4 ] || fail "grpcurl TestStorage: $r"; ok "grpcurl socket TestStorage: passed, four steps"
	r=$(gwl -d '{"tenant":"grpc1","receiver":{"name":"scanner1","enabled":true,"storage":"main","dimse":{"port":11200,"aeTitle":"NOPACS","allowedCallers":{"rules":[{"callingAeTitle":"PRISMA1","addressRange":"10.20.0.0/24"}]}}}}' "unix://$sock" nopacs.v1.ReceiverService/AddReceiver)
	echo "$r" | grep -q '"quietPeriod": "60s"' && echo "$r" | grep -q '"maxFileBytes": "4294967296"' || fail "grpcurl AddReceiver: $r"; ok "grpcurl socket AddReceiver scanner1: defaults filled in"
	# grpcurl 1.9.4 reads a field mask only as an object of paths, not in the
	# string form of protobuf's JSON mapping; curl sends the walk-through's
	# own body, with the string form, which the server reads.
	r=$(gwl -d '{"tenant":"grpc1","receiver":{"name":"scanner1","quietPeriod":"120s"},"updateMask":{"paths":["quiet_period"]}}' "unix://$sock" nopacs.v1.ReceiverService/SetReceiver)
	echo "$r" | grep -q '"quietPeriod": "120s"' || fail "grpcurl SetReceiver: $r"; ok "grpcurl socket SetReceiver quietPeriod: 120s"
	out=$(viasock /nopacs.v1.ReceiverService/SetReceiver '{"tenant":"grpc1","receiver":{"name":"scanner1","quietPeriod":"300s"},"updateMask":"quietPeriod"}')
	[ "${out%% *}" = 200 ] && echo "${out#* }" | grep -q '"quietPeriod":"300s"' || fail "curl SetReceiver: $out"; ok "curl socket SetReceiver with the walk-through's body: quietPeriod 300s"
	r=$(gwl -d '{"tenant":"grpc1","receiver":{"name":"scanner1"},"updateMask":{"paths":["dimse"]}}' "unix://$sock" nopacs.v1.ReceiverService/SetReceiver | flat)
	echo "$r" | grep -q '^Code: InvalidArgument' || fail "grpcurl SetReceiver dimse: $r"; ok "grpcurl socket SetReceiver with the mask dimse: $r"
	r=$(gwl -d '{"tenant":"grpc1","receiver":{"name":"scanner2","storage":"main","dimse":{"port":104,"aeTitle":"NOPACS"}}}' "unix://$sock" nopacs.v1.ReceiverService/AddReceiver | flat)
	echo "$r" | grep -q '^Code: FailedPrecondition' || fail "grpcurl AddReceiver port 104: $r"; ok "grpcurl socket AddReceiver on a port not allotted: $r"
	r=$(gwl -d '{"tenant":"grpc1","state":"QUARANTINE_STATE_HELD"}' "unix://$sock" nopacs.v1.QuarantineService/ListQuarantinedFiles | flat)
	[ "$r" = "{}" ] || fail "grpcurl ListQuarantinedFiles: $r"; ok "grpcurl socket ListQuarantinedFiles held: none"
	r=$(gwl -d '{"tenant":"grpc1","id":"q-19d2","reason":""}' "unix://$sock" nopacs.v1.QuarantineService/RejectQuarantinedFile | flat)
	echo "$r" | grep -q '^Code: InvalidArgument' || fail "grpcurl Reject with no reason: $r"; ok "grpcurl socket RejectQuarantinedFile with no reason: $r"
	r=$(gwl -d '{"tenant":"grpc1","id":"q-19d2","mode":"RELEASE_MODE_AS_IS"}' "unix://$sock" nopacs.v1.QuarantineService/ReleaseQuarantinedFile | flat)
	echo "$r" | grep -q '^Code: NotFound' || fail "grpcurl Release of no file: $r"; ok "grpcurl socket ReleaseQuarantinedFile of no file: $r"
	r=$(gwl -d '{"tenant":"grpc1","name":"main"}' "unix://$sock" nopacs.v1.StorageService/RemoveStorage | flat)
	echo "$r" | grep -q '^Code: FailedPrecondition' || fail "grpcurl RemoveStorage named by a receiver: $r"; ok "grpcurl socket RemoveStorage while a receiver names it: $r"
fi

echo "== python grpcio"
NOPACS_CHECK_TENANTS=$real NOPACS_CHECK_ROOT=$work/roots/fast "$PYTHON" "$root/scripts/check/server.py" "$root/api/proto" "$sock" "127.0.0.1:$port" "$work/cert.pem" "$nogrant" "$admin" "$work/metrics"

# No key secret in the server's log.
for s in "$nogrant" "$admin"; do
	if grep -qF "${s##*_}" "$work/serve.log"; then fail "a key's secret is in the server's log"; fi
done
ok "no key secret in the server's log ($(wc -l < "$work/serve.log" | tr -d ' ') lines)"

kill -TERM "$server_pid"
status=0; wait "$server_pid" || status=$?
server_pid=
[ "$status" = 0 ] || fail "the server exited $status on SIGTERM"
ok "the server exited 0 on SIGTERM"
}

port=${PORT:-18443}
echo "==== the stand-in handlers"
check "$work/standin" "$port" 0
echo "==== the real program with the real handlers"
check "$work/nopacs-server" "$((port + 1))" 1
echo "check-server: all checks passed"
