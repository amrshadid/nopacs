package tenant

import "context"

// QuotaUse is what a tenant holds against its quota (quota.bytes,
// quota.instances), read from its database's running totals, never from a
// walk of its storages. Every part counts instance files only: the
// manifests and metadata files a scan keeps beside them are the archive's,
// not the tenant's data.
type QuotaUse struct {
	// HeldInstances and HeldBytes are the instances the catalog holds, and
	// the bytes of their files: the catalog's running totals.
	HeldInstances, HeldBytes int64
	// PurgeInstances and PurgeBytes are the files corrections own that
	// their purges have not removed yet: deleted instances, revisions a
	// replace or an edit superseded, and the old keys of a relabel. They
	// stay on the storage until the purge, so they count until it.
	PurgeInstances, PurgeBytes int64
	// ReceivingInstances and ReceivingBytes are the files the tenant's
	// receptions recorded as received and not yet archived, found again or
	// quarantined: acknowledged to their senders, so they are archived
	// whatever the quota says, and count from the moment they are recorded.
	ReceivingInstances, ReceivingBytes int64
}

// Instances returns every instance file the use counts.
func (u QuotaUse) Instances() int64 {
	return u.HeldInstances + u.PurgeInstances + u.ReceivingInstances
}

// Bytes returns the bytes of every file the use counts.
func (u QuotaUse) Bytes() int64 { return u.HeldBytes + u.PurgeBytes + u.ReceivingBytes }

// Plus returns u with files more instances of bytes more bytes received.
func (u QuotaUse) Plus(files, bytes int64) QuotaUse {
	u.ReceivingInstances += files
	u.ReceivingBytes += bytes
	return u
}

// QuotaBounds are a tenant's quota settings as they apply on it.
type QuotaBounds struct {
	// Bytes is quota.bytes, 0 for no bound.
	Bytes int64
	// Instances is quota.instances, 0 for no bound.
	Instances int64
	// WarnPercent is quota.warn, the share of a bound past which the
	// tenant is warned, from 1 to 100.
	WarnPercent int
}

// Bounded reports whether either bound is set.
func (b QuotaBounds) Bounded() bool { return b.Bytes > 0 || b.Instances > 0 }

// QuotaState is where a tenant's use stands against its bounds.
type QuotaState string

// The states.
const (
	// QuotaUnder: under the warning of every bound, or no bound set.
	QuotaUnder QuotaState = "under"
	// QuotaWarned: at or past quota.warn of a bound, under every bound.
	QuotaWarned QuotaState = "warned"
	// QuotaReached: at or past a bound. New files are refused as a
	// temporary failure; what was received before is archived.
	QuotaReached QuotaState = "reached"
)

// Reached reports whether u is at or past a bound of b: the next file
// would take the tenant past it. A bound is reached when the use equals it,
// so a bound of n instances holds n and refuses the next.
func (b QuotaBounds) Reached(u QuotaUse) bool {
	return (b.Bytes > 0 && u.Bytes() >= b.Bytes) || (b.Instances > 0 && u.Instances() >= b.Instances)
}

// State returns where u stands against b.
func (b QuotaBounds) State(u QuotaUse) QuotaState {
	switch {
	case b.Reached(u):
		return QuotaReached
	case warnPassed(u.Bytes(), b.Bytes, b.WarnPercent) || warnPassed(u.Instances(), b.Instances, b.WarnPercent):
		return QuotaWarned
	}
	return QuotaUnder
}

// warnPassed reports whether used is at or past percent of bound, never
// for no bound. It divides rather than multiplies, so a bound near the
// largest size does not overflow.
func warnPassed(used, bound int64, percent int) bool {
	if bound <= 0 {
		return false
	}
	share := bound / 100 * int64(percent)
	share += bound % 100 * int64(percent) / 100
	return used >= share
}

// Valid reports whether s is one of the three states.
func (s QuotaState) Valid() bool { return s == QuotaUnder || s == QuotaWarned || s == QuotaReached }

// EventQuota is what a tenant holds against its storage quota changing
// state (QuotaChanged).
const EventQuota EventType = "quota"

// QuotaChanged is what a tenant holds against its storage quota moving to
// another state, the contract's QuotaChanged message: one event for each
// change of state, never one per file. The server reports it
// (AppendEvents), since it is a reading of the running totals over time,
// not a change of one row.
type QuotaChanged struct {
	// State is the state after the change.
	State QuotaState `json:"state"`
	// UsedBytes and UsedInstances are what the tenant held then.
	UsedBytes int64 `json:"used_bytes"`
	// UsedInstances is the files the tenant held then.
	UsedInstances int64 `json:"used_instances"`
	// BytesBound and InstancesBound are the bounds then, 0 for none.
	BytesBound int64 `json:"bytes_bound"`
	// InstancesBound is quota.instances then, 0 for none.
	InstancesBound int64 `json:"instances_bound"`
}

// EventType implements EventPayload.
func (*QuotaChanged) EventType() EventType { return EventQuota }

// validate implements EventPayload.
func (p *QuotaChanged) validate() error {
	if !p.State.Valid() {
		return invalid("a quota's state is under, warned or reached")
	}
	if p.UsedBytes < 0 || p.UsedInstances < 0 || p.BytesBound < 0 || p.InstancesBound < 0 {
		return invalid("a quota's use and bounds are not negative")
	}
	return nil
}

// Quotas is the part of a tenant's store that counts its use against its
// quota.
type Quotas interface {
	// QuotaUse returns what the tenant holds now, read in one read
	// transaction, so the parts agree: a commit that archives a received
	// file moves it from Receiving to Held at once, and a purge's removal
	// takes it out of Purge in the transaction that records it.
	QuotaUse(ctx context.Context) (QuotaUse, error)
}
