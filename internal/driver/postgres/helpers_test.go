package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// addressEnv names the variable that holds the address of a Postgres
// database the tests may make schemas in. requireEnv, when set to anything,
// turns a missing address from a skip into a failure: CI sets it on Linux,
// where a Postgres is always there, so a service that failed to start cannot
// make the tests pass by not running.
const (
	addressEnv = "NOPACS_TEST_POSTGRES"
	requireEnv = "NOPACS_REQUIRE_POSTGRES"
)

// testAddress returns the address of the test database. Without one it skips
// the test and says why, or fails it when requireEnv is set.
func testAddress(tb testing.TB) string {
	tb.Helper()
	address := os.Getenv(addressEnv)
	if address != "" {
		return address
	}
	if os.Getenv(requireEnv) != "" {
		tb.Fatalf("%s is not set, and %s is: the Postgres tests must run here, not be skipped", addressEnv, requireEnv)
	}
	tb.Skipf("%s is not set; the Postgres tests are skipped (point it at a postgres:// URL of a scratch database to run them)", addressEnv)
	return ""
}

// admin opens a connection to the database at address for a test's own
// setup and checks, closed when tb ends. A failure is reported without the
// address.
func admin(tb testing.TB, address string) *pgx.Conn {
	tb.Helper()
	// The pool's own parameters mean nothing to a single connection, and
	// pgx would send them to the server as settings, which refuses them.
	u, err := url.Parse(address)
	if err != nil {
		tb.Fatal("the test address is not a URL")
	}
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(k, "pool_") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	address = u.String()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, address)
	if err != nil {
		cfg, _ := pgx.ParseConfig(address)
		tb.Fatalf("cannot connect to the test database: %s", newRedactor(address, cfg).text(err.Error()))
	}
	tb.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = conn.Close(cctx) // A failed close of a test's connection hides nothing.
	})
	return conn
}

// randomName returns prefix and 12 hex digits, a name no other run uses.
func randomName(tb testing.TB, prefix string) string {
	tb.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

// withParams returns address with the query parameters set.
func withParams(tb testing.TB, address string, params map[string]string) string {
	tb.Helper()
	u, err := url.Parse(address)
	if err != nil {
		tb.Fatal("the test address is not a URL")
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// mustExec runs statements on conn, failing the test on an error.
func mustExec(tb testing.TB, conn *pgx.Conn, statements ...string) {
	tb.Helper()
	for _, s := range statements {
		if _, err := conn.Exec(context.Background(), s); err != nil {
			tb.Fatalf("%s: %v", s, err)
		}
	}
}

// newSchema makes a schema no other test uses in the test database, dropped
// when tb ends, and returns an address whose search_path is that schema.
// Each test therefore has its own tables, in the one database the CI service
// or the local container has, and runs never collide. The pool is kept to
// three connections: the suite opens a store per test and runs them in
// parallel, and Postgres allows 100 connections by default.
func newSchema(tb testing.TB) string {
	tb.Helper()
	return newSchemaAt(tb, testAddress(tb))
}

// schemaWork bounds how many schemas are created (with their migration) or
// dropped at the same moment. Creating a schema's tables and indexes in one
// transaction, and dropping it with CASCADE, each hold one lock per table and
// index until the transaction commits, and Postgres keeps every lock in one
// shared table of max_locks_per_transaction (64 by default) times
// max_connections (100), 6,400 entries. The tenant schema has so many objects
// that the suite's parallel subtests, each making and dropping a schema,
// fill that table and fail with "out of shared memory" (SQLSTATE 53200).
// Four at a time stay far under the limit; the tests themselves still run in
// parallel. The channel only bounds, and no test changes it.
var schemaWork = make(chan struct{}, 4)

// holdSchemaWork takes a place among the schema creations and drops running
// at once and returns the function that gives it back.
func holdSchemaWork() (release func()) {
	schemaWork <- struct{}{}
	return func() { <-schemaWork }
}

// newSchemaAt is newSchema in the database that base names.
func newSchemaAt(tb testing.TB, base string) string {
	tb.Helper()
	conn := admin(tb, base)
	schema := randomName(tb, "mt_")
	func() {
		defer holdSchemaWork()()
		mustExec(tb, conn, `CREATE SCHEMA `+schema)
	}()
	tb.Cleanup(func() {
		c := admin(tb, base)
		defer holdSchemaWork()()
		mustExec(tb, c, `DROP SCHEMA `+schema+` CASCADE`)
	})
	return withParams(tb, base, map[string]string{"search_path": schema, "pool_max_conns": "3"})
}

// openUnmigrated opens a store in a new schema without migrating it, closed
// when tb ends.
func openUnmigrated(tb testing.TB) *Master {
	tb.Helper()
	return openAt(tb, newSchema(tb))
}

// openAt opens a store at address, closed when tb ends.
func openAt(tb testing.TB, address string) *Master {
	tb.Helper()
	m, err := OpenMaster(context.Background(), address)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = m.Close() })
	return m
}

// settledAcquired returns how many connections are out of m's pool once the
// count has stopped changing. After a call whose context ended, pgx closes the
// connection the canceled statement used and the pool drops it in the
// background, so the count is read again, for up to two seconds, until it is
// zero.
func settledAcquired(m *base) int32 {
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := m.pool.Stat().AcquiredConns()
		if n == 0 || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// openTest opens a fresh, migrated master in a new schema, closed when tb
// ends.
func openTest(tb testing.TB) master.Store {
	tb.Helper()
	m := openUnmigrated(tb)
	defer holdSchemaWork()()
	if err := m.Migrate(context.Background()); err != nil {
		tb.Fatal(err)
	}
	return m
}

// What the concurrency tests share (pipeline_concurrency_test.go and the
// files beside it, the model, the retry tests): several stores on one
// schema, as several processes would be, a connection of their own that
// reads the rows, and the files, claims and commits they make.

// cT0 is the time the concurrency tests start at.
var cT0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

// cenv is one concurrency test: its stores on one schema, a connection that
// reads the rows, and a source of ids.
type cenv struct {
	t      *testing.T
	stores []*Tenant
	db     *pgx.Conn
	ctx    context.Context
	seq    atomic.Int64
	// app is the application_name every connection of the test gives the
	// server, so the test can find its own sessions in pg_stat_activity
	// among those of the tests that run beside it on the same database.
	app string
}

// newCenv opens n migrated stores of one tenant on a new schema, each with a
// pool of conns connections.
func newCenv(t *testing.T, n, conns int) *cenv {
	t.Helper()
	app := randomName(t, "conc_")
	address := withParams(t, newSchema(t), map[string]string{"pool_max_conns": fmt.Sprint(conns), "application_name": app})
	e := &cenv{t: t, ctx: context.Background(), db: admin(t, address), app: app}
	for range n {
		e.stores = append(e.stores, openTenantAt(t, address, "conc"))
	}
	return e
}

// waitBlocked returns once n of the test's own sessions are waiting for a
// lock another transaction holds. A test that holds a transaction open
// calls it before letting that transaction go, so the call it started is
// known to be inside the server and waiting, whatever the scheduler did;
// it is a wait for a state, never a guess at how long one takes.
func (e *cenv) waitBlocked(n int) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := e.scalar(`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND wait_event_type = 'Lock'`, e.app)
		if got >= int64(n) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%d of the test's sessions wait for a lock after 30s; want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// id returns a new id with the prefix.
func (e *cenv) id(p string) string { return fmt.Sprintf("%s%07d", p, e.seq.Add(1)) }

// audit returns a new audit entry.
func (e *cenv) audit() master.AuditEntry {
	return master.AuditEntry{ID: e.id("a"), Time: cT0, Method: "Test", Outcome: master.OutcomeDone}
}

// ok fails the test on an error.
func (e *cenv) ok(what string, err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("%s: %v", what, err)
	}
}

// p is the first store.
func (e *cenv) p() *Tenant { return e.stores[0] }

// storage adds a directory storage.
func (e *cenv) storage(name string) {
	e.t.Helper()
	_, err := e.p().AddStorage(e.ctx, tenant.Storage{Name: name, Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "fast", Path: name}}, cT0, e.audit(), nil)
	e.ok("AddStorage", err)
}

// receiver adds a receiver on the storage.
func (e *cenv) receiver(name, storage string, port int) {
	e.t.Helper()
	_, err := e.p().AddReceiver(e.ctx, tenant.Receiver{Name: name, Type: tenant.ReceiverDIMSE, Storage: storage,
		DIMSE:       tenant.DIMSEReceiver{Port: port, AETitle: "ARCHIVE"},
		QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes},
		cT0, e.audit(), nil)
	e.ok("AddReceiver", err)
}

// cUID returns a valid UID unique to kind and n.
func cUID(kind, n int) string { return fmt.Sprintf("2.25.%d.%d", kind, n+1) }

// cSum returns a checksum made from n, never zero.
func cSum(n int) tenant.SHA256 {
	var s tenant.SHA256
	for i := range s {
		s[i] = byte(n >> (8 * (i % 4)) & 0xff)
	}
	s[31] = 1
	return s
}

// cFile is received file n of reception r, in series se.
func cFile(r tenant.Reception, se, n int) tenant.ReceivedFile {
	return tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: cUID(3, n), SeriesUID: cUID(2, se), StudyUID: r.StudyUID,
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: int64(1000 + n%1000), SHA256: cSum(n),
		Incoming: fmt.Sprintf("%s/f%07d.dcm", r.ID, n), Caller: tenant.Caller{CallingAETitle: "SCANNER1", Address: "192.0.2.7:51234"},
		Association: "assoc-" + r.ID}
}

// claimOfFile is the claim of a received file.
func claimOfFile(f tenant.ReceivedFile) tenant.InstanceClaim {
	return tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
}

// identified opens a reception of study at receiver, records files, and
// identifies it to storage with the labels given.
func (e *cenv) identified(receiver, studyUID, storage, subject, session string, files func(tenant.Reception) []tenant.ReceivedFile) (tenant.Reception, []tenant.ReceivedFile) {
	e.t.Helper()
	r, _, err := e.p().OpenReception(e.ctx, receiver, studyUID, e.id("rc"), cT0)
	e.ok("OpenReception", err)
	fs := files(r)
	for i := 0; i < len(fs); i += tenant.MaxReceivedBatch {
		_, err = e.p().AddReceivedFiles(e.ctx, fs[i:min(i+tenant.MaxReceivedBatch, len(fs))], cT0, nil)
		e.ok("AddReceivedFiles", err)
	}
	r, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: cT0.Add(time.Minute)}, nil)
	e.ok("quiet", err)
	r, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify, At: cT0.Add(2 * time.Minute),
		SubjectLabel: subject, SessionLabel: session, Storage: storage}, nil)
	e.ok("identify", err)
	return r, fs
}

// scanCommit is the commit of files (of one series) of r into scan.
func scanCommit(r tenant.Reception, subject, session, scan, storage string, files []tenant.ReceivedFile) tenant.ScanCommit {
	p := label.Path{Subject: subject, Session: session, Scan: scan}
	c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: files[0].SeriesUID, SubjectLabel: subject,
		SessionLabel: session, ScanLabel: scan, Storage: storage, At: cT0.Add(3 * time.Minute)}
	for _, f := range files {
		key, _ := layout.InstanceKey(p, f.SOPInstanceUID)
		c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size,
			Size: f.Size, ArchivedSHA256: f.SHA256})
	}
	return c
}

// atOnce runs fn(i) for i in [0, n) on n goroutines released together, and
// waits for all of them.
func atOnce(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			fn(i)
		})
	}
	close(start)
	wg.Wait()
}

// deadlocks is how many changes the stores ran again after a deadlock.
func (e *cenv) deadlocks() int64 {
	var n int64
	for _, s := range e.stores {
		n += s.retried.deadlocks.Load()
	}
	return n
}

// scalar reads one integer with the test's own connection.
func (e *cenv) scalar(query string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.QueryRow(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// checkTotals fails unless the catalog's and the quarantine's running
// totals equal counts over their rows, each scan's counts equal its
// instances, each session's its scans and their instances, each subject's
// its sessions and their instances, a session's modalities are exactly
// those of its scans, and no claim is held by a reception that ended, is
// not identified, or is not there.
func (e *cenv) checkTotals(when string) {
	e.t.Helper()
	for _, c := range []struct{ what, query string }{
		{"the catalog's totals disagree with its rows", `SELECT count(*) FROM catalog_totals WHERE
			subjects <> (SELECT count(*) FROM subjects) OR sessions <> (SELECT count(*) FROM sessions) OR
			scans <> (SELECT count(*) FROM scans) OR instances <> (SELECT count(*) FROM instances)`},
		{"scans' counts disagree with their instances", `SELECT count(*) FROM scans s WHERE
			instances <> (SELECT count(*) FROM instances i WHERE i.scan_id = s.id) OR
			bytes <> (SELECT coalesce(sum(size), 0) FROM instances i WHERE i.scan_id = s.id)`},
		{"sessions' counts disagree with their scans", `SELECT count(*) FROM sessions s WHERE
			scan_count <> (SELECT count(*) FROM scans c WHERE c.session_id = s.id) OR
			instance_count <> (SELECT count(*) FROM instances i JOIN scans c ON c.id = i.scan_id WHERE c.session_id = s.id) OR
			bytes <> (SELECT coalesce(sum(i.size), 0) FROM instances i JOIN scans c ON c.id = i.scan_id WHERE c.session_id = s.id)`},
		{"subjects' counts disagree with their sessions", `SELECT count(*) FROM subjects u WHERE
			session_count <> (SELECT count(*) FROM sessions s WHERE s.subject_id = u.id) OR
			instance_count <> (SELECT coalesce(sum(instance_count), 0) FROM sessions s WHERE s.subject_id = u.id) OR
			bytes <> (SELECT coalesce(sum(bytes), 0) FROM sessions s WHERE s.subject_id = u.id)`},
		{"a session lacks a modality of its scans", `SELECT count(*) FROM (SELECT DISTINCT session_id, modality FROM scans
			WHERE modality <> '') AS d WHERE NOT EXISTS (SELECT 1 FROM session_modalities m WHERE m.session_id = d.session_id AND m.modality = d.modality)`},
		{"a session has a modality none of its scans has", `SELECT count(*) FROM session_modalities m
			WHERE NOT EXISTS (SELECT 1 FROM scans c WHERE c.session_id = m.session_id AND c.modality = m.modality)`},
		{"the quarantine's totals disagree with its records", `SELECT count(*) FROM quarantine_totals WHERE
			files <> (SELECT count(*) FROM quarantine WHERE state = 'held') OR
			bytes <> (SELECT coalesce(sum(size), 0) FROM quarantine WHERE state = 'held' AND content_kept)`},
		{"claims are held by receptions that ended or are not archiving", `SELECT count(*) FROM instance_claims i
			LEFT JOIN receptions r ON r.id = i.reception_id WHERE r.id IS NULL OR r.ended OR r.storage = ''`},
		{"receptions' archived counts disagree with what they confirmed", `SELECT count(*) FROM receptions r WHERE
			files_archived <> (SELECT count(*) FROM instances i WHERE i.reception_id = r.id) OR
			bytes_archived <> (SELECT coalesce(sum(size), 0) FROM instances i WHERE i.reception_id = r.id)`},
		{"a study's arrivals are not numbered 1 to n", `SELECT count(*) FROM (SELECT study_uid FROM receptions GROUP BY study_uid
			HAVING max(arrival) <> count(*) OR min(arrival) <> 1 OR count(DISTINCT arrival) <> count(*)) AS bad`},
		{"a reception done breaks received = archived + duplicate + quarantined", `SELECT count(*) FROM receptions WHERE state = 'done' AND
			(files_received <> files_archived + files_duplicate + files_quarantined OR
			 bytes_received <> bytes_archived + bytes_duplicate + bytes_quarantined)`},
	} {
		if n := e.scalar(c.query); n != 0 {
			e.t.Fatalf("%s: %s (%d)", when, c.what, n)
		}
	}
}

// portAnswer reports whether err is one of the port's own answers to a
// change that met another (a precondition, or a Conflict: a step from a
// state left, a refusal of the core, a label conflict, a lease lost), never
// an unclassified failure.
func portAnswer(err error) bool {
	k := fault.KindOf(err)
	return k == fault.Conflict || k == fault.Precondition
}

// uniqueRetries is how many changes the stores ran again after a unique
// violation: every insert of the pipeline meets a row made meanwhile with ON
// CONFLICT DO NOTHING and a read, so none should be.
func (e *cenv) uniqueRetries() int64 {
	var n int64
	for _, s := range e.stores {
		n += s.retried.unique.Load()
	}
	return n
}
