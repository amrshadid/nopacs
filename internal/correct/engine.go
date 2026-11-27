package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/interrupt"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Config is what an Engine needs.
type Config struct {
	// Tenant is the tenant's name, for the audit entries the engine writes.
	Tenant string
	// TenantID is the tenant's id, which every correction record the engine
	// writes is authenticated for (record.EncodeCorrection), so a record
	// copied to another tenant's storage does not verify there. Required.
	TenantID string
	// Keys authenticates the correction records: the installation keys
	// (secret.Keys). Required: a record is never written without its MAC,
	// since every reader refuses one.
	Keys record.Keyring
	// Store is the tenant's database.
	Store tenant.Corrections
	// OpenStorage opens one of the tenant's storages by name.
	OpenStorage func(ctx context.Context, name string) (blob.Store, error)
	// Now is the server's clock; nil for time.Now.
	Now func() time.Time
	// Grace answers the tenant's delete.grace now; nil for
	// tenant.DefaultGrace.
	Grace func(ctx context.Context) (time.Duration, error)
	// Log takes what the engine reports; nil for none.
	Log *slog.Logger
	// ConfirmKey is the key, derived from the installation key for
	// secret.ConfirmToken, a delete's preview token is made with
	// (tenant.KeyedConfirm), so only this server makes one a delete takes.
	// The server sets it; nil leaves the token unkeyed, the store's own,
	// which only a test that drives the store directly wants.
	ConfirmKey *secret.DerivedKey
	// LockScan takes the lock the reception pipeline holds while it
	// rewrites a scan's manifest and sidecar (receive.Pipeline.LockScan),
	// by storage and manifest key, and returns its release; a purge or a
	// replace that rewrites them in place holds it too. Nil uses a lock of
	// the engine's own, which serializes only the engine's writers.
	LockScan func(storage, manifestKey string) func()
	// RemoveIncoming removes, from the incoming area, the bytes of the
	// files a delete's purge cleared (tenant.Corrections.ClearWithdrawn);
	// nil leaves them to the reception pipeline's start, which removes
	// every file no row keeps.
	RemoveIncoming func(ctx context.Context, paths []string) error
	// Fence, when not nil, is asked before each step that writes to the
	// tenant's storages (a delete's, a relabel's and a replace's start, a
	// relabel's copy, a purge): an error stops the step before it writes,
	// and is answered. The server's reads the owner record of each storage
	// (the registry's CheckFence), so a tenant another installation took
	// over since its last check, or one handed off, writes nothing more.
	Fence func(ctx context.Context) error
	// Pause, when not nil, is entered before every write to a storage and
	// every removal from one (a record, a purge's removal and its manifest
	// and metadata rewritten, a relabel's copies, a replace's new revision,
	// what an abandon undoes), the storage opened only once admitted, and
	// released once the write returned (storageswitch.Gate): a switch of
	// that storage to its replica waits for a write admitted and holds the
	// next one, so no step running when the switch began writes to a place
	// its storage's name no longer names. A relabel and a replace hold their
	// storages from their first copy until their publish is answered (hold).
	// Fence refuses a step that starts while a switch runs; Pause holds one
	// that started before. Nil admits every write.
	Pause WritePause
	// Published, when not nil, is called once a correction's publish has
	// committed and its records were rewritten, with the correction as
	// published, before the call that published it returns. The server's
	// writes at once the hold records a relabel owes under the labels it
	// moved, so a rebuild or restore read soon after finds each hold on the
	// labels the catalog has.
	Published func(ctx context.Context, c tenant.Correction)
	// Judge judges each new revision an edit would write, before it is
	// written (ProfileJudge): the check after an edit. Nil refuses every
	// edit: one is never archived unjudged.
	Judge EditJudge
}

// WritePause is the gate a write to a tenant's storage asks before it lands
// there (storageswitch.Gate, the shape of receive.WritePause).
type WritePause interface {
	// Enter admits a write to the storage named and returns its release,
	// called once after the write's last effect; an error refuses it.
	Enter(ctx context.Context, storage string) (func(), error)
}

// fence asks Config.Fence, when there is one, whether the tenant may still
// write to its storages.
func (e *Engine) fence(ctx context.Context) error {
	if e.cfg.Fence == nil {
		return nil
	}
	return e.cfg.Fence(ctx)
}

// enter asks Config.Pause, when there is one, to admit a write to (or a
// removal from) the storage named, and returns its release. A storage
// whose gate the caller holds already (holding) is admitted at once: a
// gate paused by a switch waits for that hold to end, so entering it a
// second time under the hold would wait for itself until the gate refuses.
func (e *Engine) enter(ctx context.Context, storage string) (func(), error) {
	if e.cfg.Pause == nil || isHeld(ctx, storage) {
		return func() {}, nil
	}
	return e.cfg.Pause.Enter(ctx, storage)
}

// heldKey is the context key of the storages whose gates a step holds.
type heldKey struct{}

// holding returns ctx carrying that the caller holds the gates of the
// storages named (hold), so the writes it makes under them do not enter
// those gates again.
func holding(ctx context.Context, storages []string) context.Context {
	held, _ := ctx.Value(heldKey{}).([]string)
	return context.WithValue(ctx, heldKey{}, append(slices.Clone(held), storages...))
}

// isHeld reports whether ctx carries a hold of the gate of storage.
func isHeld(ctx context.Context, storage string) bool {
	held, _ := ctx.Value(heldKey{}).([]string)
	return slices.Contains(held, storage)
}

// gated runs write on the storage named inside its write gate
// (Config.Pause): the gate is entered first, the storage opened only once
// admitted, and the gate released after write returned. A switch swaps the
// storage's place while the gate holds its writes, so a store opened before
// the gate admitted may be the place the name no longer names, and a write
// or a removal through it would land on the old place.
func (e *Engine) gated(ctx context.Context, storage string, write func(st blob.Store) error) error {
	release, err := e.enter(ctx, storage)
	if err != nil {
		return err
	}
	defer release()
	st, err := e.cfg.OpenStorage(ctx, storage)
	if err != nil {
		return err
	}
	return write(st)
}

// hold enters the write gate of each storage named, in order of name, and
// returns one release of them all, which may be called more than once. A
// step whose writes must all land on one place per storage (a relabel's
// copies and the publish that names them, a replace's new revision and the
// manifest that names it) holds its storages from before it opens them until
// after its publish: a switch swapping a place between two of its writes
// would leave the first on a place its storage no longer names, while the
// catalog names it there. The caller releases before it abandons, whose
// own writes enter the gate again.
func (e *Engine) hold(ctx context.Context, storages []string) (func(), error) {
	names := slices.Clone(storages)
	slices.Sort(names)
	names = slices.Compact(names)
	var releases []func()
	var once sync.Once
	release := func() {
		once.Do(func() {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
		})
	}
	for _, name := range names {
		r, err := e.enter(ctx, name)
		if err != nil {
			release()
			return func() {}, err
		}
		releases = append(releases, r)
	}
	return release, nil
}

// Engine makes corrections on a tenant's storages and database.
type Engine struct {
	cfg Config
	// seams are the test's.
	seams seams
	// locks is the engine's own scan lock, when Config.LockScan is nil.
	locks keyLocks
	// copying holds the relabels whose copy runs in this process: a stop
	// asked meanwhile leaves the removal of the copies to the copy, which
	// sees the stop before its next write.
	mu      sync.Mutex
	copying map[string]bool
	// usedTokens maps a keyed confirm token that made a delete to the
	// store's token it stood for and the target it was for (remember).
	usedTokens map[string]usedToken
}

// seams are the points a test changes the engine at.
type seams struct {
	// at is called after each durable step, by name; an error stops the
	// engine there as a crash would, nothing after it run.
	at func(step string) error
	// held answers whether a purge must wait before an object, on any of
	// its copies: an active hold covers the correction. New sets it to ask
	// the store when it answers holds (holdAsker), false otherwise.
	held func(ctx context.Context, c tenant.Correction) bool
}

// reached marks the durable step name of c done: a server built for a
// harness that kills it there ends at this call (internal/interrupt, named
// correction:<kind>:<name>), then the test's seam answers whether to stop.
func (e *Engine) reached(c tenant.Correction, name string) error {
	interrupt.At("correction", string(c.Kind), name)
	return e.seams.at(name)
}

// keyLocks is a set of locks by name, each made when first taken and
// dropped when its last holder releases it.
type keyLocks struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

// keyLock is one lock and how many hold or wait for it.
type keyLock struct {
	mu   sync.Mutex
	refs int
}

// lock takes the lock of name and returns its release.
func (k *keyLocks) lock(name string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyLock{}
	}
	l := k.locks[name]
	if l == nil {
		l = &keyLock{}
		k.locks[name] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.locks, name)
		}
		k.mu.Unlock()
	}
}

// lockScan takes the scan lock of the manifest key on a storage.
func (e *Engine) lockScan(storage, manifestKey string) func() {
	if e.cfg.LockScan != nil {
		return e.cfg.LockScan(storage, manifestKey)
	}
	return e.locks.lock(storage + "\x00" + manifestKey)
}

// startCopy marks a relabel's copy running in this process, and returns
// its end.
func (e *Engine) startCopy(id string) func() {
	e.mu.Lock()
	e.copying[id] = true
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		delete(e.copying, id)
		e.mu.Unlock()
	}
}

// isCopying reports whether a relabel's copy runs in this process.
func (e *Engine) isCopying(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.copying[id]
}

// New returns an engine over cfg.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil || cfg.OpenStorage == nil {
		return nil, fault.New(fault.Internal, "a corrections engine needs a store and its storages")
	}
	if err := tenant.ValidateName("tenant", cfg.Tenant); err != nil {
		return nil, err
	}
	if cfg.TenantID == "" || cfg.Keys == nil || cfg.Keys.SealVersion() == 0 {
		return nil, fault.New(fault.Internal, "a corrections engine needs its tenant's id and the installation keys to authenticate its records")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.ConfirmKey != nil && cfg.ConfirmKey.Purpose() != secret.ConfirmToken {
		return nil, fault.New(fault.Internal, "a corrections engine's confirm key is derived for confirm tokens")
	}
	e := &Engine{cfg: cfg, copying: map[string]bool{}, usedTokens: map[string]usedToken{}, seams: seams{
		at:   func(string) error { return nil },
		held: func(context.Context, tenant.Correction) bool { return false },
	}}
	if h, ok := cfg.Store.(holdAsker); ok {
		e.seams.held = func(ctx context.Context, c tenant.Correction) bool {
			id, err := h.CorrectionHeld(ctx, c.ID)
			// A hold that cannot be read is taken as one: the purge stops
			// and is run again, rather than remove what a hold may cover.
			return err != nil || id != ""
		}
	}
	return e, nil
}

// holdAsker is the part of the tenant's database that answers whether an
// active hold covers a correction (tenant.Holds.CorrectionHeld): a store
// that has it is asked before each object a purge removes, on every copy.
type holdAsker interface {
	CorrectionHeld(ctx context.Context, id string) (string, error)
}

// now is the server's clock, in UTC to the microsecond, as the stores keep
// it.
func (e *Engine) now() time.Time { return e.cfg.Now().UTC().Truncate(time.Microsecond) }

// grace answers the grace period to publish with.
func (e *Engine) grace(ctx context.Context) (time.Duration, error) {
	if e.cfg.Grace == nil {
		return tenant.DefaultGrace, nil
	}
	g, err := e.cfg.Grace(ctx)
	if err != nil {
		return 0, err
	}
	return g, tenant.ValidateGrace(g)
}

// The phrases of the state reasons the engine writes.
const (
	reasonStopped    = "the server stopped before it was published"
	reasonUnreadable = "an object could not be read whole"
	reasonChanged    = "the target changed while it was prepared"
	reasonStorage    = "storage does not answer"
	reasonPublish    = "its publishing transaction could not be committed"
)

// errCopy is an object a relabel or replace could not write and read back
// whole.
var errCopy = errors.New("an object could not be read whole")

// recordOf returns the record of c on the storage named, naming the scans
// given (a replica's are its primary's) and, for a purge, what that storage
// did.
func recordOf(c tenant.Correction, scans []tenant.CorrectionScan, storage string) record.Correction {
	r := record.Correction{Format: record.Format, ID: c.ID, Kind: c.Kind, State: c.State, ReasonSHA256: record.ReasonDigest(c.Reason),
		Created: c.Created}
	if c.State.Published() || c.State == record.StateUndone {
		r.CommittedAt, r.PurgeAfter = c.CommitTime, c.PurgeAfter
	}
	for _, s := range scans {
		r.Scans = append(r.Scans, record.Scan{Subject: s.Path.Subject, Session: s.Path.Session, Scan: s.Path.Scan,
			StudyUID: s.StudyUID, SeriesUID: s.SeriesUID})
	}
	switch c.Kind {
	case record.KindDelete:
		r.Level = c.Level
		if c.Level == record.LevelInstance {
			r.Instances = []string{c.Target.Instance}
		}
	case record.KindRelabel:
		from := map[string]string{record.LevelSubject: c.Target.Subject, record.LevelSession: c.Target.Session,
			record.LevelScan: c.Target.Scan}[c.Level]
		r.Relabel = &record.Relabel{Level: c.Level, From: from, To: c.ToLabel}
	case record.KindReplace:
		r.Replace = &record.Replacement{SOPInstanceUID: c.Target.Instance, OldRevision: c.OldRevision, NewRevision: c.NewRevision,
			OldSHA256: hex.EncodeToString(c.OldSHA256[:]), NewSHA256: hex.EncodeToString(c.NewSHA256[:])}
	case record.KindEdit:
		r.Level, r.Edits = c.Level, editRecords(c.Edits, scans)
	}
	if c.State == record.StatePurging || c.State == record.StatePurged {
		r.Purge = &record.Purge{}
		for _, d := range c.Dispositions {
			if storage != "" && d.Storage == storage {
				r.Purge = &record.Purge{Removed: d.Removed, Hidden: d.Hidden, Retained: d.Retained, Unknown: d.Unknown,
					RetainUntil: d.RetainUntil}
			}
		}
	}
	return r
}

// byStorage groups scans by their storage, in order of storage.
func byStorage(scans []tenant.CorrectionScan) ([]string, map[string][]tenant.CorrectionScan) {
	var names []string
	out := map[string][]tenant.CorrectionScan{}
	for _, s := range scans {
		if _, ok := out[s.Storage]; !ok {
			names = append(names, s.Storage)
		}
		out[s.Storage] = append(out[s.Storage], s)
	}
	return names, out
}

// writeRecords puts c's record, as its row says now and authenticated for
// the tenant, on every storage it touches, then records that they are
// written; then on each replica of those storages, where a replica that
// does not take it is owed it (writeReplicaRecords). A start rewrites the
// records that lag their rows through here too, so every record on a
// storage carries a MAC this installation made.
func (e *Engine) writeRecords(ctx context.Context, c tenant.Correction) error {
	scans, err := e.cfg.Store.CorrectionScans(ctx, c.ID)
	if err != nil {
		return err
	}
	names, groups := byStorage(scans)
	for _, name := range names {
		if err := e.putRecord(ctx, c, groups[name], name); err != nil {
			return err
		}
	}
	if err := e.cfg.Store.MarkRecords(ctx, c.ID, c.State); err != nil {
		return err
	}
	return e.writeReplicaRecords(ctx, c, groups)
}

// putRecord puts c's record on the storage named, naming scans, inside the
// storage's write gate (gated): a record put on a place a switch swapped
// away would be on the old store only.
func (e *Engine) putRecord(ctx context.Context, c tenant.Correction, scans []tenant.CorrectionScan, storage string) error {
	if c.Kind == record.KindEdit && len(c.Edits) == 0 {
		// A correction read with a list carries no edits; its record names
		// them.
		edits, err := e.editsOf(ctx, c)
		if err != nil {
			return err
		}
		c.Edits = edits
	}
	b, err := record.EncodeCorrection(recordOf(c, scans, storage), e.cfg.TenantID, e.cfg.Keys)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "a correction record cannot be made")
	}
	key, err := recordOf(c, nil, "").Key()
	if err != nil {
		return fault.Wrap(fault.Internal, err, "a correction record's key cannot be made")
	}
	return e.gated(ctx, storage, func(st blob.Store) error {
		_, err := st.Put(ctx, key, bytes.NewReader(b))
		return err
	})
}

// settle rewrites c's records after a step whose row is committed; a
// failure is logged, not answered, since the row is the authority and a
// start rewrites what lags (PendingCorrections).
func (e *Engine) settle(ctx context.Context, c tenant.Correction) {
	if err := e.writeRecords(ctx, c); err != nil {
		e.cfg.Log.Error("a correction record could not be rewritten; it is rewritten at the next start", "correction", c.ID,
			"state", string(c.State), "error", fault.Message(err))
	}
}

// readAll reads a whole object of at most limit bytes; NotFound when it is
// not there.
func readAll(ctx context.Context, st blob.Store, key string, limit int64) ([]byte, error) {
	rc, err := st.Read(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fault.Wrap(fault.Unavailable, err, "an object cannot be read")
	}
	if int64(len(b)) > limit {
		return nil, fault.New(fault.Unavailable, "an object is larger than its form allows")
	}
	return b, nil
}

// hashOf reads key whole and returns its size and SHA-256.
func hashOf(ctx context.Context, st blob.Store, key string) (int64, [sha256.Size]byte, error) {
	rc, err := st.Read(ctx, key, 0, -1)
	if err != nil {
		return 0, [sha256.Size]byte{}, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	if err != nil {
		return 0, sum, fault.Wrap(fault.Unavailable, err, "an object cannot be read")
	}
	return n, sum, nil
}

// sha256Text returns a checksum as a record and a manifest write it.
func sha256Text(s [sha256.Size]byte) string { return hex.EncodeToString(s[:]) }

// shaOf parses 64 lowercase hex digits; the zero sum for anything else.
func shaOf(s string) tenant.SHA256 {
	var out tenant.SHA256
	b, err := hex.DecodeString(s)
	if err == nil && len(b) == len(out) {
		copy(out[:], b)
	}
	return out
}

// serverAudit returns the server's audit entry for a step it took itself.
func (e *Engine) serverAudit(method, id string, now time.Time) *master.AuditEntry {
	return &master.AuditEntry{ID: "srv" + id + fmt.Sprintf("%x", now.UnixMicro()&0xffffff), Time: now, LocalUser: "server",
		Method: method, TenantName: e.cfg.Tenant, Object: id, Outcome: master.OutcomeDone}
}
