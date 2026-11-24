package dimse_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/receive/dimse"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// writePair writes a self-signed certificate for 127.0.0.1, valid from
// notBefore to notAfter, and its key, as cert.pem and key.pem in dir; with
// otherKey the key file holds a key that is not the certificate's.
func writePair(t *testing.T, dir string, notBefore, notAfter time.Time, otherKey bool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "archive"},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: nil,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if otherKey {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			t.Fatal(err)
		}
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"cert.pem": {Type: "CERTIFICATE", Bytes: der}, "key.pem": {Type: "PRIVATE KEY", Bytes: kb},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// tlsRow is row() with TLS from cert.pem and key.pem.
func tlsRow() tenant.Receiver {
	return row(func(r *tenant.Receiver) {
		r.DIMSE.TLS = &tenant.ReceiverTLS{CertificateFile: "cert.pem", PrivateKeyFile: "key.pem"}
	})
}

// TestTLSReceiverAnswersOverTLSOnly guards a receiver with TLS: dcmtk's
// echoscu over TLS is answered, and the same over plain TCP is not and is
// reported as a failed handshake.
func TestTLSReceiverAnswersOverTLSOnly(t *testing.T) {
	echoscu := tool(t, "echoscu")
	dir := t.TempDir()
	writePair(t, dir, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), false)
	s := listen(t, tlsRow(), &fakeSink{}, nil, func(c *dimse.Config) { c.TLSDir = dir })
	if out, err := command(t, echoscu, "+tla", "-ic", "-aet", "SCANNER", "-aec", "ARCHIVE", s.host, s.port).CombinedOutput(); err != nil { // #nosec G204
		t.Fatalf("echoscu over TLS: %v\n%s", err, out)
	}
	if out, err := command(t, echoscu, "-to", "5", "-aet", "SCANNER", "-aec", "ARCHIVE", s.host, s.port).CombinedOutput(); err == nil { // #nosec G204
		t.Fatalf("echoscu without TLS was answered:\n%s", out)
	}
	// The plain caller's failed handshake is reported, with its address and
	// no AE titles, which a handshake never reaches.
	waitFor(t, "the failed handshake to be reported", func() bool {
		for _, f := range s.reports.get() {
			if f.failure == tenant.FailureTLSFailed {
				if f.caller.Address != "127.0.0.1" || f.caller.CallingAETitle != "" {
					t.Fatalf("the failed handshake was reported with %+v", f.caller)
				}
				return true
			}
		}
		return false
	})
}

// TestUnusableTLSFilesAreDownWithTheirPhrase guards Start's refusal of TLS
// files it cannot use, each with the health's phrase: no file, a key that
// is not the certificate's, a certificate past its end, and one before its
// start.
func TestUnusableTLSFilesAreDownWithTheirPhrase(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		write  func(dir string)
		detail string
	}{
		{"no file", func(string) {}, "certificate file cannot be read"},
		{"another key", func(d string) { writePair(t, d, now.Add(-time.Hour), now.Add(time.Hour), true) }, "certificate and key do not match"},
		{"expired", func(d string) { writePair(t, d, now.Add(-2*time.Hour), now.Add(-time.Hour), false) }, "certificate expired"},
		{"not yet valid", func(d string) { writePair(t, d, now.Add(time.Hour), now.Add(2*time.Hour), false) }, "certificate not yet valid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.write(dir)
			l, err := dimse.New(dimse.Config{Receiver: tlsRow(), TLSDir: dir, BindAddress: "127.0.0.1", Sink: &fakeSink{}})
			if err != nil {
				t.Fatal(err)
			}
			dimse.SetAnyPort(l)
			err = l.Start(context.Background())
			var d *dimse.DownError
			if !errors.As(err, &d) || d.Reason != tenant.DownTLS || d.Detail != c.detail {
				_ = l.Stop(context.Background())
				t.Fatalf("Start: %v, want tls: %s", err, c.detail)
			}
			if !contains(tenant.HealthPhrases(d.Reason), d.Detail) {
				t.Fatalf("the phrase %q is not one of the server's for %s", d.Detail, d.Reason)
			}
		})
	}
}

// contains reports whether list holds s.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestCheckSeesACertificateExpireWhileServing guards Check: a listener that
// started with a valid certificate answers nil while it stays valid, and,
// once the clock passes its end, a DownError with the expired phrase, so
// the supervisor stops it; a listener without TLS always answers nil.
func TestCheckSeesACertificateExpireWhileServing(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writePair(t, dir, now.Add(-time.Hour), now.Add(time.Hour), false)
	s := listen(t, tlsRow(), &fakeSink{}, nil, func(c *dimse.Config) { c.TLSDir = dir })
	if err := s.l.Check(now); err != nil {
		t.Fatalf("Check while valid: %v", err)
	}
	err := s.l.Check(now.Add(2 * time.Hour))
	var d *dimse.DownError
	if !errors.As(err, &d) || d.Reason != tenant.DownTLS || d.Detail != tenant.PhraseCertificateExpired {
		t.Fatalf("Check after the end: %v, want tls, certificate expired", err)
	}
	plain := listen(t, row(), &fakeSink{}, nil)
	if err := plain.l.Check(now.Add(100 * 365 * 24 * time.Hour)); err != nil {
		t.Fatalf("Check without TLS: %v", err)
	}
}
