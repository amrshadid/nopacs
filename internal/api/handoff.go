package api

import (
	"context"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Handoffs is what an export and a reclaim ask of the rest of the server.
type Handoffs interface {
	// Drain returns once the writers of the tenant named name, already
	// moved, have stopped: its reception pipeline and the background work
	// of its corrections; an error when ctx ends first.
	Drain(ctx context.Context, name string) error
	// Advance takes every storage's owner record of the tenant one
	// generation further, as an export does before its snapshot
	// (backup.OwnerCheck.Advance).
	Advance(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error)
	// Reclaim checks that every storage's owner record of the tenant still
	// names this installation and the generation its database holds, then
	// takes each one generation further, so no export made before can be
	// imported once the tenant is writable again
	// (backup.OwnerCheck.Reclaim).
	Reclaim(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error)
}

// The details of an export's and a reclaim's audit entries.
const (
	detailHandoff          = "handoff"
	detailSizeBytes        = "size_bytes"
	detailSHA256           = "sha256"
	detailOwnerGenerations = "owner_generations"
	// detailGivenBackBy names who gives back a tenant whose export failed:
	// the server, for the key that asked, which may since have been
	// revoked or have expired.
	detailGivenBackBy = "given_back_by"
)

// handoffSlot orders the hand-offs of one server: one export at a time
// (one backup or export, as the contract says), and no reclaim of a tenant
// beside an export of it, which would make it writable while the export
// still hands out its transfer key. Its zero value is ready to use.
type handoffSlot struct {
	mu sync.Mutex
	// exporting is the tenant an export runs for; "" for none.
	exporting string
	// backingUp is set while a backup runs.
	backingUp bool
	// reclaiming are the tenants a reclaim is taking back.
	reclaiming map[string]bool
}

// errOneAtATime is the refusal of a backup or an export asked while another
// runs.
var errOneAtATime = fault.New(fault.Exhausted, "a backup or an export is running on this server; one runs at a time")

// takeBackup holds the slot for a backup. One backup or export runs at a
// time, as the contract says: each holds a read transaction open on the
// databases it reads for as long as its client takes, and two would only
// double that cost for no gain, the second holding what the first holds.
// A reclaim may run beside a backup: the backup reads whatever state the
// tenant is in.
func (s *handoffSlot) takeBackup() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exporting != "" || s.backingUp {
		return errOneAtATime
	}
	s.backingUp = true
	return nil
}

// endBackup lets the slot go after a backup.
func (s *handoffSlot) endBackup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backingUp = false
}

// takeExport holds the slot for an export of the tenant named name.
func (s *handoffSlot) takeExport(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.exporting != "" || s.backingUp:
		return errOneAtATime
	case s.reclaiming[name]:
		return fault.New(fault.Precondition, "a reclaim of the tenant "+name+" is running; export it once the reclaim has ended")
	}
	s.exporting = name
	return nil
}

// endExport lets the slot go after an export.
func (s *handoffSlot) endExport() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exporting = ""
}

// takeReclaim holds the tenant named name for a reclaim.
func (s *handoffSlot) takeReclaim(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.exporting == name:
		return fault.New(fault.Precondition, "an export of the tenant "+name+" is running; take it back once the export has ended, "+
			"and only if no import followed")
	case s.reclaiming[name]:
		return fault.New(fault.Precondition, "a reclaim of the tenant "+name+" is running already")
	}
	if s.reclaiming == nil {
		s.reclaiming = map[string]bool{}
	}
	s.reclaiming[name] = true
	return nil
}

// endReclaim lets the tenant named name go after its reclaim.
func (s *handoffSlot) endReclaim(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reclaiming, name)
}

// exportChunk is the most bytes of the file one message of an export
// carries: 1 MiB, as the contract says.
const exportChunk = 1 << 20

// backupService is BackupService.
type backupService struct{ c *core }

// ExportTenant hands a tenant to another installation (backup.proto): the
// tenant made moved with the entry that opens the stream, its writers
// drained, its storages' owner records taken one generation further, then
// its export streamed, a chunk of at most 1 MiB a message, and the summary
// with the transfer key last, once the tenant is found still moved. A
// failure before the summary is written gives the tenant back, ready, with
// an entry saying so; once the summary is written, moved stands.
func (s backupService) ExportTenant(ctx context.Context, req *v1.ExportTenantRequest, stream nopacsv1connect.BackupServiceExportTenantServerStream) error {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.BackupServiceExportTenantProcedure)
	if err != nil {
		return err
	}
	name := req.GetTenant()
	facts := access.Facts{Object: tenantObject(name), TenantName: name}
	if err := tenantName("tenant", name); err != nil {
		return c.failWith(d, nil, facts, err)
	}
	if c.handoffs == nil {
		return c.failWith(d, nil, facts, fault.New(fault.Unavailable, "this server hands off no tenant"))
	}
	if err := c.exports.takeExport(name); err != nil {
		return c.failWith(d, nil, facts, err)
	}
	defer c.exports.endExport()
	row, err := c.master.TenantByName(ctx, name)
	if fault.KindOf(err) == fault.NotFound {
		err = noTenant(d)
	}
	if err != nil {
		return c.failWith(d, nil, facts, err)
	}
	st, err := c.reg.Store(name)
	if err != nil {
		return c.failWith(d, nil, facts, err)
	}
	sdb, ok := st.(snapshot.Database)
	if !ok {
		return c.failWith(d, nil, facts, fault.New(fault.Internal, "api: the tenant's database cannot be exported"))
	}
	if err := exportPreconditions(ctx, st); err != nil {
		return c.failWith(d, nil, facts, err)
	}
	by := d.Actor(master.Installation, master.RoleAdmin)
	opened := facts
	opened.Details = map[string]string{detailStream: "opened", detailHandoff: "moved"}
	if err := c.reg.SetHandoff(ctx, name, master.StateMoved, "handed to another installation by an export", d.Entry(master.OutcomeDone, "", opened), by); err != nil {
		return c.failWith(d, nil, facts, err)
	}
	// From here the tenant is moved; anything that ends before the summary
	// is written gives it back. The server gives it back, not the key that
	// asked: that key may have been revoked or have expired meanwhile (the
	// very reason the export may have ended), and a store refuses a change
	// by such a key, which would leave the tenant moved with no transfer
	// key anywhere. The entry still names the key, which asked for the
	// export, and says who gave the tenant back. Only a moved tenant is
	// given back: one fenced meanwhile stays fenced.
	giveBack := func(why string, cause error) error {
		back := facts
		back.Details = map[string]string{detailStream: "ended", detailEnded: "failed", detailHandoff: "ready", detailGivenBackBy: "server"}
		reason := "the export failed before its summary was written, so no transfer key reached anyone: " + why + "; the tenant is ready again"
		rctx := context.WithoutCancel(ctx)
		if err := c.reg.GiveBack(rctx, name, d.Entry(master.OutcomeFailed, reason, back)); err != nil {
			c.log.Error("api: a tenant whose export failed could not be made ready again; reclaim it", "tenant", name,
				"kind", fault.KindOf(err).String(), "message", fault.Message(err))
		}
		return c.toAPI(d, cause)
	}
	ctx, stop := c.watchKey(ctx, d)
	defer stop()
	if err := c.handoffs.Drain(ctx, name); err != nil {
		return giveBack("its writers did not stop: "+fault.Message(err), err)
	}
	w := &chunkWriter{send: func(b []byte) error {
		return stream.Send(&v1.ExportTenantResponse{Kind: &v1.ExportTenantResponse_Data{Data: b}})
	}}
	sum, key, err := backup.ExportTenant(ctx, backup.ExportConfig{Installation: c.installation, Keys: c.keys, Tenant: name, TenantID: row.ID,
		Driver: row.Driver, DB: sdb, Store: st, Owners: c.handoffs, Out: w})
	if err == nil {
		err = w.flush()
	}
	if err == nil {
		// The tenant must still be moved when its transfer key goes out:
		// a reclaim is refused while this export holds the slot, and this
		// asks again under the tenant's own lock, so no key is handed out
		// for a tenant writable here.
		if err = c.reg.CheckMoved(name); err != nil {
			err = fault.Wrap(fault.Precondition, err, "the tenant is no longer moved, so its export is not handed out: "+fault.Message(err))
		}
	}
	if err != nil {
		clear(key)
		if refusal, ok := refusedWhileOpen(ctx); ok {
			return giveBack("the caller's key may no longer export", refusal)
		}
		if ctx.Err() != nil {
			return giveBack("the client went away or the server stopped", fault.Wrap(fault.Unavailable, ctx.Err(), "the export ended before its summary"))
		}
		return giveBack(fault.Message(err), err)
	}
	msg := &v1.ExportTenantResponse{Kind: &v1.ExportTenantResponse_Summary{Summary: &v1.TenantExportSummary{Summary: summaryMessage(backup.ExportFormat, sum), TransferKey: key}}}
	if err = c.seams.summarySend; err == nil {
		err = stream.Send(msg)
	}
	clear(key)
	if err != nil {
		// A Send that failed may still have delivered the summary, and
		// with it the transfer key. Before the tenant is writable again
		// its owner records go one generation past the ones this export
		// holds, so its import is refused as one older than a reclaim
		// would be; if they cannot be, the tenant stays moved, for a
		// reclaim (which takes them further) to decide.
		cause := fault.Wrap(fault.Unavailable, err, "the summary could not be sent")
		if _, aerr := c.handoffs.Advance(context.WithoutCancel(ctx), row.ID, name, st); aerr != nil {
			c.log.Error("api: the summary of an export may have been delivered and the owner records could not be taken further; "+
				"the tenant stays moved: reclaim it", "tenant", name, "kind", fault.KindOf(aerr).String(), "message", fault.Message(aerr))
			kept := facts
			kept.Details = map[string]string{detailStream: "ended", detailEnded: "failed", detailHandoff: "moved"}
			c.record(d, nil, master.OutcomeFailed, "the summary could not be sent and may have been delivered; the owner records could not be "+
				"taken further, so the tenant stays moved", kept)
			return c.toAPI(d, cause)
		}
		return giveBack("the summary could not be sent", cause)
	}
	ended := facts
	ended.Details = map[string]string{detailStream: "ended", detailEnded: "complete", detailHandoff: "moved",
		detailSizeBytes: strconv.FormatInt(sum.Size, 10), detailSHA256: hex.EncodeToString(sum.SHA256)}
	c.record(d, nil, master.OutcomeDone, "", ended)
	return nil
}

// exportPreconditions refuses an export of a tenant a rebuild is running
// for (it would hand off a shadow catalog half made) or with a correction
// prepared (its request is still being answered).
func exportPreconditions(ctx context.Context, st tenant.Store) error {
	p, ok := st.(tenant.Pipeline)
	if !ok {
		return fault.New(fault.Internal, "api: the tenant's database keeps no jobs")
	}
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxWholeListPages {
		jobs, next, err := p.ListJobs(ctx, tenant.JobFilter{InProgress: true}, page)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Kind == tenant.JobRebuild {
				return fault.New(fault.Precondition, "a rebuild of the tenant is running (job "+j.ID+"); export it once the rebuild has ended")
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	prepared, _, err := p.ListCorrections(ctx, tenant.CorrectionFilter{States: []record.State{record.StatePrepared}}, master.Page{Limit: 1})
	if err != nil {
		return err
	}
	if len(prepared) > 0 {
		return fault.New(fault.Precondition, "a correction of the tenant is prepared ("+prepared[0].ID+"); export it once the correction has been answered")
	}
	return nil
}

// chunkWriter sends what is written to it as the data of a backup's or an
// export's messages, at most exportChunk bytes each, holding at most one
// chunk: send blocks while the client reads nothing (the transport's flow
// control), and with it the writer, so what is read from the databases
// waits on the client rather than piling up in memory.
type chunkWriter struct {
	send func([]byte) error
	buf  []byte
}

// Write implements io.Writer.
func (w *chunkWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		take := min(len(p), exportChunk-len(w.buf))
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		if len(w.buf) == exportChunk {
			if err := w.flush(); err != nil {
				return n - len(p), err
			}
		}
	}
	return n, nil
}

// flush sends the bytes held, if any.
func (w *chunkWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	err := w.send(w.buf)
	w.buf = make([]byte, 0, exportChunk)
	if err != nil {
		return fault.Wrap(fault.Unavailable, err, "the stream could not be sent")
	}
	return nil
}

// summaryMessage returns a backup's or an export's summary, of the format
// named, as the contract writes it. An export's key version is 0, since a
// transfer key encrypts it (backup.Summary.KeyVersion is not set for one).
func summaryMessage(format string, s backup.Summary) *v1.BackupSummary {
	out := &v1.BackupSummary{Format: format, FormatVersion: backup.FormatVersion, InstallationId: s.Installation,
		Time: timestamp(s.Time), SizeBytes: s.Size, Sha256: s.SHA256}
	if format == backup.Format {
		out.KeyVersion = int32(s.KeyVersion) //nolint:gosec // G115: a key version is at most 2^31-1.
	}
	for _, sec := range s.Sections {
		sum, _ := hex.DecodeString(sec.SHA256)
		m := &v1.BackupSection{Tenant: sec.Tenant, SchemaVersion: int32(sec.Point.SchemaVersion), Tables: int32(sec.Tables), Rows: sec.Rows, //nolint:gosec // G115: a schema version and a count of tables are small.
			Sha256: sum, SnapshotTime: timestamp(sec.Point.SnapshotTime), EventHistory: sec.Point.EventHistory,
			LastCorrection: sec.Point.LastCorrection, LastReception: sec.Point.LastReception, KeyVersion: int32(sec.Point.KeyVersion)} //nolint:gosec // G115: a key version is at most 2^31-1.
		for _, o := range sec.Point.Owners {
			m.Owners = append(m.Owners, &v1.StorageOwner{Storage: o.Storage, Generation: o.Generation})
		}
		out.Sections = append(out.Sections, m)
	}
	return out
}

// ReclaimTenant takes back a tenant whose hand-off no import followed
// (tenant.proto): the local operator alone; only while every storage's
// owner record still names this installation and the generation the
// tenant holds, and never while an export of it runs. Each record is taken
// one generation further before the tenant is ready. A ready tenant is
// answered as it stands.
func (s tenantService) ReclaimTenant(ctx context.Context, req *v1.ReclaimTenantRequest) (*v1.ReclaimTenantResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.TenantServiceReclaimTenantProcedure)
	if err != nil {
		return nil, err
	}
	name := req.GetName()
	facts := access.Facts{Object: tenantObject(name), TenantName: name}
	if !d.Principal().IsLocalOperator() {
		return nil, c.failWith(d, nil, facts, fault.New(fault.Denied, "only the local operator takes a tenant back: whoever runs this installation's host "+
			"knows whether the import went ahead"))
	}
	if err := tenantName("name", name); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	if c.handoffs == nil {
		return nil, c.failWith(d, nil, facts, fault.New(fault.Unavailable, "this server takes no tenant back"))
	}
	if err := c.exports.takeReclaim(name); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	defer c.exports.endReclaim(name)
	row, err := c.master.TenantByName(ctx, name)
	if fault.KindOf(err) == fault.NotFound {
		err = noTenant(d)
	}
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	state, ok := c.reg.State(name)
	if !ok {
		return nil, c.failWith(d, nil, facts, noTenant(d))
	}
	if state.State == master.StateReady {
		c.record(d, nil, master.OutcomeDone, "", facts)
		return &v1.ReclaimTenantResponse{Tenant: c.tenantView(row)}, nil
	}
	if !state.State.ReadOnly() {
		return nil, c.failWith(d, nil, facts, fault.New(fault.Precondition, "the tenant "+name+" is "+string(state.State)+", not moved"))
	}
	st, err := c.reg.Reader(name)
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	// The owner records go one generation further before the tenant is
	// writable again, so an export made before this reclaim meets a later
	// generation than it holds and its import is refused: the tenant goes
	// on changing here, and that export no longer says what it holds.
	if _, err := c.handoffs.Reclaim(ctx, row.ID, name, st); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	done := facts
	done.Details = map[string]string{detailHandoff: "ready", detailOwnerGenerations: ownerGenerations(ctx, st)}
	if err := c.reg.SetHandoff(ctx, name, master.StateReady, "", d.Entry(master.OutcomeDone, "", done), d.Actor(master.Installation, master.RoleAdmin)); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	row, err = c.master.TenantByName(ctx, name)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.ReclaimTenantResponse{Tenant: c.tenantView(row)}, nil
}

// ownerGenerations returns the owner generation the tenant holds for each
// storage, "storage=generation" joined by commas, for the audit entry; ""
// when it holds none or they cannot be read.
func ownerGenerations(ctx context.Context, st tenant.Store) string {
	rs, ok := st.(tenant.Restoring)
	if !ok {
		return ""
	}
	held, err := rs.StorageOwners(ctx)
	if err != nil {
		return ""
	}
	out := make([]string, 0, len(held))
	for _, o := range held {
		out = append(out, o.Storage+"="+strconv.FormatInt(o.Generation, 10))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
