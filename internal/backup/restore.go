package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// RestoreConfig is what Restore needs.
type RestoreConfig struct {
	// File is the backup's path. It is read twice: once whole to check it,
	// then to load it.
	File string
	// Keys is the installation key file the backup was made with.
	Keys *secret.Keys
	// KeyFile is that key file's path: copied into the data directory when
	// it is not there already.
	KeyFile string
	// DataDir is the data directory to restore into: absent, empty, or
	// holding only the key file.
	DataDir string
	// MasterAddress is the master database's address: SQLite in DataDir, or
	// an empty Postgres schema.
	MasterAddress string
	// Scratch is the Postgres schema the catalog of a tenant on Postgres is
	// rebuilt in (tenant.RebuildJob); required when a tenant is on
	// Postgres.
	Scratch string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log takes the rebuilds' lines; nil discards them.
	Log *slog.Logger
	// RebuildTimeout bounds each tenant's rebuild; 0 is DefaultRebuildTimeout.
	RebuildTimeout time.Duration
	// TakeOver lets the restore write a storage's owner record over one
	// that names another installation or tenant, or a generation past the
	// backup's; without it such a storage fails the restore, since someone
	// took it over after the backup.
	TakeOver bool

	// method and localUser name the audit entries the reconcile writes:
	// an import's; "" for a restore's.
	method, localUser string
	// catalogFirst are the deletes whose data the rebuild catalogs, for
	// the reconcile to hide after it (tenant.Restoring.HideDeletes).
	catalogFirst []string
}

// DefaultRebuildTimeout bounds one tenant's rebuild at restore.
const DefaultRebuildTimeout = 24 * time.Hour

// keyFileName is the key file's name in a data directory, as init writes it.
const keyFileName = "key"

// Report is what a restore did and what it could not bring back.
type Report struct {
	// Installation is the restored installation's id.
	Installation string
	// BackupTime is when the backup was made.
	BackupTime time.Time
	// Sections are the databases restored, the master first.
	Sections []Section
	// KeysSuspended counts the keys suspended.
	KeysSuspended int64
	// Tenants are each tenant's reconcile, in order of name.
	Tenants []TenantResult
}

// TenantResult is one tenant's reconcile.
type TenantResult struct {
	// Tenant is its name.
	Tenant string
	// QuarantineMarked counts the quarantined files held whose bytes were
	// kept, marked incoming lost (tenant.Restoring.MarkQuarantineLost).
	QuarantineMarked int64
	// Owners are the owner generations taken, by storage.
	Owners []Owner
	// Added, Moved, Disagreed and Scans count the correction records
	// adopted (tenant.Adopted); Records counts every record read.
	Added, Moved, Disagreed, Scans, Records int
	// Held counts the deletes the backup holds committed that the restore
	// kept, each now unresolved until an admin confirms it
	// (tenant.Restoring.HoldCommittedDeletes).
	Held int64
	// Disagreements are the ids of the first records that disagree with the
	// backup's rows (at most maxDisagreements).
	Disagreements []string
	// Unauthenticated counts the records whose MAC is missing or does not
	// verify under the key file for the tenant: each was treated as if it
	// were not there, neither adopted nor applied by the rebuild. It is
	// counted in Records.
	Unauthenticated int
	// UnauthenticatedIDs are the operation ids, by their keys, of the first
	// of them (at most maxDisagreements).
	UnauthenticatedIDs []string
	// Rebuild is the rebuild job that made the catalog again, and Tallies
	// what it counted.
	Rebuild tenant.Job
	Tallies []tenant.JobTally
	// Relinked counts the rows a delete in its grace period hid that were
	// pointed at their parents' new ids (tenant.Restoring.RelinkHidden).
	Relinked int64
	// Loss is what could not be brought back.
	Loss TenantLoss
	// Credentials are the TLS files its receivers and senders name that the
	// data directory restored into lacks: a backup holds none of them.
	Credentials []Credential
}

// Restore restores the backup in cfg.File into an empty data directory, with
// no server running (package doc): it checks the whole file first, then
// loads every database, suspends every key, reconciles each tenant with its
// storages, and writes the installation's id file last.
func Restore(ctx context.Context, cfg RestoreConfig) (Report, error) {
	if cfg.Keys == nil || cfg.File == "" || cfg.DataDir == "" || cfg.MasterAddress == "" {
		return Report{}, fault.New(fault.Internal, "backup: a restore needs the file, the key file, the data directory and the master's address")
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
	if err := checkEmptyDir(cfg.DataDir, cfg.KeyFile); err != nil {
		return Report{}, err
	}
	// The file stays open from the check to the load: the load reads the
	// same open file again, not whatever the path names by then.
	f, err := os.Open(cfg.File) // #nosec G304 -- the file a person named on the command line.
	if err != nil {
		return Report{}, fault.Wrap(fault.Invalid, err, "backup: the backup "+cfg.File+" cannot be opened")
	}
	defer func() { _ = f.Close() }()
	v, err := Verify(f, cfg.Keys)
	if err != nil {
		return Report{}, err
	}
	if v.Header.Format != Format {
		return Report{}, fault.New(fault.Invalid, "backup: "+cfg.File+" is a tenant's export, not a backup; load it with nopacs-server tenant import")
	}
	if err := v.CheckVersions(); err != nil {
		return Report{}, err
	}
	// Nothing was written before this point.
	rep := Report{Installation: v.Header.Installation, BackupTime: v.Header.Time}
	for i, s := range v.Sums {
		rep.Sections = append(rep.Sections, Section{Tenant: s.Tenant, Tables: s.Tables, Rows: s.Rows, SHA256: s.SHA256, Point: v.Ends[i].Point})
	}
	lock, err := lockDir(cfg.DataDir, cfg.KeyFile)
	if err != nil {
		return rep, err
	}
	defer func() { _ = lock.Release() }()
	if err := prepareDir(cfg.DataDir, cfg.KeyFile); err != nil {
		return rep, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return rep, fault.Wrap(fault.Invalid, err, "backup: the backup cannot be read again")
	}
	l, err := load(ctx, cfg, f, v)
	if l != nil {
		defer l.close()
	}
	if err != nil {
		return rep, err
	}
	now := cfg.Now().UTC().Truncate(time.Microsecond)
	if rep.KeysSuspended, err = suspendKeys(ctx, l.master, v, now); err != nil {
		return rep, err
	}
	lost := map[string]TenantLoss{}
	for _, x := range v.Lost {
		lost[x.Tenant] = x
	}
	for _, t := range l.tenants {
		res, err := reconcile(ctx, cfg, l.master, v.Header.Installation, t, now)
		res.Loss = lost[t.row.Name]
		rep.Tenants = append(rep.Tenants, res)
		if err != nil {
			return rep, fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+t.row.Name+" cannot be reconciled with its storages: "+fault.Message(err))
		}
	}
	if err := appendRestoreAudit(ctx, l.master, rep, now); err != nil {
		return rep, err
	}
	l.close()
	// Last: the server starts only on a data directory with its id, so a
	// restore stopped before here leaves one the server refuses, and the
	// marker, removed only once the id is written, says why.
	if err := server.WriteInstallationID(cfg.DataDir, v.Header.Installation); err != nil {
		return rep, err
	}
	if err := os.Remove(MarkerPath(cfg.DataDir)); err != nil {
		return rep, fault.Wrap(fault.Internal, err, "backup: the restore's marker cannot be removed; remove "+MarkerPath(cfg.DataDir)+
			" before the server is started")
	}
	return rep, nil
}

// MarkerName is the file a restore writes in the data directory's run
// directory before it writes anything else, and removes only once it has
// written the installation's id: a data directory that holds it is one a
// restore was interrupted in.
const MarkerName = "restoring"

// MarkerPath returns the path of the restore's marker in dataDir.
func MarkerPath(dataDir string) string {
	return filepath.Join(server.RunDir(dataDir), MarkerName)
}

// Interrupted reports whether a restore was interrupted in dataDir: its
// marker is there.
func Interrupted(dataDir string) bool {
	_, err := os.Lstat(MarkerPath(dataDir))
	return err == nil
}

// InterruptedAdvice is what to do with a data directory a restore was
// interrupted in.
const InterruptedAdvice = "a restore was interrupted: remove the data directory's contents except the key file (and empty the Postgres " +
	"schemas it filled, the master's and the tenants'), then run restore again"

// checkEmptyDir refuses a data directory that holds anything but the key
// file a restore is given, and a run directory holding only the lock file
// (which a refused start, or this restore's own lock, leaves). A directory
// a restore was interrupted in is refused with what to do.
func checkEmptyDir(dir, keyFile string) error {
	if Interrupted(dir) {
		return fault.New(fault.Conflict, "backup: in the data directory "+dir+", "+InterruptedAdvice)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fault.Wrap(fault.Invalid, err, "backup: the data directory "+dir+" cannot be read")
	}
	inside := keyFile != "" && filepath.Clean(keyFile) == filepath.Join(dir, keyFileName)
	for _, e := range entries {
		if inside && e.Name() == keyFileName {
			continue
		}
		if e.Name() == filepath.Base(server.RunDir(dir)) && onlyLock(server.RunDir(dir)) {
			continue
		}
		return fault.New(fault.Conflict, "backup: the data directory "+dir+" is not empty ("+e.Name()+" is there); a restore writes only into an empty one")
	}
	return nil
}

// onlyLock reports whether the run directory holds the lock file and
// nothing else.
func onlyLock(run string) bool {
	entries, err := os.ReadDir(run)
	return err == nil && len(entries) == 1 && entries[0].Name() == server.LockName
}

// CheckVersions refuses a backup with a section made from another baseline
// schema than this binary's, or of a schema version this binary does not
// migrate to; a restore and an import ask it before anything is written,
// so such a file is refused whole rather than after its master is loaded.
// The baseline is compared first: a schema version counts migrations after
// one baseline, so the same number after another baseline names another
// schema, and a file written before sections recorded their baseline
// (Schema empty) was made by an earlier development build.
func (v Verified) CheckVersions() error {
	masterVersion, tenantVersion, err := driver.SchemaVersions()
	if err != nil {
		return err
	}
	masterSchema, tenantSchema, err := driver.SchemaFingerprints()
	if err != nil {
		return err
	}
	for i, s := range v.Starts {
		known, schema := tenantVersion, tenantSchema
		name := "the section of tenant " + output.Clean(s.Tenant)
		if s.Database == DatabaseMaster {
			known, schema, name = masterVersion, masterSchema, "the master's section"
		}
		p := v.Ends[i].Point
		if p.Schema != schema {
			return fault.New(fault.Invalid, "backup: "+name+" was made by an earlier development build of nopacs, from another baseline "+
				"schema than this version's; it cannot be restored or imported here: restore it with the build that made it, or make it again")
		}
		if got := p.SchemaVersion; got < 1 || got > known {
			return fault.New(fault.Invalid, "backup: "+name+" is of schema version "+strconv.Itoa(got)+", and this version of nopacs knows "+
				"versions 1 to "+strconv.Itoa(known)+"; restore it with the version of nopacs that made it, or a later one")
		}
	}
	return nil
}

// lockDir makes the data directory, private, takes its lock (the one a
// server takes, so no server and no second restore runs on it), checks
// again that it is empty, now that no other restore can be writing it, and
// writes the restore's marker before anything else.
func lockDir(dir, keyFile string) (*server.Lock, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil { //nolint:gosec // G301: the parent of the data directory is the operator's; the data directory itself is private.
		return nil, fault.Wrap(fault.Internal, err, "backup: the parent of "+dir+" cannot be made")
	}
	if err := server.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	lock, err := server.TakeLock(dir)
	if fault.KindOf(err) == fault.Conflict {
		return nil, fault.Wrap(fault.Conflict, err, "backup: the data directory "+dir+" is locked by another process, a server or another "+
			"restore: "+fault.Message(err))
	}
	if err != nil {
		return nil, err
	}
	if err := checkEmptyDir(dir, keyFile); err != nil {
		_ = lock.Release()
		return nil, err
	}
	if err := os.WriteFile(MarkerPath(dir), []byte("a restore writes this data directory\n"), 0o600); err != nil {
		_ = lock.Release()
		return nil, fault.Wrap(fault.Internal, err, "backup: the restore's marker cannot be written")
	}
	return lock, nil
}

// prepareDir makes the data directory's tenants and run directories,
// private, and copies the key file into it when it is elsewhere.
func prepareDir(dir, keyFile string) error {
	for _, sub := range []string{driver.TenantsDir, "run"} {
		if err := server.EnsurePrivateDir(filepath.Join(dir, sub)); err != nil {
			return err
		}
	}
	dst := filepath.Join(dir, keyFileName)
	if keyFile == "" || filepath.Clean(keyFile) == dst {
		return nil
	}
	f, err := secret.OpenPrivate(keyFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var b bytes.Buffer
	if _, err := b.ReadFrom(f); err != nil {
		return fault.Wrap(fault.Invalid, err, "backup: the key file cannot be read")
	}
	if err := os.WriteFile(dst, b.Bytes(), 0o600); err != nil {
		return fault.Wrap(fault.Internal, err, "backup: the key file cannot be copied into the data directory")
	}
	return nil
}

// openedTenant is a tenant's database the restore loaded.
type openedTenant struct {
	row   master.Tenant
	store tenant.Store
}

// loaded is what the second pass opened.
type loaded struct {
	master  master.Store
	tenants []openedTenant
}

// close closes every database opened, once.
func (l *loaded) close() {
	for _, t := range l.tenants {
		_ = t.store.Close()
	}
	l.tenants = nil
	if l.master != nil {
		_ = l.master.Close()
		l.master = nil
	}
}

// load reads the backup a second time from f, at its start, and loads each
// section into its database, each at its own schema version, then migrates
// it. Each section read must have the digest the first pass found before
// its load commits, so a file changed between the two passes stops the
// restore before a changed section is written.
func load(ctx context.Context, cfg RestoreConfig, f io.Reader, v Verified) (*loaded, error) {
	br, err := secret.NewBackupReader(f, cfg.Keys)
	if err != nil {
		return nil, err
	}
	rd := newReader(br)
	if _, err := rd.readHeader(); err != nil {
		return nil, err
	}
	l := &loaded{}
	m, err := driver.OpenMaster(ctx, cfg.MasterAddress)
	if err != nil {
		return l, err
	}
	l.master = m
	if err := loadSection(ctx, rd, m, v, 0); err != nil {
		return l, err
	}
	if err := m.Migrate(ctx); err != nil {
		return l, err
	}
	if err := checkIdentity(ctx, m, cfg.Keys, v.Header.Installation); err != nil {
		return l, err
	}
	for i := 1; i < len(v.Starts); i++ {
		s := v.Starts[i]
		row, err := m.Tenant(ctx, s.TenantID)
		if err != nil {
			return l, fault.Wrap(fault.Invalid, err, "backup: the master holds no tenant "+s.Tenant+" for its section")
		}
		if row.Name != s.Tenant {
			return l, fault.New(fault.Invalid, "backup: the section of "+s.Tenant+" names a tenant the master calls "+row.Name)
		}
		st, err := OpenTenant(ctx, cfg.Keys, cfg.DataDir, row)
		if err != nil {
			return l, err
		}
		l.tenants = append(l.tenants, openedTenant{row: row, store: st})
		if err := loadSection(ctx, rd, st, v, i); err != nil {
			return l, fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+s.Tenant+"'s database cannot be loaded: "+fault.Message(err))
		}
		if err := st.Migrate(ctx); err != nil {
			return l, err
		}
	}
	if _, more, err := rd.nextSection(); err != nil || more {
		if err == nil {
			err = badLayout("it holds more sections than it did")
		}
		return l, err
	}
	return l, nil
}

// versioned is a database that reports its schema version.
type versioned interface {
	Version(ctx context.Context) (int, error)
}

// loadSection loads section i from rd into db, which must be empty.
func loadSection(ctx context.Context, rd *reader, db versioned, v Verified, i int) error {
	return loadSectionVia(ctx, rd, db, v, i, nil)
}

// loadSectionVia is loadSection with each row passing through wrap on its
// way to the load (an import's resealing), unless wrap is nil.
func loadSectionVia(ctx context.Context, rd *reader, db versioned, v Verified, i int, wrap func(snapshot.Source) snapshot.Source) error {
	sdb, ok := db.(snapshot.Database)
	if !ok {
		return fault.New(fault.Internal, "backup: this database type cannot be restored into")
	}
	got, err := db.Version(ctx)
	if err != nil {
		return err
	}
	if got != 0 {
		return fault.New(fault.Conflict, "backup: the database to restore into is not empty (it is at schema version "+strconv.Itoa(got)+
			"); a restore writes only into an empty one")
	}
	start, ok, err := rd.nextSection()
	if err != nil {
		return err
	}
	if !ok || start != v.Starts[i] {
		return fault.New(fault.Invalid, "backup: the file changed between its check and its load")
	}
	version := v.Ends[i].Point.SchemaVersion
	if err := sdb.MigrateTo(ctx, version); err != nil {
		return err
	}
	src := &checkedSection{reader: rd, want: v.Sums[i]}
	var load snapshot.Source = src
	if wrap != nil {
		load = wrap(src)
	}
	if err := sdb.Load(ctx, version, load); err != nil {
		return err
	}
	if !src.checked {
		return fault.New(fault.Internal, "backup: a section was loaded without its digest checked")
	}
	return nil
}

// checkedSection is the snapshot.Source of one section that, when the load
// asks for the sequences (the last thing it reads before it commits), reads
// the section's end and compares its digest with the first pass's: a
// section that differs fails the load, which then commits nothing.
type checkedSection struct {
	*reader
	want    FooterSection
	checked bool
}

// Sequences implements snapshot.Source.
func (c *checkedSection) Sequences() (map[string]int64, error) {
	seqs, err := c.reader.Sequences()
	if err != nil {
		return nil, err
	}
	_, sum, err := c.endSection()
	if err != nil {
		return nil, err
	}
	if sum != c.want {
		return nil, fault.New(fault.Invalid, "backup: the file changed between its check and its load")
	}
	c.checked = true
	return seqs, nil
}

// checkIdentity refuses a master that is not the installation the header
// names, or whose key check the key file does not open.
func checkIdentity(ctx context.Context, m master.Store, keys *secret.Keys, installation string) error {
	id, err := m.Setting(ctx, server.InstallationIDSetting)
	if err != nil || id != installation {
		return fault.New(fault.Invalid, "backup: the master database in the backup is not the installation its header names")
	}
	check, err := m.Setting(ctx, server.KeyCheckSetting)
	if err != nil {
		return fault.New(fault.Invalid, "backup: the master database in the backup has no key check")
	}
	return server.CheckKeyFile(keys, id, check)
}

// OpenTenant opens a tenant's database, of the type and at the address its
// row in the master holds (a SQLite file in dataDir's tenants directory),
// with no registry: for a backup made with the server stopped, and a
// restore. It does not migrate.
func OpenTenant(ctx context.Context, keys *secret.Keys, dataDir string, row master.Tenant) (tenant.Store, error) {
	plain, err := keys.Open(registry.AddressContext(row.ID), row.SealedAddress)
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "backup: the tenant "+row.Name+"'s database address does not open with this key file")
	}
	address := string(plain)
	clear(plain)
	switch row.Driver {
	case master.SQLiteDriver:
		return driver.SQLiteTenants{DataDir: dataDir}.Open(ctx, row.ID, row.Name, address)
	case master.PostgresDriver:
		return driver.PostgresTenants{}.Open(ctx, row.ID, row.Name, address)
	}
	return nil, fault.New(fault.Invalid, "backup: the tenant "+row.Name+" is on a database type this server does not know")
}

// restoreMethod is the method of the audit entries a restore writes.
const restoreMethod = "nopacs backup restore"

// suspendKeys suspends every key the master holds.
func suspendKeys(ctx context.Context, m master.Store, v Verified, now time.Time) (int64, error) {
	ks, ok := m.(master.KeySuspension)
	if !ok {
		return 0, fault.New(fault.Internal, "backup: this database type cannot suspend keys")
	}
	return ks.SuspendKeys(ctx, now, master.AuditEntry{ID: server.NewID(), Time: now, LocalUser: "restore", Method: restoreMethod,
		Object: "keys", Outcome: master.OutcomeDone,
		Reason: "every key a restore brings back is suspended, since a key revoked after the backup would work again; the local operator rotates each key to be used again"})
}

// appendRestoreAudit writes the restore's own entry, with what it lost, in
// the master's log.
func appendRestoreAudit(ctx context.Context, m master.Store, rep Report, now time.Time) error {
	e := master.AuditEntry{ID: server.NewID(), Time: now, LocalUser: "restore", Method: restoreMethod, Object: rep.Installation,
		Outcome: master.OutcomeDone, Reason: "the installation restored from a backup made at " + rep.BackupTime.Format(time.RFC3339)}
	pairs := []string{"backup_time", rep.BackupTime.Format(time.RFC3339Nano), "keys_suspended", strconv.FormatInt(rep.KeysSuspended, 10)}
	for _, t := range rep.Tenants {
		var files, receptions int64
		for _, r := range t.Loss.Receptions {
			files += r.Files
			receptions += r.Receptions
		}
		pairs = append(pairs, "lost_receptions."+t.Tenant, strconv.FormatInt(receptions, 10),
			"lost_files."+t.Tenant, strconv.FormatInt(files, 10),
			"lost_quarantine_files."+t.Tenant, strconv.FormatInt(t.Loss.QuarantineFiles, 10))
	}
	return m.AppendAudit(ctx, e.WithDetails(pairs...))
}
