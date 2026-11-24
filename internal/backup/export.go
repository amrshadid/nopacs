package backup

import (
	"context"
	"crypto/sha256"
	"io"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A tenant's export is the plaintext of a backup (doc.go) with
// "format":"nopacs-tenant-export" in its header and one section, the
// tenant's, encrypted with the backup cipher under a transfer key made for
// the export (secret.NewTransferKey) rather than one derived from the key
// file. Every sealed value in it is opened with the exporting
// installation's key file and sealed again under the transfer key, with
// the same context, so the two key files never meet on one machine: the
// import opens each with the transfer key and seals it with its own. The
// tenant keeps its id across the move, so each value's context, and the
// correction records' and owner records' binding to the tenant, stay
// what they were.

// sealedColumn is a column of a tenant's database that holds a value
// sealed with the installation key: the column of the row's name, the
// sealed column, and the context the value is sealed under.
type sealedColumn struct {
	name, sealed string
	context      func(tenantID, name string) string
}

// sealedColumns are, by table, the sealed values a tenant's database holds:
// its own secrets (the naming, UID and date-shift keys), an S3 storage's
// secret access key, and a stow sender's credential.
var sealedColumns = map[string]sealedColumn{
	"tenant_secrets": {name: "name", sealed: "sealed", context: func(id, name string) string {
		return registry.SecretContext(id, tenant.SecretName(name))
	}},
	"storages": {name: "name", sealed: "s3_secret", context: place.SecretContext},
	"senders":  {name: "name", sealed: "credential", context: place.CredentialContext},
}

// resealer opens each sealed value of a table as it passes, with from, and
// seals it again under to, with the same context. A value that does not
// open fails it: a value nobody can open on the other side is not carried
// as if it were whole.
type resealer struct {
	tenantID string
	from, to *secret.Keys
	// table is the current table's name; nameAt and sealedAt the positions
	// of its sealed column's two columns, -1 for a table with none.
	table            string
	nameAt, sealedAt int
	// clear is a column to empty on its way, -1 for none (records_state,
	// on an import: correctionsReset).
	clearAt int
	// clearColumns are, by table, the column an import empties.
	clearColumns map[string]string
}

// begin takes the next table's columns.
func (r *resealer) begin(t snapshot.Table) {
	r.table, r.nameAt, r.sealedAt, r.clearAt = t.Name, -1, -1, -1
	sc, ok := sealedColumns[t.Name]
	clearCol := r.clearColumns[t.Name]
	for i, c := range t.Columns {
		switch {
		case ok && c.Name == sc.name:
			r.nameAt = i
		case ok && c.Name == sc.sealed:
			r.sealedAt = i
		case clearCol != "" && c.Name == clearCol:
			r.clearAt = i
		}
	}
	if r.nameAt < 0 {
		r.sealedAt = -1
	}
}

// row reseals the current table's sealed value in values, and empties the
// column to clear.
func (r *resealer) row(values []any) error {
	if r.clearAt >= 0 {
		values[r.clearAt] = ""
	}
	if r.sealedAt < 0 || values[r.sealedAt] == nil {
		return nil
	}
	sealed, ok := values[r.sealedAt].([]byte)
	name, ok2 := values[r.nameAt].(string)
	if !ok || !ok2 {
		return fault.New(fault.Invalid, "backup: a sealed value of "+r.table+" is not of its column's kind")
	}
	context := sealedColumns[r.table].context(r.tenantID, name)
	plain, err := r.from.Open(context, sealed)
	if err != nil {
		return fault.Wrap(fault.Invalid, err, "backup: a sealed value of "+r.table+" ("+name+") does not open; it cannot be carried")
	}
	again, err := r.to.Seal(context, plain)
	clear(plain)
	if err != nil {
		return err
	}
	values[r.sealedAt] = again
	return nil
}

// resealSink is the snapshot.Sink of an export: each row resealed on its
// way to the file.
type resealSink struct {
	snapshot.Sink
	r *resealer
}

// Table implements snapshot.Sink.
func (s resealSink) Table(t snapshot.Table) error {
	s.r.begin(t)
	return s.Sink.Table(t)
}

// Row implements snapshot.Sink.
func (s resealSink) Row(values []any) error {
	if err := s.r.row(values); err != nil {
		return err
	}
	return s.Sink.Row(values)
}

// resealSource is the snapshot.Source of an import: each row resealed on
// its way to the database.
type resealSource struct {
	snapshot.Source
	r *resealer
}

// NextTable implements snapshot.Source.
func (s resealSource) NextTable() (snapshot.Table, bool, error) {
	t, ok, err := s.Source.NextTable()
	if ok && err == nil {
		s.r.begin(t)
	}
	return t, ok, err
}

// NextRow implements snapshot.Source.
func (s resealSource) NextRow(values []any) (bool, error) {
	ok, err := s.Source.NextRow(values)
	if !ok || err != nil {
		return ok, err
	}
	return true, s.r.row(values)
}

// ExportConfig is what ExportTenant needs.
type ExportConfig struct {
	// Installation is the exporting installation's id.
	Installation string
	// Keys is its key file, which opens the tenant's sealed values.
	Keys *secret.Keys
	// Tenant, TenantID and Driver name the tenant and its database type.
	Tenant, TenantID, Driver string
	// DB reads the tenant's database.
	DB snapshot.Database
	// Store is the same database as the tenant's port, whose owner
	// generations Owners takes further.
	Store tenant.Store
	// Owners takes every storage's owner record one generation further
	// before the snapshot (OwnerCheck.Advance).
	Owners OwnerAdvancer
	// Out receives the encrypted file.
	Out io.Writer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// OwnerAdvancer takes every storage's owner record of a tenant one
// generation further (OwnerCheck.Advance).
type OwnerAdvancer interface {
	// Advance writes each storage's owner record, and the generation the
	// tenant's database holds for it, one generation further, and returns
	// the generations written.
	Advance(ctx context.Context, id, name string, st tenant.Store) ([]Owner, error)
}

// ExportTenant writes the tenant's export to cfg.Out, its database read in
// one read transaction without the catalog, every sealed value sealed again
// under a new transfer key and the whole encrypted under it, and returns
// what it holds with the transfer key, which the caller hands on once and
// clears. What it wrote before a failure is no export and opens to
// nothing: the cipher's last chunk is written only by a whole one.
//
// Before the snapshot every storage's owner record is taken one generation
// further (cfg.Owners), so the export holds the newest generation of each:
// a reclaim takes them further again, and an import of this export after
// it meets a later generation than the export holds and is refused, rather
// than bring back on the other installation what the source changed since.
func ExportTenant(ctx context.Context, cfg ExportConfig) (Summary, []byte, error) {
	if cfg.Keys == nil || cfg.DB == nil || cfg.Store == nil || cfg.Owners == nil || cfg.Out == nil || cfg.Installation == "" || cfg.Tenant == "" || cfg.TenantID == "" {
		return Summary{}, nil, fault.New(fault.Internal, "backup: an export needs the key file, the tenant's database and owner records, its name and id, "+
			"the installation's id and a place to write")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if _, err := cfg.Owners.Advance(ctx, cfg.TenantID, cfg.Tenant, cfg.Store); err != nil {
		return Summary{}, nil, fault.Wrap(fault.KindOf(err), err, "the owner records of the tenant "+cfg.Tenant+" cannot be taken further: "+fault.Message(err))
	}
	key, tk, err := secret.NewTransferKey()
	if err != nil {
		return Summary{}, nil, err
	}
	sum, err := exportUnder(ctx, cfg, tk)
	if err != nil {
		clear(key)
		return Summary{}, nil, err
	}
	return sum, key, nil
}

// exportUnder writes the export under the transfer key tk.
func exportUnder(ctx context.Context, cfg ExportConfig, tk *secret.Keys) (Summary, error) {
	now := cfg.Now().UTC().Truncate(time.Microsecond)
	out := &countingWriter{w: cfg.Out, h: sha256.New()}
	bw, err := secret.NewBackupWriter(out, tk)
	if err != nil {
		return Summary{}, err
	}
	pw := newWriter(bw)
	sum := Summary{Installation: cfg.Installation, Time: now}
	if err := pw.header(Header{Format: ExportFormat, Installation: cfg.Installation, Time: now, Sections: 1}); err != nil {
		return Summary{}, err
	}
	if err := pw.begin(SectionStart{Index: 0, Database: DatabaseTenant, Tenant: cfg.Tenant, TenantID: cfg.TenantID, Driver: cfg.Driver}); err != nil {
		return Summary{}, err
	}
	p, err := cfg.DB.Snapshot(ctx, resealSink{Sink: pw, r: &resealer{tenantID: cfg.TenantID, from: cfg.Keys, to: tk}})
	if err != nil {
		return Summary{}, fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+cfg.Tenant+"'s database cannot be read: "+fault.Message(err))
	}
	point := pointOf(p, int64(cfg.Keys.SealVersion()))
	f, err := pw.end(point)
	if err != nil {
		return Summary{}, err
	}
	sum.Sections = append(sum.Sections, Section{Tenant: cfg.Tenant, Tables: f.Tables, Rows: f.Rows, SHA256: f.SHA256, Point: point})
	if err := pw.footer(); err != nil {
		return Summary{}, err
	}
	if err := bw.Close(); err != nil {
		return Summary{}, err
	}
	sum.Size, sum.SHA256 = out.n, out.h.Sum(nil)
	return sum, nil
}
