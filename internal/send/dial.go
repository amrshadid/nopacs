package send

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// connectError is a connection to a sender's peer that failed before
// anything was sent: what counts toward the sender's health. Its fault and
// detail are the contract's fixed words; err is the cause, for the log
// only, and never answered.
type connectError struct {
	fault  tenant.SenderFault
	detail string
	err    error
}

// Error says the fault and the cause.
func (e *connectError) Error() string {
	if e.err == nil {
		return "send: " + e.detail
	}
	return "send: " + e.detail + ": " + e.err.Error()
}

// Unwrap returns the cause.
func (e *connectError) Unwrap() error { return e.err }

// The fixed phrases of a connection's faults (SenderHealth.detail).
const (
	phraseNoResolve       = "the host name did not resolve"
	phraseUnreachable     = "the peer could not be reached in time"
	phraseNotAllowed      = "the host resolves to no address sender.destinations allows"
	phraseNoDestinations  = "sender.destinations cannot be read"
	phrasePlainHTTP       = "plain http is not allowed by sender.allow-plain-http"
	phraseTLSFiles        = "a TLS file of the sender cannot be read or does not hold what it should"
	phraseTLSHandshake    = "the TLS handshake failed"
	phraseCertificate     = "the peer's certificate does not verify"
	phraseCredential      = "the peer does not take the credential"
	phraseNoContexts      = "no presentation context accepted"
	phraseAborted         = "the peer aborted the association"
	phraseWrongAnswer     = "the peer answered what the protocol does not allow"
	phraseServerError     = "the peer answered a server error"
	phraseRedirect        = "the peer answered a redirect, which is not followed"
	phraseNoService       = "the URL does not name a DICOMweb service"
	phraseNoCredential    = "the sender's credential cannot be opened" //nolint:gosec // G101: a phrase for a person, not a credential.
	phraseEchoNotAccepted = "the peer did not answer C-ECHO with success"
)

// resolve returns the addresses a connection to host at port may reach:
// host resolved once, and those of its addresses the installation's
// sender.destinations allows for host and port (settings.Destinations.
// Allowed), in the order of the answer. A connection is made to these
// addresses alone, the first that takes it, so a DNS answer cannot move it
// to an address the setting does not allow.
func (s *Supervisor) resolve(ctx context.Context, host string, port int) ([]netip.AddrPort, error) {
	dests, err := s.cfg.Installation.SenderDestinations(ctx)
	if err != nil {
		return nil, &connectError{fault: tenant.SenderFaultAddressNotAllowed, detail: phraseNoDestinations, err: err}
	}
	var ips []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil && a.Zone() == "" {
		ips = []netip.Addr{a}
	} else {
		ips, err = s.cfg.seams.lookup(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, &connectError{fault: tenant.SenderFaultUnreachable, detail: phraseNoResolve, err: err}
		}
	}
	var out []netip.AddrPort
	var refusal error
	for _, ip := range ips {
		ip = ip.Unmap().WithZone("")
		if err := dests.Allowed(host, ip, port); err != nil {
			refusal = err
			continue
		}
		out = append(out, netip.AddrPortFrom(ip, uint16(port))) // #nosec G115 -- a stored sender's port is 1 to 65535.
	}
	if len(out) == 0 {
		return nil, &connectError{fault: tenant.SenderFaultAddressNotAllowed, detail: phraseNotAllowed, err: refusal}
	}
	return out, nil
}

// connRecord keeps the last connection of a client that failed before an
// HTTP request could be sent on it. net/http and the DICOMweb client above
// it may hand back only a closed pipe for such a request, so the dial
// itself says what happened.
type connRecord struct {
	mu   sync.Mutex
	last *connectError
}

// set records a failed connection.
func (r *connRecord) set(ce *connectError) {
	r.mu.Lock()
	r.last = ce
	r.mu.Unlock()
}

// take returns the failure recorded since the last take, and forgets it.
func (r *connRecord) take() *connectError {
	r.mu.Lock()
	defer r.mu.Unlock()
	ce := r.last
	r.last = nil
	return ce
}

// dial connects to host at port through resolve, trying each allowed
// address in turn until one takes the connection, and records a failure in
// rec when rec is not nil.
func (s *Supervisor) dial(ctx context.Context, host string, port int, d *net.Dialer, rec *connRecord) (net.Conn, error) {
	aps, err := s.resolve(ctx, host, port)
	if err != nil {
		if rec != nil {
			var ce *connectError
			if errors.As(err, &ce) {
				rec.set(ce)
			}
		}
		return nil, err
	}
	var last error
	for _, ap := range aps {
		conn, err := d.DialContext(ctx, "tcp", ap.String())
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	ce := &connectError{fault: tenant.SenderFaultUnreachable, detail: phraseUnreachable, err: last}
	if rec != nil {
		rec.set(ce)
	}
	return nil, ce
}

// dialContext returns a dial function that connects to host at port only
// through resolve, whatever address it is asked for: an HTTP transport's.
// Its connections cut a write that makes no progress for stall
// (stallConn).
func (s *Supervisor) dialContext(host string, port int, d *net.Dialer, stall time.Duration, rec *connRecord) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := s.dial(ctx, host, port, d, rec)
		if err != nil {
			return nil, err
		}
		return &stallConn{Conn: conn, timeout: stall}, nil
	}
}

// dialTLSContext returns a dial function that connects as dialContext does
// and completes the TLS handshake with cfg, recording a handshake that
// fails as tls_failed.
func (s *Supervisor) dialTLSContext(host string, port int, d *net.Dialer, cfg *tls.Config, stall time.Duration, rec *connRecord) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		raw, err := s.dial(ctx, host, port, d, rec)
		if err != nil {
			return nil, err
		}
		conn := &stallConn{Conn: raw, timeout: stall}
		tc := tls.Client(conn, cfg.Clone())
		hctx, cancel := context.WithTimeout(ctx, d.Timeout)
		defer cancel()
		if err := tc.HandshakeContext(hctx); err != nil {
			_ = conn.Close()
			phrase, ok := tlsFailure(err)
			if !ok {
				phrase = phraseTLSHandshake
			}
			ce := &connectError{fault: tenant.SenderFaultTLSFailed, detail: phrase, err: err}
			if !ok && hctx.Err() != nil {
				// A peer that never answered the handshake.
				ce = &connectError{fault: tenant.SenderFaultUnreachable, detail: phraseUnreachable, err: err}
			}
			rec.set(ce)
			return nil, ce
		}
		return tc, nil
	}
}

// tlsConfig returns the TLS configuration of a sender's connections: the
// peer verified against the authorities of t's CA file, or the system roots
// when it names none (a stow sender), for t's server name or host; the
// client certificate shown when t names one (PS3.15 B.9, BCP 195). Each file
// is a name in the tenant's TLS directory dir; a name with a path in it is
// refused, though the store refuses it first.
func tlsConfig(dir string, t *tenant.SenderTLS, host string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if t == nil {
		return cfg, nil
	}
	if t.ServerName != "" {
		cfg.ServerName = t.ServerName
	}
	file := func(name string) (string, error) {
		if name == "" || filepath.Base(name) != name || name[0] == '.' {
			return "", &connectError{fault: tenant.SenderFaultTLSFailed, detail: phraseTLSFiles}
		}
		return filepath.Join(dir, name), nil
	}
	if t.CAFile != "" {
		path, err := file(t.CAFile)
		if err != nil {
			return nil, err
		}
		pem, err := os.ReadFile(path) // #nosec G304 -- a file name the store validated, in the tenant's TLS directory.
		if err != nil {
			return nil, &connectError{fault: tenant.SenderFaultTLSFailed, detail: phraseTLSFiles, err: err}
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, &connectError{fault: tenant.SenderFaultTLSFailed, detail: phraseTLSFiles}
		}
		cfg.RootCAs = pool
	}
	if t.CertificateFile != "" || t.PrivateKeyFile != "" {
		certPath, err := file(t.CertificateFile)
		if err != nil {
			return nil, err
		}
		keyPath, err := file(t.PrivateKeyFile)
		if err != nil {
			return nil, err
		}
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, &connectError{fault: tenant.SenderFaultTLSFailed, detail: phraseTLSFiles, err: err}
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// tlsFailure reports whether err is a TLS handshake that failed, and with
// which phrase: a certificate that does not verify, or any other.
func tlsFailure(err error) (string, bool) {
	var verify *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &invalid):
		return phraseCertificate, true
	}
	var record tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &record) || errors.As(err, &alert) {
		return phraseTLSHandshake, true
	}
	return "", false
}
