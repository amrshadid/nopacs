package record

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/secret"
)

// holdRec returns an active hold record of a session.
func holdRec() Hold {
	return Hold{Format: Format, Kind: HoldKind, ID: opID, Level: LevelSession, Subject: "SUB-001", Session: "ses-1", State: HoldActive,
		ReasonSHA256: ReasonDigest("litigation 2026-17"), PlacedBy: "K1234567890a", Placed: t0, Seq: 1}
}

// TestHoldRecordRoundTrip guards the hold record's form: written, it reads
// back as it was, tenant included, its MAC last and its reason only a
// digest; a released record carries its released time; every level reads
// back with its labels.
func TestHoldRecordRoundTrip(t *testing.T) {
	h := holdRec()
	b, err := EncodeHold(h, tenantA, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if !macField.Match(b) || !bytes.HasPrefix(b, []byte(`{"format":1,"kind":"hold","id":"`)) {
		t.Fatalf("the record's form: %s", b)
	}
	if bytes.Contains(b, []byte("litigation")) {
		t.Fatalf("the record carries the reason's text: %s", b)
	}
	got, err := DecodeHold(bytes.NewReader(b), tenantA, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	h.Tenant, h.KeyVersion, h.MAC = tenantA, 2, got.MAC
	if got != h || !got.Active() {
		t.Fatalf("read back %+v, want %+v", got, h)
	}
	r := holdRec()
	r.State, r.Released, r.Seq = HoldReleased, t0.Add(time.Hour), 3
	b, err = EncodeHold(r, tenantA, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeHold(bytes.NewReader(b), tenantA, testKeys); err != nil || got.Active() || !got.Released.Equal(r.Released) || got.Seq != 3 {
		t.Fatalf("a released record reads back %+v, %v", got, err)
	}
	for _, x := range []Hold{
		{Level: LevelTenant},
		{Level: LevelSubject, Subject: "SUB-001"},
		{Level: LevelScan, Subject: "SUB-001", Session: "ses-1", Scan: "T1w"},
	} {
		h := holdRec()
		h.Level, h.Subject, h.Session, h.Scan = x.Level, x.Subject, x.Session, x.Scan
		b, err := EncodeHold(h, tenantA, testKeys)
		if err != nil {
			t.Fatalf("%s: %v", x.Level, err)
		}
		got, err := DecodeHold(bytes.NewReader(b), tenantA, testKeys)
		if err != nil || got.Level != x.Level || got.Subject != x.Subject || got.Session != x.Session || got.Scan != x.Scan {
			t.Fatalf("%s: read back %+v, %v", x.Level, got, err)
		}
	}
}

// TestHoldRecordRefusesWhatItWouldNotWrite guards the form's rules on both
// sides: a record that names labels below its level, or none at its level,
// a released time while active or none once released, a count below 1, an
// unknown field or another kind is a form error, never written and never
// read.
func TestHoldRecordRefusesWhatItWouldNotWrite(t *testing.T) {
	for name, change := range map[string]func(*Hold){
		"kind":             func(h *Hold) { h.Kind = "correction" },
		"format":           func(h *Hold) { h.Format = 2 },
		"id":               func(h *Hold) { h.ID = "op-1" },
		"level":            func(h *Hold) { h.Level = "instance" },
		"label below":      func(h *Hold) { h.Scan = "T1w" },
		"no label":         func(h *Hold) { h.Session = "" },
		"tenant labels":    func(h *Hold) { h.Level = LevelTenant },
		"bad label":        func(h *Hold) { h.Subject = ".corrections" },
		"reason":           func(h *Hold) { h.ReasonSHA256 = "litigation" },
		"key id":           func(h *Hold) { h.PlacedBy = "a key" },
		"no placed":        func(h *Hold) { h.Placed = time.Time{} },
		"released active":  func(h *Hold) { h.Released = t0.Add(time.Hour) },
		"released no time": func(h *Hold) { h.State = HoldReleased },
		"released before":  func(h *Hold) { h.State, h.Released = HoldReleased, t0.Add(-time.Hour) },
		"state":            func(h *Hold) { h.State = "lifted" },
		"seq":              func(h *Hold) { h.Seq = 0 },
	} {
		h := holdRec()
		change(&h)
		if b, err := EncodeHold(h, tenantA, testKeys); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: wrote %s, %v", name, b, err)
		}
	}
	good, err := EncodeHold(holdRec(), tenantA, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"unknown field": strings.Replace(string(good), `"kind":"hold",`, `"kind":"hold","why":"x",`, 1),
		"trailing":      string(good) + "{}",
	} {
		if _, err := DecodeHold(strings.NewReader(in), tenantA, testKeys); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", name, err)
		}
	}
}

// TestHoldRecordAuthenticated guards the hold record's MAC: a record with
// its state moved back to active, its target, count or release changed,
// its MAC taken out, read for another tenant or under another
// installation's keys is refused as unauthenticated and its content not
// returned; and a hold record and a correction record are never read as
// each other, since anyone who can write to a storage could otherwise turn
// one into the other.
func TestHoldRecordAuthenticated(t *testing.T) {
	r := holdRec()
	r.State, r.Released, r.Seq = HoldReleased, t0.Add(time.Hour), 2
	good, err := EncodeHold(r, tenantA, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	s := string(good)
	for name, in := range map[string]string{
		"active again": strings.Replace(strings.Replace(s, `"state":"released"`, `"state":"active"`, 1),
			`,"released":"2026-11-20T09:30:00.123456Z"`, "", 1),
		"subject": strings.Replace(s, `"subject":"SUB-001"`, `"subject":"SUB-002"`, 1),
		"seq":     strings.Replace(s, `"seq":2`, `"seq":3`, 1),
		"no mac":  macField.ReplaceAllString(s, "}\n"),
		"tenant":  strings.Replace(s, tenantA, tenantB, 1),
	} {
		if in == s {
			t.Fatalf("%s: the case changes nothing", name)
		}
		got, err := DecodeHold(strings.NewReader(in), tenantA, testKeys)
		if !errors.Is(err, ErrUnauthenticated) || got.ID != "" {
			t.Errorf("%s: %+v, %v, want ErrUnauthenticated and nothing returned", name, got, err)
		}
	}
	if _, err := DecodeHold(bytes.NewReader(good), tenantB, testKeys); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("another tenant: err = %v", err)
	}
	stranger := fakeKeys{seal: 2, keys: map[uint32][]byte{2: bytes.Repeat([]byte{9}, 32)}}
	if _, err := DecodeHold(bytes.NewReader(good), tenantA, stranger); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("another installation's keys: err = %v", err)
	}
	if _, err := DecodeCorrection(bytes.NewReader(good), tenantA, testKeys); err == nil {
		t.Error("a hold record read as a correction record")
	}
	c, err := encode(deleteRec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeHold(bytes.NewReader(c), tenantA, testKeys); err == nil {
		t.Error("a correction record read as a hold record")
	}
	if b, err := EncodeHold(holdRec(), tenantA, fakeKeys{}); err == nil {
		t.Errorf("written with no keys: %s", b)
	}
}

// TestHoldRecordSealedUnderItsOwnKey guards that a hold record's MAC is
// made under the key derived for hold records, never the correction
// records' key: the same message under the correction records' key is not
// its MAC, and a record whose MAC is that one is refused. The two kinds are
// then kept apart by their keys, not by how their bodies happen to begin.
func TestHoldRecordSealedUnderItsOwnKey(t *testing.T) {
	h := holdRec()
	b, err := EncodeHold(h, tenantA, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`,"mac":"([0-9a-f]{64})"}\n$`).FindSubmatchIndex(b)
	if m == nil {
		t.Fatalf("no MAC field in %s", b)
	}
	body, mac := append(bytes.Clone(b[:m[0]]), '}'), string(b[m[2]:m[3]])
	msg := macMessage(tenantA, h.ID, body)
	want, _ := testKeys.HoldMAC(2, msg)
	other, _ := testKeys.RecordMAC(2, msg)
	if mac != hex.EncodeToString(want) {
		t.Fatalf("the record's MAC %s is not the hold key's %x", mac, want)
	}
	if mac == hex.EncodeToString(other) {
		t.Fatal("the hold record's MAC is the correction records' key's")
	}
	forged := append(append(bytes.Clone(b[:m[2]]), hex.EncodeToString(other)...), b[m[3]:]...)
	if _, err := DecodeHold(bytes.NewReader(forged), tenantA, testKeys); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a record sealed under the correction records' key: err = %v, want ErrUnauthenticated", err)
	}
}

// TestHoldRecordMACInPython guards the hold record's MAC against another
// implementation: scripts/check/record-mac.py, which computes a record's
// MAC from the sealing's description with Python's hmac and hashlib, finds
// the MAC of a hold record this package wrote under a real key file equal
// to the record's, and refuses it for another tenant, changed or without
// its MAC: the hold records' key is derived with its own label. It runs when
// NOPACS_CHECK_RECORD_MAC_PYTHON names an interpreter (make
// check-record-mac).
func TestHoldRecordMACInPython(t *testing.T) {
	python, asked := os.LookupEnv(recordMACPythonEnv)
	if !asked {
		t.Skip(recordMACPythonEnv + " is not set; run make check-record-mac")
	}
	if python == "" {
		t.Fatal(recordMACPythonEnv + " is set but empty: name a Python interpreter")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "check", "record-mac.py"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit to be entered.
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "nopacs.key")
	if err := secret.CreateKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeHold(holdRec(), tenantA, keys)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeHold(bytes.NewReader(b), tenantA, keys)
	if err != nil {
		t.Fatalf("the record does not verify in Go: %v", err)
	}
	recFile := filepath.Join(dir, "hold.json")
	if err := os.WriteFile(recFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", script, keyFile, tenantA, recFile) //nolint:gosec // G702: the interpreter is the one the person running the test named.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		t.Logf("python: %s", line)
	}
	if err != nil {
		t.Fatalf("record-mac.py: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(string(out), "mac "+back.MAC+"\n") {
		t.Fatalf("Python computed another MAC than the hold record's %s:\n%s", back.MAC, out)
	}
	t.Logf("go: the hold record's MAC %s equals Python's", back.MAC)
}
