package backup

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ImportMethod is the method of the audit entries an import writes.
const ImportMethod = "nopacs tenant import"

// importUser is the local user of an import's audit entries.
const importUser = "import"

// ImportConfig is what ImportTenant needs.
type ImportConfig struct {
	// File is the export's path. It is read twice: once whole to check it,
	// then to load it.
	File string
	// TransferKey is the export's transfer key, 32 bytes.
	TransferKey []byte
	// Keys is this installation's key file: every sealed value is sealed
	// again with it, and the owner and correction records are
	// authenticated with it.
	Keys *secret.Keys
	// Installation is this installation's id.
	Installation string
	// Master is this installation's master database, open and migrated.
	Master master.Store
	// DataDir is this installation's data directory, where a tenant on
	// SQLite gets its file.
	DataDir string
	// Driver is the database type the tenant is loaded into, SQLite unless
	// it is master.PostgresDriver; Address is the address of an empty
	// Postgres schema for it.
	Driver, Address string
	// Scratch is the Postgres schema the catalog of a tenant on Postgres is
	// rebuilt in.
	Scratch string
	// TakeOver lets the import write a storage's owner record over one that
	// names another installation than the exporting one, or a later
	// generation than the export holds.
	TakeOver bool
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log takes the rebuild's lines; nil discards them.
	Log *slog.Logger
	// RebuildTimeout bounds the rebuild; 0 is DefaultRebuildTimeout.
	RebuildTimeout time.Duration
}

// ImportReport is what an import did.
type ImportReport struct {
	// Tenant and TenantID name the tenant loaded.
	Tenant, TenantID string
	// From is the exporting installation's id; Exported when it exported.
	From     string
	Exported time.Time
	// Section is the tenant's section, with its recovery point.
	Section Section
	// Result is the reconcile with the storages: the owner generations
	// taken, the correction records written again under this
	// installation's key, the catalog rebuilt.
	Result TenantResult
	// Rewritten counts the correction records written again under this
	// installation's key.
	Rewritten int
}

// ImportTenant loads a tenant's export into this installation, with the
// server stopped (the caller holds the data directory's lock): it checks the
// whole file under the transfer key before writing anything; loads the
// tenant's database, each sealed value sealed again with this
// installation's key file; takes each storage over with an owner record
// one generation past the export's, before the tenant opens, so the
// exporting installation, started again against the storages, fences the
// tenant; writes every correction record again under this installation's
// key, as its row says (each row came in the authenticated file, and
// nothing was written after the export's snapshot); rebuilds the catalog
// from the storages, which begins a new event history; and registers the
// tenant, ready, last, with its audit entry. A tenant of the same name or
// id refuses the import.
func ImportTenant(ctx context.Context, cfg ImportConfig) (ImportReport, error) {
	var rep ImportReport
	if cfg.Keys == nil || cfg.Master == nil || cfg.File == "" || cfg.DataDir == "" || cfg.Installation == "" {
		return rep, fault.New(fault.Internal, "backup: an import needs the file, the key file, the master, the data directory and the installation's id")
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
	// The caller holds the lock, so no restore or reconcile can begin now.
	if err := CheckNotInterrupted(cfg.DataDir); err != nil {
		return rep, err
	}
	tk, err := secret.TransferKeys(cfg.TransferKey)
	if err != nil {
		return rep, err
	}
	f, err := os.Open(cfg.File) // #nosec G304 -- the file a person named on the command line.
	if err != nil {
		return rep, fault.Wrap(fault.Invalid, err, "backup: the export "+cfg.File+" cannot be opened")
	}
	defer func() { _ = f.Close() }()
	v, err := Verify(f, tk)
	if err != nil {
		return rep, err
	}
	if v.Header.Format != ExportFormat || len(v.Starts) != 1 {
		return rep, fault.New(fault.Invalid, "backup: "+cfg.File+" is not a tenant's export; a backup is restored with nopacs-server backup restore")
	}
	start, end := v.Starts[0], v.Ends[0]
	rep.Tenant, rep.TenantID, rep.From, rep.Exported = start.Tenant, start.TenantID, v.Header.Installation, v.Header.Time
	rep.Section = Section{Tenant: start.Tenant, Tables: v.Sums[0].Tables, Rows: v.Sums[0].Rows, SHA256: v.Sums[0].SHA256, Point: end.Point}
	if err := v.CheckVersions(); err != nil {
		return rep, err
	}
	if v.Header.Installation == cfg.Installation {
		return rep, fault.New(fault.Invalid, "backup: the export was made by this installation; a tenant handed off is taken back with nopacs tenant reclaim")
	}
	if err := checkNoTenant(ctx, cfg.Master, start); err != nil {
		return rep, err
	}
	// Nothing was written before this point.
	row := master.Tenant{ID: start.TenantID, Name: start.Tenant, Driver: master.SQLiteDriver, State: master.StateReady}
	if cfg.Driver == master.PostgresDriver {
		row.Driver = master.PostgresDriver
	}
	st, err := createTenantDB(ctx, cfg, row)
	if err != nil {
		return rep, err
	}
	defer func() { _ = st.Close() }()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return rep, fault.Wrap(fault.Invalid, err, "backup: the export cannot be read again")
	}
	if err := loadExport(ctx, f, tk, cfg.Keys, st, v); err != nil {
		return rep, fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+row.Name+"'s database cannot be loaded: "+fault.Message(err)+
			"; remove what the import made of it before running it again"+leftAt(cfg, row))
	}
	now := cfg.Now().UTC().Truncate(time.Microsecond)
	res, rewritten, err := reconcileImport(ctx, cfg, row, st, v.Header.Installation, now)
	rep.Result, rep.Rewritten = res, rewritten
	if err != nil {
		return rep, fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+row.Name+" cannot be reconciled with its storages: "+fault.Message(err)+
			"; remove what the import made of it before running it again"+leftAt(cfg, row))
	}
	if err := register(ctx, cfg, row, rep, now); err != nil {
		return rep, err
	}
	return rep, nil
}

// leftAt names where the import left the tenant's database, for a person to
// remove before an import is run again.
func leftAt(cfg ImportConfig, row master.Tenant) string {
	if row.Driver == master.SQLiteDriver {
		if p, err := (driver.SQLiteTenants{DataDir: cfg.DataDir}).Path(row.Name); err == nil {
			return " (" + p + ")"
		}
	}
	return " (the Postgres schema given)"
}

// checkNoTenant refuses an import whose tenant's name or id this
// installation already holds.
func checkNoTenant(ctx context.Context, m master.Store, s SectionStart) error {
	if _, err := m.TenantByName(ctx, s.Tenant); err == nil {
		return fault.New(fault.Conflict, "backup: this installation already has a tenant named "+s.Tenant)
	} else if fault.KindOf(err) != fault.NotFound {
		return err
	}
	if _, err := m.Tenant(ctx, s.TenantID); err == nil {
		return fault.New(fault.Conflict, "backup: this installation already has the tenant "+s.TenantID+" under another name")
	} else if fault.KindOf(err) != fault.NotFound {
		return err
	}
	return nil
}

// createTenantDB opens the new tenant's database, which must not exist yet
// (SQLite) or be empty (Postgres).
func createTenantDB(ctx context.Context, cfg ImportConfig, row master.Tenant) (tenant.Store, error) {
	if row.Driver == master.PostgresDriver {
		if cfg.Address == "" {
			return nil, fault.New(fault.Invalid, "backup: a tenant imported into Postgres needs the address of an empty schema")
		}
		return driver.PostgresTenants{}.Open(ctx, row.ID, row.Name, cfg.Address)
	}
	if cfg.Address != "" {
		return nil, fault.New(fault.Invalid, "backup: a tenant on SQLite takes no database address; its file is made in the data directory")
	}
	d := driver.SQLiteTenants{DataDir: cfg.DataDir}
	path, err := d.Path(row.Name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fault.New(fault.Conflict, "backup: "+path+" exists: an import was interrupted, or a tenant of that name was removed and its file left; "+
			"move it away before importing")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fault.Wrap(fault.Internal, err, "backup: "+path+" cannot be checked")
	}
	if err := server.EnsurePrivateDir(cfg.DataDir + string(os.PathSeparator) + driver.TenantsDir); err != nil {
		return nil, err
	}
	return d.Open(ctx, row.ID, row.Name, "")
}

// loadExport reads the export a second time from f, at its start, and loads
// its section into st, each sealed value sealed again from the transfer key
// to the key file and every correction marked as having no records written
// under this installation's key yet; then migrates it.
func loadExport(ctx context.Context, f io.Reader, tk, keys *secret.Keys, st tenant.Store, v Verified) error {
	br, err := secret.NewBackupReader(f, tk)
	if err != nil {
		return err
	}
	rd := newReader(br)
	if _, err := rd.readHeader(); err != nil {
		return err
	}
	r := &resealer{tenantID: v.Starts[0].TenantID, from: tk, to: keys, clearColumns: map[string]string{"corrections": "records_state"}}
	if err := loadSectionVia(ctx, rd, st, v, 0, func(src snapshot.Source) snapshot.Source { return resealSource{Source: src, r: r} }); err != nil {
		return err
	}
	if _, more, err := rd.nextSection(); err != nil || more {
		if err == nil {
			err = badLayout("it holds more sections than it did")
		}
		return err
	}
	return st.Migrate(ctx)
}

// reconcileImport takes the tenant's storages over from the exporting
// installation, writes every correction record again under this
// installation's key, and rebuilds the catalog, as a restore's reconcile
// does (reconcile).
func reconcileImport(ctx context.Context, cfg ImportConfig, row master.Tenant, st tenant.Store, from string, now time.Time) (TenantResult, int, error) {
	res := TenantResult{Tenant: row.Name}
	p, ok := st.(tenant.Pipeline)
	rs, ok2 := st.(tenant.Restoring)
	cs, ok3 := st.(tenant.Corrections)
	if !ok || !ok2 || !ok3 {
		return res, 0, fault.New(fault.Internal, "backup: this database type cannot be reconciled")
	}
	var err error
	if res.Credentials, err = missingCredentials(ctx, p, cfg.DataDir, row.Name); err != nil {
		return res, 0, err
	}
	if res.QuarantineMarked, err = rs.MarkQuarantineLost(ctx, nil); err != nil {
		return res, 0, err
	}
	opener := place.Opener{Values: settingvalue.Reader{Master: cfg.Master}, Keys: cfg.Keys, StaleUploadAge: -1}
	pt := place.TenantOf(row.ID, row.Name, st)
	held, err := rs.StorageOwners(ctx)
	if err != nil {
		return res, 0, err
	}
	generations := map[string]int64{}
	for _, o := range held {
		generations[o.Storage] = o.Generation
	}
	storages, err := listStorages(ctx, p)
	if err != nil {
		return res, 0, err
	}
	open := map[string]blob.Store{}
	defer func() {
		for _, s := range open {
			_ = s.Close()
		}
	}()
	for _, s := range storages {
		bs, err := opener.Open(ctx, pt, s)
		if err != nil {
			return res, 0, fault.Wrap(fault.KindOf(err), err, "the storage "+s.Name+" cannot be opened (this installation must list its root or endpoint): "+
				fault.Message(err))
		}
		open[s.Name] = bs
		gen, err := takeOwner(ctx, bs, ownerClaim{installation: cfg.Installation, tenantID: row.ID, storage: s.Name, held: generations[s.Name],
			takeOver: cfg.TakeOver, keys: cfg.Keys, from: from, what: "the export",
			again: "run the import again with --take-over, so this installation owns it"}, now)
		if err != nil {
			return res, 0, err
		}
		if err := rs.SetStorageOwner(ctx, s.Name, gen, now); err != nil {
			return res, 0, err
		}
		res.Owners = append(res.Owners, Owner{Storage: s.Name, Generation: gen})
	}
	// The export's database may hold a rebuild its server was working, which
	// would refuse the import's own (endRebuilds).
	if err := endRebuilds(ctx, rs, p, now); err != nil {
		return res, 0, err
	}
	rewritten, err := rewriteRecords(ctx, cfg, row, cs, open)
	if err != nil {
		return res, rewritten, err
	}
	// As a restore's reconcile does: the export's rows were written by the
	// exporting installation, which may itself have made a delete from its
	// record.
	if _, err := rs.HoldEarlierSubjectLabels(ctx); err != nil {
		return res, rewritten, err
	}
	rcfg := RestoreConfig{Keys: cfg.Keys, Scratch: cfg.Scratch, Log: cfg.Log, RebuildTimeout: cfg.RebuildTimeout, method: ImportMethod, localUser: importUser}
	job, tallies, err := rebuildCatalog(ctx, rcfg, cfg.Master, openedTenant{row: row, store: st}, p, opener, now)
	res.Rebuild, res.Tallies = job, tallies
	if err != nil {
		return res, rewritten, err
	}
	res.Relinked, err = rs.RelinkHidden(ctx)
	return res, rewritten, err
}

// rewriteRecords writes every correction's records again, as its row says,
// under this installation's key: the exporting installation's MACs do not
// verify here, and a rebuild applies no record that does not. The engine's
// own start does it (correct.Engine.Recover rewrites each correction whose
// records lag its row, which the load made every one), so a record is
// written by the one code that writes them.
func rewriteRecords(ctx context.Context, cfg ImportConfig, row master.Tenant, cs tenant.Corrections, open map[string]blob.Store) (int, error) {
	pending, err := countPending(ctx, cs)
	if err != nil || pending == 0 {
		return 0, err
	}
	e, err := correct.New(correct.Config{Tenant: row.Name, TenantID: row.ID, Keys: cfg.Keys, Store: cs, Now: cfg.Now, Log: cfg.Log,
		OpenStorage: func(_ context.Context, name string) (blob.Store, error) {
			if s, ok := open[name]; ok {
				return s, nil
			}
			return nil, fault.New(fault.NotFound, "the tenant has no storage "+name)
		}})
	if err != nil {
		return 0, err
	}
	if err := e.Recover(ctx); err != nil {
		return 0, fault.Wrap(fault.KindOf(err), err, "the correction records cannot be written again: "+fault.Message(err))
	}
	left, err := countPending(ctx, cs)
	if err != nil {
		return 0, err
	}
	if left != 0 {
		return pending - left, fault.New(fault.Unavailable, strconv.Itoa(left)+" corrections' records could not be written again")
	}
	return pending, nil
}

// countPending counts the corrections whose records lag their rows.
func countPending(ctx context.Context, cs tenant.Corrections) (int, error) {
	n, after := 0, ""
	for {
		page, err := cs.PendingCorrections(ctx, after, 1000)
		if err != nil {
			return 0, err
		}
		for _, c := range page {
			if c.RecordsState != c.State {
				n++
			}
			after = c.ID
		}
		if len(page) < 1000 {
			return n, nil
		}
	}
}

// register writes the tenant's row in the master, ready, with the import's
// audit entry: last, so a tenant the server opens was taken over whole.
func register(ctx context.Context, cfg ImportConfig, row master.Tenant, rep ImportReport, now time.Time) error {
	sealed, err := cfg.Keys.Seal(registry.AddressContext(row.ID), []byte(cfg.Address))
	if err != nil {
		return fault.Wrap(fault.Internal, err, "backup: the tenant's database address cannot be sealed")
	}
	row.SealedAddress, row.Created = sealed, now
	var owners []string
	for _, o := range rep.Result.Owners {
		owners = append(owners, o.Storage+"="+strconv.FormatInt(o.Generation, 10))
	}
	e := master.AuditEntry{ID: server.NewID(), Time: now, LocalUser: importUser, Method: ImportMethod, TenantName: row.Name,
		Object: "tenant/" + row.Name, Outcome: master.OutcomeDone,
		Reason: "the tenant imported from installation " + rep.From + ", exported at " + rep.Exported.Format(time.RFC3339)}
	e = e.WithDetails("from_installation", rep.From, "exported", rep.Exported.Format(time.RFC3339Nano), "section_sha256", rep.Section.SHA256,
		"owner_generations", strings.Join(owners, ","), "records_rewritten", strconv.Itoa(rep.Rewritten))
	return cfg.Master.CreateTenant(ctx, row, e, master.LocalOperator())
}
