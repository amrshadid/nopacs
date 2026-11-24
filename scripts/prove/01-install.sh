# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "1. install and run".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "1. install and run"
out=$("$bin" version 2>&1) || true
case $out in
	"nopacs "*) ok "1.1 nopacs version answers" "$out" ;;
	*) fail "1.1 nopacs version answers" "unexpected output" "$out" ;;
esac

data=$work/d
sock=$data/run/nopacs.sock
mkdir -m 700 "$data"
if "$sbin" init --data-dir "$data" > "$work/init.out" 2>&1 &&
	[ "$(mode_of "$data/key")" = 600 ] && [ "$(mode_of "$data/run")" = 700 ] && [ "$(mode_of "$data")" = 700 ] &&
	[ -f "$data/master.db" ] && [ -f "$data/installation" ] && [ -d "$data/tenants" ]; then
	again=0
	"$sbin" init --data-dir "$data" > "$work/init2.out" 2>&1 || again=$?
	if [ "$again" != 0 ] && grep -q 'already holds an installation' "$work/init2.out"; then
		ok "1.2 init in an empty directory" "key file 0600, data dir and run/ 0700, master.db, installation id, tenants/; a second init is refused (exit $again)"
	else
		fail "1.2 init in an empty directory" "a second init was not refused" "$(cat "$work/init2.out")"
	fi
else
	fail "1.2 init in an empty directory" "init failed or made the wrong files" "$(cat "$work/init.out")" "$(ls -la "$data" 2>&1)"
	exit 1
fi

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
	-subj /CN=nopacs-proof -addext subjectAltName=IP:127.0.0.1 \
	-keyout "$work/key.pem" -out "$work/cert.pem" 2> /dev/null
port=$(free_port)
if start_server "$work/serve.log" "$data" --listen "127.0.0.1:$port" --tls-cert "$work/cert.pem" --tls-key "$work/key.pem" --log-level debug; then
	main_pid=$SERVER_PID
	# The sections that follow send hundreds of refused requests from this
	# one address on purpose, to show each refusal; the limits on failed
	# calls would block it after twenty. They are raised as far as they go
	# on this installation, on the local socket, before any request over
	# the network reads them; section 12 proves the limits on an
	# installation of its own.
	cli "$data" /nopacs.v1.SettingsService/SetSetting "*" setting set access.failure-limit 10000
	cli "$data" /nopacs.v1.SettingsService/SetSetting "*" setting set access.failure-window 1s
	cli "$data" /nopacs.v1.AccessService/WhoAmI "*" whoami
	whoami=$(head -1 "$work/cli.out")
	tls_subject=$(echo | openssl s_client -connect "127.0.0.1:$port" -CAfile "$work/cert.pem" -verify_return_error 2> /dev/null | grep -c 'Verify return code: 0 (ok)' || true)
	if [ "$CLI_STATUS" = 0 ] && [ "$tls_subject" -ge 1 ] && [ -S "$sock" ] && [ "$(mode_of "$data/run")" = 700 ]; then
		ok "1.3 serve on the local socket and on TLS on loopback" "socket in a 0700 run/ answers: '$whoami'; openssl s_client verifies 127.0.0.1:$port with the script's certificate"
	else
		fail "1.3 serve on the local socket and on TLS on loopback" "whoami exit $CLI_STATUS, TLS verified $tls_subject" "$(cat "$work/cli.err")" "$(tail -5 "$work/serve.log")"
	fi
else
	fail "1.3 serve on the local socket and on TLS on loopback" "the server did not start" "$(tail -20 "$work/serve.log")"
	exit 1
fi

# The standard health protocol, which grpcurl needs as a file: the contract
# does not include it, and the server offers no reflection.
mkdir -p "$work/health/grpc/health/v1"
cat > "$work/health/grpc/health/v1/health.proto" << 'EOF'
syntax = "proto3";
package grpc.health.v1;
message HealthCheckRequest { string service = 1; }
message HealthCheckResponse {
  enum ServingStatus { UNKNOWN = 0; SERVING = 1; NOT_SERVING = 2; SERVICE_UNKNOWN = 3; }
  ServingStatus status = 1;
}
service Health { rpc Check(HealthCheckRequest) returns (HealthCheckResponse); }
EOF
gout=$("$GRPCURL" -import-path "$work/health" -proto grpc/health/v1/health.proto -cacert "$work/cert.pem" -v -d '{}' "127.0.0.1:$port" grpc.health.v1.Health/Check 2>&1) || true
grid=$(printf '%s\n' "$gout" | awk -F ': ' 'tolower($1) == "nopacs-request-id" { print $2 }')
[ -z "$grid" ] || ledger "$grid" /grpc.health.v1.Health/Check "" none "*" "grpcurl health"
api tls "" grpc.health.v1.Health/Check '{}'
if printf '%s' "$gout" | grep -q '"status": "SERVING"' && [ "$API_STATUS" = 200 ] && [ "$API_BODY" = '{"status":"SERVING"}' ]; then
	ok "1.4 health answers without a key" "grpcurl over TLS: SERVING; curl over TLS: 200 $API_BODY"
else
	fail "1.4 health answers without a key" "grpcurl or curl did not get SERVING" "$gout" "$API_STATUS $API_BODY"
fi
