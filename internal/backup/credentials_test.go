package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// tlsSender is a DIMSE sender that verifies its peer and shows a client
// certificate, each a file in the tenant's TLS directory.
func tlsSender(name string) tenant.Sender {
	return tenant.Sender{Name: name, Type: tenant.SenderDIMSE, Enabled: true, DIMSE: tenant.DIMSESender{Host: "127.0.0.1", Port: 4242,
		CalledAETitle: "PEER", CallingAETitle: "NOPACS",
		TLS: &tenant.SenderTLS{CAFile: "ca.pem", CertificateFile: "client.pem", PrivateKeyFile: "client.key", ServerName: "peer"}}}
}

// TestMissingCredentials guards the TLS files a restore names: every file a
// receiver or a sender names that the tenant's TLS directory lacks, by its
// role and name, receivers first; a file put back is no longer named; a
// receiver or sender without TLS is never named; and the report's lines
// name each without a byte of what a file holds.
func TestMissingCredentials(t *testing.T) {
	r := newRig(t, false)
	now := time.Now()
	if _, err := r.p.AddReceiver(r.ctx, tenant.Receiver{Name: "secure", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: 11113, AETitle: "SECURE", AnyCaller: true, TLS: &tenant.ReceiverTLS{CertificateFile: "server.pem",
			PrivateKeyFile: "server.key"}}}, now, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	sending := r.st.(tenant.Sending)
	if _, err := sending.AddSender(r.ctx, tlsSender("peer"), now, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	plain := tlsSender("plain")
	plain.DIMSE.TLS = nil
	if _, err := sending.AddSender(r.ctx, plain, now, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	creds, err := missingCredentials(r.ctx, r.p, dataDir, "lab")
	if err != nil {
		t.Fatal(err)
	}
	dir := server.TLSDir(dataDir, "lab")
	want := []Credential{
		{Kind: "receiver", Name: "secure", Files: []string{"TLS certificate server.pem", "TLS private key server.key"}, Dir: dir},
		{Kind: "sender", Name: "peer", Files: []string{"TLS certificate authorities ca.pem", "TLS client certificate client.pem",
			"TLS private key client.key"}, Dir: dir},
	}
	if len(creds) != len(want) {
		t.Fatalf("the credentials named are %+v, want %+v", creds, want)
	}
	for i := range want {
		if creds[i].Kind != want[i].Kind || creds[i].Name != want[i].Name || strings.Join(creds[i].Files, "|") != strings.Join(want[i].Files, "|") ||
			creds[i].Dir != want[i].Dir {
			t.Errorf("credential %d is %+v, want %+v", i, creds[i], want[i])
		}
	}
	const secretText = "-----BEGIN PRIVATE KEY----- not a real key"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"server.pem", "server.key", "client.key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(secretText), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	creds, err = missingCredentials(r.ctx, r.p, dataDir, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Name != "peer" || strings.Join(creds[0].Files, "|") != "TLS certificate authorities ca.pem|TLS client certificate client.pem" {
		t.Fatalf("with the receiver's files and the sender's key put back, the credentials named are %+v", creds)
	}
	text := CredentialsText(creds)
	if !strings.Contains(text, "external credentials to re-enter") || !strings.Contains(text, "sender peer: TLS certificate authorities ca.pem, TLS client certificate client.pem") ||
		!strings.Contains(text, dir) || strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, "plain") {
		t.Errorf("the report's lines are %q", text)
	}
	if CredentialsText(nil) != "" {
		t.Errorf("with nothing missing the report still has lines: %q", CredentialsText(nil))
	}
}

// TestMissingCredentialsUncheckable guards a TLS file the restore cannot
// look at: a symbolic link to nothing is missing, as the receiver reading
// it would find; a directory that is a file, or one that may not be read,
// names each file as one that could not be checked, with the system's
// reason and not its path; and none of these fails the check.
func TestMissingCredentialsUncheckable(t *testing.T) {
	files := []tlsFile{{"TLS certificate", "server.pem"}, {"TLS private key", "server.key"}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "server.key")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if got := strings.Join(missing(dir, files), "|"); got != "TLS private key server.key" {
		t.Errorf("with the key a link to nothing, missing = %q, want the key", got)
	}
	notDir := filepath.Join(t.TempDir(), "tls")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := missing(notDir, files)
	if len(got) != 2 || !strings.HasPrefix(got[0], "TLS certificate server.pem (could not be checked: ") || strings.Contains(got[0], notDir) {
		t.Errorf("under a path through a file, missing = %q, want both named as not checked, without the path", got)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return // a mode does not stop the owner on Windows, nor root anywhere
	}
	locked := t.TempDir()
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a test's own directory, given back to its owner so it can be removed.
	got = missing(filepath.Join(locked, "lab"), files)
	if len(got) != 2 || !strings.Contains(got[1], "(could not be checked: permission denied)") {
		t.Errorf("under a directory that may not be read, missing = %q, want both named as not checked: permission denied", got)
	}
}
