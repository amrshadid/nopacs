package client_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// TestClientCertificateIsPresented guards the client side of a key's client
// certificate rule: with ClientCert and ClientKey the TLS handshake
// presents that certificate, which the server sees by the SHA-256 it pins;
// without them none is presented. Before the client could present one, a
// key limited to a certificate could not be used from the command line at
// all.
func TestClientCertificateIsPresented(t *testing.T) {
	fake := fakeserver.New()
	addr := fake.ServeTLS(t)
	ca := caFile(t, fake)
	certPath, keyPath, sum := fakeserver.WriteClientCert(t, t.TempDir(), "bridge")
	keyEnv := env(map[string]string{client.EnvKey: testKey})

	for _, c := range []struct {
		name       string
		cert, key  string
		wantSHA256 string
	}{
		{"with a certificate", certPath, keyPath, sum},
		{"without one", "", "", ""},
	} {
		before := len(fake.SeenRequests())
		cl, err := client.New(client.Config{Server: addr, CAFile: ca, Getenv: keyEnv, ClientCert: c.cert, ClientKey: c.key})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_, err = listTenants(cl)
		cl.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		seen := fake.SeenRequests()[before:]
		if len(seen) != 1 || seen[0].ClientCertSHA256 != c.wantSHA256 {
			t.Errorf("%s: the server saw %d requests, the first presenting %q; want one presenting %q", c.name, len(seen), firstSum(seen), c.wantSHA256)
		}
	}
}

// firstSum returns the client certificate sum of the first request seen, or
// "" for none.
func firstSum(seen []fakeserver.Seen) string {
	if len(seen) == 0 {
		return ""
	}
	return seen[0].ClientCertSHA256
}

// TestClientCertificateRefusals guards what is refused before any call: a
// certificate without its key or a key without its certificate, a key file
// others may read, a missing file, a certificate and a key that do not
// match, and a certificate over plain http, where no handshake could carry
// it. Each is a usage error that names the file and never quotes the key.
func TestClientCertificateRefusals(t *testing.T) {
	fake := fakeserver.New()
	addr := fake.ServeTLS(t)
	dir := t.TempDir()
	certPath, keyPath, _ := fakeserver.WriteClientCert(t, dir, "one")
	_, otherKey, _ := fakeserver.WriteClientCert(t, dir, "two")
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyEnv := env(map[string]string{client.EnvKey: testKey})

	cases := []struct {
		name, server, cert, key, want string
	}{
		{"certificate alone", addr, certPath, "", "needs both its file and its private key's file"},
		{"key alone", addr, "", keyPath, "needs both its file and its private key's file"},
		{"missing certificate", addr, filepath.Join(dir, "none.pem"), keyPath, "cannot read the client certificate file " + filepath.Join(dir, "none.pem")},
		{"missing key", addr, certPath, filepath.Join(dir, "none.key"), "cannot read the client key file " + filepath.Join(dir, "none.key")},
		{"mismatched pair", addr, certPath, otherKey, "are not a PEM certificate and the private key that matches it"},
		{"plain http", "http://127.0.0.1:1", certPath, keyPath, "a client certificate is presented only over https"},
	}
	if runtime.GOOS != "windows" {
		open := filepath.Join(dir, "open.key")
		if err := os.WriteFile(open, keyPEM, 0o600); err != nil { //nolint:gosec // G703: a path in the test's own directory.
			t.Fatal(err)
		}
		if err := os.Chmod(open, 0o644); err != nil { //nolint:gosec // G302: the file is opened to others on purpose, to be refused.
			t.Fatal(err)
		}
		cases = append(cases, struct{ name, server, cert, key, want string }{"key others may read", addr, certPath, open, "chmod 600 " + open})
	}
	for _, c := range cases {
		_, err := client.New(client.Config{Server: c.server, Getenv: keyEnv, ClientCert: c.cert, ClientKey: c.key})
		if fault.KindOf(err) != fault.Invalid || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (kind %v), want invalid saying %q", c.name, err, fault.KindOf(err), c.want)
			continue
		}
		if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), string(keyPEM[30:60])) {
			t.Errorf("%s: the error quotes the key: %v", c.name, err)
		}
	}
	if n := fake.Requests(); n != 0 {
		t.Errorf("%d requests reached the server, want none", n)
	}

	// The local socket never reads the files.
	local := fakeserver.New().ServeUnix(t)
	c, err := client.New(client.Config{DataDir: local, ClientCert: filepath.Join(dir, "none.pem"), ClientKey: filepath.Join(dir, "none.key")})
	if err != nil {
		t.Fatalf("a client certificate with the local socket: %v", err)
	}
	c.Close()
}
