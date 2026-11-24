package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// backedUp is one backup's stream, read to its end.
type backedUp struct {
	data      []byte
	summary   *v1.BackupSummary
	err       error
	requestID string
}

// createBackup reads a backup as c to its end.
func createBackup(c clients) backedUp {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx, info := connect.NewClientContext(ctx)
	var out backedUp
	st, err := c.backups.CreateBackup(ctx, &v1.CreateBackupRequest{})
	if err == nil {
		defer func() { _ = st.Close() }()
		for {
			m, rerr := st.Receive()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				err = rerr
				break
			}
			out.data = append(out.data, m.GetData()...)
			if s := m.GetSummary(); s != nil {
				out.summary = s
			}
		}
	}
	out.err, out.requestID = err, info.ResponseHeader().Get(api.RequestIDHeader)
	return out
}

// outcome returns the stream as the table's outcome.
func (b backedUp) outcome() outcome { return outcome{err: b.err, requestID: b.requestID} }

// streamEntries returns a request's entries in the installation's log by
// their stream detail, "opened" and "ended".
func streamEntries(log []master.AuditEntry, requestID string) map[string]master.AuditEntry {
	out := map[string]master.AuditEntry{}
	for _, en := range entriesOf(log, requestID) {
		out[en.Details["stream"]] = en
	}
	return out
}

// gatedDB is a database whose Snapshot, once it has passed its first table
// to the sink (so its read transaction is open), signals on reached and
// waits for release, or fails there with fail when set.
type gatedDB struct {
	snapshot.Database
	reached, release chan struct{}
	fail             error
}

// gatedSink is the sink gatedDB's Snapshot reads into.
type gatedSink struct {
	snapshot.Sink
	db     *gatedDB
	passed bool
}

// Row passes the first row of the first table on, then holds or fails.
func (s *gatedSink) Row(values []any) error {
	if err := s.Sink.Row(values); err != nil {
		return err
	}
	if s.passed {
		return nil
	}
	s.passed = true
	if s.db.fail != nil {
		return s.db.fail
	}
	if s.db.reached != nil {
		s.db.reached <- struct{}{}
		<-s.db.release
	}
	return nil
}

// Snapshot implements snapshot.Database through the gate. A failure set is
// answered as the database's own, whatever the driver made of it.
func (g *gatedDB) Snapshot(ctx context.Context, sink snapshot.Sink) (snapshot.Point, error) {
	p, err := g.Database.Snapshot(ctx, &gatedSink{Sink: sink, db: g})
	if err != nil && g.fail != nil {
		return p, g.fail
	}
	return p, err
}

// testCreateBackup guards CreateBackup: admin on the installation only (an
// operator on it and a tenant's admin refused); a backup streams a file the
// installation's key file opens, whose size and SHA-256 the summary holds,
// with the master's section then lab1's, under the key version in use, and
// writes an entry when it opens and one when it ends, naming its tenants;
// a second backup, and an export, asked while one runs are refused as
// resource exhausted, the audit's sealing pass is paused while it runs and
// resumed after, and the first ends whole; a database that fails part
// way ends the stream with its error and no summary, and the entry that
// ends it says it failed.
func testCreateBackup(t *testing.T) {
	f := newFixture(t)
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: func(c clients) outcome { return createBackup(c).outcome() }, code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: func(c clients) outcome { return createBackup(c).outcome() },
			code: connect.CodePermissionDenied},
		{name: "admin of the tenant only", as: f.lab1Admin, call: func(c clients) outcome { return createBackup(c).outcome() },
			code: connect.CodePermissionDenied},
	})

	x := createBackup(f.as(f.instAdmin))
	if x.err != nil || x.summary == nil {
		t.Fatalf("a backup: %v", x.err)
	}
	digest := sha256.Sum256(x.data)
	sum := x.summary
	if sum.GetFormat() != backup.Format || sum.GetSizeBytes() != int64(len(x.data)) || !bytes.Equal(sum.GetSha256(), digest[:]) ||
		sum.GetKeyVersion() != int32(f.keys.SealVersion()) || sum.GetInstallationId() != testInstallation || len(sum.GetSections()) != 2 || //nolint:gosec // G115: a key version is small.
		sum.GetSections()[0].GetTenant() != "" || sum.GetSections()[1].GetTenant() != "lab1" || sum.GetSections()[1].GetRows() == 0 {
		t.Fatalf("the backup's summary: %v (%d bytes of data)", sum, len(x.data))
	}
	v, err := backup.Verify(bytes.NewReader(x.data), f.keys)
	if err != nil || v.Header.Format != backup.Format || len(v.Starts) != 2 || v.Starts[1].Tenant != "lab1" {
		t.Fatalf("the file under the installation's key file: %+v %v", v.Header, err)
	}
	entries := streamEntries(f.masterLog(), x.requestID)
	opened, ended := entries["opened"], entries["ended"]
	if len(entries) != 2 || opened.Outcome != master.OutcomeDone || opened.Details["tenants"] != "1" || opened.Details["tenant_names"] != "lab1" ||
		ended.Outcome != master.OutcomeDone || ended.Details["ended"] != "complete" || ended.Details["size_bytes"] == "" ||
		ended.Details["sha256"] == "" || ended.Details["tenant_names"] != "lab1" || ended.KeyID != f.instAdminID {
		t.Fatalf("a backup's entries: %+v", entries)
	}

	gate := &gatedDB{reached: make(chan struct{}), release: make(chan struct{})}
	api.SetBackupDatabases(f.handlers, func(name string, db snapshot.Database) snapshot.Database {
		if name != "lab1" {
			return db
		}
		gate.Database = db
		return gate
	})
	done := make(chan backedUp, 1)
	go func() { done <- createBackup(f.as(f.instAdmin)) }()
	<-gate.reached
	second := createBackup(f.as(f.instAdmin))
	exp := export(f.as(f.instAdmin), "lab1")
	paused := f.sealPauses.Load()
	close(gate.release)
	first := <-done
	if paused != 1 || f.sealPauses.Load() != 0 {
		t.Fatalf("the sealing pass was paused %d times during the backup and %d after it", paused, f.sealPauses.Load())
	}
	if connect.CodeOf(second.err) != connect.CodeResourceExhausted || !strings.Contains(second.err.Error(), "one runs at a time") ||
		exp.code() != connect.CodeResourceExhausted {
		t.Fatalf("a backup and an export during a backup: %v, %v", second.err, exp.err)
	}
	if first.err != nil || first.summary == nil {
		t.Fatalf("the backup held open: %v", first.err)
	}
	if _, err := backup.Verify(bytes.NewReader(first.data), f.keys); err != nil {
		t.Fatalf("the backup held open does not verify: %v", err)
	}

	failing := &gatedDB{fail: fault.New(fault.Unavailable, "the tenant's database stopped answering")}
	api.SetBackupDatabases(f.handlers, func(name string, db snapshot.Database) snapshot.Database {
		if name != "lab1" {
			return db
		}
		failing.Database = db
		return failing
	})
	broken := createBackup(f.as(f.instAdmin))
	if broken.summary != nil || connect.CodeOf(broken.err) != connect.CodeUnavailable || !strings.Contains(broken.err.Error(), "lab1") {
		t.Fatalf("a backup whose tenant failed part way: %v, summary %v", broken.err, broken.summary != nil)
	}
	end := streamEntries(f.masterLog(), broken.requestID)["ended"]
	if end.Outcome != master.OutcomeFailed || end.Details["ended"] != "unavailable" || !strings.Contains(end.Reason, "stopped answering") {
		t.Fatalf("the entry that ends a failed backup: %+v", end)
	}
	api.SetBackupDatabases(f.handlers, nil)
	if again := createBackup(f.as(f.instAdmin)); again.err != nil || again.summary == nil {
		t.Fatalf("a backup after a failed one: %v", again.err)
	}
}

// holdLab1 makes every backup by f read lab1 through a gate that holds its
// read transaction open after its first row, and returns the gate.
func holdLab1(f *fixture) *gatedDB {
	gate := &gatedDB{reached: make(chan struct{}), release: make(chan struct{})}
	api.SetBackupDatabases(f.handlers, func(name string, db snapshot.Database) snapshot.Database {
		if name != "lab1" {
			return db
		}
		gate.Database = db
		return gate
	})
	return gate
}

// TestABackupStopsAtItsMaxDuration guards the bound on a whole backup: one
// whose client is slow past backup.max-duration (here 300 ms, for the
// setting's least value of a minute) ends with resource exhausted naming
// the setting and no summary, its end entry saying max_duration, and
// gives back the one place for a backup, so the next one is made.
func TestABackupStopsAtItsMaxDuration(t *testing.T) {
	f := newFixture(t)
	api.SetBackupLimit(f.handlers, 300*time.Millisecond)
	gate := holdLab1(f)
	done := make(chan backedUp, 1)
	go func() { done <- createBackup(f.as(f.instAdmin)) }()
	<-gate.reached
	time.Sleep(600 * time.Millisecond)
	close(gate.release)
	x := <-done
	if x.summary != nil || connect.CodeOf(x.err) != connect.CodeResourceExhausted || !strings.Contains(x.err.Error(), "backup.max-duration") {
		t.Fatalf("a backup past its max duration: %v, summary %v", x.err, x.summary != nil)
	}
	end := streamEntries(f.masterLog(), x.requestID)["ended"]
	if end.Outcome != master.OutcomeFailed || end.Details["ended"] != "max_duration" || !strings.Contains(end.Reason, "backup.max-duration") {
		t.Fatalf("the entry that ends it: %+v", end)
	}
	api.SetBackupDatabases(f.handlers, nil)
	api.SetBackupLimit(f.handlers, 0)
	if again := createBackup(f.as(f.instAdmin)); again.err != nil || again.summary == nil {
		t.Fatalf("a backup after one stopped at its max duration: %v", again.err)
	}
}

// TestABackupIsRefusedWhenItsOpeningEntryCannotBeWritten guards the entry
// that opens a backup: written before anything is read, so a master whose
// audit log does not take it refuses the backup as unavailable, with no
// byte streamed.
func TestABackupIsRefusedWhenItsOpeningEntryCannotBeWritten(t *testing.T) {
	f := newFixture(t)
	f.faulty.failAppendAudit.Store(true)
	x := createBackup(f.as(f.instAdmin))
	if x.summary != nil || len(x.data) != 0 || connect.CodeOf(x.err) != connect.CodeUnavailable || !strings.Contains(x.err.Error(), "audit log") {
		t.Fatalf("a backup whose opening entry failed: %v, %d bytes", x.err, len(x.data))
	}
	f.faulty.failAppendAudit.Store(false)
	x = createBackup(f.as(f.instAdmin))
	if x.err != nil || streamEntries(f.masterLog(), x.requestID)["opened"].Details["stream"] != "opened" {
		t.Fatalf("a backup once the log answers: %v", x.err)
	}
}

// TestABackupEndsWhenItsKeyIsRevoked guards a key revoked while its backup
// runs: at the next check of the key (here every 50 ms) the backup ends
// with the refusal a new request would get, and no summary.
func TestABackupEndsWhenItsKeyIsRevoked(t *testing.T) {
	f := newFixture(t)
	api.SetRecheck(f.handlers, 50*time.Millisecond)
	gate := holdLab1(f)
	done := make(chan backedUp, 1)
	go func() { done <- createBackup(f.as(f.instAdmin)) }()
	<-gate.reached
	if o := call(f.as(local).access.RevokeKey, &v1.RevokeKeyRequest{Id: f.instAdminID}); o.err != nil {
		close(gate.release)
		t.Fatal(o.err)
	}
	time.Sleep(300 * time.Millisecond)
	close(gate.release)
	x := <-done
	if x.summary != nil || connect.CodeOf(x.err) != connect.CodeUnauthenticated {
		t.Fatalf("a backup whose key was revoked: %v, summary %v", x.err, x.summary != nil)
	}
}

// TestABackupRefusesATenantNotReady guards a backup asked while a tenant's
// database does not answer: it is refused as unavailable naming the
// tenant, before anything is streamed, rather than made without it.
func TestABackupRefusesATenantNotReady(t *testing.T) {
	f := newFixture(t)
	f.downTenant("lab9")
	x := createBackup(f.as(f.instAdmin))
	if x.summary != nil || len(x.data) != 0 || connect.CodeOf(x.err) != connect.CodeUnavailable || !strings.Contains(x.err.Error(), "lab9") {
		t.Fatalf("a backup with a tenant down: %v, %d bytes, summary %v", x.err, len(x.data), x.summary != nil)
	}
}
