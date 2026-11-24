package auditseal

import (
	"crypto/ed25519"

	"github.com/amrshadid/nopacs/internal/core/auditchain"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// chainEntry returns e as the chain encodes it, at seq.
func chainEntry(seq int64, e master.AuditEntry) auditchain.Entry {
	return auditchain.Entry{
		Seq: uint64(seq), ID: e.ID, Time: e.Time, KeyID: e.KeyID, LocalUser: e.LocalUser, //nolint:gosec // G115: a seq is from 1.
		RemoteAddress: e.RemoteAddress, Method: e.Method, TenantName: e.TenantName, Object: e.Object,
		Outcome: string(e.Outcome), Reason: e.Reason, Details: e.Details, RequestID: e.RequestID,
	}
}

// chainCheckpoint returns c as the chain signs it.
func chainCheckpoint(c master.AuditCheckpoint) auditchain.Checkpoint {
	return auditchain.Checkpoint{
		Installation: c.Installation, Log: c.Log, From: uint64(c.From), To: uint64(c.To), //nolint:gosec // G115: a store holds seqs from 1.
		Head: auditchain.Digest(c.Head), Previous: auditchain.Digest(c.Previous), Time: c.Time,
		KeyVersion: c.KeyVersion, Signature: c.Signature,
	}
}

// SignedText returns the bytes c's signature covers, rebuilt from its fields
// (audit_seal.proto, AuditCheckpoint.signed_text), so an answer carries them
// and a check outside needs no rule of its own to rebuild them.
func SignedText(c master.AuditCheckpoint) ([]byte, error) { return chainCheckpoint(c).SignedText() }

// storeCheckpoint returns a signed checkpoint as a store keeps it, with its
// digest.
func storeCheckpoint(c auditchain.Checkpoint, digest auditchain.Digest) master.AuditCheckpoint {
	return master.AuditCheckpoint{
		Installation: c.Installation, Log: c.Log, From: int64(c.From), To: int64(c.To), //nolint:gosec // G115: from a store's seqs plus one batch.
		Head: master.AuditDigest(c.Head), Previous: master.AuditDigest(c.Previous), Digest: master.AuditDigest(digest),
		Time: c.Time, KeyVersion: c.KeyVersion, Signature: c.Signature,
	}
}

// stateAfter returns the chain's state after c, as c holds it: the start
// from which the checkpoint after it is sealed or checked.
func stateAfter(c master.AuditCheckpoint) auditchain.State {
	return auditchain.State{
		Seq: uint64(c.To), Head: auditchain.Digest(c.Head), Checkpoint: auditchain.Digest(c.Digest), //nolint:gosec // G115: a store holds seqs from 1.
		KeyVersion: c.KeyVersion, Time: c.Time,
	}
}

// PublicKey is the public key of one version of an installation key's
// audit signing key.
type PublicKey struct {
	// Installation is the id of the installation whose key it derives from.
	Installation string
	// Version is the installation key's version.
	Version uint32
	// Key is the Ed25519 public key (RFC 8032 5.1.5).
	Key ed25519.PublicKey
	// Current reports that it signs the checkpoints made now.
	Current bool
}

// PEM returns the key as a PEM SubjectPublicKeyInfo (RFC 8410), which
// openssl reads.
func (k PublicKey) PEM() ([]byte, error) { return auditchain.MarshalPublicKey(k.Key) }

// PublicKeys returns the public keys of every version of the installation
// key file holds, oldest first, the one Derive uses now marked current: the
// keys that check every checkpoint the installation signed. They are
// derived from the key file, never read from a database, so a database
// rewritten with keys of its own does not check.
func PublicKeys(keys *secret.Keys, installation string) ([]PublicKey, error) {
	current := keys.SealVersion()
	var out []PublicKey
	for v := uint32(1); v <= current; v++ {
		d, err := keys.DeriveVersion(secret.AuditSeal, v)
		if fault.KindOf(err) == fault.NotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		s, err := d.Signer()
		if err != nil {
			return nil, err
		}
		out = append(out, PublicKey{Installation: installation, Version: v, Key: s.Public(), Current: v == current})
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Internal, "auditseal: the key file holds no key")
	}
	return out, nil
}

// keyMap returns the keys of one installation by version, as a Verifier
// takes them.
func keyMap(keys []PublicKey, installation string) auditchain.PublicKeys {
	out := auditchain.PublicKeys{}
	for _, k := range keys {
		if k.Installation == installation {
			out[k.Version] = k.Key
		}
	}
	return out
}
