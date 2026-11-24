# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "13. a reading installation, built by sending public studies".
# It makes an installation and a server of its own, so what sections 13 to
# 19 do (hundreds of reads, and refusals on purpose) touches no other
# section and the main server's audit log keeps only the requests of its
# ledger. A tenant on SQLite, and one on Postgres when this run has one,
# each with a directory storage, a DIMSE receiver that dcmtk's storescu
# sends public studies to, and a STOW-RS receiver; the checks are made by
# scripts/prove/reading.py. Sections 14 to 19 read what it built; 19 stops
# its server.
# shellcheck shell=bash

section "13. a reading installation, built by sending public studies"
reading=$root/scripts/prove/reading.py
rd=$work/rd
mkdir -m 700 "$rd"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
	-subj /CN=nopacs-reading-proof -addext subjectAltName=IP:127.0.0.1 \
	-keyout "$rd/key.pem" -out "$rd/cert.pem" 2> /dev/null
rd_port=$(free_port)
rd_pid=""
if ! "$sbin" init --data-dir "$rd/d" > "$work/rd-init.out" 2>&1; then
	fail "13.0 the reading installation is made and served" "init failed" "$(cat "$work/rd-init.out")"
elif ! start_server "$work/rd-serve.log" "$rd/d" --listen "127.0.0.1:$rd_port" --tls-cert "$rd/cert.pem" --tls-key "$rd/key.pem"; then
	fail "13.0 the reading installation is made and served" "its server did not start" "$(tail -20 "$work/rd-serve.log")"
else
	rd_pid=$SERVER_PID
	# Section 19 sends refused requests from this one address on purpose,
	# with two bad keys and a key without a grant, to show each refusal; the
	# limits on failed calls would slow and then block them. They are raised
	# as far as they go here, as section 1 does on the main server; section
	# 12 proves the limits on an installation of its own.
	rd_cli() { env -u NOPACS_SERVER -u NOPACS_KEY "$bin" --data-dir "$rd/d" "$@" > "$work/rd-cli.out" 2>&1; }
	if rd_cli setting set access.failure-limit 10000 && rd_cli setting set access.failure-window 1s; then
		ok "13.0 the reading installation is made and served" "init in rd/d; serve on the local socket and TLS on 127.0.0.1:$rd_port; access.failure-limit 10000 and access.failure-window 1s for the refusals of section 19"
	else
		fail "13.0 the reading installation is made and served" "the limits on failed calls could not be raised" "$(cat "$work/rd-cli.out")"
	fi
fi

# The tenant on Postgres, in this run's Postgres (section 3), in a database
# of its own that the runner's cleanup drops with the others named
# nopacs_proof*.
rd_pg=""
if [ -n "$rd_pid" ] && [ -n "$pg_made" ]; then
	if psql_su -c "create database nopacs_proof_read owner nopacs_proof" > /dev/null 2> "$work/rd-pg.err"; then
		RD_PG_DSN=$(pg_dsn nopacs_proof_read)
		secret "$RD_PG_DSN"
		export RD_PG_DSN
		rd_pg=RD_PG_DSN
	else
		fail "13.0 a reading tenant on Postgres" "its database could not be made" "$(cat "$work/rd-pg.err")"
	fi
elif [ -n "$rd_pid" ]; then
	skip "13.0 a reading tenant on Postgres" "this run has no Postgres (NOPACS_PROVE_ONLY=reading runs without section 3)"
fi

if [ -n "$rd_pid" ]; then
	"$PYTHON" "$reading" --work "$work" setup --bin "$bin" --data "$rd/d" --port "$rd_port" --ca "$rd/cert.pem" \
		--storescu "$STORESCU" --pg-env "$rd_pg" || true
fi
unset RD_PG_DSN
