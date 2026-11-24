# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "12. where a key may be used from".
# It uses the runner's variables and helpers (result, ok, fail, skip, secret,
# the work directory, the binary and the certificate of section 1). It runs
# its own installation and server, so the settings it changes (the networks
# refused, the proxies trusted, the limits on failed calls) touch no other
# section and the main server's audit log keeps only the requests of its
# ledger. Every request is made by curl from outside; what the server
# recorded is read with sqlite3 from its master database.
# shellcheck shell=bash

section "12. where a key may be used from"
acc=$work/acc
mkdir -m 700 "$acc"
acc_port=$(free_port)
"$sbin" init --data-dir "$acc" > "$work/acc-init.out" 2>&1 || { fail "12.0 a second installation" "init failed" "$(cat "$work/acc-init.out")"; return 0; }
if ! start_server "$work/acc.log" "$acc" --listen "127.0.0.1:$acc_port" --tls-cert "$work/cert.pem" --tls-key "$work/key.pem"; then
	fail "12.0 a second installation" "its server did not start" "$(tail -20 "$work/acc.log")"
	return 0
fi
acc_pid=$SERVER_PID
mkdir -m 700 "$acc/keys"

# acc_cli ARGS...: the binary's own command on the second installation's
# local socket; status in ACC_STATUS, output in acc.out and acc.err.
acc_cli() {
	ACC_STATUS=0
	env -u NOPACS_SERVER -u NOPACS_KEY NOPACS_DATA_DIR="$acc" "$bin" "$@" > "$work/acc.out" 2> "$work/acc.err" || ACC_STATUS=$?
}
# acc_key NAME ARGS...: a key made with "key create ARGS"; the key in
# acc/keys/NAME, its id in acc/keys/NAME.id.
acc_key() {
	local name=$1
	shift
	acc_cli key create --holder "proof $name" "$@"
	(umask 077 && head -1 "$work/acc.out" > "$acc/keys/$name")
	secret "$(cat "$acc/keys/$name")"
	cut -d _ -f 2 < "$acc/keys/$name" > "$acc/keys/$name.id"
}
# acc_set NAME VALUE [--tenant T]: an installation or tenant setting, then a
# wait for the server's copy of it to be read again (it keeps one for 5 s).
acc_set() {
	acc_cli setting set "$@"
	sleep 6
}
acc_unset() {
	acc_cli setting unset "$@"
	sleep 6
}
# acc_call KEY METHOD BODY [HEADER...]: one call by curl over TLS with the
# key in acc/keys/KEY (or none for ""), and each HEADER given as is. Sets
# ACC_HTTP and ACC_TIME (s); the answer's body in acc.body. The key goes in a
# header file, never in curl's arguments.
acc_call() {
	local key=$1 method=$2 body=$3 out h
	shift 3
	local args=(-sS -o "$work/acc.body" -w '%{http_code} %{time_total}' --cacert "$work/cert.pem" -H 'Content-Type: application/json' -d "$body")
	if [ -n "$key" ]; then
		(umask 077 && printf 'Authorization: Bearer %s\n' "$(cat "$acc/keys/$key")" > "$work/acc-auth.h")
		args+=(-H "@$work/acc-auth.h")
	fi
	for h in "$@"; do args+=(-H "$h"); done
	out=$(curl "${args[@]}" "https://127.0.0.1:$acc_port/$method" 2> "$work/acc-curl.err") || out="000 0"
	rm -f "$work/acc-auth.h"
	ACC_HTTP=${out%% *}
	ACC_TIME=${out#* }
}
# acc_sql QUERY: the second installation's master database, read by sqlite3
# once the audit writer has written what it holds (it writes every 200 ms).
acc_sql() {
	sleep 0.5
	"$SQLITE3" "$acc/master.db" "$1"
}
who=nopacs.v1.AccessService/WhoAmI
printf 'nopacs_%s_%s\n' 111111111111 11111111111111111111111111111111111111111111 > "$acc/keys/bogus"
printf '%s\n' 111111111111 > "$acc/keys/bogus.id"

# 12.1 A key used from a network it does not list.
acc_key netkey --network 10.0.0.0/8
acc_call netkey "$who" '{}'
outside=$ACC_HTTP
acc_cli key set "$(cat "$acc/keys/netkey.id")" --network 127.0.0.0/8
acc_call netkey "$who" '{}'
inside=$ACC_HTTP
reason=$(acc_sql "select reason from audit where key_id = '$(cat "$acc/keys/netkey.id")' and outcome = 'refused' order by at desc limit 1")
if [ "$outside" = 401 ] && [ "$inside" = 200 ] && [ "$reason" = "refused by the rule key-networks" ]; then
	ok "12.1 a key used from a network it does not list is refused" "key with 10.0.0.0/8 from 127.0.0.1: HTTP $outside, as a wrong key; audit reason '$reason'; after key set --network 127.0.0.0/8: HTTP $inside"
else
	fail "12.1 a key used from a network it does not list is refused" "outside $outside, inside $inside, reason '$reason'" "$(cat "$work/acc.err")"
fi

# 12.2 A tenant's networks.
acc_cli tenant create acc
acc_key tenkey
acc_cli grant set "$(cat "$acc/keys/tenkey.id")" --role reader --tenant acc
acc_call tenkey nopacs.v1.StorageService/ListStorages '{"tenant":"acc"}'
before=$ACC_HTTP
acc_set access.tenant-networks 10.0.0.0/8 --tenant acc
acc_call tenkey nopacs.v1.StorageService/ListStorages '{"tenant":"acc"}'
after=$ACC_HTTP
reason=$(acc_sql "select reason || ' / ' || tenant_name from audit where key_id = '$(cat "$acc/keys/tenkey.id")' and outcome = 'refused' order by at desc limit 1")
acc_unset access.tenant-networks --tenant acc
acc_call tenkey nopacs.v1.StorageService/ListStorages '{"tenant":"acc"}'
back=$ACC_HTTP
if [ "$before" = 200 ] && [ "$after" = 401 ] && [ "$reason" = "refused by the rule tenant-networks / acc" ] && [ "$back" = 200 ]; then
	ok "12.2 a tenant's networks apply to every key used on it" "reader of acc: HTTP $before; with access.tenant-networks 10.0.0.0/8 on acc: HTTP $after, audit '$reason'; unset: HTTP $back"
else
	fail "12.2 a tenant's networks apply to every key used on it" "before $before, after $after, reason '$reason', back $back" "$(cat "$work/acc.err")"
fi

# 12.3 The installation's deny list, before any key is read.
acc_key admin --no-expiry
acc_cli grant set "$(cat "$acc/keys/admin.id")" --role admin --installation
acc_set access.denied-networks 127.0.0.1
acc_call admin "$who" '{}'
denied=$ACC_HTTP
acc_cli whoami
local_status=$ACC_STATUS
reason=$(acc_sql "select reason || ' / ' || key_id || ' / ' || substr(remote_addr, 1, 10) from audit where outcome = 'refused' order by at desc limit 1")
acc_unset access.denied-networks
acc_call admin "$who" '{}'
if [ "$denied" = 401 ] && [ "$local_status" = 0 ] && [ "$reason" = "refused by the rule installation-denied-networks /  / 127.0.0.1:" ] && [ "$ACC_HTTP" = 200 ]; then
	ok "12.3 the installation's deny list refuses first" "admin key from 127.0.0.1 with access.denied-networks 127.0.0.1: HTTP $denied, audit '$reason' (no key read); the local socket still answers; unset: HTTP $ACC_HTTP"
else
	fail "12.3 the installation's deny list refuses first" "denied $denied, local $local_status, reason '$reason', after $ACC_HTTP" "$(cat "$work/acc.err")"
fi

# 12.4 A forwarded address from a peer that is not a trusted proxy is ignored.
acc_call bogus "$who" '{}' 'X-Forwarded-For: 203.0.113.9' 'Forwarded: for=203.0.113.9'
untrusted=$(acc_sql "select remote_addr from audit where key_id = '111111111111' order by at desc limit 1")
case $untrusted in
	127.0.0.1:*) ok "12.4 a forwarded address from an untrusted peer is ignored" "no trusted proxy: X-Forwarded-For and Forwarded 203.0.113.9 sent, audit records the peer, $untrusted" ;;
	*) fail "12.4 a forwarded address from an untrusted peer is ignored" "audit recorded '$untrusted'" ;;
esac

# 12.5 A forwarded address from a trusted proxy is the caller, read from
# the one header access.forwarded-header names: X-Forwarded-For by default,
# where a Forwarded header alone is not read and the peer is the caller;
# Forwarded (RFC 7239) once the setting names it.
acc_set access.trusted-proxies 127.0.0.1
acc_call bogus "$who" '{}' 'X-Forwarded-For: 198.51.100.1, 203.0.113.9'
xff=$(acc_sql "select remote_addr from audit where key_id = '111111111111' order by at desc limit 1")
acc_call bogus "$who" '{}' 'Forwarded: for="[2001:db8::7]:4711"'
fwd_default=$(acc_sql "select remote_addr from audit where key_id = '111111111111' order by at desc limit 1")
acc_set access.forwarded-header forwarded
acc_call bogus "$who" '{}' 'Forwarded: for="[2001:db8::7]:4711"'
fwd=$(acc_sql "select remote_addr from audit where key_id = '111111111111' order by at desc limit 1")
acc_unset access.forwarded-header
acc_key fresh
acc_call fresh "$who" '{}' 'X-Forwarded-For: 203.0.113.10'
last=$(acc_sql "select last_address from api_keys where id = '$(cat "$acc/keys/fresh.id")'")
case $fwd_default in 127.0.0.1:*) fwd_ignored=1 ;; *) fwd_ignored=0 ;; esac
if [ "$xff" = 203.0.113.9 ] && [ "$fwd_ignored" = 1 ] && [ "$fwd" = 2001:db8::7 ] && [ "$last" = 203.0.113.10 ]; then
	ok "12.5 a forwarded address from a trusted proxy is the caller" "access.trusted-proxies 127.0.0.1: X-Forwarded-For '198.51.100.1, 203.0.113.9' recorded as $xff (the rightmost untrusted); Forwarded for=[2001:db8::7]:4711 alone not read by default ($fwd_default, the peer), and with access.forwarded-header forwarded recorded as $fwd; a key's last-used address $last"
else
	fail "12.5 a forwarded address from a trusted proxy is the caller" "X-Forwarded-For: '$xff', Forwarded by default: '$fwd_default', Forwarded when named: '$fwd', last used from '$last'"
fi

# 12.6 An address blocked for bad keys; behind the trusted proxy each
# forwarded address is its own caller, so the block does not reach the
# proxy itself.
acc_set access.failure-limit 2
times=""
for i in 1 2 3 4; do
	acc_call bogus "$who" '{}' 'X-Forwarded-For: 198.51.100.66'
	times="$times $ACC_HTTP/${ACC_TIME}s"
done
acc_call admin "$who" '{}' 'X-Forwarded-For: 198.51.100.66'
blocked=$ACC_HTTP
acc_call admin "$who" '{}' 'X-Forwarded-For: 198.51.100.67'
other=$ACC_HTTP
slow=$(printf '%s\n' "$times" | awk '{ split($3, a, "/"); sub(/s$/, "", a[2]); print (a[2] >= 1.0) ? "yes" : "no" }')
nblock=$(acc_sql "select count(*) from audit where reason = 'refused by the rule address-blocked' and remote_addr = '198.51.100.66'")
if [ "$blocked" = 401 ] && [ "$other" = 200 ] && [ "$slow" = yes ] && [ "$nblock" -ge 2 ]; then
	ok "12.6 an address that keeps sending bad keys is slowed, then blocked" "access.failure-limit 2, four bad keys from 198.51.100.66:$times (the third held 1 s); then the admin key from it: HTTP $blocked; from 198.51.100.67: HTTP $other; $nblock audit entries of the block"
else
	fail "12.6 an address that keeps sending bad keys is slowed, then blocked" "times:$times; blocked $blocked; other $other; slowed $slow; block entries $nblock"
fi

# 12.7 A valid key blocked after repeated refusals for its grants.
acc_key weak
codes=""
for i in 1 2 3 4; do
	acc_call weak nopacs.v1.AccessService/ListKeys '{}' 'X-Forwarded-For: 198.51.100.70'
	codes="$codes $ACC_HTTP"
done
acc_call weak "$who" '{}' 'X-Forwarded-For: 198.51.100.70'
weak_after=$ACC_HTTP
reason=$(acc_sql "select count(*) from audit where key_id = '$(cat "$acc/keys/weak.id")' and reason = 'refused by the rule key-blocked'")
if [ "$codes" = " 403 403 403 403" ] && [ "$weak_after" = 401 ] && [ "$reason" -ge 2 ]; then
	ok "12.7 a valid key refused again and again is blocked" "a key with no grant asks ListKeys four times: HTTP$codes; then its own WhoAmI: HTTP $weak_after; $reason audit entries of the key's block"
else
	fail "12.7 a valid key refused again and again is blocked" "ListKeys:$codes; WhoAmI after: $weak_after; block entries $reason"
fi

# 12.8 A busy key is never slowed: a hundred successful calls under a limit
# of two failures.
acc_key busy
n200=0
slowest=0
for i in $(seq 1 100); do
	acc_call busy "$who" '{}' 'X-Forwarded-For: 198.51.100.80'
	[ "$ACC_HTTP" != 200 ] || n200=$((n200 + 1))
	slowest=$(awk -v a="$slowest" -v b="$ACC_TIME" 'BEGIN { print (b > a) ? b : a }')
done
if [ "$n200" = 100 ] && awk -v s="$slowest" 'BEGIN { exit !(s < 1.0) }'; then
	ok "12.8 a busy key is never slowed" "100 WhoAmI with one key under access.failure-limit 2: $n200 answered 200, the slowest in ${slowest} s"
else
	fail "12.8 a busy key is never slowed" "$n200 of 100 answered 200; the slowest took ${slowest} s"
fi

# 12.9 Rotation: the successor works at once, the rotated key until its
# overlap ends, and a key may rotate itself.
acc_key rot
acc_cli key rotate "$(cat "$acc/keys/rot.id")" --overlap 3s
rot_status=$ACC_STATUS
(umask 077 && head -1 "$work/acc.out" > "$acc/keys/rot2")
secret "$(cat "$acc/keys/rot2")"
acc_call rot "$who" '{}' 'X-Forwarded-For: 198.51.100.90'
during=$ACC_HTTP
acc_call rot2 "$who" '{}' 'X-Forwarded-For: 198.51.100.90'
successor=$ACC_HTTP
sleep 4
acc_call rot "$who" '{}' 'X-Forwarded-For: 198.51.100.90'
past=$ACC_HTTP
rot2_id=$(cut -d _ -f 2 < "$acc/keys/rot2")
named=$(acc_sql "select successor from api_keys where id = '$(cat "$acc/keys/rot.id")'")
acc_call rot2 nopacs.v1.AccessService/RotateKey "{\"id\":\"$rot2_id\"}" 'X-Forwarded-For: 198.51.100.90'
self=$ACC_HTTP
self_secret=$(json_get '["secret"]' < "$work/acc.body" 2> /dev/null || echo "")
[ -z "$self_secret" ] || secret "$self_secret"
if [ "$rot_status" = 0 ] && [ "$during" = 200 ] && [ "$successor" = 200 ] && [ "$past" = 401 ] && [ "$named" = "$rot2_id" ] &&
	[ "$self" = 200 ] && [ -n "$self_secret" ]; then
	ok "12.9 a rotated key works through its overlap and not after it" "key rotate --overlap 3s: the old key HTTP $during, the new $successor; 4 s later the old key HTTP $past; the old key's row names its successor; the new key rotates itself over the API: HTTP $self, a secret answered once"
else
	fail "12.9 a rotated key works through its overlap and not after it" "rotate exit $rot_status; during $during; successor $successor; past $past; successor named '$named'; self-rotation $self" "$(cat "$work/acc.err")"
fi

# 12.10 Behind a proxy that appends X-Forwarded-For and passes a client's
# Forwarded header through, the client cannot choose its address: only the
# header access.forwarded-header names is read, the other ignored.
acc_key tenonly --network 10.0.0.0/8
acc_call tenonly "$who" '{}' 'X-Forwarded-For: 203.0.113.9' 'Forwarded: for=10.1.1.1'
spoof=$ACC_HTTP
spoof_addr=$(acc_sql "select remote_addr from audit where key_id = '$(cat "$acc/keys/tenonly.id")' order by at desc limit 1")
acc_set access.forwarded-header forwarded
acc_call tenonly "$who" '{}' 'X-Forwarded-For: 203.0.113.9' 'Forwarded: for=10.1.1.1'
chosen=$ACC_HTTP
chosen_addr=$(acc_sql "select remote_addr from audit where key_id = '$(cat "$acc/keys/tenonly.id")' order by at desc limit 1")
acc_unset access.forwarded-header
if [ "$spoof" = 401 ] && [ "$spoof_addr" = 203.0.113.9 ] && [ "$chosen" = 200 ] && [ "$chosen_addr" = 10.1.1.1 ]; then
	ok "12.10 a client-written Forwarded header is not believed behind an X-Forwarded-For proxy" "key with 10.0.0.0/8, X-Forwarded-For 203.0.113.9 and Forwarded for=10.1.1.1 both sent through the trusted proxy: HTTP $spoof, audit records $spoof_addr; with access.forwarded-header forwarded the same request is read from Forwarded: HTTP $chosen, audit records $chosen_addr"
else
	fail "12.10 a client-written Forwarded header is not believed behind an X-Forwarded-For proxy" "default header: HTTP $spoof from '$spoof_addr'; forwarded: HTTP $chosen from '$chosen_addr'" "$(cat "$work/acc.err")"
fi

stop_server "$acc_pid"
