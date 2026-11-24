package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// certRecheck is how long the network listener serves its certificate
// before it reads the files again on a handshake: a certificate renewed in
// place (by an ACME client, or by hand) is served within this time, with no
// restart and no request lost, and the files are read at most once in it.
const certRecheck = 5 * time.Second

// maxCertFile bounds what is read of the certificate's chain or key file:
// a chain of a few certificates is a few kilobytes.
const maxCertFile = 1 << 20

// certFiles is the network listener's certificate, kept from its files
// (--tls-cert, --tls-key). It is read at start, where a pair that does not
// load stops the server, and again on a handshake once certRecheck has
// passed: when either file's content changed, the pair is loaded and
// served from then on. A pair that does not load then (a renewal half
// written, a key that does not match) is logged and the pair served before
// is kept, so a mistake on disk never takes the listener down; it is tried
// again when the files change again.
type certFiles struct {
	certPath, keyPath string
	log               *slog.Logger
	// now is the clock the recheck is timed by: time.Now, but in a test.
	now func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	sum     [sha256.Size]byte
	checked time.Time
}

// loadCertFiles reads the pair at start. The error names the files and why
// they do not load; it holds no key material.
func loadCertFiles(certPath, keyPath string, log *slog.Logger) (*certFiles, error) {
	c := &certFiles{certPath: certPath, keyPath: keyPath, log: log, now: time.Now}
	certPEM, keyPEM, sum, err := c.read()
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "server: the TLS certificate and key do not load: "+err.Error())
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	clear(keyPEM)
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "server: the TLS certificate and key do not load: "+err.Error())
	}
	c.cert, c.sum, c.checked = &cert, sum, c.now()
	return c, nil
}

// read returns the content of both files and the SHA-256 of the two
// together, which tells a change of either.
func (c *certFiles) read() (certPEM, keyPEM []byte, sum [sha256.Size]byte, err error) {
	certPEM, err = readCertFile(c.certPath)
	if err != nil {
		return nil, nil, sum, err
	}
	keyPEM, err = readCertFile(c.keyPath)
	if err != nil {
		return nil, nil, sum, err
	}
	h := sha256.New()
	h.Write(certPEM)
	h.Write([]byte{0})
	h.Write(keyPEM)
	copy(sum[:], h.Sum(nil))
	return certPEM, keyPEM, sum, nil
}

// readCertFile reads one file, at most maxCertFile bytes.
func readCertFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator's own --tls-cert and --tls-key.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var b bytes.Buffer
	if _, err := io.Copy(&b, io.LimitReader(f, maxCertFile+1)); err != nil {
		return nil, err
	}
	if b.Len() > maxCertFile {
		return nil, errors.New(path + " is larger than a certificate or a key can be")
	}
	return b.Bytes(), nil
}

// get is the listener's tls.Config.GetCertificate: the pair as last loaded,
// read again first when certRecheck has passed since the last look.
func (c *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if now.Sub(c.checked) < certRecheck {
		return c.cert, nil
	}
	c.checked = now
	certPEM, keyPEM, sum, err := c.read()
	defer clear(keyPEM)
	if err != nil {
		c.log.Warn("server: the TLS certificate files cannot be read; the certificate loaded before is still served", "error", err)
		return c.cert, nil
	}
	if sum == c.sum {
		return c.cert, nil
	}
	c.sum = sum
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		c.log.Warn("server: the TLS certificate files changed but do not load; the certificate loaded before is still served",
			"cert", c.certPath, "key", c.keyPath, "error", err)
		return c.cert, nil
	}
	c.cert = &cert
	c.log.Info("server: the TLS certificate was loaded again from its files", "cert", c.certPath)
	return c.cert, nil
}
