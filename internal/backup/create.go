package backup

import (
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"time"

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

// Create writes a backup of the master and every tenant's database to
// cfg.Out, each read in its own read transaction, encrypted as it is
// written, so memory stays bounded whatever the databases hold. A database
// that fails fails the backup: a backup is of the whole installation or
// nothing. What Create wrote before a failure is no backup and opens to
// nothing: the cipher's last chunk is written only by a whole one.
func Create(ctx context.Context, cfg CreateConfig) (Summary, error) {
	if cfg.Keys == nil || cfg.Master == nil || cfg.Out == nil || cfg.Installation == "" {
		return Summary{}, fault.New(fault.Internal, "backup: a backup needs the key file, the master database, the installation's id and a place to write")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	now := cfg.Now().UTC().Truncate(time.Microsecond)
	keyVersion := int64(cfg.Keys.SealVersion())
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
		p, err := d.DB.Snapshot(ctx, pw)
		if err != nil {
			if d.Tenant != "" {
				return Summary{}, fault.Wrap(fault.KindOf(err), err, "backup: the tenant "+d.Tenant+"'s database cannot be read: "+fault.Message(err))
			}
			return Summary{}, err
		}
		point := pointOf(p, keyVersion)
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
