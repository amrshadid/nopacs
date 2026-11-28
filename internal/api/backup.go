package api

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// The details of a backup's audit entries.
const (
	// detailTenants is how many tenants the backup holds.
	detailTenants = "tenants"
	// detailTenantNames is their names, joined by commas, cut to what an
	// entry's detail holds (master.MaxAuditDetailValueLength) and then
	// ending in "...": every name is in the summary the client keeps.
	detailTenantNames = "tenant_names"
)

// CreateBackup streams an encrypted backup of the installation while the
// server runs (backup.proto). It is made by backup.Create, the one writer
// the command made with the server stopped uses too, from the same
// snapshot reads (snapshot.Database.Snapshot): the master's database, then
// each tenant's, each in its own read transaction, so a section stands for
// its own moment and the server keeps writing meanwhile (backup.Create says
// why sections a few seconds apart restore whole).
//
// Memory is bounded: the file is encrypted as it is written and sent in
// messages of at most 1 MiB, holding one at a time, and a Send waits on the
// client's flow control, so the reads wait on the client. A read
// transaction held open takes no lock and holds no writer back on either
// database: on SQLite readers and the writer go on beside it (write-ahead
// log); on Postgres a read-only transaction at repeatable read waits on
// nothing, and the event feed orders its writers by a lock of its own (the
// feed's row), never by the oldest open transaction, so events go on being
// written and read during the backup. What it costs while it is open is
// space: SQLite's log cannot be reset past it, and Postgres keeps every old
// row version of the whole cluster it may still read (vacuum is held back).
//
// So a backup is bounded twice. A client that reads nothing is cut by the
// server's write stall bound (server.StallTimeout, 5 minutes): the Send
// fails. A client that reads slowly but steadily passes that bound at each
// write, so the whole backup is bounded by backup.max-duration, read when
// it starts: past it the backup's context ends, its read is stopped at the
// next row or write, and the stream ends with resource exhausted naming the
// setting, without its summary, which the client must take as no backup. A
// key revoked meanwhile ends it within RecheckInterval (watchKey) in the
// same way; while a Send waits on a client that reads nothing, no byte
// leaves either, and the stall bound ends it.
//
// One backup or export runs at a time; another is answered resource
// exhausted. The audit's sealing pass is paused for the backup's length
// (its passes skipped, not waited on), so no database is read holding an
// anchor of a checkpoint that another database's section, read earlier,
// does not hold. The entry that opens the stream is written to the
// installation's log before anything is read, and a backup whose entry
// cannot be written is refused: it is the largest read the API answers. The
// entry that ends it is written as every other request's.
func (s backupService) CreateBackup(ctx context.Context, _ *v1.CreateBackupRequest, stream nopacsv1connect.BackupServiceCreateBackupServerStream) error {
	c := s.c
	// A backup holds every tenant's configuration and audit log: the key
	// is checked again before anything is read, as a change's is, and then
	// every RecheckInterval while the stream is open (watchKey).
	d, err := c.changing(ctx, nopacsv1connect.BackupServiceCreateBackupProcedure)
	if err != nil {
		return err
	}
	sm, ok := c.master.(snapshot.Database)
	if !ok {
		return c.fail(d, fault.New(fault.Internal, "api: the master database cannot be backed up"))
	}
	if err := c.exports.takeBackup(); err != nil {
		return c.fail(d, err)
	}
	defer c.exports.endBackup()
	limit := c.seams.backupLimit
	if limit == 0 {
		if limit, err = c.values().BackupMaxDuration(ctx); err != nil {
			return c.fail(d, err)
		}
	}
	ctx, endLimit := context.WithTimeoutCause(ctx, limit, errPastMaxDuration{limit})
	defer endLimit()
	if c.sealPause != nil {
		resume, err := c.sealPause(ctx)
		if err != nil {
			return c.failBackupEarly(ctx, d, err)
		}
		defer resume()
	}
	tenants, err := c.backupTenants(ctx)
	if err != nil {
		return c.failBackupEarly(ctx, d, err)
	}
	mdb := sm
	if wrap := c.seams.backupDatabase; wrap != nil {
		mdb = wrap("", mdb)
		for i := range tenants {
			tenants[i].DB = wrap(tenants[i].Tenant, tenants[i].DB)
		}
	}
	held := tenantDetails(tenants)
	opened := d.Entry(master.OutcomeDone, "", access.Facts{Details: withDetails(held, map[string]string{detailStream: "opened"})})
	if err := c.master.AppendAudit(ctx, opened); err != nil {
		var past errPastMaxDuration
		if errors.As(context.Cause(ctx), &past) {
			return c.failBackupEarly(ctx, d, err)
		}
		c.log.Error("api: the entry that opens a backup could not be written; the backup was refused",
			"request_id", opened.RequestID, "method", opened.Method, "error", err.Error())
		return c.toAPI(d, fault.New(fault.Unavailable, "the installation's audit log cannot be written now; no backup was made"))
	}

	ctx, stop := c.watchKey(ctx, d)
	defer stop()
	w := &chunkWriter{send: func(b []byte) error {
		return stream.Send(&v1.CreateBackupResponse{Kind: &v1.CreateBackupResponse_Data{Data: b}})
	}}
	sum, err := backup.Create(ctx, backup.CreateConfig{Installation: c.installation, Keys: c.keys, Master: mdb, Tenants: tenants, Out: w, Now: c.now})
	if err == nil {
		err = w.flush()
	}
	if err == nil {
		if err = stream.Send(&v1.CreateBackupResponse{Kind: &v1.CreateBackupResponse_Summary{Summary: summaryMessage(backup.Format, sum)}}); err != nil {
			err = fault.Wrap(fault.Unavailable, err, "the summary could not be sent")
		}
	}
	end := func(how string, more map[string]string) access.Facts {
		return access.Facts{Details: withDetails(withDetails(held, more), map[string]string{detailStream: "ended", detailEnded: how})}
	}
	if refusal, ok := refusedWhileOpen(ctx); ok {
		// Confirm recorded the refusal.
		return c.toAPI(d, refusal)
	}
	var past errPastMaxDuration
	switch {
	case err == nil:
		c.record(d, nil, master.OutcomeDone, "", end("complete", map[string]string{
			detailSizeBytes: strconv.FormatInt(sum.Size, 10), detailSHA256: hex.EncodeToString(sum.SHA256)}))
		return nil
	case errors.As(context.Cause(ctx), &past):
		bound := fault.New(fault.Exhausted, past.Error())
		c.record(d, nil, master.OutcomeFailed, fault.Message(bound), end("max_duration", nil))
		return c.toAPI(d, bound)
	case ctx.Err() != nil:
		c.record(d, nil, master.OutcomeFailed, "the stream ended before its summary: the client went away or the server stopped; it is no backup",
			end("canceled", nil))
		return c.toAPI(d, fault.Wrap(fault.Unavailable, ctx.Err(), "the backup ended before its summary"))
	}
	return c.failWith(d, nil, end(strings.ReplaceAll(fault.KindOf(err).String(), " ", "_"), nil), err)
}

// failBackupEarly ends a backup that failed before its stream opened. The
// bound of backup.max-duration runs from the start, so a master or tenant
// list read slowly can pass it before anything is streamed: the answer
// then names the setting, as it does when the bound ends the stream, and
// not the read the bound cut short.
func (c *core) failBackupEarly(ctx context.Context, d access.Decision, err error) error {
	var past errPastMaxDuration
	if errors.As(context.Cause(ctx), &past) {
		bound := fault.New(fault.Exhausted, past.Error())
		return c.fail(d, bound)
	}
	return c.fail(d, err)
}

// errPastMaxDuration is the cause of a backup's context ended by
// backup.max-duration.
type errPastMaxDuration struct{ limit time.Duration }

// Error says what ended the backup and what to do.
func (e errPastMaxDuration) Error() string {
	return "the backup ran past " + settings.BackupMaxDuration + " (" + e.limit.String() + ") and was stopped; it is no backup: " +
		"take it again from a client nearer the server, or raise the setting"
}

// backupTenants returns the database of every tenant the master lists, in
// order of name, as the registry serves it now: a tenant that is ready,
// moved or fenced (whose database is read as it stands). Any other tenant,
// and one whose database cannot be read as a snapshot, fails the backup,
// naming it: a backup is of the whole installation or nothing, rather than
// one leaving out a tenant a person would believe was kept.
func (c *core) backupTenants(ctx context.Context) ([]backup.Database, error) {
	var out []backup.Database
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		rows, next, err := c.master.ListTenants(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			st, err := c.reg.Reader(row.Name)
			if err != nil {
				return nil, fault.Wrap(fault.Unavailable, err, "the tenant "+row.Name+" cannot be backed up, and a backup is of the whole installation or nothing: "+
					fault.Message(err))
			}
			sdb, ok := st.(snapshot.Database)
			if !ok {
				return nil, fault.New(fault.Internal, "api: the tenant "+row.Name+"'s database cannot be backed up")
			}
			out = append(out, backup.Database{Tenant: row.Name, TenantID: row.ID, Driver: row.Driver, DB: sdb})
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
}

// tenantDetails returns the details that name the tenants a backup holds.
func tenantDetails(tenants []backup.Database) map[string]string {
	names := make([]string, 0, len(tenants))
	for _, t := range tenants {
		names = append(names, t.Tenant)
	}
	joined := strings.Join(names, ",")
	if len(joined) > master.MaxAuditDetailValueLength {
		joined = joined[:master.MaxAuditDetailValueLength-3]
		if i := strings.LastIndexByte(joined, ','); i > 0 {
			joined = joined[:i+1]
		}
		joined += "..."
	}
	return map[string]string{detailTenants: strconv.Itoa(len(tenants)), detailTenantNames: joined}
}
