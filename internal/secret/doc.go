// Package secret holds the installation's key file, seals and opens the
// secrets stored in the master database with it, derives the keys that sign
// the audit and encrypt backups, and makes and checks API keys.
//
// # The key file
//
// A text file with one line per key version:
//
//	nopacs-key-v1 <version> <key>
//
// The version is a decimal integer from 1 to 2147483647 (2^31-1, the largest
// the API's int32 carries) written without leading zeros, and the key is 32 random bytes in standard base64 with
// padding (44 characters). Fields are separated by one space; a line ends
// with a line feed, or with a carriage return and a line feed. No two lines
// share a version. The highest version seals; every version in the file
// opens, so a key can be rotated later by adding a line, with no change of
// format.
//
// CreateKeyFile writes a new file with one key, version 1, and never replaces
// a file that exists. LoadKeyFile refuses a file that other users may open or
// change, and says how to fix that, because anyone who can read the file can
// open every secret it sealed, and anyone who can replace it can make the
// archive seal under a key they know. What that means differs between
// systems, and the difference is kept in perm_unix.go, acl_darwin.go and
// perm_windows.go and nowhere else:
//
//   - Linux and macOS: the file must be a regular file (it is opened without
//     blocking, so a FIFO is refused rather than waited on), owned by the
//     effective user, with no permission bit for the group or others. Its
//     directory must exist, be owned by the effective user and be writable
//     by nobody else. A POSIX access list on Linux is covered by the mode
//     check, because it shows its mask in the group bits. On macOS an access
//     list is kept apart from the mode, so it is read from the kernel
//     (fgetattrlist, without cgo), and any entry that grants anything, on
//     the file or on its directory, is refused.
//   - Windows: the file must be a regular file owned by the current user,
//     whose access list grants access to the current user and nobody else.
//     The directory is not checked on Windows in this version.
//
// On other Unix systems, which no release targets, access lists are not
// read.
//
// # A sealed value
//
// Seal encrypts with AES-256-GCM (NIST SP 800-38D) under a fresh random
// 96-bit nonce. The sealed value is, byte by byte:
//
//	1 byte    format, 0x01
//	4 bytes   key version, big-endian
//	12 bytes  nonce
//	n+16      ciphertext of the n-byte plaintext, then the 16-byte tag
//
// The additional authenticated data is the first five bytes (format and key
// version) followed by the context the caller names, such as
// "tenant/<id>/database". The context binds a secret to the row it belongs
// to, so a sealed value copied into another row does not open; the header in
// the additional data means a changed format byte or key version fails
// authentication as well as the lookup. scripts/check/seal.py implements this
// format with Python's cryptography package, and a test runs it against this
// package in both directions.
//
// Open fails, with kind Internal and one fixed message, whatever the reason:
// a wrong key or context, a changed or missing byte, an unknown format or key
// version. A reason would tell a caller which part of a forgery to fix.
//
// # Derived keys
//
// Keys.Derive gives a key for one purpose from one version of the
// installation key, with HKDF-SHA-256 (RFC 5869):
//
//	PRK = HMAC-SHA-256(32 zero bytes, key)          (Extract, no salt)
//	OKM = HMAC-SHA-256(PRK, label || 0x01)          (Expand, L = 32)
//
// The label is fixed per purpose, in ASCII: "nopacs/audit-seal/v1" for the
// Ed25519 seed (RFC 8032 5.1.5) that signs the audit's checkpoints,
// "nopacs/backup/v1" for the backup key, "nopacs/confirm-token/v1" for the
// HMAC key of a delete's confirm token, and "nopacs/correction-record/v1" for
// the HMAC key of the correction records on the storages (Keys.RecordMAC),
// "nopacs/owner-record/v1" for that of the owner records (Keys.OwnerMAC),
// and "nopacs/hold-record/v1" for that of the hold records (Keys.HoldMAC).
// Purposes are a closed set of
// constants, so no caller can choose a label, and a key derived for one
// purpose is refused by the code of another. A derived key, and the Ed25519
// signer made from it, print a fixed text like Keys.
//
// # A backup file
//
// NewBackupWriter and NewBackupReader encrypt and decrypt a stream of any
// length with AES-256-GCM (NIST SP 800-38D) in chunks, byte by byte:
//
//	8 bytes   "NOPACSBK"
//	1 byte    format, 0x01
//	4 bytes   key version, big-endian
//	4 bytes   chunk size S, big-endian (65536; a reader takes 1 to 1048576)
//	32 bytes  salt, random per file
//	then chunks i = 0, 1, ...: the ciphertext of S plaintext bytes (the last
//	chunk: 0 to S bytes) followed by its 16-byte tag
//
//	file key = HKDF-SHA-256(IKM = the Backup derived key of that version,
//	                        salt = the 32-byte salt,
//	                        info = "nopacs/backup/v1/file", L = 32)
//	nonce_i  = i as 8 bytes big-endian || 00 00 00 || last
//	AD_i     = the 49 header bytes || i as 8 bytes big-endian || last
//
// where last is the byte 0x01 for the final chunk and 0x00 for every other.
// The final chunk is the one the file ends with; a full chunk is written as
// the final one only when nothing follows it, and an empty final chunk only
// when the stream is empty. So a file cut at any byte, with chunks swapped or
// dropped, or with bytes or a chunk appended, fails authentication, and the
// reader returns io.EOF only after the final chunk opened as final.
//
// The reader hands out each chunk once that chunk is authenticated, before
// the final chunk is reached, so memory stays bounded. Bytes read before
// io.EOF may therefore belong to a file that turns out to be cut short or
// changed further on. A restore must write what it reads to a staging place
// and make it live only after io.EOF, and discard all of it on any error.
// scripts/check/backup-cipher.py implements the format with Python's
// cryptography package.
//
// # API keys
//
// A key is nopacs_<id>_<secret>: a 12-character public id that finds the key's
// row, and a 44-character secret holding 256 random bits, both drawn from the
// Base58 alphabet (digits and letters without 0, O, I and l, which are easy
// to confuse). The prefix and the underscores let a secret scanner recognize
// a leaked key, and a double-click selects the whole of it. Only the SHA-256
// of the secret is stored: the secret is random and 256 bits long, so a slow
// password hash would add nothing (NIST SP 800-63B-4, 3.1.2.2).
//
// MakeAPIKey returns the whole key only through NewAPIKey.Key; printed by
// fmt, log/slog or encoding/json, a NewAPIKey shows its id alone.
//
// # Printing
//
// Every type here that holds a secret (Keys, DerivedKey, Signer,
// BackupWriter, BackupReader, NewAPIKey) prints a fixed text through Format
// and LogValue. As an unexported field of another value fmt cannot call
// those methods and walks the fields instead, so each secret sits behind two
// pointers: the type points to an unexported struct, and that struct holds
// each secret behind a pointer of its own. Under a verb that does not fit a
// pointer (%s, %q, %t, %f and others) fmt prints a pointer again with %v from
// depth 0, which follows it once; from then on it prints every pointer it
// meets as an address. print_test.go checks every verb, log/slog and
// encoding/json, with each type held by value and by pointer, in exported
// and unexported fields.
//
// Buffers that held a key or a key file are cleared when this package is
// done with them, but Go cannot promise that wipes the memory: the garbage
// collector may have copied a buffer first, and the cipher keeps its own
// expanded key while the Keys lives.
//
// This package must never write a key, a secret's plaintext or an API key's
// secret into an error, a log line or any value it returns other than the one
// that is the point of the call, and it must never use cryptography outside
// the Go standard library.
package secret
