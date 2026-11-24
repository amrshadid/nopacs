package postgres

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests in this file need several writers at once, which only a
// database such as Postgres has: many connections, through several stores
// on one schema as several processes would be, calling at the same moment.
// Each runs its race many rounds and then checks the rows themselves, read
// with a connection of its own.

// TestStorageRemovalRacesIdentifyAndCommit guards a storage's row as the
// lock of its use, eighty rounds: a removal of a storage and an identify to
// it at once never both succeed (one is a precondition failure); a change
// of where its files are and an identify at once fail with nothing but a
// precondition, and both succeed only when the change committed before the
// identify took its lock on the storage, never when the identify was first;
// and a commit into a storage and its removal at once leave the commit made
// and the removal refused, with no other error.
func TestStorageRemovalRacesIdentifyAndCommit(t *testing.T) {
	e := newCenv(t, 2, 6)
	e.storage("store")
	e.receiver("a", "store", 11112)
	for round := range 80 {
		name := fmt.Sprintf("s%03d", round)
		e.storage(name)
		r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, round), e.id("rc"), cT0)
		e.ok("OpenReception", err)
		r, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: cT0}, nil)
		e.ok("quiet", err)
		var identify, other error
		move := round%2 == 1
		// sawMoved is what the identify's last attempt saw of the storage,
		// inside its transaction once it holds the storage's row: whether
		// the change of path had committed. The hook reads it through the
		// test's own connection, which nothing else uses meanwhile.
		var sawMoved bool
		hook := func(ctx context.Context, _, _ tenant.Reception) error {
			var path string
			if err := e.db.QueryRow(ctx, `SELECT dir_path FROM storages WHERE name = $1`, name).Scan(&path); err != nil {
				return err
			}
			sawMoved = path == name+"-moved"
			return nil
		}
		atOnce(2, func(i int) {
			if i == 0 {
				_, identify = e.stores[0].StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify,
					At: cT0, SubjectLabel: "sub-01", SessionLabel: "ses-" + r.ID, Storage: name}, hook)
				return
			}
			if move {
				_, other = e.stores[1].SetStorage(e.ctx, tenant.Storage{Name: name, Directory: tenant.DirectoryStorage{Path: name + "-moved"}},
					[]tenant.StorageField{tenant.FieldDirectoryPath}, cT0, e.audit(), nil)
			} else {
				_, other = e.stores[1].RemoveStorage(e.ctx, name, e.audit(), nil)
			}
		})
		for _, err := range []error{identify, other} {
			if err != nil && fault.KindOf(err) != fault.Precondition {
				t.Fatalf("round %d: %v, want success or a precondition failure", round, err)
			}
		}
		// A move and an identify may both succeed: the move first, then the
		// identify, as one writer would order them, before any file is on
		// the storage. A removal and an identify may not: the reception
		// would name a storage that is gone.
		if !move && (identify == nil) == (other == nil) {
			t.Fatalf("round %d: identify %v and the removal %v; want exactly one to succeed", round, identify, other)
		}
		if move && identify == nil && other == nil && !sawMoved {
			t.Fatalf("round %d: the identify held the storage before the change of path committed, and both succeeded", round)
		}
		if move && identify != nil && other != nil {
			t.Fatalf("round %d: identify %v and the change of path %v both failed; want at least one to succeed", round, identify, other)
		}
	}
	// A commit and a removal at once.
	for round := range 20 {
		name := fmt.Sprintf("c%03d", round)
		e.storage(name)
		study := cUID(1, 1000+round)
		r, fs := e.identified("a", study, name, "sub-01", "ses-"+study, func(r tenant.Reception) []tenant.ReceivedFile {
			return []tenant.ReceivedFile{cFile(r, 1000+round, 100000+round)}
		})
		_, err := e.p().ClaimInstances(e.ctx, r.ID, []tenant.InstanceClaim{claimOfFile(fs[0])}, cT0, nil)
		e.ok("ClaimInstances", err)
		var commit, remove error
		atOnce(2, func(i int) {
			if i == 0 {
				_, commit = e.stores[0].CommitScan(e.ctx, scanCommit(r, "sub-01", "ses-"+study, "scan-1", name, fs))
			} else {
				_, remove = e.stores[1].RemoveStorage(e.ctx, name, e.audit(), nil)
			}
		})
		if commit != nil || fault.KindOf(remove) != fault.Precondition {
			t.Fatalf("round %d: commit %v, removal %v; want the commit made and the removal refused", round, commit, remove)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM receptions r WHERE NOT ended AND storage <> '' AND NOT EXISTS (SELECT 1 FROM storages s WHERE s.name = r.storage)`); n != 0 {
		t.Errorf("%d receptions in progress name a storage that was removed", n)
	}
	e.checkTotals("after the races")
}

// TestConfigChangesAtOnce guards the rules across rows of configuration with
// changes made at once through two stores, sixty rounds: two storages in one
// directory, two receivers on one port, and a storage's removal with a
// receiver added to archive to it, never both succeed.
func TestConfigChangesAtOnce(t *testing.T) {
	e := newCenv(t, 2, 6)
	e.storage("store")
	for round := range 60 {
		var errs [2]error
		var what string
		switch round % 3 {
		case 0:
			what = "two storages in one directory"
			atOnce(2, func(i int) {
				_, errs[i] = e.stores[i].AddStorage(e.ctx, tenant.Storage{Name: fmt.Sprintf("d%03d-%d", round, i), Type: tenant.StorageDirectory,
					Directory: tenant.DirectoryStorage{Root: "fast", Path: fmt.Sprintf("same%03d", round)}}, cT0, e.audit(), nil)
			})
		case 1:
			what = "two receivers on one port"
			atOnce(2, func(i int) {
				_, errs[i] = e.stores[i].AddReceiver(e.ctx, tenant.Receiver{Name: fmt.Sprintf("p%03d-%d", round, i), Type: tenant.ReceiverDIMSE,
					Storage: "store", DIMSE: tenant.DIMSEReceiver{Port: 20000 + round, AETitle: "ARCHIVE"},
					QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes}, cT0, e.audit(), nil)
			})
		case 2:
			what = "a storage removed and a receiver added on it"
			name := fmt.Sprintf("gone%03d", round)
			e.storage(name)
			atOnce(2, func(i int) {
				if i == 0 {
					_, errs[0] = e.stores[0].RemoveStorage(e.ctx, name, e.audit(), nil)
					return
				}
				_, errs[1] = e.stores[1].AddReceiver(e.ctx, tenant.Receiver{Name: fmt.Sprintf("on%03d", round), Type: tenant.ReceiverDIMSE,
					Storage: name, DIMSE: tenant.DIMSEReceiver{Port: 30000 + round, AETitle: "ARCHIVE"},
					QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes}, cT0, e.audit(), nil)
			})
		}
		for _, err := range errs {
			if err != nil && fault.KindOf(err) != fault.Precondition {
				t.Fatalf("round %d: %s: %v, want success or a precondition failure", round, what, err)
			}
		}
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: %s: %v and %v; want exactly one to succeed", round, what, errs[0], errs[1])
		}
	}
}

// TestStorageMoveWaitsForIdentify guards SetStorage's lock on the storage's
// row: a change of where a storage's files are, made while an identify to
// that storage is inside its transaction, waits for the identify and is
// then refused as a precondition, since a reception in progress archives
// to the storage; it never succeeds after the identify committed. The
// identify is held open in its step hook until the change is waiting, so
// the two meet in this order every run.
func TestStorageMoveWaitsForIdentify(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.receiver("a", "store", 11112)
	r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, 0), "rc-1", cT0)
	e.ok("open", err)
	_, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: cT0.Add(time.Minute)}, nil)
	e.ok("quiet", err)
	inside, release := make(chan struct{}), make(chan struct{})
	identified := make(chan error, 1)
	go func() {
		var once sync.Once
		_, err := e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify,
			At: cT0.Add(2 * time.Minute), SubjectLabel: "sub-01", SessionLabel: "ses-01", Storage: "store"},
			func(context.Context, tenant.Reception, tenant.Reception) error {
				once.Do(func() {
					close(inside)
					<-release
				})
				return nil
			})
		identified <- err
	}()
	<-inside
	moved := make(chan error, 1)
	go func() {
		_, err := e.p().SetStorage(e.ctx, tenant.Storage{Name: "store", Directory: tenant.DirectoryStorage{Path: "elsewhere"}},
			[]tenant.StorageField{tenant.FieldDirectoryPath}, cT0, e.audit(), nil)
		moved <- err
	}()
	e.waitBlocked(1)
	close(release)
	e.ok("identify", <-identified)
	if err := <-moved; fault.KindOf(err) != fault.Precondition {
		t.Fatalf("SetStorage of where the files are, during an identify to it = %v (kind %v); want a precondition failure", err, fault.KindOf(err))
	}
	s, err := e.p().Storage(e.ctx, "store")
	e.ok("Storage", err)
	if s.Directory.Path != "store" {
		t.Fatalf("the storage's path is %q after the refused change; want %q", s.Directory.Path, "store")
	}
}
