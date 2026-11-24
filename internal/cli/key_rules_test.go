package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
)

// TestKeyRotateCommand guards key rotate: the successor alone on standard
// output, the note naming both keys on standard error, --overlap sent, the
// successor shown with the rotated key's rules, a second rotation refused,
// and a malformed overlap refused before any request.
func TestKeyRotateCommand(t *testing.T) {
	r := newRig(t)
	r.must("key", "create", "--holder", "ci", "--network", "10.0.0.0/8")
	res := r.run("key", "rotate", "k00000000001", "--overlap", "2h")
	if res.code != 0 || res.stdout != "nopacs_k00000000002_"+strings.Repeat("R", 44)+"\n" {
		t.Fatalf("rotate: %+v", res)
	}
	if !strings.Contains(res.stderr, "key k00000000002 (ci) replaces k00000000001, which stops working 2026-11-17 17:00:00 +03:00") ||
		!strings.Contains(res.stderr, "only time the new key is shown") {
		t.Errorf("rotate's note: %q", res.stderr)
	}
	show := r.run("key", "show", "k00000000002").stdout
	if !strings.Contains(show, "Networks:  10.0.0.0/8") {
		t.Errorf("successor:\n%s", show)
	}
	if old := r.run("key", "show", "k00000000001").stdout; !strings.Contains(old, "Successor: k00000000002") {
		t.Errorf("rotated key:\n%s", old)
	}
	if res := r.run("key", "rotate", "k00000000001"); res.code == 0 || !strings.Contains(res.stderr, "already rotated") {
		t.Errorf("a second rotation: %+v", res)
	}
	before := r.fake.Requests()
	if res := r.run("key", "rotate", "k00000000002", "--overlap", "soon"); res.code != 2 || r.fake.Requests() != before {
		t.Errorf("a malformed overlap: %+v", res)
	}
	doc := document(t, r.run("key", "rotate", "k00000000002", "--json"))
	if doc["secret"] == nil || doc["rotated"].(map[string]any)["successorId"] == nil {
		t.Errorf("rotate --json: %v", doc)
	}
}

// TestKeySetCommand guards key set: the networks and the fingerprint of a
// certificate file sent, the key shown with them, and no flag clearing both.
func TestKeySetCommand(t *testing.T) {
	r := newRig(t)
	r.must("key", "create", "--holder", "ci")
	// A self-signed certificate; its SHA-256 over the DER bytes, as
	// "openssl x509 -noout -fingerprint -sha256" prints it without colons.
	pemFile := filepath.Join(t.TempDir(), "client.pem")
	if err := os.WriteFile(pemFile, []byte(testCertPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.run("key", "set", "k00000000001", "--network", "192.0.2.0/24", "--network", "2001:db8::1", "--client-cert", pemFile)
	if res.code != 0 || !strings.Contains(res.stdout, "Networks:    192.0.2.0/24, 2001:db8::1") || !strings.Contains(res.stdout, "Client cert: sha256 "+testCertSHA256) {
		t.Fatalf("set: %+v", res)
	}
	if res := r.run("key", "set", "k00000000001"); res.code != 0 || strings.Contains(res.stdout, "Networks") || strings.Contains(res.stdout, "Client cert") {
		t.Errorf("set with no flag: %+v", res)
	}
	if res := r.run("key", "set", "k00000000001", "--client-cert", pemFile, "--client-cert-sha256", testCertSHA256); res.code != 2 {
		t.Errorf("both forms of the certificate: %+v", res)
	}
	if res := r.run("key", "set", "k00000000001", "--client-cert", filepath.Join(t.TempDir(), "none.pem")); res.code != 2 {
		t.Errorf("a missing certificate file: %+v", res)
	}
}

// TestKeyLastAddressShown guards that a key's last use is shown with the
// address it came from, in show and in list.
func TestKeyLastAddressShown(t *testing.T) {
	r := newRig(t)
	r.fake.AddKey(&v1.Key{Id: "kused0000001", Holder: "bot", CreateTime: timestamppb.New(fixedNow), LastUseTime: timestamppb.New(fixedNow), LastUseAddress: "203.0.113.9"})
	if s := r.run("key", "show", "kused0000001").stdout; !strings.Contains(s, "Last used: 2026-11-17 15:00:00 +03:00 from 203.0.113.9") {
		t.Errorf("show:\n%s", s)
	}
	if s := r.run("key", "list").stdout; !strings.Contains(s, "2026-11-17 15:00:00 +03:00 from 203.0.113.9") {
		t.Errorf("list:\n%s", s)
	}
}

// testCertPEM is a self-signed certificate made for this test with
// "openssl req -x509 -newkey ec"; only the certificate, public, is kept.
const testCertPEM = `-----BEGIN CERTIFICATE-----
MIIBjjCCATOgAwIBAgIUEIjdg7KTaG0k07hpGhFUW9EW8kYwCgYIKoZIzj0EAwIw
HDEaMBgGA1UEAwwRY29yaWEtdGVzdC1jbGllbnQwHhcNMjYxMTIwMTIyOTUwWhcN
MzYxMTE3MTIyOTUwWjAcMRowGAYDVQQDDBFjb3JpYS10ZXN0LWNsaWVudDBZMBMG
ByqGSM49AgEGCCqGSM49AwEHA0IABLRjQzikcrATfb5Hyv4GhYeZcEJU7uvwhzxF
cyYRKpWxYoak2QJU2smYtthpE5+932F+ccNp7VAmn1t/a66DKhKjUzBRMB0GA1Ud
DgQWBBQAR5Voo3aOmby6M5YAVMbQSFdcZjAfBgNVHSMEGDAWgBQAR5Voo3aOmby6
M5YAVMbQSFdcZjAPBgNVHRMBAf8EBTADAQH/MAoGCCqGSM49BAMCA0kAMEYCIQCZ
YG3jwysYu0Obz+u03irB1rlXPoi/TEN1hAV5CTkAugIhAKUDjIFQ7dH/5ykHfzzB
SfpquU+wxbUgHkbyNZY75edD
-----END CERTIFICATE-----
`

// testCertSHA256 is testCertPEM's fingerprint as
// "openssl x509 -noout -fingerprint -sha256" prints it, colons removed and
// lowered.
const testCertSHA256 = "57137ea885158fbc7ad9e2b8fc21c2de7d0fd9522fe538812ce310e8b45c2a1d"

// TestKeyRevokeNamesLiveSuccessor guards key revoke after a rotation: the
// rotated key alone is revoked, and a note on standard error names its
// successor that still works and the flag that ends it; --with-successors
// revokes the whole chain, each key in turn, and lists them all with
// --json.
func TestKeyRevokeNamesLiveSuccessor(t *testing.T) {
	r := newRig(t)
	r.must("key", "create", "--holder", "ci")
	r.must("key", "rotate", "k00000000001")
	r.must("key", "rotate", "k00000000002")
	res := r.run("key", "revoke", "k00000000001")
	if res.code != 0 || !strings.HasPrefix(res.stdout, "key k00000000001 (ci) revoked at ") || strings.Contains(res.stdout, "k00000000002") {
		t.Fatalf("revoke of a rotated key: %+v", res)
	}
	for _, id := range []string{"k00000000002", "k00000000003"} {
		if !strings.Contains(res.stderr, "key "+id+" (ci), made from it by rotation, still works; to end it too: nopacs key revoke k00000000001 --with-successors") {
			t.Errorf("the note does not name %s: %q", id, res.stderr)
		}
	}
	if show := r.run("key", "show", "k00000000002").stdout; !strings.Contains(show, "Status:    active") {
		t.Errorf("the successor was revoked too:\n%s", show)
	}
	doc := document(t, r.run("key", "revoke", "k00000000001", "--with-successors", "--json"))
	keys, _ := doc["keys"].([]any)
	if len(keys) != 3 {
		t.Fatalf("revoke --with-successors --json: %v", doc)
	}
	for i, id := range []string{"k00000000001", "k00000000002", "k00000000003"} {
		k := keys[i].(map[string]any)
		if k["id"] != id || k["revokeTime"] == nil {
			t.Errorf("key %d of the chain: %v, want %s revoked", i, k, id)
		}
	}
	if res := r.run("key", "revoke", "k00000000001"); res.stderr != "" {
		t.Errorf("a note after the chain was revoked: %q", res.stderr)
	}
}

// TestKeyRevokeSaysWhatWasRevokedOnFailure guards a revoke whose later call
// fails, after the key itself was revoked: the error is reported with its
// exit code, and standard error first says which keys were revoked, so the
// failure does not read as if nothing had happened.
func TestKeyRevokeSaysWhatWasRevokedOnFailure(t *testing.T) {
	for _, c := range []struct {
		name      string
		args      []string
		procedure string
		revoked   []string
	}{
		{"reading a successor", []string{"key", "revoke", "k00000000001"}, nopacsv1connect.AccessServiceGetKeyProcedure, []string{"k00000000001"}},
		{"revoking a successor", []string{"key", "revoke", "k00000000001", "--with-successors"}, "", []string{"k00000000001"}},
	} {
		r := newRig(t)
		r.must("key", "create", "--holder", "ci")
		r.must("key", "rotate", "k00000000001")
		procedure := c.procedure
		if procedure == "" {
			// The first revoke is let through, the second fails.
			r.fake.FailAfter(nopacsv1connect.AccessServiceRevokeKeyProcedure, 1, connect.CodeUnavailable, "the master does not answer")
		} else {
			r.fake.FailWith(procedure, connect.CodeUnavailable, "the master does not answer")
		}
		res := r.run(c.args...)
		if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "the master does not answer") {
			t.Errorf("%s: %+v, want exit 1 with the server's error", c.name, res)
		}
		for _, id := range c.revoked {
			if !strings.Contains(res.stderr, "nopacs: key "+id+" (ci) was revoked at ") {
				t.Errorf("%s: standard error does not say %s was revoked: %q", c.name, id, res.stderr)
			}
		}
	}
}
