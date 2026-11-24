package server_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/server"
)

// TestCertificateRenewedInPlace guards the network listener's certificate
// against a renewal: once its files are replaced and the recheck has
// passed, a new handshake is served the new certificate, with no restart;
// before the recheck the old one is still served; and files that change
// into a pair that does not load are logged and leave the last good
// certificate served. Loaded once at start, as it was, a renewed
// certificate was served only after a restart, and an expired one until
// then.
func TestCertificateRenewedInPlace(t *testing.T) {
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	e := newEnv(t, options{beforeRun: func(s *server.Server) { s.SetCertClock(clock) }})
	addr := strings.TrimPrefix(e.netURL, "https://")
	served := func(roots *x509.CertPool) []byte {
		t.Helper()
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		defer func() { _ = conn.Close() }()
		return conn.ConnectionState().PeerCertificates[0].Raw
	}
	first := served(e.roots)

	// testCert writes a new pair over the files the server was started
	// with.
	_, _, roots := testCert(t, e.dir)
	if got := served(e.roots); !bytes.Equal(got, first) {
		t.Fatal("the files were read again before the recheck")
	}
	offset.Store(int64(6 * time.Second))
	second := served(roots)
	if bytes.Equal(second, first) {
		t.Fatal("the renewed certificate was not served after the recheck")
	}

	// A pair that does not load: the key is replaced by garbage.
	if err := os.WriteFile(filepath.Join(e.dir, "cert-key.pem"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	offset.Store(int64(12 * time.Second))
	if got := served(roots); !bytes.Equal(got, second) {
		t.Error("files that do not load replaced the certificate served")
	}
	if log := e.log.String(); !strings.Contains(log, "the TLS certificate files changed but do not load") || strings.Contains(log, "not a key") {
		t.Errorf("the failed reload was not logged as it should be:\n%s", log)
	}
}
