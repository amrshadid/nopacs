package secret

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// knownBackupPlaintext is what knownBackupHex holds.
const knownBackupPlaintext = "a backup made outside Go, in three chunks"

// knownBackupHex is knownBackupPlaintext written as a backup under version 2
// of keyLine(1, 1)+keyLine(2, 2), with 16-byte chunks, once, by
// scripts/check/backup-cipher.py make (HKDF over Python's hmac and hashlib,
// AES-GCM from the cryptography package), not by this package. It is fixed,
// so the fuzz workers, which are separate processes, agree on the one
// genuine file, and so a change to the format that still round-trips in Go
// alone fails here.
const knownBackupHex = "4e4f50414353424b010000000200000010758af7b87c8247e785b61a7f7f37b82c06295a0f0152d0397bb9ea2a260a06a65a0b5ab7208c960fa3f270a886fd3049fef370ae52893237ce8ace1f7931069e8bd5beb314dd54cc4e9686408951c0814070fd5a46c50db48d3b3d67142ecff89a442b43f16a1e1bdc2dfbccc7da6e87a67d64598f135baa93"

// knownBackup returns knownBackupHex as bytes.
func knownBackup(tb testing.TB) []byte {
	tb.Helper()
	b, err := hex.DecodeString(knownBackupHex)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// TestBackupOpensKnownFile guards the format against drift: a backup written
// once by another implementation still opens to its plaintext.
func TestBackupOpensKnownFile(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	got, err := readBackup(keys, knownBackup(t))
	if err != nil {
		t.Fatalf("the backup Python wrote does not open: %v", err)
	}
	if string(got) != knownBackupPlaintext {
		t.Fatalf("the backup Python wrote opens to %q", got)
	}
}

// FuzzBackupReader guards the reader against any input: it never panics,
// whatever it returns before an error is a prefix of the genuine plaintext
// (each chunk is authenticated before its bytes are given), and it reads to
// the end only the one genuine file of the seed corpus. Anything else that
// read to the end would be a forgery.
func FuzzBackupReader(f *testing.F) {
	keys, err := parseKeyFile([]byte(keyLine(1, 1) + keyLine(2, 2)))
	if err != nil {
		f.Fatal(err)
	}
	genuine := knownBackup(f)
	f.Add(genuine)
	f.Add(genuine[:len(genuine)-1])
	f.Add(genuine[:backupHeaderSize+smallChunk+gcmTagSize])
	f.Add(append(append([]byte(nil), genuine...), genuine[backupHeaderSize:backupHeaderSize+smallChunk+gcmTagSize]...))
	f.Add(genuine[:backupHeaderSize])
	f.Add([]byte(backupMagic))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, file []byte) {
		got, err := readBackup(keys, file)
		if !bytes.HasPrefix([]byte(knownBackupPlaintext), got) {
			t.Fatalf("returned %d bytes that are not the genuine plaintext's start", len(got))
		}
		if err == nil && (!bytes.Equal(file, genuine) || string(got) != knownBackupPlaintext) {
			t.Fatalf("read to the end a file that was not written: %d bytes", len(file))
		}
	})
}
