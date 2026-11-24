package dimse

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxTLSFile bounds a certificate chain or key file read: 1 MiB is far past
// any real chain, and keeps a file that is not one from being read whole.
const maxTLSFile = 1 << 20

// loadTLS reads a receiver's certificate chain and private key from the
// tenant's TLS directory (receiver.proto, ReceiverTls) and returns the
// server side of TLS (PS3.15 B.9: TLS 1.2 at least, BCP 195). Each file is
// opened through an os.Root of the directory, so a name cannot leave it. A
// file that cannot be read, a chain whose first certificate does not parse,
// a key that does not match it, or a certificate outside its validity at
// now is a DownError with the health's phrase for it.
func loadTLS(dir string, files tenant.ReceiverTLS, now time.Time) (*tls.Config, *x509.Certificate, error) {
	certPEM, err := readTLSFile(dir, files.CertificateFile)
	if err != nil {
		return nil, nil, &DownError{Reason: tenant.DownTLS, Detail: tenant.PhraseCertificateUnreadable, Err: err}
	}
	keyPEM, err := readTLSFile(dir, files.PrivateKeyFile)
	if err != nil {
		return nil, nil, &DownError{Reason: tenant.DownTLS, Detail: tenant.PhraseKeyUnreadable, Err: err}
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, &DownError{Reason: tenant.DownTLS, Detail: tenant.PhraseCertificateUnreadable, Err: errors.New("no PEM certificate")}
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, &DownError{Reason: tenant.DownTLS, Detail: tenant.PhraseCertificateUnreadable, Err: err}
	}
	if k, _ := pem.Decode(keyPEM); k == nil {
		return nil, nil, &DownError{Reason: tenant.DownTLS, Detail: tenant.PhraseKeyUnreadable, Err: errors.New("no PEM key")}
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, &DownError{Reason: tenant.DownTLS, Detail: tenant.PhraseCertificateKeyMismatch, Err: err}
	}
	if err := validAt(leaf, now); err != nil {
		return nil, nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, leaf, nil
}

// validAt returns the DownError of a certificate outside its validity at
// now (RFC 5280 4.1.2.5), or nil.
func validAt(leaf *x509.Certificate, now time.Time) error {
	switch {
	case now.After(leaf.NotAfter):
		return &DownError{Reason: tenant.DownTLS, Detail: tenant.PhraseCertificateExpired, Err: errors.New("certificate expired")}
	case now.Before(leaf.NotBefore):
		return &DownError{Reason: tenant.DownTLS, Detail: tenant.PhraseCertificateNotYet, Err: errors.New("certificate not yet valid")}
	}
	return nil
}

// readTLSFile reads one file of the TLS directory, at most maxTLSFile bytes.
func readTLSFile(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxTLSFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxTLSFile {
		return nil, errors.New("the file is larger than a TLS file can be")
	}
	return b, nil
}
