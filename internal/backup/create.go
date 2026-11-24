package backup

import (
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Database is one database a backup reads: the master, or a tenant's.
type Database struct {
	// Tenant and TenantID name a tenant's database; empty for the master.
	Tenant, TenantID string
	// Driver is the tenant's database type; empty for the master.
	Driver string
	// DB reads it.
	DB snapshot.Database
}

// CreateConfig is what Create needs.
type CreateConfig struct {
	// Installation is the installation's id.
	Installation string
	// Keys is the installation key file; the backup is encrypted under the
	// key derived from its highest version.
	Keys *secret.Keys
	// Master is the master database.
	Master snapshot.Database
	// Tenants are every tenant's database, in order of name.
	Tenants []Database
	// Out receives the encrypted file.
	Out io.Writer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Section is one database in a backup, as a summary or a restore reports it.
type Section struct {
	// Tenant is the tenant's name; empty for the master.
	Tenant string
	// Tables and Rows are what it holds.
	Tables int
	Rows   int64
	// SHA256 is the digest of its plaintext, in hex.
	SHA256 string
	// Point is its recovery point.
	Point Point
}

// Summary is what a backup holds and how to check it.
type Summary struct {
	// Installation is the id of the installation that made it.
	Installation string
	// Time is when it was made.
	Time time.Time
	// Size and SHA256 are the encrypted file's, as written.
	Size   int64
	SHA256 []byte
	// KeyVersion is the version of the installation key it is encrypted
	// under.
	KeyVersion int64
	// Sections are its databases, the master first.
	Sections []Section
}

// OpenTenants opens the database of every tenant the master lists, in order
// of name (OpenTenant), for a backup made with the server stopped; close
// closes them. A tenant whose database does not open fails it: a backup is
// of the whole installation or nothing.
func OpenTenants(ctx context.Context, m master.Store, keys *secret.Keys, dataDir string) (dbs []Database, closeAll func(), err error) {
	var opened []tenant.Store
	closeAll = func() {
		for _, s := range opened {
			_ = s.Close()
		}
		opened = nil
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()
	cursor := ""
	for {
		rows, next, err := m.ListTenants(ctx, master.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, closeAll, err
		}
		for _, row := range rows {
			st, err := OpenTenant(ctx, keys, dataDir, row)
			if err != nil {
				return nil, closeAll, fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+row.Name+"'s database cannot be opened: "+fault.Message(err))
			}
			opened = append(opened, st)
			sdb, ok := st.(snapshot.Database)
			if !ok {
				return nil, closeAll, fault.New(fault.Internal, "backup: the tenant "+row.Name+"'s database type cannot be backed up")
			}
			dbs = append(dbs, Database{Tenant: row.Name, TenantID: row.ID, Driver: row.Driver, DB: sdb})
		}
		if next == "" {
			return dbs, closeAll, nil
		}
		cursor = next
	}
}

// countingWriter counts and digests what is written through it.
type countingWriter struct {
	w io.Writer
	n int64
	h hash.Hash
}

// Write implements io.Writer.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.h.Write(p[:n])
	return n, err
}

// checkSchemas refuses the first of dbs whose recorded migrations are not
// this binary's (snapshot.Database's CheckSchema), naming it.
func checkSchemas(ctx context.Context, dbs []Database) error {
	for _, d := range dbs {
		if d.DB == nil {
			continue
		}
		if err := d.DB.CheckSchema(ctx); err != nil {
			if d.Tenant != "" {
				return fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+d.Tenant+"'s database cannot be backed up: "+fault.Message(err))
			}
			return fault.Wrap(fault.KindOf(err), err, "backup: the master database cannot be backed up: "+fault.Message(err))
		}
	}
	return nil
}

// masterTenants passes the master's section to its Sink and keeps the id of
// every row of its tenants table, so the backup can check that it holds a
// section for each tenant the master's section lists, and none other.
type masterTenants struct {
	snapshot.Sink
	// in is set while the rows are the tenants table's; col is its id
	// column, -1 when it has none.
	in  bool
	col int
	ids map[string]bool
}

// Table implements snapshot.Sink.
func (m *masterTenants) Table(t snapshot.Table) error {
	m.in, m.col = t.Name == "tenants", -1
	if m.in {
		for i, c := range t.Columns {
			if c.Name == "id" {
				m.col = i
			}
		}
	}
	return m.Sink.Table(t)
}

// Row implements snapshot.Sink.
func (m *masterTenants) Row(values []any) error {
	if m.in && m.col >= 0 && m.col < len(values) {
		if id, ok := values[m.col].(string); ok {
			m.ids[id] = true
		}
	}
	return m.Sink.Row(values)
}

// match refuses a master's section whose tenants are not exactly the
// tenants given. With the server stopped they always are; with it running,
// a tenant created or removed between the listing of the tenants and the
// master's read transaction would leave a backup whose master names a
// tenant with no section (restored as a tenant with an empty database) or
// a section the master does not name (which a restore refuses). Either is
// answered unavailable: the same backup asked again a moment later
// succeeds.
func (m *masterTenants) match(tenants []Database) error {
	if m.col < 0 && len(m.ids) == 0 && len(tenants) > 0 {
		return fault.New(fault.Internal, "backup: the master's section has no tenants table with an id")
	}
	given := make(map[string]bool, len(tenants))
	for _, d := range tenants {
		given[d.TenantID] = true
	}
	if len(given) != len(m.ids) {
		return tenantsChanged
	}
	for id := range m.ids {
		if !given[id] {
			return tenantsChanged
		}
	}
	return nil
}

// tenantsChanged is the refusal of a backup whose master section lists
// other tenants than the backup was given.
var tenantsChanged = fault.New(fault.Unavailable, "backup: a tenant was created or removed while the backup was being taken; take it again")

// Create writes a backup of the master and every tenant's database to
// cfg.Out, each read in its own read transaction, encrypted as it is
// written, so memory stays bounded whatever the databases hold. A database
// that fails fails the backup: a backup is of the whole installation or
// nothing. What Create wrote before a failure is no backup and opens to
// nothing: the cipher's last chunk is written only by a whole one.
//
// The same function makes the backup with the server stopped and over the
// API while it runs. Running, each database is read as of its own moment,
// the master first and then each tenant in turn, so the sections stand for
// moments a few seconds apart; nothing a restore needs depends on their
// being one. Every reference between databases is a tenant's id in the
// master, which match checks against the sections. Each tenant is
// reconciled on its own: its correction records newer than its own recovery
// point are applied, its catalog rebuilt from the storages as they are at
// the restore, and what it lost after its own point reported. Every key the
// master holds is suspended by a restore, so a key changed between the
// master's moment and a tenant's grants nothing. The master is read first
// so that the anchors it holds of each tenant's sealed audit log are never
// ahead of that log as its section holds it.
func Create(ctx context.Context, cfg CreateConfig) (Summary, error) {
	if cfg.Keys == nil || cfg.Master == nil || cfg.Out == nil || cfg.Installation == "" {
		return Summary{}, fault.New(fault.Internal, "backup: a backup needs the key file, the master database, the installation's id and a place to write")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	now := cfg.Now().UTC().Truncate(time.Microsecond)
	keyVersion := int64(cfg.Keys.SealVersion())
	masterSchema, tenantSchema, err := driver.SchemaFingerprints()
	if err != nil {
		return Summary{}, err
	}
	// Each section is stamped with this binary's baseline, so every
	// database must hold this binary's schema: one made by another build
	// is refused before a byte is written.
	if err := checkSchemas(ctx, append([]Database{{DB: cfg.Master}}, cfg.Tenants...)); err != nil {
		return Summary{}, err
	}
	out := &countingWriter{w: cfg.Out, h: sha256.New()}
	bw, err := secret.NewBackupWriter(out, cfg.Keys)
	if err != nil {
		return Summary{}, err
	}
	pw := newWriter(bw)
	sum := Summary{Installation: cfg.Installation, Time: now, KeyVersion: keyVersion}
	if err := pw.header(Header{Installation: cfg.Installation, Time: now, KeyVersion: keyVersion, Sections: 1 + len(cfg.Tenants)}); err != nil {
		return Summary{}, err
	}
	dbs := append([]Database{{DB: cfg.Master}}, cfg.Tenants...)
	for i, d := range dbs {
		start := SectionStart{Index: i, Database: DatabaseMaster}
		if i > 0 {
			if d.Tenant == "" || d.TenantID == "" || d.DB == nil {
				return Summary{}, fault.New(fault.Internal, "backup: a tenant's database is given without its name, id or store")
			}
			start = SectionStart{Index: i, Database: DatabaseTenant, Tenant: d.Tenant, TenantID: d.TenantID, Driver: d.Driver}
		}
		if err := pw.begin(start); err != nil {
			return Summary{}, err
		}
		var sink snapshot.Sink = pw
		var listed *masterTenants
		if i == 0 {
			listed = &masterTenants{Sink: pw, ids: map[string]bool{}}
			sink = listed
		}
		p, err := d.DB.Snapshot(ctx, sink)
		if err == nil && listed != nil {
			err = listed.match(cfg.Tenants)
		}
		if err != nil {
			if d.Tenant != "" {
				return Summary{}, fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+d.Tenant+"'s database cannot be read: "+fault.Message(err))
			}
			return Summary{}, err
		}
		schema := masterSchema
		if i > 0 {
			schema = tenantSchema
		}
		point := pointOf(p, schema, keyVersion)
		f, err := pw.end(point)
		if err != nil {
			return Summary{}, err
		}
		sum.Sections = append(sum.Sections, Section{Tenant: d.Tenant, Tables: f.Tables, Rows: f.Rows, SHA256: f.SHA256, Point: point})
	}
	if err := pw.footer(); err != nil {
		return Summary{}, err
	}
	if err := bw.Close(); err != nil {
		return Summary{}, err
	}
	sum.Size, sum.SHA256 = out.n, out.h.Sum(nil)
	return sum, nil
}
