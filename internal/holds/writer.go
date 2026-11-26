package holds

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// DefaultInterval is how often the worker looks for records owed: a replica
// that did not take a hold's record, or a hold a relabel moved, has it
// within this.
const DefaultInterval = 15 * time.Second

// Store is the part of a tenant's database the writer reaches: its holds,
// and its storages, to tell a primary from a replica and to read every
// storage's records.
type Store interface {
	tenant.Holds
	// Storage returns the storage of that name, or NotFound.
	Storage(ctx context.Context, name string) (tenant.Storage, error)
	// ListStorages returns a page of the storages in order of name.
	ListStorages(ctx context.Context, page master.Page) ([]tenant.Storage, string, error)
}

// WritePause is the gate every write to a tenant's storage asks before it
// lands bytes there (storageswitch.Gate, the shape of receive.WritePause):
// a switch of the storage to its replica holds a write until the swap, so a
// record never lands on a place its storage's name no longer names.
type WritePause interface {
	// Enter admits a write to the storage named and returns its release,
	// called once after the write's last effect; an error refuses it.
	Enter(ctx context.Context, storage string) (func(), error)
}

// Config is what a Writer needs.
type Config struct {
	// Tenant is the tenant's name, for the log and the audit entries of the
	// holds adopted.
	Tenant string
	// TenantID is the tenant's id, which every record is sealed for.
	// Required.
	TenantID string
	// Keys seals and checks the records: the installation keys. Required: a
	// record is never written without its MAC.
	Keys record.Keyring
	// Store is the tenant's database. Required.
	Store Store
	// Open opens one of the tenant's storages by name to write; the writer
	// closes each store it opens. Required.
	Open func(ctx context.Context, storage string) (blob.Store, error)
	// Pause is asked before each write; nil admits every write.
	Pause WritePause
	// Now is the server's clock; nil is time.Now.
	Now func() time.Time
	// Interval is how often Run writes the records owed; 0 is
	// DefaultInterval.
	Interval time.Duration
	// Audit returns the audit entry of a hold adopted from its record, by
	// its id and the storage it was found on, at now; nil writes one of
	// the server's own (AdoptEntry, by the local user "server").
	Audit func(id, storage string, now time.Time) master.AuditEntry
	// Log takes what the writer reports: names, ids, counts, never a
	// reason. Nil discards it.
	Log *slog.Logger
}

// Writer writes one tenant's hold records.
type Writer struct {
	cfg Config
}

// New returns the writer of cfg, or an Internal fault for a config that
// lacks what it needs.
func New(cfg Config) (*Writer, error) {
	if cfg.TenantID == "" || cfg.Keys == nil || cfg.Keys.SealVersion() == 0 || cfg.Store == nil || cfg.Open == nil {
		return nil, fault.New(fault.Internal, "holds: the writer is given no tenant id, installation keys, database or storage opener")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Audit == nil {
		cfg.Audit = func(id, storage string, now time.Time) master.AuditEntry {
			return AdoptEntry(cfg.Tenant, "server", id, storage, now)
		}
	}
	return &Writer{cfg: cfg}, nil
}

// now returns the server's time as a row keeps it: UTC, to the microsecond,
// so a record and its row name one time.
func (w *Writer) now() time.Time { return w.cfg.Now().UTC().Truncate(time.Microsecond) }

// put writes rec on the storage named, through the gate.
func (w *Writer) put(ctx context.Context, storage string, rec record.Hold) error {
	b, err := record.EncodeHold(rec, w.cfg.TenantID, w.cfg.Keys)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "a hold record cannot be made")
	}
	key, err := rec.Key()
	if err != nil {
		return fault.Wrap(fault.Internal, err, "a hold record's key cannot be made")
	}
	if w.cfg.Pause != nil {
		release, err := w.cfg.Pause.Enter(ctx, storage)
		if err != nil {
			return err
		}
		defer release()
	}
	st, err := w.cfg.Open(ctx, storage)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	_, err = st.Put(ctx, key, bytes.NewReader(b))
	return err
}

// definite reports whether err is a refusal that says for certain no row
// was changed, as opposed to a database that did not answer, after which
// the row may have committed.
func definite(err error) bool {
	switch fault.KindOf(err) {
	case fault.Invalid, fault.NotFound, fault.Precondition, fault.Conflict, fault.Denied, fault.Unauthenticated, fault.OutOfRange:
		return true
	}
	return false
}

// Placing is a hold to place.
type Placing struct {
	// Target is what it covers.
	Target tenant.HoldTarget
	// Reason is the person's words (1 to 1024 bytes, no control
	// character).
	Reason string
	// KeyID is the key placing it, empty for the local operator.
	KeyID string
	// Audit is its audit entry, the hold's id set by the store; Guard the
	// caller's check, made in the store's transaction.
	Audit master.AuditEntry
	Guard tenant.Guard
}

// Place places a hold: the active hold with the same target and reason is
// answered as it stands, and false; otherwise the new hold's record goes on
// every storage the target touches, every primary first, then its row
// commits (tenant.Holds.PlaceHold), and it is answered with true. A primary
// that does not take the record refuses the hold with its error and no row
// is made; a replica that does not is owed it.
func (w *Writer) Place(ctx context.Context, p Placing) (tenant.Hold, bool, error) {
	now := w.now()
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		return tenant.Hold{}, false, err
	}
	hp := tenant.HoldPlace{ID: id, Target: p.Target, Reason: p.Reason, KeyID: p.KeyID, Now: now}
	if err := hp.Validate(); err != nil {
		return tenant.Hold{}, false, err
	}
	_, found, err := w.cfg.Store.MatchingHold(ctx, p.Target, p.Reason)
	if err != nil {
		return tenant.Hold{}, false, err
	}
	if found {
		// The store answers the match and audits the call; a match released
		// since is a precondition failure (no record written), and a new
		// hold is placed.
		h, made, err := w.cfg.Store.PlaceHold(ctx, hp, p.Audit, p.Guard)
		if err == nil || fault.KindOf(err) != fault.Precondition {
			return h, made, err
		}
	}
	storages, err := w.cfg.Store.HoldStorages(ctx, p.Target)
	if err != nil {
		return tenant.Hold{}, false, err
	}
	rec := recordOf(tenant.Hold{ID: id, Target: p.Target, Reason: p.Reason, State: tenant.HoldActive, PlacedBy: p.KeyID, Placed: now, Seq: 1})
	written, err := w.putAll(ctx, rec, storages)
	if err != nil {
		w.abandon(rec, written)
		return tenant.Hold{}, false, err
	}
	hp.Written, hp.SealVersion = written, w.cfg.Keys.SealVersion()
	h, made, err := w.cfg.Store.PlaceHold(ctx, hp, p.Audit, p.Guard)
	switch {
	case err != nil && definite(err), err == nil && !made:
		// No row of this id is made: its records would otherwise be found
		// active with no row and adopted as a hold no one placed.
		w.abandon(rec, written)
	case err != nil:
		w.cfg.Log.Warn("holds: a hold's row may not have committed; its records stay active and are adopted if it did not", "tenant",
			w.cfg.Tenant, "hold", id, "kind", fault.KindOf(err).String())
	}
	return h, made, err
}

// putAll puts rec on every primary of storages, then on each replica, and
// returns the storages that took it. A primary that does not is the error,
// with those written before it; a replica that does not is logged and
// left owed.
func (w *Writer) putAll(ctx context.Context, rec record.Hold, storages []tenant.HoldStorage) ([]string, error) {
	var written []string
	for _, s := range storages {
		if s.Replica {
			continue
		}
		if err := w.put(ctx, s.Name, rec); err != nil {
			return written, err
		}
		written = append(written, s.Name)
	}
	for _, s := range storages {
		if !s.Replica {
			continue
		}
		if err := w.put(ctx, s.Name, rec); err != nil {
			w.cfg.Log.Warn("holds: a replica did not take a hold's record; it is owed it", "tenant", w.cfg.Tenant, "hold", rec.ID,
				"storage", s.Name, "kind", fault.KindOf(err).String())
			continue
		}
		written = append(written, s.Name)
	}
	return written, nil
}

// abandon rewrites the records of a hold whose row was not made as
// released, so no reading of the storages adopts it. A storage that does
// not take it is logged: the record stays active, and a later adoption
// makes a hold of it, the lesser error.
func (w *Writer) abandon(rec record.Hold, written []string) {
	if len(written) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rec.State, rec.Released, rec.Seq = record.HoldReleased, w.now(), rec.Seq+1
	for _, s := range written {
		if err := w.put(ctx, s, rec); err != nil {
			w.cfg.Log.Warn("holds: a record of a hold not placed could not be rewritten released; a later reading adopts it", "tenant",
				w.cfg.Tenant, "hold", rec.ID, "storage", s, "kind", fault.KindOf(err).String())
		}
	}
}

// Releasing is a hold to release.
type Releasing struct {
	// ID names the hold; Reason is the person's; KeyID the key releasing
	// it, empty for the local operator.
	ID, Reason, KeyID string
	// Audit is its audit entry; Guard the caller's check.
	Audit master.AuditEntry
	Guard tenant.Guard
}

// Release releases a hold, its count reserved before any record says
// released (tenant.Holds, "A release reserves its count first"): the
// release begins (tenant.Holds.BeginHoldRelease), the hold still holding at
// its next count; its record is rewritten released at that count on every
// primary it is on, each marked; then the release commits at that count
// (tenant.Holds.ReleaseHold) and is answered; each replica's record is
// rewritten then, or owed. A primary that does not take the record, or a
// release that does not commit, refused or unanswered, ends the release
// (endRelease) and answers the error: the hold is active at the count
// after, so its active records outrank every released record written. A
// hold released already is answered as it stands.
func (w *Writer) Release(ctx context.Context, r Releasing) (tenant.Hold, error) {
	now := w.now()
	hr := tenant.HoldRelease{ID: r.ID, Reason: r.Reason, KeyID: r.KeyID, Now: now}
	if err := hr.Validate(); err != nil {
		return tenant.Hold{}, err
	}
	b, err := w.cfg.Store.BeginHoldRelease(ctx, hr)
	if err != nil {
		return tenant.Hold{}, err
	}
	if b.State == tenant.HoldReleased {
		return w.cfg.Store.ReleaseHold(ctx, hr, r.Audit, r.Guard)
	}
	primaries, replicas, err := w.split(ctx, b.Storages)
	if err != nil {
		w.endRelease(b, nil)
		return tenant.Hold{}, err
	}
	hr.Seq = b.Seq
	released := b
	released.State, released.Released, released.ReleasedBy, released.ReleaseReason, released.Releasing = tenant.HoldReleased, now, r.KeyID,
		r.Reason, false
	var written []string
	for _, s := range primaries {
		if err := w.putMarked(ctx, s, released); err != nil {
			w.endRelease(b, written)
			return tenant.Hold{}, err
		}
		written = append(written, s)
	}
	out, err := w.cfg.Store.ReleaseHold(ctx, hr, r.Audit, r.Guard)
	if err != nil {
		w.endRelease(b, written)
		return out, err
	}
	for _, s := range replicas {
		_ = w.putMarked(ctx, s, out) // owed when it fails: the worker writes it
	}
	return out, nil
}

// endRelease ends the release of b that did not commit
// (tenant.Holds.AbortHoldRelease): the hold is active at the count after,
// and its record is written there on every storage it is on, so it
// outranks the released records written at b's count; a storage that does
// not take it stays owed. A release that did commit after all is left as
// it is. A database that does not answer leaves the release under way
// until the worker ends it as stale (StaleRelease); the storages written
// released are then given the active record of the count after at once,
// unmarked, so a database lost before then still finds the hold active.
func (w *Writer) endRelease(b tenant.Hold, written []string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	h, err := w.cfg.Store.AbortHoldRelease(ctx, b.ID, b.Seq)
	if err != nil {
		w.cfg.Log.Error("holds: a release that did not commit could not be ended; the hold holds, and is made active again shortly", "tenant",
			w.cfg.Tenant, "hold", b.ID, "kind", fault.KindOf(err).String())
		next := b
		next.Seq, next.Releasing = b.Seq+1, false
		for _, s := range written {
			unlock := recordLocks.lock(w.cfg.TenantID, b.ID)
			err := w.put(ctx, s, recordOf(next))
			unlock()
			if err != nil {
				w.cfg.Log.Error("holds: a hold's record written released could not be written active again; the hold still holds in the "+
					"database", "tenant", w.cfg.Tenant, "hold", b.ID, "storage", s, "kind", fault.KindOf(err).String())
			}
		}
		return
	}
	if h.State != tenant.HoldActive {
		return
	}
	for _, s := range h.Storages {
		if err := w.putMarked(ctx, s, h); err != nil {
			w.cfg.Log.Warn("holds: a storage did not take a hold's record after its release did not commit; it is owed it", "tenant",
				w.cfg.Tenant, "hold", h.ID, "storage", s, "kind", fault.KindOf(err).String())
		}
	}
}

// putMarked puts the record of h on the storage named and marks it
// written at h's count, under the hold's lock, so no other writer of the
// hold puts its record between the two.
func (w *Writer) putMarked(ctx context.Context, storage string, h tenant.Hold) error {
	unlock := recordLocks.lock(w.cfg.TenantID, h.ID)
	defer unlock()
	if err := w.put(ctx, storage, recordOf(h)); err != nil {
		return err
	}
	w.mark(ctx, h, storage)
	return nil
}

// mark records the hold's record written on the storage at its count,
// under the installation key's seal version; a failure leaves it owed, and
// the worker writes it again.
func (w *Writer) mark(ctx context.Context, h tenant.Hold, storage string) {
	if err := w.cfg.Store.MarkHoldRecord(ctx, h.ID, storage, h.Seq, w.cfg.Keys.SealVersion()); err != nil && fault.KindOf(err) != fault.NotFound {
		w.cfg.Log.Warn("holds: a hold record written could not be marked; it is written again", "tenant", w.cfg.Tenant, "hold", h.ID,
			"storage", storage, "kind", fault.KindOf(err).String())
	}
}

// split returns the storages named, primaries and replicas apart. A
// storage the tenant no longer has is left out: no record can go there.
func (w *Writer) split(ctx context.Context, names []string) ([]string, []string, error) {
	var primaries, replicas []string
	for _, n := range names {
		s, err := w.cfg.Store.Storage(ctx, n)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			continue
		case err != nil:
			return nil, nil, err
		case s.ReplicaOf != "":
			replicas = append(replicas, n)
		default:
			primaries = append(primaries, n)
		}
	}
	return primaries, replicas, nil
}
