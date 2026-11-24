package client

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
)

// Environment variables the command line reads. They are named here so the
// help text and the tests use the one spelling.
const (
	// EnvDataDir names the data directory whose run/nopacs.sock is the
	// default way to reach the API.
	EnvDataDir = "NOPACS_DATA_DIR"
	// EnvServer names the address of a server on the network.
	EnvServer = "NOPACS_SERVER"
	// EnvCAFile names a PEM file of certificate authorities to trust for the
	// network, in place of the system's.
	EnvCAFile = "NOPACS_CA_FILE"
	// EnvKey holds the key used over the network. It is the only place a key
	// can come from besides a file.
	EnvKey = "NOPACS_KEY"
	// EnvClientCert names a PEM file of a TLS client certificate to present
	// to the server on the network.
	EnvClientCert = "NOPACS_TLS_CLIENT_CERT"
	// EnvClientKey names the PEM file of that certificate's private key.
	EnvClientKey = "NOPACS_TLS_CLIENT_KEY"
)

// DefaultTimeout is how long one call may take when the person gives no
// --timeout, the same period the contract says the server applies.
const DefaultTimeout = 30 * time.Second

// maxKeyFile bounds the key file that is read: a whole key is under 100 bytes,
// so a larger file is a mistake (a binary, a log) and is refused unread.
const maxKeyFile = 1024

// SocketPath returns where the local socket of a data directory is, as the
// installation's layout fixes it.
func SocketPath(dataDir string) string {
	return filepath.Join(dataDir, "run", "nopacs.sock")
}

// Config says how to reach the API. Exactly one of DataDir and Server is set.
type Config struct {
	// DataDir reaches the API on the local socket of this data directory,
	// where the caller is the local operator and sends no key.
	DataDir string
	// Server is the address of the API on the network, https://host:port, or
	// http:// for a loopback host.
	Server string
	// KeyFile is the path of a file holding the key, for Server. When empty
	// the key is read from the EnvKey variable.
	KeyFile string
	// Getenv reads the environment; os.Getenv when nil. It is a field so a
	// test can pass an environment without changing the process's.
	Getenv func(string) string
	// Timeout bounds each call; DefaultTimeout when zero.
	Timeout time.Duration
	// CAFile is the path of a PEM file of certificate authorities that
	// replaces the system's store for Server, for an institution with a
	// private authority. It is a path, not a secret: the file holds public
	// certificates only. It has no effect on the local socket.
	CAFile string
	// ClientCert and ClientKey are the PEM files of a TLS client
	// certificate and its private key, presented to Server, for a key the
	// server accepts only with that certificate (a key's client
	// certificate rule); both or neither. The key file must be private to
	// the person, as a key file is. They have no effect on the local
	// socket.
	ClientCert, ClientKey string
}

// getenv returns the configured environment reader.
func (c Config) getenv() func(string) string {
	if c.Getenv != nil {
		return c.Getenv
	}
	return os.Getenv
}

// ParseServer checks the address of a server and returns it without a
// trailing slash. https is accepted for any host; http only for a loopback
// host, because a key sent over plain http to another machine can be read on
// the way. An address with a user name or password in it is refused, as that
// is a secret in a place that is logged. No message quotes the address
// itself, which may hold such a secret.
func ParseServer(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return nil, fault.New(fault.Invalid, "the server address is not a URL such as https://host:8443")
	}
	if u.User != nil {
		return nil, fault.New(fault.Invalid, "the server address must not hold a user name or password; the key is read from "+EnvKey+" or --key-file")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fault.New(fault.Invalid, "the server address must not have a query or a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, fault.New(fault.Invalid, "the server address "+u.Scheme+"://"+u.Host+" is refused: http is allowed only for a loopback host (localhost, 127.0.0.1, ::1); use https://")
		}
	default:
		return nil, fault.New(fault.Invalid, "the server address must start with https:// (or http:// for a loopback host)")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// isLoopback reports whether a host names this machine only: the name
// localhost or an address in the loopback range. Any other name could resolve
// anywhere, so it does not count.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ReadKey returns the key to send: the content of keyFile when it is given,
// otherwise the EnvKey variable. Only the key's shape is checked, enough to
// keep a header safe (printable ASCII, no spaces); the server decides whether
// it is good. A message names where the key was looked for and never shows
// any of it.
func ReadKey(keyFile string, getenv func(string) string) (string, error) {
	var key, where string
	if keyFile != "" {
		where = "the key file " + keyFile
		// The file must be private to the person, by the rule the
		// installation's own key file follows, because a key others can read
		// is a key others can use. The refusal says how to fix the file and
		// never quotes it.
		f, err := secret.OpenPrivate(keyFile)
		if err != nil {
			msg := strings.TrimPrefix(fault.Message(err), "secret: ")
			if fault.KindOf(err) == fault.NotFound {
				msg = "cannot read the key file " + keyFile + ": it does not exist"
			}
			return "", fault.Wrap(fault.Invalid, err, msg)
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
		if err != nil {
			return "", fault.Wrap(fault.Invalid, err, "cannot read the key file "+keyFile)
		}
		if len(b) > maxKeyFile {
			return "", fault.New(fault.Invalid, "the key file "+keyFile+" is larger than a key can be")
		}
		key = strings.TrimSpace(string(b))
	} else {
		where = "the " + EnvKey + " variable"
		key = strings.TrimSpace(getenv(EnvKey))
	}
	if key == "" {
		return "", fault.New(fault.Invalid, "no key: set "+EnvKey+" or pass --key-file PATH; "+where+" is empty")
	}
	for i := range len(key) {
		if key[i] <= ' ' || key[i] > '~' {
			return "", fault.New(fault.Invalid, where+" holds characters a key does not have")
		}
	}
	return key, nil
}

// maxCAFile bounds a CA file read: a bundle of every public authority is
// about 200 KiB, so a megabyte is generous and a larger file is a mistake.
const maxCAFile = 1 << 20

// LoadCAs reads a PEM file of certificate authorities into a pool that
// replaces the system's. A file that cannot be read, that is too large, or
// that holds no certificate is a usage error that names the file and never
// quotes any of it.
func LoadCAs(path string) (*x509.CertPool, error) {
	f, err := os.Open(path) // #nosec G304 -- the path is the person's own --ca-file
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read the CA file "+path)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxCAFile+1))
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read the CA file "+path)
	}
	if len(b) > maxCAFile {
		return nil, fault.New(fault.Invalid, "the CA file "+path+" is larger than a file of certificates can be")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fault.New(fault.Invalid, "the CA file "+path+" holds no certificate in PEM form")
	}
	return pool, nil
}

// maxClientFile bounds a client certificate's chain or key file read: a
// chain of a few certificates is a few kilobytes, so a megabyte is generous
// and a larger file is a mistake.
const maxClientFile = 1 << 20

// LoadClientCert reads a TLS client certificate and its private key from PEM
// files. The key file is opened only when it is private to the person
// (secret.OpenPrivate), by the rule a key file follows: a private key others
// can read lets them pass for its holder. Both paths or neither: one alone is
// a usage error. A file that cannot be read, is too large, or does not hold a
// certificate and the key that matches it is a usage error that names the
// file and never quotes any of it. The bytes of the key are cleared once
// read.
func LoadClientCert(certFile, keyFile string) (tls.Certificate, error) {
	if (certFile == "") != (keyFile == "") {
		return tls.Certificate{}, fault.New(fault.Invalid, "a client certificate needs both its file and its private key's file (--tls-client-cert and --tls-client-key, or "+
			EnvClientCert+" and "+EnvClientKey+")")
	}
	f, err := os.Open(certFile) // #nosec G304 -- the path is the person's own --tls-client-cert
	if err != nil {
		return tls.Certificate{}, fault.Wrap(fault.Invalid, err, "cannot read the client certificate file "+certFile)
	}
	certPEM, err := readBounded(f, maxClientFile)
	_ = f.Close()
	if err != nil {
		return tls.Certificate{}, fault.Wrap(fault.Invalid, err, "cannot read the client certificate file "+certFile+": "+err.Error())
	}
	kf, err := secret.OpenPrivate(keyFile)
	if err != nil {
		msg := strings.TrimPrefix(fault.Message(err), "secret: ")
		if fault.KindOf(err) == fault.NotFound {
			msg = "cannot read the client key file " + keyFile + ": it does not exist"
		}
		return tls.Certificate{}, fault.Wrap(fault.Invalid, err, msg)
	}
	keyPEM, err := readBounded(kf, maxClientFile)
	_ = kf.Close()
	defer clear(keyPEM)
	if err != nil {
		return tls.Certificate{}, fault.Wrap(fault.Invalid, err, "cannot read the client key file "+keyFile+": "+err.Error())
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// The library's error says which input it could not use; it never
		// holds the input itself, but it is kept out of the message anyway.
		return tls.Certificate{}, fault.Wrap(fault.Invalid, err, "the client certificate "+certFile+" and the key "+keyFile+
			" are not a PEM certificate and the private key that matches it")
	}
	return pair, nil
}

// readBounded reads all of r, refusing more than limit bytes.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return b, err
	}
	if int64(len(b)) > limit {
		clear(b)
		return nil, errors.New("the file is larger than a certificate or a key can be")
	}
	return b, nil
}
