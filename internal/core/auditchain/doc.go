// Package auditchain makes an audit log tamper-evident: a canonical encoding
// of each entry, a hash chain over the entries in the order they were
// sealed, checkpoints that record the chain's head signed with Ed25519
// (RFC 8032), and the check of a range of entries against its checkpoints
// with the public key alone.
//
// It holds no key and reaches no database: a caller gives it entries, a
// Signer for the private key, and public keys to verify with.
//
// # An entry's encoding
//
// Every field is written as a 4-byte big-endian length followed by that many
// bytes; text is UTF-8 (an entry with text that is not valid UTF-8 is not
// encoded), integers are fixed-width big-endian inside their field. The
// fields, in this order:
//
//	 1  "nopacs/audit-entry/v1"           the encoding's name, 21 bytes
//	 2  seq                              8 bytes, unsigned, from 1
//	 3  id
//	 4  time                             8 bytes, signed: microseconds since
//	                                     1970-01-01T00:00:00Z
//	 5  key id
//	 6  local user
//	 7  remote address
//	 8  method
//	 9  tenant name
//	10  object
//	11  outcome                          "done", "refused" or "failed"
//	12  reason
//	13  details                          not a field itself: a 4-byte
//	                                     big-endian count of pairs, then for
//	                                     each pair, in increasing byte order of
//	                                     the name, the name as a field and the
//	                                     value as a field
//	14  request id
//
// So the empty text is 00 00 00 00, and a field can never be read as part of
// its neighbor.
//
// # The chain
//
//	h_0 = 32 zero bytes
//	h_n = SHA-256(h_{n-1} || encode(entry with seq n))
//
// Changing, removing, inserting or reordering any sealed entry changes the
// head of every later position.
//
// # A checkpoint
//
// A checkpoint covers the entries from seq From to seq To, both included,
// directly after the checkpoint before it (or from seq 1). Its signed text
// uses the same field encoding:
//
//	1  "nopacs/audit-checkpoint/v1"       26 bytes
//	2  installation id
//	3  log                               "master" or a tenant's id
//	4  from                              8 bytes
//	5  to                                8 bytes
//	6  head, h_to                        32 bytes
//	7  previous                          32 bytes: the digest of the
//	                                     checkpoint before, zero for the first
//	8  time                              8 bytes, microseconds as above
//	9  key version                       4 bytes
//
// The signature is pure Ed25519 (RFC 8032 5.1.6) over the signed text, made
// with the seed derived from that version of the installation key. A
// checkpoint's digest, which the next one names as previous, is the SHA-256
// of its signed text. The public key is exported as PEM of the PKIX
// structure (RFC 8410), which openssl reads.
//
// Along a log, a checkpoint's key version and its time never go down: a
// Verifier refuses a checkpoint whose key version is lower than the one
// before's, or whose time is earlier. Once a newer key version has signed,
// an older one, which may have been retired because it leaked, can no longer
// append to the log, whatever time it writes. Seal refuses such a signer, and
// raises a time earlier than the last checkpoint's (a clock stepped back) to
// that checkpoint's time.
//
// # What a seal proves, and what it does not
//
// A checkpoint that verifies proves that whoever held the audit seed of its
// key version, derived from the installation key file, signed exactly the
// entries it covers, in that order, after the checkpoints before it. It does
// not prove:
//
//   - that the key holder told the truth: anyone with the key file, the
//     server among them, can write and seal any entries it likes;
//   - that the log is whole at its end: deleting the newest checkpoints
//     together with the entries they cover, in the master and the tenant
//     databases alike (as restoring both from an older backup does), leaves a
//     shorter log that verifies. Only something known from outside those
//     databases finds that: a checkpoint, or its seq, copied elsewhere when it
//     was made, given to Verifier.Expect as an Anchor;
//   - anything about entries not yet sealed: entries after the last
//     checkpoint can be changed or deleted without trace until a checkpoint
//     covers them, and a Verifier reports them as unsealed.
//
// The public keys a Verifier checks with must come from outside the database
// being checked (a copy taken when the key was made, or derived again from
// the key file): whoever can change the database could otherwise change the
// keys with it. ParsePublicKey accepts any 32-byte Ed25519 point, low-order
// points included, so it is no check that a key is genuine.
//
// scripts/check/auditchain.py recomputes the encoding and the chain with
// Python's hashlib and checks each signature with openssl pkeyutl -verify
// -rawin; TestChainChecksInPython runs it against this package.
package auditchain
