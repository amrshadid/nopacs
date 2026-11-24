package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
)

// rulesOf returns a fixed reading of the access settings, changed by set.
func rulesOf(set func(*server.AccessRules)) func(context.Context) (server.AccessRules, error) {
	r := server.DefaultAccessRules()
	if set != nil {
		set(&r)
	}
	return func(context.Context) (server.AccessRules, error) { return r, nil }
}

// nets parses networks a test wrote.
func nets(t testing.TB, v string) settings.Networks {
	t.Helper()
	n, err := settings.ParseNetworks(v)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// post calls WhoAmI or another unary method over the TLS listener as plain
// HTTP with JSON, with authorization and any extra headers, and returns the
// HTTP status.
func (e *env) post(c *http.Client, method, authorization string, headers map[string]string) int {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.netURL+method, strings.NewReader("{}"))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", "Bearer "+authorization)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}

// whoAmIPath is WhoAmI's procedure, any authenticated caller's method.
const whoAmIPath = "/nopacs.v1.AccessService/WhoAmI"

// listKeysPath is ListKeys's procedure, which needs admin on the
// installation.
const listKeysPath = "/nopacs.v1.AccessService/ListKeys"

// entries returns the installation's audit entries with outcome, newest
// first, once the audit writer has written what it holds.
func (e *env) entries(outcome master.Outcome) []master.AuditEntry {
	e.t.Helper()
	if err := e.access.FlushAudit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	got, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{Outcome: outcome}, master.Page{Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// withReason returns the entries whose reason is reason.
func withReason(entries []master.AuditEntry, reason string) []master.AuditEntry {
	var out []master.AuditEntry
	for _, en := range entries {
		if en.Reason == reason {
			out = append(out, en)
		}
	}
	return out
}

// TestInstallationNetworks guards the installation's networks: a request
// from a denied network, or from outside the allowed ones, is refused as an
// unknown key is, before its key is read, and recorded with the rule; the
// local socket is never refused by them.
func TestInstallationNetworks(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*server.AccessRules)
		rule string
	}{
		{"denied", func(r *server.AccessRules) { r.Denied = nets(t, "127.0.0.0/8") }, access.RuleDeniedNetworks},
		{"not allowed", func(r *server.AccessRules) { r.Allowed = nets(t, "10.0.0.0/8") }, access.RuleAllowedNetworks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, options{accessRules: rulesOf(tc.set)})
			key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
			c := e.tlsClient(false)
			if got := e.post(c, whoAmIPath, key, nil); got != http.StatusUnauthorized {
				t.Errorf("a good key from the refused network: %d, want 401", got)
			}
			if got := e.post(c, whoAmIPath, "nopacs_nope_nope", nil); got != http.StatusUnauthorized {
				t.Errorf("a bad key from the refused network: %d, want 401", got)
			}
			got := withReason(e.entries(master.OutcomeRefused), access.RuleReason(tc.rule))
			if len(got) != 2 || got[0].KeyID != "" || !strings.HasPrefix(got[0].RemoteAddress, "127.0.0.1:") {
				t.Errorf("entries %+v", got)
			}
			if _, err := e.local().access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
				t.Errorf("the local socket was refused: %v", err)
			}
		})
	}
}

// TestTrustedProxies guards the caller's address behind a proxy: from a
// trusted proxy, the forwarded client is recorded as the caller (in the
// audit entry and as the key's last-used address) and judged by the key's
// networks; from any other peer the header is ignored.
func TestTrustedProxies(t *testing.T) {
	e := newEnv(t, options{accessRules: rulesOf(func(r *server.AccessRules) { r.TrustedProxies = nets(t, "127.0.0.1") })})
	key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	c := e.tlsClient(false)
	if got := e.post(c, whoAmIPath, "nopacs_nope_nope", map[string]string{"X-Forwarded-For": "198.51.100.1, 203.0.113.9"}); got != http.StatusUnauthorized {
		t.Fatalf("%d", got)
	}
	if got := e.post(c, whoAmIPath, "nopacs_nope_nope", map[string]string{"Forwarded": `for="[2001:db8::7]:1234"`}); got != http.StatusUnauthorized {
		t.Fatalf("%d", got)
	}
	// X-Forwarded-For is the header read by default; a Forwarded header
	// alone is ignored, and the proxy itself is the caller.
	refused := e.entries(master.OutcomeRefused)
	if len(refused) < 2 || !strings.HasPrefix(refused[0].RemoteAddress, "127.0.0.1:") || refused[1].RemoteAddress != "203.0.113.9" {
		t.Errorf("forwarded callers recorded as %+v", refused)
	}
	id := strings.Split(key, "_")[1]
	if _, err := e.local().access.SetKeyRules(e.ctx, &nopacsv1.SetKeyRulesRequest{Id: id, Networks: []string{"203.0.113.0/24"}}); err != nil {
		t.Fatal(err)
	}
	if got := e.post(c, whoAmIPath, key, map[string]string{"X-Forwarded-For": "203.0.113.9"}); got != http.StatusOK {
		t.Errorf("a key from inside its networks, through the proxy: %d", got)
	}
	if got := e.post(c, whoAmIPath, key, map[string]string{"X-Forwarded-For": "198.51.100.1"}); got != http.StatusUnauthorized {
		t.Errorf("a key from outside its networks, through the proxy: %d", got)
	}
	if err := e.access.Close(e.ctx); err != nil {
		t.Fatal(err)
	}
	k, err := e.master.Key(e.ctx, id)
	if err != nil || k.LastAddress != "203.0.113.9" {
		t.Errorf("last-used address %q, %v; want the forwarded client", k.LastAddress, err)
	}
}

// TestUntrustedProxyIgnored guards that a forwarded header from a peer that
// is not a trusted proxy is ignored: the peer is the caller.
func TestUntrustedProxyIgnored(t *testing.T) {
	e := newEnv(t, options{accessRules: rulesOf(func(r *server.AccessRules) { r.TrustedProxies = nets(t, "10.0.0.0/8") })})
	c := e.tlsClient(false)
	e.post(c, whoAmIPath, "nopacs_nope_nope", map[string]string{"X-Forwarded-For": "203.0.113.9", "Forwarded": "for=203.0.113.9"})
	refused := e.entries(master.OutcomeRefused)
	if len(refused) != 1 || !strings.HasPrefix(refused[0].RemoteAddress, "127.0.0.1:") {
		t.Errorf("entries %+v, want the peer's own address", refused)
	}
}

// guarded starts a server whose limits on failed calls are limit failures
// in a minute and a block of an hour, on a clock the test moves, with
// slowed refusals held for delay.
func guarded(t *testing.T, limit int, delay time.Duration) (*env, *atomic.Int64) {
	var offset atomic.Int64
	sink := &eventSink{}
	e := newEnv(t, options{
		accessRules: rulesOf(func(r *server.AccessRules) {
			r.FailureLimit, r.FailureWindow, r.BlockTime = limit, time.Minute, time.Hour
		}),
		events: sink.raise,
		beforeRun: func(s *server.Server) {
			s.SetGuardClock(func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }, delay)
		},
	})
	return e, &offset
}

// eventSink collects access events.
type eventSink struct {
	mu sync.Mutex
	ev []access.Event
}

// raise is the sink's access.Events.
func (s *eventSink) raise(e access.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ev = append(s.ev, e)
}

// TestAddressBlocked guards the limit per address: past the limit a bad
// key's refusal is slowed, at twice it the address is blocked, audited once
// as a block, and then even a good key from it is refused, until the block
// ends.
func TestAddressBlocked(t *testing.T) {
	const delay = 300 * time.Millisecond
	e, offset := guarded(t, 2, delay)
	key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	c := e.tlsClient(false)
	var took []time.Duration
	for range 4 {
		start := time.Now()
		if got := e.post(c, whoAmIPath, "nopacs_nope_nope", nil); got != http.StatusUnauthorized {
			t.Fatalf("%d", got)
		}
		took = append(took, time.Since(start))
	}
	if took[1] >= delay || took[2] < delay || took[3] < delay {
		t.Errorf("refusals took %v; the third and fourth, past the limit of 2, are held %v", took, delay)
	}
	if got := e.post(c, whoAmIPath, key, nil); got != http.StatusUnauthorized {
		t.Errorf("a good key from a blocked address: %d, want 401", got)
	}
	refused := e.entries(master.OutcomeRefused)
	blocked := withReason(refused, access.RuleReason(access.RuleAddressBlocked))
	if len(blocked) != 2 {
		t.Errorf("%d entries of the block (its start and the good key refused), want 2: %+v", len(blocked), refused)
	}
	offset.Store(int64(time.Hour + time.Second))
	if got := e.post(c, whoAmIPath, key, nil); got != http.StatusOK {
		t.Errorf("a good key after the block: %d", got)
	}
	if _, err := e.local().access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
		t.Errorf("the local socket was blocked: %v", err)
	}
}

// TestKeyBlocked guards the limit per key: a good key refused for its
// grants again and again is blocked, then refused for any call, while
// another key from the same address is not.
func TestKeyBlocked(t *testing.T) {
	e, _ := guarded(t, 2, 0)
	weak := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	other := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	c := e.tlsClient(false)
	for range 4 {
		if got := e.post(c, listKeysPath, weak, nil); got != http.StatusForbidden {
			t.Fatalf("%d, want 403", got)
		}
	}
	if got := e.post(c, whoAmIPath, weak, nil); got != http.StatusUnauthorized {
		t.Errorf("a blocked key's allowed call: %d, want 401", got)
	}
	if got := e.post(c, whoAmIPath, other, nil); got != http.StatusOK {
		t.Errorf("another key from the same address: %d", got)
	}
	id := strings.Split(weak, "_")[1]
	blocked := withReason(e.entries(master.OutcomeRefused), access.RuleReason(access.RuleKeyBlocked))
	if len(blocked) != 2 || blocked[0].KeyID != id {
		t.Errorf("entries of the block %+v", blocked)
	}
}

// TestBusyKeyNeverSlowed guards that a key's successful requests are never
// counted: many in a row, with a limit of one failure, are neither slowed
// nor blocked, while the refusals of a bad key that follow are held, so the
// count of holds the test reads is the one that rises.
func TestBusyKeyNeverSlowed(t *testing.T) {
	const delay = 100 * time.Millisecond
	e, _ := guarded(t, 1, delay)
	key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	c := e.tlsClient(false)
	start := time.Now()
	for i := range 200 {
		if got := e.post(c, whoAmIPath, key, nil); got != http.StatusOK {
			t.Fatalf("request %d: %d", i, got)
		}
	}
	// Counted, not timed: a run under the race detector on a busy machine
	// takes as long as one hold for 200 requests that were never held.
	if held := e.srv.GuardHolds(); held != 0 {
		t.Errorf("200 successful requests (%v) held %d times; none may be held", time.Since(start), held)
	}
	if blocked := withReason(e.entries(master.OutcomeRefused), access.RuleReason(access.RuleAddressBlocked)); len(blocked) != 0 {
		t.Errorf("a block of a busy key's address: %+v", blocked)
	}
	for range 2 {
		if got := e.post(c, whoAmIPath, "nopacs_nope_nope", nil); got != http.StatusUnauthorized {
			t.Fatalf("a bad key: %d", got)
		}
	}
	if held := e.srv.GuardHolds(); held != 1 {
		t.Errorf("two bad keys past the limit of one held %d times, want 1", held)
	}
}

// TestKeyClientCertOverTLS guards a key that must come with a client
// certificate: over TLS with that certificate it is allowed, without one or
// with another it is refused.
func TestKeyClientCertOverTLS(t *testing.T) {
	e := newEnv(t, options{})
	key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	mine, fp := clientCert(t)
	theirs, _ := clientCert(t)
	id := strings.Split(key, "_")[1]
	if _, err := e.local().access.SetKeyRules(e.ctx, &nopacsv1.SetKeyRulesRequest{Id: id, ClientCertSha256: fp}); err != nil {
		t.Fatal(err)
	}
	with := func(cert *tls.Certificate) *http.Client {
		cfg := &tls.Config{RootCAs: e.roots, MinVersion: tls.VersionTLS12}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		e.track(c)
		return c
	}
	if got := e.post(with(&mine), whoAmIPath, key, nil); got != http.StatusOK {
		t.Errorf("with its certificate: %d", got)
	}
	if got := e.post(with(nil), whoAmIPath, key, nil); got != http.StatusUnauthorized {
		t.Errorf("without a certificate: %d", got)
	}
	if got := e.post(with(&theirs), whoAmIPath, key, nil); got != http.StatusUnauthorized {
		t.Errorf("with another certificate: %d", got)
	}
}

// clientCert makes a self-signed client certificate and returns it with its
// SHA-256 fingerprint as a key holds it.
func clientCert(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "client"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

// TestCanceledRequestStillRefreshesRules guards the refresh of the access
// settings against a caller who resets its request at once: when such a
// request is the one that reads the settings again, the read still happens,
// and the next request is judged by the new rules, a deny list just written
// included. A read on the request's context failed with it, and the old
// rules were kept for another refresh period, again and again.
func TestCanceledRequestStillRefreshesRules(t *testing.T) {
	var reads atomic.Int64
	read := func(ctx context.Context) (server.AccessRules, error) {
		if err := ctx.Err(); err != nil {
			return server.AccessRules{}, err
		}
		r := server.DefaultAccessRules()
		if reads.Add(1) >= 2 {
			r.Denied = nets(t, "203.0.113.0/24")
		}
		return r, nil
	}
	var now atomic.Int64
	current := server.GuardRules(read, func() time.Time { return time.Unix(now.Load(), 0) })
	if r, ok := current(context.Background()); !ok || len(r.Denied) != 0 {
		t.Fatalf("first rules %+v, %v", r, ok)
	}
	now.Add(6) // Past the refresh period.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	current(canceled)
	r, ok := current(context.Background())
	if !ok || len(r.Denied) == 0 {
		t.Errorf("after a canceled request did the refresh, the next request sees %+v (%d reads); want the new deny list", r, reads.Load())
	}
}

// TestOneRulesReadAtATime guards the refresh of the access settings under
// load: while one read is in flight, other requests are judged by the rules
// last read without waiting for it and without reading again, and the
// guard's lock is not held across the read.
func TestOneRulesReadAtATime(t *testing.T) {
	var reads atomic.Int64
	gate := make(chan struct{})
	read := func(context.Context) (server.AccessRules, error) {
		r := server.DefaultAccessRules()
		if reads.Add(1) >= 2 {
			<-gate
			r.FailureLimit = 99
		}
		return r, nil
	}
	var now atomic.Int64
	current := server.GuardRules(read, func() time.Time { return time.Unix(now.Load(), 0) })
	current(context.Background())
	now.Add(6)
	slow := make(chan server.AccessRules, 1)
	go func() {
		r, _ := current(context.Background())
		slow <- r
	}()
	for reads.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			done := make(chan server.AccessRules, 1)
			go func() {
				r, _ := current(context.Background())
				done <- r
			}()
			select {
			case r := <-done:
				if r.FailureLimit != 10 {
					t.Errorf("a request during the read sees limit %d, want the last rules' 10", r.FailureLimit)
				}
			case <-time.After(5 * time.Second):
				t.Error("a request waited on the read in flight")
			}
		})
	}
	wg.Wait()
	if n := reads.Load(); n != 2 {
		t.Errorf("%d reads, want 2: one in flight at a time", n)
	}
	close(gate)
	if r := <-slow; r.FailureLimit != 99 {
		t.Errorf("the reading request got limit %d, want the new 99", r.FailureLimit)
	}
	if r, _ := current(context.Background()); r.FailureLimit != 99 {
		t.Errorf("after the read the rules hold limit %d, want 99", r.FailureLimit)
	}
}

// TestRefusedNetworkSlowed guards the installation's networks under the
// limits on failed calls: requests from a denied network are counted per
// address like bad keys, past the limit held before they are answered,
// and at twice it the address is blocked.
func TestRefusedNetworkSlowed(t *testing.T) {
	const delay = 300 * time.Millisecond
	e := newEnv(t, options{
		accessRules: rulesOf(func(r *server.AccessRules) {
			r.Denied = nets(t, "127.0.0.0/8")
			r.FailureLimit, r.FailureWindow, r.BlockTime = 2, time.Minute, time.Hour
		}),
		beforeRun: func(s *server.Server) { s.SetGuardClock(time.Now, delay) },
	})
	key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	c := e.tlsClient(false)
	var took []time.Duration
	for range 4 {
		start := time.Now()
		if got := e.post(c, whoAmIPath, key, nil); got != http.StatusUnauthorized {
			t.Fatalf("%d, want 401", got)
		}
		took = append(took, time.Since(start))
	}
	if took[1] >= delay || took[2] < delay || took[3] < delay {
		t.Errorf("refusals took %v; the third and fourth, past the limit of 2, are held %v", took, delay)
	}
	if blocked := withReason(e.entries(master.OutcomeRefused), access.RuleReason(access.RuleAddressBlocked)); len(blocked) != 1 {
		t.Errorf("%d entries of a block, want 1 (its start)", len(blocked))
	}
}

// TestMalformedMessageCountsFailures guards the check made when a request's
// message cannot be read: a key it refuses is counted like any key refused
// for its grants, so a malformed body is no way around the limits; past
// the limit the refusal is held, and at twice it the key is blocked.
func TestMalformedMessageCountsFailures(t *testing.T) {
	const delay = 300 * time.Millisecond
	e, _ := guarded(t, 2, delay)
	weak := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	c := e.tlsClient(false)
	var took []time.Duration
	for range 4 {
		req, _ := http.NewRequestWithContext(e.ctx, http.MethodPost, e.netURL+nopacsv1connect.SettingsServiceGetSettingProcedure, strings.NewReader("{not json"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+weak)
		start := time.Now()
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		took = append(took, time.Since(start))
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%d, want 403", res.StatusCode)
		}
	}
	if took[1] >= delay || took[2] < delay || took[3] < delay {
		t.Errorf("refusals took %v; the third and fourth, past the limit of 2, are held %v", took, delay)
	}
	if got := e.post(c, whoAmIPath, weak, nil); got != http.StatusUnauthorized {
		t.Errorf("the key after four refusals of a malformed message: %d, want 401 (blocked)", got)
	}
}
