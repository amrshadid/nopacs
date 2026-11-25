package backup

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reconcile of an installation whose databases a site restored by its
// own means (a Postgres point-in-time recovery, a copy of the SQLite files,
// a volume snapshot): the same reconcile a restore runs after it loads a
// backup (reconcile), without the load.
//
// The recovery point is what the restored databases hold: there is no
// backup file to say it, and none is needed, since everything the
// reconcile decides is decided per row against what the storages say.
// A correction record newer than the point is one whose row is missing or
// behind it, and tenant.Restoring.AdoptCorrection reads each record against
// its row by the same rule a restore does (record.Correction.Alone for a
// record with no row); the catalog is rebuilt from the storages, each
// record applied as its row then says (rebuild.ByRow), so it cannot be
// older than the bytes; every key is suspended, since a key revoked after
// the point is not revoked in the copy; and the rebuild's activation begins
// a new event history, so a cursor read before the point is refused rather
// than skipping events the copy lost.

// reconcileMethod is the method of the audit entries a reconcile of a
// site's own copy writes.
const reconcileMethod = "nopacs backup restore --reconcile-only"

// ReconcileMarkerName is the file a reconcile of a site's own copy writes
// in the data directory's run directory before it changes anything, and
// removes once it has ended: a data directory that holds it is one a
// reconcile was interrupted in, which the server refuses to start on, since
// its catalog or its owner records may be half taken. It is not the
// restore's marker (MarkerName), whose advice is to empty the data
// directory: here that would destroy the site's only copy.
const ReconcileMarkerName = "reconciling"

// ReconcileMarkerPath returns the path of the reconcile's marker in dataDir.
func ReconcileMarkerPath(dataDir string) string {
	return filepath.Join(server.RunDir(dataDir), ReconcileMarkerName)
}

// ReconcileInterrupted reports whether a reconcile of a site's own copy was
// interrupted in dataDir: its marker is there.
func ReconcileInterrupted(dataDir string) bool {
	_, err := os.Lstat(ReconcileMarkerPath(dataDir))
	return err == nil
}

// ReconcileInterruptedAdvice is what to do with a data directory a
// reconcile was interrupted in. Running it again is safe: each step reads
// what the last one left (an owner record taken, a record adopted, keys
// already suspended) and the catalog is rebuilt whole again.
const ReconcileInterruptedAdvice = "a reconcile of the restored databases was interrupted: run nopacs-server backup restore --reconcile-only " +
	"again on this data directory (with --take-over if it says a storage's owner record is ahead)"

// CheckNotInterrupted refuses a data directory a restore or a reconcile of
// a site's own copy was interrupted in, with what to do about it. Every
// command that runs on the data directory with the server stopped asks it,
// and asks it again once it holds the directory's lock, since a run
// interrupted between the first look and the lock leaves its marker: a
// server would serve, a backup copy and an import write into, databases
// loaded or reconciled part way.
func CheckNotInterrupted(dir string) error {
	// A restore leaves its marker until the installation's id is written;
	// its advice is to empty the data directory and restore again.
	if Interrupted(dir) {
		return fault.New(fault.Invalid, "the data directory "+dir+": "+InterruptedAdvice)
	}
	// A reconcile leaves its own marker until it ends: the catalog or the
	// owner records may be half taken, and the advice is to run it again,
	// never to empty anything, which would destroy the site's only copy.
	if ReconcileInterrupted(dir) {
		return fault.New(fault.Invalid, "the data directory "+dir+": "+ReconcileInterruptedAdvice)
	}
	return nil
}

// ReconcileConfig is what ReconcileOnly needs.
type ReconcileConfig struct {
	// Keys is the installation key file, which must open the master's key
	// check.
	Keys *secret.Keys
	// DataDir is the installation's data directory: its key file, and its
	// SQLite databases when they are there, in place.
	DataDir string
	// MasterAddress is the master database's address: SQLite in DataDir, or
	// the Postgres database the site restored.
	MasterAddress string
	// Scratch is the Postgres schema the catalog of a tenant on Postgres is
	// rebuilt in; required when a tenant is on Postgres.
	Scratch string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log takes the rebuilds' lines; nil discards them.
	Log *slog.Logger
	// RebuildTimeout bounds each tenant's rebuild; 0 is DefaultRebuildTimeout.
	RebuildTimeout time.Duration
	// TakeOver lets the reconcile write a storage's owner record over one
	// that names another installation or tenant, or a generation past the
	// restored database's (RestoreConfig.TakeOver). Without it, a tenant a
	// storage of which another installation's record names is left fenced
	// and not reconciled.
	TakeOver bool
	// IncomingRoot is the root of the tenants' incoming areas, in place:
	// a quarantined file held whose bytes are there whole is kept
	// (keptQuarantine). Empty is DefaultIncomingDir in DataDir.
	IncomingRoot string
}

// DefaultIncomingDir is the incoming areas' directory in a data directory,
// as the server keeps them.
const DefaultIncomingDir = "incoming"

// ReconcileOnly reconciles an installation whose databases a site restored
// by its own point-in-time recovery with its storages, with no server
// running: it takes the data directory's lock, refuses a master that is
// empty, from another baseline schema (its migration refuses it), holds no
// installation id, or whose key check the key file does not open, migrates
// each database, reads each one's recovery point, what it lost and every
// storage's owner record (claimsOf), then suspends every key and
// reconciles each tenant exactly as Restore does after its load
// (reconcile). A tenant a storage of which another installation's owner
// record names is not reconciled but left fenced in the master, its
// storages and its rows as they were, and named in the report; the others
// are reconciled. The id file is written when the data directory lacks it.
func ReconcileOnly(ctx context.Context, cfg ReconcileConfig) (Report, error) {
	if cfg.Keys == nil || cfg.DataDir == "" || cfg.MasterAddress == "" {
		return Report{}, fault.New(fault.Internal, "backup: a reconcile needs the key file, the data directory and the master's address")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.RebuildTimeout <= 0 {
		cfg.RebuildTimeout = DefaultRebuildTimeout
	}
	// A data directory a restore was interrupted in holds databases loaded
	// part way: reconciling them would serve a part of a backup.
	if Interrupted(cfg.DataDir) {
		return Report{}, fault.New(fault.Conflict, "backup: in the data directory "+cfg.DataDir+", "+InterruptedAdvice)
	}
	if _, err := os.Stat(cfg.DataDir); err != nil {
		return Report{}, fault.Wrap(fault.Invalid, err, "backup: the data directory "+cfg.DataDir+" cannot be read; a reconcile runs on the installation in place")
	}
	lock, err := server.TakeLock(cfg.DataDir)
	if fault.KindOf(err) == fault.Conflict {
		return Report{}, prefixed(err, "backup: the data directory "+cfg.DataDir+" is locked by another process, a server or a restore: stop it first; ")
	}
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = lock.Release() }()
	if cfg.IncomingRoot == "" {
		cfg.IncomingRoot = filepath.Join(cfg.DataDir, DefaultIncomingDir)
	}
	rcfg := RestoreConfig{Keys: cfg.Keys, DataDir: cfg.DataDir, MasterAddress: cfg.MasterAddress, Scratch: cfg.Scratch, Now: cfg.Now,
		Log: cfg.Log, RebuildTimeout: cfg.RebuildTimeout, TakeOver: cfg.TakeOver, reconcileOnly: true, incoming: cfg.IncomingRoot}
	l, id, err := openInPlace(ctx, rcfg)
	if l != nil {
		defer l.close()
	}
	if err != nil {
		return Report{}, err
	}
	rep := Report{Installation: id, ReconcileOnly: true}
	if err := pointsOf(ctx, l, &rep); err != nil {
		return rep, err
	}
	// Every owner record is read and judged before anything is written: a
	// tenant whose storage another installation writes now is left to it,
	// and any other refusal stops the reconcile before a key is suspended
	// or a storage taken.
	others, err := claimsOf(ctx, rcfg, l.master, id, l.tenants)
	if err != nil {
		return rep, err
	}
	// Written before anything changes, removed once all has: a server does
	// not start on a reconcile left half way.
	if err := os.WriteFile(ReconcileMarkerPath(cfg.DataDir), []byte("a reconcile of the restored databases writes this data directory\n"), 0o600); err != nil {
		return rep, fault.Wrap(fault.Internal, err, "backup: the reconcile's marker cannot be written")
	}
	now := cfg.Now().UTC().Truncate(time.Microsecond)
	if rep.KeysSuspended, err = suspendKeys(ctx, l.master, reconcileMethod, reconcileUser, now); err != nil {
		return rep, err
	}
	for i, t := range l.tenants {
		if others[i] != "" {
			rep.Tenants[i].Fenced = others[i]
			if err := fenceTenant(ctx, rcfg, l.master, t.row, others[i], now); err != nil {
				return rep, prefixed(err, "backup: the tenant "+t.row.Name+" cannot be left fenced: ")
			}
			continue
		}
		res, err := reconcile(ctx, rcfg, l.master, id, t, now)
		res.Loss = rep.Tenants[i].Loss
		rep.Tenants[i] = res
		if err != nil {
			return rep, prefixed(err, "backup: the tenant "+t.row.Name+" cannot be reconciled with its storages: ")
		}
	}
	if err := appendRestoreAudit(ctx, l.master, rep, now); err != nil {
		return rep, err
	}
	l.close()
	if _, err := server.ReadInstallationID(cfg.DataDir); fault.KindOf(err) == fault.NotFound {
		if err := server.WriteInstallationID(cfg.DataDir, id); err != nil {
			return rep, err
		}
	}
	if err := os.Remove(ReconcileMarkerPath(cfg.DataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return rep, fault.Wrap(fault.Internal, err, "backup: the reconcile's marker cannot be removed; remove "+ReconcileMarkerPath(cfg.DataDir)+
			" before the server is started")
	}
	return rep, nil
}

// openInPlace opens the master at cfg.MasterAddress and every tenant it
// lists, each migrated, and returns the installation's id. Before it
// migrates anything, so a mistyped address is never written, it refuses an
// empty master, a master with no installation id or whose key check
// cfg.Keys does not open, and a data directory whose id file names another
// installation; then a database made from another baseline schema
// (Migrate refuses it).
func openInPlace(ctx context.Context, cfg RestoreConfig) (*loaded, string, error) {
	l := &loaded{}
	m, err := driver.OpenMaster(ctx, cfg.MasterAddress)
	if err != nil {
		return l, "", err
	}
	l.master = m
	if err := refuseEmpty(ctx, m, "the master database"); err != nil {
		return l, "", err
	}
	id, err := m.Setting(ctx, server.InstallationIDSetting)
	if fault.KindOf(err) == fault.NotFound {
		return l, "", fault.New(fault.Invalid, "backup: the master database holds no installation id: it is not an installation's master; "+
			"check the address (--master-env)")
	}
	if err != nil {
		return l, "", err
	}
	if err := checkIdentity(ctx, m, cfg.Keys, id); err != nil {
		return l, "", err
	}
	switch got, err := server.ReadInstallationID(cfg.DataDir); {
	case err == nil && got != id:
		return l, "", fault.New(fault.Invalid, "backup: the master database belongs to another installation than the data directory "+cfg.DataDir+
			"; check the address (--master-env) and the data directory")
	case err != nil && fault.KindOf(err) != fault.NotFound:
		return l, "", err
	}
	if err := migrateRestored(ctx, m, "the master database"); err != nil {
		return l, "", err
	}
	// Every tenant the master lists, as a backup reads them: a tenant whose
	// database the site did not restore fails the reconcile, which is of
	// the whole installation or nothing.
	dbs, closeAll, err := OpenTenants(ctx, m, cfg.Keys, cfg.DataDir)
	if err != nil {
		return l, "", err
	}
	for _, d := range dbs {
		st, ok := d.DB.(tenant.Store)
		row, err := m.Tenant(ctx, d.TenantID)
		if err != nil || !ok {
			closeAll()
			l.tenants = nil
			if err == nil {
				err = fault.New(fault.Internal, "backup: this database type cannot be reconciled")
			}
			return l, "", err
		}
		// From here l closes each store it holds; closeAll is not called.
		l.tenants = append(l.tenants, openedTenant{row: row, store: st})
	}
	for _, t := range l.tenants {
		if err := migrateRestored(ctx, t.store, "the tenant "+t.row.Name+"'s database"); err != nil {
			return l, "", err
		}
	}
	return l, id, nil
}

// migratable is a database whose schema version is read and brought to
// this binary's.
type migratable interface {
	versioned
	Migrate(ctx context.Context) error
}

// refuseEmpty refuses an empty database, which a site's recovery did not
// restore (or an address mistyped), reading only its schema version.
func refuseEmpty(ctx context.Context, db versioned, name string) error {
	v, err := db.Version(ctx)
	if err != nil {
		return err
	}
	if v == 0 {
		return fault.New(fault.Invalid, "backup: "+name+" is empty: a reconcile runs on databases a site restored, and never fills one; "+
			"check the address and that the recovery restored it")
	}
	return nil
}

// migrateRestored refuses an empty database (refuseEmpty) and migrates any
// other to this binary's schema; the migration refuses one made from
// another baseline schema, or whose applied migrations differ from this
// binary's.
func migrateRestored(ctx context.Context, db migratable, name string) error {
	if err := refuseEmpty(ctx, db, name); err != nil {
		return err
	}
	if err := db.Migrate(ctx); err != nil {
		return prefixed(err, "backup: "+name+" cannot be brought to this version's schema: ")
	}
	return nil
}

// pointsOf reads each restored database once, as a backup would read it
// (snapshot.Database.Snapshot), writing nothing, and puts in rep its
// section, with the recovery point the database's own rows give (newest
// correction, newest reception, event history, owner generations), and,
// per tenant, what it holds as not finished, which the reconcile cannot
// bring back (lossCounter, the rule a restore counts a backup's rows by).
func pointsOf(ctx context.Context, l *loaded, rep *Report) error {
	masterSchema, tenantSchema, err := driver.SchemaFingerprints()
	if err != nil {
		return err
	}
	read := func(db any, name string, loss *lossCounter, schema string) error {
		sdb, ok := db.(snapshot.Database)
		if !ok {
			return fault.New(fault.Internal, "backup: this database type cannot be reconciled")
		}
		c := &countingSink{loss: loss}
		p, err := sdb.Snapshot(ctx, c)
		if err != nil {
			return prefixed(err, "backup: ")
		}
		rep.Sections = append(rep.Sections, Section{Tenant: name, Tables: c.tables, Rows: c.rows, Point: pointOf(p, schema, 0)})
		return nil
	}
	if err := read(l.master, "", nil, masterSchema); err != nil {
		return err
	}
	for _, t := range l.tenants {
		loss := &lossCounter{byReceiver: map[string]*ReceptionLoss{}}
		if err := read(t.store, t.row.Name, loss, tenantSchema); err != nil {
			return err
		}
		rep.Tenants = append(rep.Tenants, TenantResult{Tenant: t.row.Name, Loss: loss.result(t.row.Name)})
	}
	return nil
}

// countingSink is the snapshot.Sink pointsOf reads a database into: it
// counts tables and rows and passes a tenant's rows to its loss counter,
// keeping nothing else.
type countingSink struct {
	loss   *lossCounter
	table  string
	cols   []snapshot.Column
	tables int
	rows   int64
}

// Table implements snapshot.Sink.
func (c *countingSink) Table(t snapshot.Table) error {
	c.table, c.cols = t.Name, t.Columns
	c.tables++
	return nil
}

// Row implements snapshot.Sink.
func (c *countingSink) Row(values []any) error {
	c.rows++
	if c.loss != nil {
		c.loss.row(c.table, c.cols, values)
	}
	return nil
}

// Sequences implements snapshot.Sink.
func (c *countingSink) Sequences(map[string]int64) error { return nil }
