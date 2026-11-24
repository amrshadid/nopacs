package backup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// pageAll is a page that holds every row a test makes.
var pageAll = master.Page{Limit: 1000}

// takeLock makes dir's parent and takes dir's lock, as a server does.
func takeLock(dir string) (*server.Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return server.TakeLock(dir)
}

// yearOne is the microseconds of 0001-01-01T00:00:00Z, which a time column
// decodes to Go's zero time.Time ("never").
const yearOne = "-62135596800000000"

// restoreFresh closes the rig's databases, empties its Postgres schemas
// when it is on Postgres, and restores the backup at path into a new data
// directory, which it opens as the rig's.
func (r *rig) restoreFresh(path string) (Report, error) {
	r.t.Helper()
	_ = r.st.Close()
	_ = r.m.Close()
	target := filepath.Join(r.t.(*testing.T).TempDir(), "restored")
	cfg := RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
		MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")}
	if r.postgres {
		r.emptyPostgres()
		cfg.MasterAddress = r.masterAddr
		cfg.Scratch, _ = testkit.PostgresScratch(r.t)
	}
	rep, err := Restore(r.ctx, cfg)
	if err != nil {
		return rep, err
	}
	r.dataDir, r.masterAddr = target, cfg.MasterAddress
	r.openMaster()
	r.openTenant()
	return rep, nil
}

// TestRestoreSuspendsKeyWhateverItHeld guards that no value a backup
// carries leaves a key working: a key whose suspension, and one whose
// revocation, is the microseconds of year 1 (Go's zero time, "never", once
// decoded) are both suspended by the restore and counted, and the revoked
// one reads as revoked, on each database type.
func TestRestoreSuspendsKeyWhateverItHeld(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		path, _ := r.backup()
		plain := editRows(t, r.plainOf(path), "api_keys", func(col map[string]int, row []json.RawMessage) {
			switch string(row[col["id"]]) {
			case `"K00000000001"`:
				row[col["suspended"]] = json.RawMessage(yearOne)
			case `"K00000000002"`:
				row[col["revoked"]] = json.RawMessage(yearOne)
			}
		})
		rep, err := r.restoreFresh(r.crafted(plain))
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if rep.KeysSuspended != 2 {
			t.Fatalf("the restore suspended %d keys, want 2", rep.KeysSuspended)
		}
		var out strings.Builder
		if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), "Keys: all 2 suspended") {
			t.Fatalf("the report reads %q (%v)", out.String(), err)
		}
		for _, id := range []string{"K00000000001", "K00000000002"} {
			k, err := r.m.Key(r.ctx, id)
			if err != nil || k.Suspended.IsZero() {
				t.Fatalf("the key %s reads %+v (%v), want it suspended", id, k, err)
			}
		}
		if k, err := r.m.Key(r.ctx, "K00000000002"); err != nil || k.Revoked.IsZero() {
			t.Fatalf("the key revoked at year 1 reads %+v (%v), want it revoked", k, err)
		}
	})
}

// TestRestoreDoesNotTrustAPurgedRecord guards that a record on a storage
// saying a delete the backup has no row of was purged (one that verifies
// under the key file: what the installation could have written after the
// backup, or anyone holding its key file) purges nothing: the subject, whose data
// is still on the storage, stays in the rebuilt catalog, and the delete is
// committed and unresolved for an admin to decide, on each database type.
func TestRestoreDoesNotTrustAPurgedRecord(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		path, _ := r.backup()
		forged := r.deleteSubject(r.engine(), all[0])
		r.forgeRecord(forged.ID, func(c *record.Correction) {
			c.State, c.Purge = record.StatePurged, &record.Purge{Removed: 3}
		})
		rep, err := r.restoreFresh(path)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if got := r.subjects(r.p); !slices.Equal(got, all) {
			t.Fatalf("the restored catalog holds %v, want every subject %v", got, all)
		}
		c, err := r.p.Correction(r.ctx, forged.ID)
		if err != nil || c.State != record.StateCommitted || !c.Unresolved || c.RecordsState != record.StatePurged {
			t.Fatalf("the correction made from the record is %+v (%v), want committed, unresolved, its records to be written again", c, err)
		}
		if res := rep.Tenants[0]; res.Added != 1 || res.Disagreed != 0 {
			t.Fatalf("the tenant's reconcile is %+v", res)
		}
		var out strings.Builder
		if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), "1 with no row in the backup") ||
			!strings.Contains(out.String(), "not trusted to have purged or undone") {
			t.Fatalf("the report reads %q (%v)", out.String(), err)
		}
	})
}

// TestRestoreDoesNotTrustAnUndoneRecord guards that a record saying a
// delete the backup holds in its grace period was undone, even one that
// verifies under the key file, leaves the delete
// as the backup holds it: committed, its subject hidden and still named so
// an arrival of it is held, the record reported as disagreeing and marked
// to be written again from the row, on each database type.
func TestRestoreDoesNotTrustAnUndoneRecord(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		gone := r.deleteSubject(r.engine(), all[0])
		path, _ := r.backup()
		r.forgeRecord(gone.ID, func(c *record.Correction) { c.State = record.StateUndone })
		rep, err := r.restoreFresh(path)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if got := r.subjects(r.p); slices.Contains(got, all[0]) || len(got) != len(all)-1 {
			t.Fatalf("the restored catalog holds %v, want every subject but %s", got, all[0])
		}
		c, err := r.p.Correction(r.ctx, gone.ID)
		if err != nil || c.State != record.StateCommitted || c.Unresolved || c.RecordsState != record.StateUndone {
			t.Fatalf("the delete is %+v (%v), want committed as the backup holds it, its records to be written again", c, err)
		}
		match, err := r.p.DeletedMatches(r.ctx, tenant.DeletedProbe{Subject: all[0], StudyUID: "1.2.3"})
		if err != nil || match.Subject != gone.ID {
			t.Fatalf("the delete no longer names its subject: %+v %v", match, err)
		}
		res := rep.Tenants[0]
		if res.Disagreed != 1 || !slices.Equal(res.Disagreements, []string{gone.ID}) || res.Added != 0 {
			t.Fatalf("the tenant's reconcile is %+v", res)
		}
		var out strings.Builder
		if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), "1 that disagree with the backup's row") ||
			!strings.Contains(out.String(), gone.ID) {
			t.Fatalf("the report reads %q (%v)", out.String(), err)
		}
	})
}

// TestRestoreRefusesUnknownSchemaVersion guards that a backup with a
// section of a schema version this binary does not know is refused before
// anything is written, and that the same data directory then takes the
// backup as it was made.
func TestRestoreRefusesUnknownSchemaVersion(t *testing.T) {
	r := newRig(t, false)
	path, _ := r.backup()
	plain := editEnds(t, r.plainOf(path), func(e *SectionEnd) {
		if e.Index == 1 {
			e.Point.SchemaVersion = 99
		}
	})
	target := filepath.Join(t.TempDir(), "restored")
	cfg := RestoreConfig{File: r.crafted(plain), Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
		MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")}
	_, err := Restore(r.ctx, cfg)
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "schema version 99") {
		t.Fatalf("the restore answered %v, want Invalid naming schema version 99", err)
	}
	wantNothingWritten(t, target)
	cfg.File = path
	if _, err := Restore(r.ctx, cfg); err != nil {
		t.Fatalf("the backup as it was made does not restore into the same directory: %v", err)
	}
	if Interrupted(target) {
		t.Fatal("a restore that ended left its marker")
	}
}

// TestRestoreRefusesAnotherBaseline guards the baseline's fingerprint in a
// backup: each section records the fingerprint of the baseline schema its
// database was made from, and a section whose fingerprint is not this
// binary's (another baseline, or none, as a file of an earlier development
// build carries) is refused, master's or tenant's, with a plain message,
// before anything is written, even when its schema version is one this
// binary knows.
func TestRestoreRefusesAnotherBaseline(t *testing.T) {
	r := newRig(t, false)
	path, sum := r.backup()
	masterSchema, tenantSchema, err := driver.SchemaFingerprints()
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range sum.Sections {
		want := tenantSchema
		if i == 0 {
			want = masterSchema
		}
		if s.Point.Schema != want || len(want) != 64 {
			t.Fatalf("section %d records the baseline %q, want this binary's %q", i, s.Point.Schema, want)
		}
	}
	for _, c := range []struct {
		name    string
		section int
		schema  string
	}{
		{"the master's, of another baseline", 0, strings.Repeat("0", 64)},
		{"a tenant's, of another baseline", 1, strings.Repeat("f", 64)},
		{"a tenant's, of a file that records none", 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			plain := editEnds(t, r.plainOf(path), func(e *SectionEnd) {
				if e.Index == c.section {
					e.Point.Schema = c.schema
				}
			})
			target := filepath.Join(t.TempDir(), "restored")
			_, err := Restore(r.ctx, RestoreConfig{File: r.crafted(plain), Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
				MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")})
			if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "made by an earlier development build") {
				t.Fatalf("the restore answered %v, want Invalid saying an earlier development build made it", err)
			}
			t.Logf("refused: %s", fault.Message(err))
			wantNothingWritten(t, target)
		})
	}
}

// TestLoadChecksEachSectionBeforeItCommits guards that a section read in
// the load whose digest is not the one the check found fails before its
// load commits: the master's database is left with none of the section's
// rows.
func TestLoadChecksEachSectionBeforeItCommits(t *testing.T) {
	r := newRig(t, false)
	path, _ := r.backup()
	good := r.plainOf(path)
	v, err := Verify(bytes.NewReader(encrypt(t, good, r.keys)), r.keys)
	if err != nil {
		t.Fatal(err)
	}
	changed := editRows(t, good, "api_keys", func(col map[string]int, row []json.RawMessage) {
		row[col["holder"]] = json.RawMessage(`"someone else"`)
	})
	target := t.TempDir()
	cfg := RestoreConfig{Keys: r.keys, DataDir: target, MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")}
	l, err := load(r.ctx, cfg, bytes.NewReader(encrypt(t, reseal(t, changed), r.keys)), v)
	if l != nil {
		l.close()
	}
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "changed between its check and its load") {
		t.Fatalf("the load answered %v, want Invalid saying the file changed", err)
	}
	m, err := driver.OpenMaster(r.ctx, cfg.MasterAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if keys, _, err := m.ListKeys(r.ctx, pageAll); err != nil || len(keys) != 0 {
		t.Fatalf("the master holds %d keys (%v) after a load that failed its digest, want none", len(keys), err)
	}
}

// TestRestoreRefusesStorageTakenOver guards that a storage whose owner
// record says another restore took it over after the backup is refused,
// saying why and leaving the marker of an interrupted restore, which a
// restore into that directory then names; with TakeOver, a restore into a
// new directory takes it at the next generation.
func TestRestoreRefusesStorageTakenOver(t *testing.T) {
	r := newRig(t, false)
	path, _ := r.backup()
	if _, err := r.restoreInto(path); err != nil {
		t.Fatal(err)
	}
	target, err := r.restoreInto(path)
	if fault.KindOf(err) != fault.Conflict || !strings.Contains(fault.Message(err), "--take-over") ||
		!strings.Contains(fault.Message(err), "owner generation 1") {
		t.Fatalf("a second restore answered %v, want a conflict naming the generation and --take-over", err)
	}
	if !Interrupted(target) {
		t.Fatal("a restore that failed after writing left no marker")
	}
	_, err = Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
		MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")})
	if fault.KindOf(err) != fault.Conflict || !strings.Contains(fault.Message(err), InterruptedAdvice) {
		t.Fatalf("a restore into an interrupted one answered %v, want the advice", err)
	}
	other := filepath.Join(t.TempDir(), "restored")
	rep, err := Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: other,
		MasterAddress: driver.SQLiteScheme + filepath.Join(other, "master.db"), TakeOver: true})
	if err != nil {
		t.Fatalf("a restore with TakeOver failed: %v", err)
	}
	if o := rep.Tenants[0].Owners; len(o) != 1 || o[0].Generation != 2 {
		t.Fatalf("the restore with TakeOver took %+v, want generation 2", o)
	}
	if owner := r.ownerRecord(); owner.Generation != 2 || owner.Installation != r.id {
		t.Fatalf("the owner record is %+v", owner)
	}
}

// TestTakeOwnerChecksTheMAC guards that a storage's owner record naming
// this installation at the generation held, but without a MAC this key
// file makes, is refused as another owner's (someone who can write to the
// storage, not this installation, wrote it); that one with the right MAC is
// taken at the next generation, its new record verifying under the key
// file; and that the takeover overrides a forged one.
func TestTakeOwnerChecksTheMAC(t *testing.T) {
	r := newRig(t, false)
	claim := ownerClaim{installation: r.id, tenantID: tenantID, storage: "main", held: 1, keys: r.keys}
	forged := []byte(`{"format":1,"installation":"` + r.id + `","tenant":"` + tenantID + `","generation":1,"written":"2026-01-02T03:04:05Z"}` + "\n")
	put := func(b []byte) {
		t.Helper()
		if _, err := r.storage.Put(r.ctx, layout.OwnerKey, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	put(forged)
	if _, err := takeOwner(r.ctx, r.storage, claim, time.Now()); fault.KindOf(err) != fault.Conflict || !strings.Contains(fault.Message(err), "MAC does not verify") {
		t.Fatalf("a record with no MAC answered %v, want a conflict naming the MAC", err)
	}
	signed, err := record.EncodeOwner(record.Owner{Format: record.Format, Installation: r.id, Tenant: tenantID, Generation: 1, Written: time.Now()}, r.keys)
	if err != nil {
		t.Fatal(err)
	}
	put(signed)
	gen, err := takeOwner(r.ctx, r.storage, claim, time.Now())
	if err != nil || gen != 2 {
		t.Fatalf("a signed record: generation %d, %v", gen, err)
	}
	if o := r.ownerRecord(); o.Generation != 2 || record.VerifyOwner(o, r.keys) != nil {
		t.Fatalf("the record written is %+v and does not verify", o)
	}
	put(forged)
	claim.takeOver = true
	if gen, err := takeOwner(r.ctx, r.storage, claim, time.Now()); err != nil || gen != 2 {
		t.Fatalf("a takeover over a forged record: generation %d, %v", gen, err)
	}
}

// TestRestoreTakesTheDataDirectoryLock guards that a restore takes the
// lock a server takes, so a second restore into the same directory (or a
// server on it) is refused while the first holds it, writing nothing.
func TestRestoreTakesTheDataDirectoryLock(t *testing.T) {
	r := newRig(t, false)
	path, _ := r.backup()
	target := filepath.Join(t.TempDir(), "restored")
	held, err := takeLock(target)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
		MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")})
	if fault.KindOf(err) != fault.Conflict || !strings.Contains(fault.Message(err), "locked by another process") {
		t.Fatalf("a restore into a locked directory answered %v, want a conflict saying it is locked", err)
	}
	if Interrupted(target) {
		t.Fatal("a restore refused the lock wrote its marker")
	}
	_ = held.Release()
	if _, err := Restore(r.ctx, RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
		MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")}); err != nil {
		t.Fatalf("once the lock is released the restore fails: %v", err)
	}
}

// TestReportCleansNames guards that the names a report prints from a
// backup or a storage cannot carry a terminal escape, a carriage return or
// a format character into the person's terminal.
func TestReportCleansNames(t *testing.T) {
	bad := "lab\x1b[2J\r\u202edone"
	rep := Report{Installation: "I\x1b]0;title\x07", Sections: []Section{{Tenant: bad}},
		Tenants: []TenantResult{{Tenant: bad, Owners: []Owner{{Storage: bad, Generation: 1}}, Disagreed: 1, Disagreements: []string{bad},
			Rebuild: tenant.Job{ID: bad}, Loss: TenantLoss{Receptions: []ReceptionLoss{{Receiver: bad}}}}}}
	var out strings.Builder
	if err := rep.Print(&out); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"\x1b", "\r", "\u202e", "\x07"} {
		if strings.Contains(out.String(), c) {
			t.Fatalf("the report carries %q:\n%q", c, out.String())
		}
	}
	if !strings.Contains(out.String(), "Tenant lab?[2J??done:") {
		t.Fatalf("the report does not name the tenant cleaned:\n%s", out.String())
	}
}

// TestRestoreMarksQuarantineLost guards that a quarantined file held with
// its bytes kept in incoming at the backup comes back marked incoming
// lost, with no bytes kept, and is counted in the report.
func TestRestoreMarksQuarantineLost(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:1]...)
	r.arrive([]byte("this is not a DICOM file at all, only some text"))
	held, _, err := r.p.ListQuarantined(r.ctx, tenant.QuarantineFilter{}, pageAll)
	if err != nil || len(held) != 1 || !held[0].ContentKept {
		t.Fatalf("the quarantine before the backup is %+v (%v), want one file kept", held, err)
	}
	path, _ := r.backup()
	rep, err := r.restoreFresh(path)
	if err != nil {
		t.Fatalf("the restore failed: %v", err)
	}
	got, _, err := r.p.ListQuarantined(r.ctx, tenant.QuarantineFilter{}, pageAll)
	if err != nil || len(got) != 1 || got[0].Reason != tenant.QuarantineIncomingLost || got[0].ContentKept || got[0].Incoming != "" {
		t.Fatalf("the restored quarantine is %+v (%v), want the file marked incoming lost", got, err)
	}
	if n := rep.Tenants[0].QuarantineMarked; n != 1 {
		t.Fatalf("the report counts %d marked, want 1", n)
	}
	var out strings.Builder
	if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), strconv.Itoa(1)+" of them marked incoming lost") {
		t.Fatalf("the report reads %q (%v)", out.String(), err)
	}
}
