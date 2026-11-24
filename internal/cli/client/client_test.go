package client_test

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// testKey is a made-up key for the fake; it is not a real credential.
const testKey = "nopacs_testtesttest_" + "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqr"

// env returns an environment reader over a map.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// listTenants is the call the tests make: the simplest one that needs no setup.
func listTenants(c *client.Client) (*v1.ListTenantsResponse, error) {
	return client.Call(context.Background(), c, "tenant list", func(ctx context.Context) (*v1.ListTenantsResponse, error) {
		return c.Tenants.ListTenants(ctx, &v1.ListTenantsRequest{})
	})
}

// TestLocalSocketCall guards the default transport: a call over the unix
// socket with HTTP/2 without TLS reaches the server, sends no key, and an
// error answer carries its kind, the server's message and the request id.
func TestLocalSocketCall(t *testing.T) {
	fake := fakeserver.New()
	fake.AddTenant(&v1.Tenant{Name: "lab1"})
	dir := fake.ServeUnix(t)
	c, err := client.New(client.Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := listTenants(c)
	if err != nil || len(res.GetTenants()) != 1 {
		t.Fatalf("ListTenants = %v, %v", res, err)
	}
	if h := fake.AuthorizationHeaders(); len(h) != 1 || h[0] != "" {
		t.Errorf("Authorization headers on the socket = %q, want one empty", h)
	}

	_, err = client.Call(context.Background(), c, "tenant show", func(ctx context.Context) (*v1.GetTenantResponse, error) {
		return c.Tenants.GetTenant(ctx, &v1.GetTenantRequest{Name: "nope"})
	})
	var ce *client.CallError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a CallError", err)
	}
	if fault.KindOf(err) != fault.NotFound || ce.RequestID != "req-000002" {
		t.Errorf("kind %v, request id %q", fault.KindOf(err), ce.RequestID)
	}
	if want := "tenant show: no tenant has that name (request id req-000002)"; err.Error() != want {
		t.Errorf("message %q, want %q", err.Error(), want)
	}
}

// TestNetworkSendsTheKeyOverTLS guards the network transport: TLS on loopback
// with a certificate the client trusts, the key from the environment sent as a
// bearer token, and a wrong key answered as unauthenticated with a request id.
func TestNetworkSendsTheKeyOverTLS(t *testing.T) {
	fake := fakeserver.New()
	fake.RequireKey = testKey
	addr := fake.ServeTLS(t)
	ca := caFile(t, fake)
	c, err := client.New(client.Config{Server: addr, CAFile: ca, Getenv: env(map[string]string{client.EnvKey: testKey})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := listTenants(c); err != nil {
		t.Fatal(err)
	}
	if h := fake.AuthorizationHeaders(); len(h) != 1 || h[0] != "Bearer "+testKey {
		t.Errorf("Authorization headers = %q", h)
	}

	bad, err := client.New(client.Config{Server: addr, CAFile: ca, Getenv: env(map[string]string{client.EnvKey: "wrong"})})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	_, err = listTenants(bad)
	if fault.KindOf(err) != fault.Unauthenticated || !strings.Contains(err.Error(), "request id req-") {
		t.Errorf("wrong key: kind %v, error %v", fault.KindOf(err), err)
	}
	if strings.Contains(err.Error(), "wrong") && strings.Contains(err.Error(), "Bearer") {
		t.Errorf("the error shows the key: %v", err)
	}
}

// TestUntrustedCertificateIsRefused guards the TLS check: a server whose
// certificate the system does not trust is refused with a line that says so,
// and the key is not sent to it.
func TestUntrustedCertificateIsRefused(t *testing.T) {
	fake := fakeserver.New()
	addr := fake.ServeTLS(t)
	c, err := client.New(client.Config{Server: addr, Getenv: env(map[string]string{client.EnvKey: testKey})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = listTenants(c)
	if err == nil || !strings.Contains(err.Error(), "its certificate is not trusted by this system (signed by an unknown authority)") {
		t.Fatalf("error = %v", err)
	}
	if fake.Requests() != 0 {
		t.Errorf("the server saw %d requests, want none", fake.Requests())
	}
}

// TestLoopbackHTTPWorks guards the one place plain http is allowed: a loopback
// address.
func TestLoopbackHTTPWorks(t *testing.T) {
	fake := fakeserver.New()
	fake.RequireKey = testKey
	addr := fake.ServeHTTP(t)
	c, err := client.New(client.Config{Server: addr, Getenv: env(map[string]string{client.EnvKey: testKey})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := listTenants(c); err != nil {
		t.Fatal(err)
	}
}

// TestServerAddressRules guards ParseServer: https anywhere, http only for
// loopback, nothing else, and no credentials in the address; and that no
// message quotes the credentials it refused.
func TestServerAddressRules(t *testing.T) {
	ok := []string{"https://example.org", "https://example.org:8443/", "http://localhost:8080", "http://127.0.0.1:1", "http://[::1]:9", "http://127.9.9.9"}
	for _, a := range ok {
		if _, err := client.ParseServer(a); err != nil {
			t.Errorf("ParseServer(%q) = %v, want accepted", a, err)
		}
	}
	bad := map[string]string{
		"http://example.org":                  "http is allowed only for a loopback host",
		"http://10.0.0.5":                     "http is allowed only for a loopback host",
		"http://localhost.example.org":        "http is allowed only for a loopback host",
		"ftp://example.org":                   "must start with https://",
		"example.org:8443":                    "not a URL",
		"example://host":                      "must start with https://",
		"":                                    "not a URL",
		"https://":                            "not a URL",
		"https://user:hunter2@example.org":    "must not hold a user name or password",
		"https://example.org?token=hunter2":   "must not have a query",
		"https://example.org/#hunter2":        "must not have a query or a fragment",
		"http://exa mple.org/hunter2":         "not a URL",
		"https://%zz:hunter2@example.org/bad": "not a URL",
	}
	for a, want := range bad {
		_, err := client.ParseServer(a)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseServer(%q) = %v, want an error containing %q", a, err, want)
			continue
		}
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("ParseServer(%q): kind %v, want invalid", a, fault.KindOf(err))
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("ParseServer(%q) quotes the credentials: %v", a, err)
		}
	}
}

// TestReadKey guards where a key comes from and that no message shows it: the
// file wins over the environment, whitespace around the key is dropped, and an
// empty, huge or non-printable key is refused with a message that names the
// source and not the content.
func TestReadKey(t *testing.T) {
	dir := t.TempDir()
	write := func(_, content string) string { return fakeserver.PrivateFile(t, content) }
	if k, err := client.ReadKey("", env(map[string]string{client.EnvKey: " " + testKey + "\n"})); err != nil || k != testKey {
		t.Errorf("from the environment: %q, %v", k, err)
	}
	if k, err := client.ReadKey(write("k", testKey+"\n"), env(map[string]string{client.EnvKey: "other"})); err != nil || k != testKey {
		t.Errorf("from the file: %q, %v", k, err)
	}
	widePath := filepath.Join(dir, "wide")
	if err := os.WriteFile(widePath, []byte(testKey), 0o644); err != nil { //nolint:gosec // G306: the mode is the point of the test.
		t.Fatal(err)
	}
	if err := os.Chmod(widePath, 0o644); err != nil { //nolint:gosec // G302: the mode is the point of the test.
		t.Fatal(err)
	}
	cases := map[string]struct {
		file string
		env  string
		want string
	}{
		"unset":       {"", "", "no key: set NOPACS_KEY or pass --key-file PATH"},
		"empty file":  {write("empty", "\n"), "", "no key"},
		"missing":     {filepath.Join(dir, "missing"), "", "cannot read the key file"},
		"huge":        {write("huge", strings.Repeat("secretsecret", 200)), "", "larger than a key can be"},
		"others read": {widePath, "", "make it private to"},
		"has space":   {"", "secret value", "holds characters a key does not have"},
		"control":     {"", "secret\x07value", "holds characters a key does not have"},
	}
	for name, c := range cases {
		_, err := client.ReadKey(c.file, env(map[string]string{client.EnvKey: c.env}))
		// The message is what a person is shown; the error's own text also
		// carries the cause, which is for the log.
		if err == nil || !strings.Contains(fault.Message(err), c.want) {
			t.Errorf("%s: error %v, want %q", name, err, c.want)
			continue
		}
		for _, content := range []string{"secretsecret", "secret value", "abcdefghijkl", "\x07"} {
			if strings.Contains(fault.Message(err), content) {
				t.Errorf("%s: the message shows key content %q: %v", name, content, err)
			}
		}
	}
}

// TestNewNeedsOneWayToReach guards the choice of transport: both a data
// directory and a server, or neither, is a usage error.
func TestNewNeedsOneWayToReach(t *testing.T) {
	for name, cfg := range map[string]client.Config{
		"neither": {},
		"both":    {DataDir: "/x", Server: "https://example.org"},
	} {
		_, err := client.New(cfg)
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("%s: error %v, want invalid", name, err)
		}
	}
	if _, err := client.New(client.Config{Server: "https://example.org", Getenv: env(nil)}); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a server with no key: %v, want invalid", err)
	}
	if _, err := client.New(client.Config{DataDir: "/x", Timeout: -time.Second}); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a negative timeout: %v, want invalid", err)
	}
}

// TestUnreachableSocket guards the message when nothing listens: it names the
// socket and is a failure, not a usage error.
func TestUnreachableSocket(t *testing.T) {
	dir := t.TempDir()
	c, err := client.New(client.Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = listTenants(c)
	want := "cannot reach the server at " + client.SocketPath(dir)
	if err == nil || !strings.Contains(err.Error(), want) || fault.KindOf(err) != fault.Unavailable {
		t.Errorf("error %v (kind %v), want %q", err, fault.KindOf(err), want)
	}
}

// TestDeadline guards the per-call deadline: a server that does not answer in
// time is reported with the timeout and the flag that changes it.
func TestDeadline(t *testing.T) {
	fake := fakeserver.New()
	fake.Delay = 2 * time.Second
	dir := fake.ServeUnix(t)
	c, err := client.New(client.Config{DataDir: dir, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = listTenants(c)
	if err == nil || !strings.Contains(err.Error(), "no answer from") || !strings.Contains(err.Error(), "--timeout") {
		t.Errorf("error = %v", err)
	}
}

// TestForcedCodes guards the mapping of every code the contract names, through
// the real transport: the kind each gets, and the request id in the message.
func TestForcedCodes(t *testing.T) {
	cases := map[connect.Code]fault.Kind{
		connect.CodeInvalidArgument:    fault.Invalid,
		connect.CodeUnauthenticated:    fault.Unauthenticated,
		connect.CodePermissionDenied:   fault.Denied,
		connect.CodeNotFound:           fault.NotFound,
		connect.CodeAlreadyExists:      fault.Conflict,
		connect.CodeUnavailable:        fault.Unavailable,
		connect.CodeInternal:           fault.Internal,
		connect.CodeResourceExhausted:  fault.Exhausted,
		connect.CodeFailedPrecondition: fault.Precondition,
		connect.CodeOutOfRange:         fault.OutOfRange,
	}
	for code, kind := range cases {
		fake := fakeserver.New()
		fake.FailWith(nopacsv1connect.TenantServiceListTenantsProcedure, code, "forced")
		dir := fake.ServeUnix(t)
		c, err := client.New(client.Config{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		_, err = listTenants(c)
		c.Close()
		if fault.KindOf(err) != kind || !strings.Contains(err.Error(), "forced (request id req-000001)") {
			t.Errorf("%v: kind %v, error %v; want kind %v with the request id", code, fault.KindOf(err), err, kind)
		}
	}
}

// caFile writes the fake's certificate as a PEM file and returns its path.
func caFile(t *testing.T, fake *fakeserver.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, fake.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCAFile guards --ca-file at the client: the server's own certificate,
// trusted through the file, works; the system's store (no file) refuses it; a
// file that is not certificates, or a missing one, is a usage error that names
// the file and not its content; a file with another server's certificate is
// refused as an untrusted one, saying which file did not trust it; and the
// file has no effect on the local socket.
func TestCAFile(t *testing.T) {
	fake := fakeserver.New()
	addr := fake.ServeTLS(t)
	keyEnv := env(map[string]string{client.EnvKey: testKey})
	try := func(ca string) error {
		c, err := client.New(client.Config{Server: addr, CAFile: ca, Getenv: keyEnv})
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = listTenants(c)
		return err
	}
	if err := try(caFile(t, fake)); err != nil {
		t.Errorf("trusted through the file: %v", err)
	}
	if err := try(""); err == nil || !strings.Contains(err.Error(), "not trusted by this system") {
		t.Errorf("without the file: %v", err)
	}

	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("-----BEGIN CERTIFICATE-----\nnot-a-certificate-SECRETWORD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(dir, "huge.pem")
	if err := os.WriteFile(huge, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ path, want string }{
		"no certificate": {garbage, "holds no certificate in PEM form"},
		"missing":        {filepath.Join(dir, "none.pem"), "cannot read the CA file"},
		"huge":           {huge, "is larger than a file of certificates can be"},
	} {
		err := try(c.path)
		if fault.KindOf(err) != fault.Invalid || err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), c.path) || strings.Contains(err.Error(), "SECRETWORD") {
			t.Errorf("%s: %v (kind %v), want invalid naming the file with %q", name, err, fault.KindOf(err), c.want)
		}
	}

	other := fakeserver.New()
	other.ServeTLS(t)
	wrong := caFile(t, other)
	if err := try(wrong); err == nil || !strings.Contains(err.Error(), "not trusted by the CA file "+wrong) {
		t.Errorf("another server's certificate: %v", err)
	}

	// The local socket never reads the file.
	dirLocal := fakeserver.New().ServeUnix(t)
	c, err := client.New(client.Config{DataDir: dirLocal, CAFile: filepath.Join(dir, "none.pem")})
	if err != nil {
		t.Fatalf("a CA file with the local socket: %v", err)
	}
	defer c.Close()
	if _, err := listTenants(c); err != nil {
		t.Errorf("call on the local socket: %v", err)
	}
}

// TestTLSOneOneIsRefused guards the floor of TLS 1.2: a server that offers
// nothing newer than TLS 1.1 is refused, and no request reaches it. The server
// states its minimum version itself, so Go's own default floor for servers
// does not stand in for the client's.
func TestTLSOneOneIsRefused(t *testing.T) {
	old := fakeserver.New()
	addr := old.ServeTLSVersions(t, tls.VersionTLS10, tls.VersionTLS11)
	c, err := client.New(client.Config{Server: addr, CAFile: caFile(t, old), Getenv: env(map[string]string{client.EnvKey: testKey})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = listTenants(c)
	// The reason must be the handshake's: a client that allowed TLS 1.1 would
	// finish the handshake and fail later, on HTTP/2's own rule, with another
	// message, so the text is what tells the two apart.
	if err == nil || fault.KindOf(err) != fault.Unavailable || !strings.Contains(err.Error(), "tls: protocol version not supported") {
		t.Fatalf("a TLS 1.1 server was accepted, or refused for another reason: %v", err)
	}
	if old.Requests() != 0 {
		t.Errorf("%d requests reached a TLS 1.1 server", old.Requests())
	}
	// The same client reaches a server that offers TLS 1.2 only.
	ok := fakeserver.New()
	addr = ok.ServeTLSVersions(t, tls.VersionTLS12, tls.VersionTLS12)
	c2, err := client.New(client.Config{Server: addr, CAFile: caFile(t, ok), Getenv: env(map[string]string{client.EnvKey: testKey})})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := listTenants(c2); err != nil {
		t.Errorf("a TLS 1.2 server was refused: %v", err)
	}
}
