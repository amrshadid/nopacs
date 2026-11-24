package secret

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestTransferKeyFile guards the transfer key's file: written private and
// read back to the same bytes; never written over a file that is there;
// and a file not in its one form (a short key, another encoding, a second
// line) refused without quoting it.
func TestTransferKeyFile(t *testing.T) {
	key, _, err := NewTransferKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lab1.key")
	if err := WriteTransferKeyFile(path, key); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTransferKeyFile(path)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("read back: %v", err)
	}
	if err := WriteTransferKeyFile(path, key); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a second write over the file: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the directory holds %d files, want the key file alone", len(entries))
	}
	for name, body := range map[string]string{
		"short":       base64.StdEncoding.EncodeToString(key[:16]) + "\n",
		"unpadded":    base64.RawStdEncoding.EncodeToString(key) + "\n",
		"url":         strings.ReplaceAll(strings.ReplaceAll(base64.StdEncoding.EncodeToString(append([]byte{0xfb, 0xff}, key[2:]...)), "+", "-"), "/", "_") + "\n",
		"second line": base64.StdEncoding.EncodeToString(key) + "\nmore\n",
	} {
		p := filepath.Join(t.TempDir(), "k")
		writePrivate(t, p, []byte(body))
		if _, err := ReadTransferKeyFile(p); fault.KindOf(err) != fault.Invalid || strings.Contains(fault.Message(err), strings.TrimSpace(body)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestTransferKey guards the transfer key: two keys are never equal; a value
// sealed and a backup file written under one open with the same bytes
// again and with nothing else (another transfer key, a key file); a key of
// any other length is refused; and it prints no byte of itself.
func TestTransferKey(t *testing.T) {
	key, k, err := NewTransferKey()
	if err != nil {
		t.Fatal(err)
	}
	key2, k2, err := NewTransferKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != TransferKeySize || bytes.Equal(key, key2) {
		t.Fatalf("two transfer keys: %d bytes, equal %v", len(key), bytes.Equal(key, key2))
	}
	sealed, err := k.Seal("tenant/t1/secret/naming", []byte("plain"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := TransferKeys(key)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Open("tenant/t1/secret/naming", sealed); err != nil || string(got) != "plain" {
		t.Fatalf("the same key opens: %q %v", got, err)
	}
	if _, err := k2.Open("tenant/t1/secret/naming", sealed); err == nil {
		t.Error("another transfer key opened the value")
	}
	if _, err := mustKeys(t, keyLine(1, 1)).Open("tenant/t1/secret/naming", sealed); err == nil {
		t.Error("a key file opened a value sealed under a transfer key")
	}
	var file bytes.Buffer
	bw, err := NewBackupWriter(&file, k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bw.Write([]byte("an export's plaintext")); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	br, err := NewBackupReader(bytes.NewReader(file.Bytes()), again)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(br); err != nil || string(got) != "an export's plaintext" {
		t.Fatalf("the file opens under the same key: %q %v", got, err)
	}
	br2, err := NewBackupReader(bytes.NewReader(file.Bytes()), k2)
	if err == nil {
		_, err = io.ReadAll(br2)
	}
	if err == nil {
		t.Error("another transfer key opened the file")
	}
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := TransferKeys(make([]byte, n)); fault.KindOf(err) != fault.Invalid {
			t.Errorf("%d bytes: err = %v, want Invalid", n, err)
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x"} {
		if out := fmt.Sprintf(verb, k); bytes.Contains([]byte(out), key) || bytes.Contains([]byte(out), []byte(fmt.Sprintf("%x", key))) {
			t.Errorf("%s printed the key", verb)
		}
	}
}
