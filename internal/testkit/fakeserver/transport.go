package fakeserver

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

// Seen is one HTTP request as the server received it, kept whole so a test can
// look for something that must not be in it.
type Seen struct {
	// URL is the request's path and query.
	URL string
	// Header is every header the request carried.
	Header http.Header
	// Body is the request's body, as sent.
	Body []byte
	// ClientCertSHA256 is the SHA-256 of the TLS client certificate the
	// connection presented, lowercase hexadecimal, as the real server pins
	// it; empty when none was presented.
	ClientCertSHA256 string
}

// SeenRequests returns every HTTP request the server has received, in order.
func (s *Server) SeenRequests() []Seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Seen(nil), s.seen...)
}

// CertPEM returns the certificate ServeTLS made, in PEM form, as a file of
// authorities a client can be told to trust. It is empty before ServeTLS.
func (s *Server) CertPEM() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.certPEM
}

// handler returns the fake as an HTTP handler, the way the real server mounts
// its services, inside a recorder of what each request carried.
func (s *Server) handler() http.Handler {
	srv := connect.NewServer(s.interceptor)
	s.Register(srv)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv)
	mux.HandleFunc("GET /tenants/{tenant}/instances/{subject}/{session}/{scan}/{id}", s.serveInstance)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
		seen := Seen{URL: r.URL.String(), Header: r.Header.Clone(), Body: body}
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
			seen.ClientCertSHA256 = hex.EncodeToString(sum[:])
		}
		s.mu.Lock()
		s.seen = append(s.seen, seen)
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
}

// serve runs an HTTP server on a listener until the test ends, and fails the
// test if the server stops for any reason but being closed.
func serve(tb testing.TB, srv *http.Server, ln net.Listener, tlsOn bool) {
	tb.Helper()
	// A client that refuses the certificate makes the server log a handshake
	// error; that is the test's intent, so it is not printed.
	srv.ErrorLog = log.New(io.Discard, "", 0)
	done := make(chan error, 1)
	go func() {
		if tlsOn {
			done <- srv.ServeTLS(ln, "", "")
		} else {
			done <- srv.Serve(ln)
		}
	}()
	tb.Cleanup(func() {
		_ = srv.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			tb.Errorf("the fake server stopped: %v", err)
		}
	})
}

// ServeUnix serves the fake on the local socket of a new data directory, with
// HTTP/2 without TLS, and returns the directory. The socket is at
// run/nopacs.sock, where the installation's layout puts it. The directory is
// short and under the system's temporary directory, because a socket path has
// a small limit that a test's own long directory name could pass.
func (s *Server) ServeUnix(tb testing.TB) (dataDir string) {
	tb.Helper()
	dir, err := os.MkdirTemp("", "cor")
	if err != nil {
		tb.Fatalf("making a data directory: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Mkdir(filepath.Join(dir, "run"), 0o700); err != nil {
		tb.Fatalf("making run/: %v", err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "run", "nopacs.sock"))
	if err != nil {
		tb.Fatalf("listening on a unix socket: %v", err)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	serve(tb, &http.Server{Handler: s.handler(), Protocols: p, ReadHeaderTimeout: 10 * time.Second}, ln, false)
	return dir
}

// ServeTLS serves the fake over TLS on the loopback address with a certificate
// made here for it, and returns its https address. CertPEM is the certificate,
// for a client told to trust it; a client with the system's authorities does
// not. A client certificate is asked for, never required, and recorded in
// Seen.
func (s *Server) ServeTLS(tb testing.TB) (addr string) {
	tb.Helper()
	return s.ServeTLSVersions(tb, 0, 0)
}

// ServeTLSVersions is ServeTLS with the versions of TLS the server offers: min
// and max are tls.VersionTLS10 and so on, and zero leaves the default. It is
// how a test shows that a client refuses a server that offers only an old
// version.
func (s *Server) ServeTLSVersions(tb testing.TB, minVersion, maxVersion uint16) (addr string) {
	tb.Helper()
	cert, certPEM := selfSigned(tb)
	s.mu.Lock()
	s.certPEM = certPEM
	s.mu.Unlock()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listening on loopback: %v", err)
	}
	srv := &http.Server{
		Handler: s.handler(),
		// A client certificate is asked for and not required, as the real
		// server asks, so a test sees what a client presents.
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, MaxVersion: maxVersion, ClientAuth: tls.RequestClientCert},
		ReadHeaderTimeout: 10 * time.Second,
	}
	if minVersion != 0 {
		srv.TLSConfig.MinVersion = minVersion
	}
	serve(tb, srv, ln, true)
	return "https://" + ln.Addr().String()
}

// ServeHTTP serves the fake over plain http on the loopback address, the way
// a server behind a TLS proxy on the same machine would be reached, and
// returns its address.
func (s *Server) ServeHTTP(tb testing.TB) (addr string) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listening on loopback: %v", err)
	}
	serve(tb, &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}, ln, false)
	return "http://" + ln.Addr().String()
}

// selfSigned makes a certificate valid for the loopback names, and the same
// certificate in PEM form.
func selfSigned(tb testing.TB) (tls.Certificate, []byte) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("making a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nopacs test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatalf("making a certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pemBytes
}

// WriteClientCert writes a new self-signed TLS client certificate and its
// private key as PEM files in dir, the key private to its owner, and returns
// their paths and the certificate's SHA-256 as the server pins it, for a
// test of a client that presents one.
func WriteClientCert(tb testing.TB, dir, name string) (certPath, keyPath, sum string) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		tb.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		tb.Fatal(err)
	}
	s := sha256.Sum256(der)
	return certPath, keyPath, hex.EncodeToString(s[:])
}
