package access

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// The bounds of a rotation's overlap: the time the rotated key keeps working
// beside its successor, so a system that holds it has that long to switch.
const (
	// DefaultRotationOverlap is the overlap when the request names none: a
	// day, time for a system that renews its key once a day to pick up the
	// new one.
	DefaultRotationOverlap = 24 * time.Hour
	// MaxRotationOverlap is the longest overlap: thirty days. A key that
	// must keep working longer is not being rotated.
	MaxRotationOverlap = 30 * 24 * time.Hour
	// MinSelfRotationOverlap is the shortest overlap a key rotating itself
	// may ask for, without admin on the installation: a thief who holds the
	// key could otherwise end it at once and take the only working key,
	// cutting its holder off before anyone could act.
	MinSelfRotationOverlap = time.Hour
)

// The keys of the details the operations of this file write in audit
// entries.
const (
	detailNetworks   = "networks"
	detailClientCert = "client_cert_sha256"
	detailOverlap    = "overlap"
)

// reasonNotAdminNorSelf is the audit reason of a rotation of another key
// asked for by a key without admin on the installation.
const reasonNotAdminNorSelf = "the decision carries neither admin on the installation nor the key to rotate"

// addRuleDetails writes a key's rules into an audit entry's details: the
// networks as they are written, cut to a detail's bound, and the client
// certificate's fingerprint, which is not a secret.
func addRuleDetails(details map[string]string, r master.KeyRules) {
	if len(r.Networks) > 0 {
		details[detailNetworks] = auditText(r.Networks.String(), master.MaxAuditDetailValueLength)
	}
	if r.ClientCert != "" {
		details[detailClientCert] = r.ClientCert
	}
}

// SetKeyRules replaces where the key id may be used from with rules, and
// returns the key with them. It needs the installation's admin. Rules that
// are not valid are Invalid, an unknown id NotFound, a revoked key Conflict.
// The audit entry names the key and the new rules. The key's next request is
// judged by them, since every check reads the key.
func (a *Access) SetKeyRules(ctx context.Context, d Decision, id string, rules master.KeyRules) (master.Key, error) {
	details := map[string]string{}
	addRuleDetails(details, rules)
	entry, by, err := a.change(ctx, d, auditText(id, master.MaxAuditFieldLength), details)
	if err != nil {
		return master.Key{}, err
	}
	if err := master.ValidateID("key id", id); err != nil {
		return master.Key{}, a.failAfter(entry, err)
	}
	if err := rules.Validate(); err != nil {
		return master.Key{}, a.failAfter(entry, err)
	}
	k, err := a.store.Key(ctx, id)
	if err == nil {
		err = a.store.SetKeyRules(ctx, id, rules, entry, by)
	}
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			err = fault.New(fault.NotFound, "no key has this id")
		}
		return master.Key{}, a.failAfter(entry, err)
	}
	k.Rules = rules
	return publicKey(k), nil
}

// RotateKey replaces the key id with a new one, its successor, which holds
// the same grants, holder and rules, and returns the successor with its
// secret, once, and the rotated key as it now stands. The rotated key keeps
// working for overlap (0 to MaxRotationOverlap), or until its own expiry when
// that is sooner, so whoever holds it has that long to switch, with no
// moment without a working key. The successor lives as long as the rotated
// key was made to live, from now, but never longer than maxLifetime (the
// installation's key.max-lifetime); a key made never to expire makes a
// successor that never expires.
//
// It needs the installation's admin, or the key id itself: a key may rotate
// itself, so a system renews its own key with no person involved, with an
// overlap of at least MinSelfRotationOverlap. Any other caller is Denied. A key revoked, expired or already rotated is Conflict:
// a key makes at most one successor. The rotation and its audit entry, which
// names both keys, are one transaction, and an EventKeyRotated is raised.
func (a *Access) RotateKey(ctx context.Context, d Decision, id string, overlap, maxLifetime time.Duration) (CreatedKey, master.Key, error) {
	if !d.checked() {
		return CreatedKey{}, master.Key{}, fault.New(fault.Denied, MessageDenied)
	}
	entry := d.Entry(master.OutcomeDone, "", Facts{})
	entry.Object = auditText(id, master.MaxAuditFieldLength)
	entry.Details = map[string]string{detailOverlap: overlap.String()}
	admin := atLeast(d.principal.Role(master.Installation), master.RoleAdmin)
	self := d.principal.isKey && d.principal.key.ID == id
	var by master.Actor
	switch {
	case admin:
		by = d.Actor(master.Installation, master.RoleAdmin)
	case self:
		by = d.Actor(master.Installation, "")
	default:
		a.record(entry, master.OutcomeRefused, reasonNotAdminNorSelf)
		return CreatedKey{}, master.Key{}, fault.New(fault.Denied, MessageDenied)
	}
	if !d.confirmed {
		if err := a.store.CheckActor(ctx, by); err != nil {
			return CreatedKey{}, master.Key{}, a.failAfter(entry, err)
		}
	}
	if err := master.ValidateID("key id", id); err != nil {
		return CreatedKey{}, master.Key{}, a.failAfter(entry, err)
	}
	if overlap < 0 || overlap > MaxRotationOverlap {
		return CreatedKey{}, master.Key{}, a.failAfter(entry, fault.New(fault.Invalid, "a rotation's overlap is 0 to 30 days"))
	}
	if !admin && overlap < MinSelfRotationOverlap {
		return CreatedKey{}, master.Key{}, a.failAfter(entry, fault.New(fault.Invalid, "a key rotating itself keeps working at least an hour beside its successor; ask for an overlap of 1h or more"))
	}
	if maxLifetime <= 0 {
		return CreatedKey{}, master.Key{}, a.failAfter(entry, fault.New(fault.Internal, "access: a rotation needs the installation's maximum key lifetime"))
	}
	old, err := a.store.Key(ctx, id)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			err = fault.New(fault.NotFound, "no key has this id")
		}
		return CreatedKey{}, master.Key{}, a.failAfter(entry, err)
	}
	now := entry.Time
	var expires time.Time
	if !old.Expires.IsZero() {
		expires = now.Add(min(old.Expires.Sub(old.Created), maxLifetime))
	}
	made, err := secret.MakeAPIKey()
	if err != nil {
		return CreatedKey{}, master.Key{}, a.failAfter(entry, err)
	}
	oldEnds := now.Add(overlap)
	successor := master.Key{ID: made.ID, Hash: made.Hash[:], Created: now, Expires: expires}
	stored, err := a.store.RotateKey(ctx, id, successor, oldEnds, entry, by)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			err = fault.New(fault.NotFound, "no key has this id")
		}
		return CreatedKey{}, master.Key{}, a.failAfter(entry, err)
	}
	old.Expires = master.RotatedExpiry(old.Expires, oldEnds)
	old.Successor = stored.ID
	a.raise(Event{Kind: EventKeyRotated, Time: now, KeyID: id, Address: entry.RemoteAddress, Until: old.Expires, Detail: stored.ID})
	return CreatedKey{Key: publicKey(stored), Secret: made}, publicKey(old), nil
}
