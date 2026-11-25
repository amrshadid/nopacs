package checkedread_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestPrimaryServesWithoutAskingTheDatabase guards the normal read: the
// primary serves it, the bytes are the catalog's, the storage reported is
// the primary, no replica is opened, and the database is asked nothing of
// replicas, even with a replica in step.
func TestPrimaryServesWithoutAskingTheDatabase(t *testing.T) {
	f := newFixture(t, 2, 5000)
	f.attach("copy1")
	f.inStep("copy1")
	s, all := f.stores()
	look := &countingReplicas{Replicas: f.p}
	checkedread.UseReplicas(s, look)
	in := f.held(uid(1))
	got, o, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in)
	if err != nil || !bytes.Equal(got, f.files[uid(1)]) {
		t.Fatalf("the read: %d bytes, %v", len(got), err)
	}
	if o.Storage != "main" || o.Replica || !slices.Equal(s.Served(), []string{"main"}) {
		t.Errorf("served by %q (replica %v), served list %v; want main", o.Storage, o.Replica, s.Served())
	}
	if r, st := all["copy1"].asked(); r+st != 0 || look.lookups.Load() != 0 {
		t.Errorf("the normal read asked the replica %d times and the database %d times", r+st, look.lookups.Load())
	}
}

// TestPrimaryThatCannotServeTurnsToTheReplicaInStep guards the turn to a
// replica: a primary's object missing, a storage that does not answer, an
// object not online, and an object a scrub marked damaged (the primary then
// not read at all) are each served by the replica in step, byte for byte
// the catalog's, with the replica reported and counted.
func TestPrimaryThatCannotServeTurnsToTheReplicaInStep(t *testing.T) {
	for _, c := range []struct {
		name    string
		damaged bool
		set     func(f *fixture, in tenant.ScanInstance, main *faulty)
	}{
		{name: "missing", set: func(f *fixture, in tenant.ScanInstance, _ *faulty) {
			if err := f.dirOf("main").Delete(f.ctx, in.Key); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not answering", set: func(_ *fixture, _ tenant.ScanInstance, main *faulty) {
			main.set(func(s *faulty) { s.fail = notAnswering })
		}},
		{name: "not online", set: func(_ *fixture, _ tenant.ScanInstance, main *faulty) {
			main.set(func(s *faulty) { s.fail = notOnline })
		}},
		{name: "marked damaged", damaged: true, set: func(*fixture, tenant.ScanInstance, *faulty) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, 2, 70000)
			f.attach("copy1")
			f.inStep("copy1")
			s, all := f.stores()
			counts := &checkedread.Counts{}
			s.WithCounts(counts)
			in := f.held(uid(2))
			in.Damaged = c.damaged
			obj := checkedread.InstanceObject(in, "", f.path)
			// The primary is opened first so a fault set on it applies.
			if _, err := s.Get(f.ctx, "main"); err != nil {
				t.Fatal(err)
			}
			c.set(f, in, all["main"])
			if err := s.Readable(f.ctx, obj); err != nil {
				t.Fatalf("readable: %v", err)
			}
			got, o, err := readAll(f.ctx, s, obj, in)
			if err != nil || !bytes.Equal(got, f.files[uid(2)]) {
				t.Fatalf("the read: %d bytes, %v; want the archived bytes", len(got), err)
			}
			if o.Storage != "copy1" || !o.Replica || !slices.Equal(s.Served(), []string{"copy1"}) {
				t.Errorf("served by %q (replica %v), list %v; want copy1", o.Storage, o.Replica, s.Served())
			}
			if n := counts.ReplicaReads(); len(n) != 1 || n[0] != (checkedread.Count{Tenant: "lab", Storage: "copy1", Value: 1}) {
				t.Errorf("the counts are %+v", n)
			}
			if r, _ := all["main"].asked(); c.damaged && r != 0 {
				t.Errorf("the primary of an object marked damaged was read %d times", r)
			}
		})
	}
}

// TestReplicaNotInStepIsNeverRead guards the rule that only a replica in
// step at the catalog's manifest digest now is read: a replica attached and
// not yet copied, and one in step before the scan changed, are never opened,
// and the read fails as with no replica (the primary's not found; data loss
// for an object marked damaged, refused by Readable before any read).
func TestReplicaNotInStepIsNeverRead(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(f *fixture)
	}{
		{name: "pending", make: func(f *fixture) {
			f.attach("copy1")
			f.copyTo("copy1")
		}},
		{name: "in step at an older digest", make: func(f *fixture) {
			f.attach("copy1")
			f.inStep("copy1")
			f.archive(10, 1, 3000)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, 2, 5000)
			c.make(f)
			s, all := f.stores()
			in := f.held(uid(1))
			if err := f.dirOf("main").Delete(f.ctx, in.Key); err != nil {
				t.Fatal(err)
			}
			_, _, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in)
			if fault.KindOf(err) != fault.NotFound {
				t.Errorf("the read: %v, want the primary's not found", err)
			}
			in.Damaged = true
			obj := checkedread.InstanceObject(in, f.series, f.path)
			if err := s.Readable(f.ctx, obj); !errors.Is(err, checkedread.ErrMarked) {
				t.Errorf("readable of a marked object: %v, want ErrMarked", err)
			}
			if _, _, err := readAll(f.ctx, s, obj, in); !errors.Is(err, checkedread.ErrMarked) {
				t.Errorf("the read of a marked object: %v, want ErrMarked", err)
			}
			if r, st := all["copy1"].asked(); r+st != 0 {
				t.Errorf("a replica not in step was asked %d times", r+st)
			}
		})
	}
}

// TestReplicaWithWrongBytesAbortsAndIsRecorded guards a replica whose bytes
// are not the catalog's: the checked read finds the mismatch and the answer
// is not passed whole, Mismatch records the replica's damage of the object
// (its scan's row damaged, for the replicator to copy again), and the
// count of a replica's wrong bytes rises; nothing is recorded for the
// primary's own mismatch.
func TestReplicaWithWrongBytesAbortsAndIsRecorded(t *testing.T) {
	f := newFixture(t, 2, 70000)
	f.attach("copy1")
	f.inStep("copy1")
	s, all := f.stores()
	counts := &checkedread.Counts{}
	s.WithCounts(counts)
	in := f.held(uid(1))
	if err := f.dirOf("main").Delete(f.ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(f.ctx, "copy1"); err != nil {
		t.Fatal(err)
	}
	all["copy1"].set(func(s *faulty) { s.flip = true })
	got, o, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in)
	if !errors.Is(err, checkedread.ErrChecksum) || len(got) >= len(f.files[uid(1)]) || !o.Replica {
		t.Fatalf("the read: %d bytes from %q, %v; want a checksum mismatch from the replica, cut short", len(got), o.Storage, err)
	}
	if err := s.Mismatch(f.ctx, o, err); err != nil {
		t.Fatal(err)
	}
	row, err := f.p.ReplicaScan(f.ctx, "copy1", f.series)
	if err != nil || row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("the replica's row is %+v, %v; want damaged with one object", row, err)
	}
	if n := counts.ReplicaMismatches(); len(n) != 1 || n[0].Storage != "copy1" || n[0].Value != 1 {
		t.Errorf("the mismatch counts are %+v", n)
	}
	// The primary's mismatch is the reader's to handle: nothing recorded.
	in2 := f.held(uid(2))
	if _, err := s.Get(f.ctx, "main"); err != nil {
		t.Fatal(err)
	}
	all["main"].set(func(s *faulty) { s.flip = true })
	_, o2, err := readAll(f.ctx, s, checkedread.InstanceObject(in2, f.series, f.path), in2)
	if !errors.Is(err, checkedread.ErrChecksum) || o2.Replica {
		t.Fatalf("the primary's read: %v from %q", err, o2.Storage)
	}
	if err := s.Mismatch(f.ctx, o2, err); err != nil {
		t.Fatal(err)
	}
	if row, _ := f.p.ReplicaScan(f.ctx, "copy1", f.series); row.Damaged != 1 {
		t.Errorf("the primary's mismatch changed the replica's damage to %d", row.Damaged)
	}
}

// TestBothMissingFailsAsWithoutReplica guards a read that no storage can
// serve: the primary's object and the replica's (in step) both missing
// answer the primary's not found, as with no replica, and the replica's
// missing object is recorded as its damage; with no replica at all the read
// fails the same way and nothing turns.
func TestBothMissingFailsAsWithoutReplica(t *testing.T) {
	f := newFixture(t, 2, 5000)
	in := f.held(uid(1))
	s, _ := f.stores()
	if err := f.dirOf("main").Delete(f.ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	_, alone, aloneErr := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in)
	if fault.KindOf(aloneErr) != fault.NotFound || alone.Replica {
		t.Fatalf("with no replica: %v", aloneErr)
	}
	f.attach("copy1")
	f.inStep("copy1")
	if err := f.dirOf("copy1").Delete(f.ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	s2, _ := f.stores()
	_, _, err := readAll(f.ctx, s2, checkedread.InstanceObject(in, f.series, f.path), in)
	if fault.KindOf(err) != fault.NotFound || err.Error() != aloneErr.Error() {
		t.Errorf("both missing: %v; want the error with no replica, %v", err, aloneErr)
	}
	row, rerr := f.p.ReplicaScan(f.ctx, "copy1", f.series)
	if rerr != nil || row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("the replica's row is %+v, %v; want its missing object recorded", row, rerr)
	}
}

// TestStorageNotThereIsTheStorages guards a primary whose storage cannot be
// opened (its directory gone): with no replica the read fails as the
// storage's failure (StorageFailed), not as the object missing, and with a
// replica in step the replica serves it.
func TestStorageNotThereIsTheStorages(t *testing.T) {
	f := newFixture(t, 1, 5000)
	in := f.held(uid(1))
	main := filepath.Join(f.root, "lab", "main")
	moved := main + ".moved"
	if err := os.Rename(main, moved); err != nil {
		t.Fatal(err)
	}
	s, _ := f.stores()
	_, _, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in)
	if !checkedread.StorageFailed(err) {
		t.Errorf("a storage not there, no replica: %v, want the storage's failure", err)
	}
	if err := os.Rename(moved, main); err != nil {
		t.Fatal(err)
	}
	f.attach("copy1")
	f.inStep("copy1")
	if err := os.Rename(main, moved); err != nil {
		t.Fatal(err)
	}
	s2, _ := f.stores()
	got, o, err := readAll(f.ctx, s2, checkedread.InstanceObject(in, f.series, f.path), in)
	if err != nil || o.Storage != "copy1" || !bytes.Equal(got, f.files[uid(1)]) {
		t.Errorf("a storage not there, a replica in step: %q, %v; want the replica's bytes", o.Storage, err)
	}
}

// TestReplicasAreTriedInOrderOfName guards the bounded order: with two
// replicas in step, the first by name serves; when its object is missing,
// the second does; each is opened once.
func TestReplicasAreTriedInOrderOfName(t *testing.T) {
	f := newFixture(t, 1, 5000)
	f.attach("copy1")
	f.attach("copy2")
	f.inStep("copy2")
	f.inStep("copy1")
	in := f.held(uid(1))
	if err := f.dirOf("main").Delete(f.ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	s, all := f.stores()
	if _, o, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in); err != nil || o.Storage != "copy1" {
		t.Fatalf("served by %q, %v; want copy1", o.Storage, err)
	}
	if r, _ := all["copy2"].asked(); r != 0 {
		t.Errorf("copy2 was read %d times while copy1 served", r)
	}
	if err := f.dirOf("copy1").Delete(f.ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	got, o, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in)
	if err != nil || o.Storage != "copy2" || !bytes.Equal(got, f.files[uid(1)]) {
		t.Fatalf("served by %q, %v; want copy2", o.Storage, err)
	}
	if r1, _ := all["copy1"].asked(); r1 != 2 {
		t.Errorf("copy1 was read %d times in two reads, want one each", r1)
	}
	if !slices.Equal(s.Served(), []string{"copy1", "copy2"}) {
		t.Errorf("served list %v", s.Served())
	}
}

// TestEndedReadNeverTurns guards that a read whose own context ended fails
// as it is and asks no replica.
func TestEndedReadNeverTurns(t *testing.T) {
	f := newFixture(t, 1, 5000)
	f.attach("copy1")
	f.inStep("copy1")
	s, all := f.stores()
	if _, err := s.Get(f.ctx, "main"); err != nil {
		t.Fatal(err)
	}
	all["main"].set(func(s *faulty) { s.fail = notAnswering })
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	in := f.held(uid(1))
	if _, err := s.Open(ctx, checkedread.InstanceObject(in, f.series, f.path), 0, in.Length); err == nil {
		t.Fatal("a read whose context ended was served")
	}
	if r, st := all["copy1"].asked(); r+st != 0 {
		t.Errorf("an ended read asked the replica %d times", r+st)
	}
}

// TestSourceKeepsItsStorage guards a read of several pieces: the storage its
// first read chose serves every later piece, so a piece never comes from
// another storage midway, and Stat answers from the replica when the
// primary's object is missing or not online.
func TestSourceKeepsItsStorage(t *testing.T) {
	f := newFixture(t, 1, 5000)
	f.attach("copy1")
	f.inStep("copy1")
	s, all := f.stores()
	in := f.held(uid(1))
	src := s.Source(checkedread.InstanceObject(in, f.series, f.path))
	rc, err := src.Read(f.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	all["main"].set(func(s *faulty) { s.fail = notAnswering })
	if _, err := src.Read(f.ctx, 100, 100); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a later piece: %v, want the chosen storage's failure", err)
	}
	if r, _ := all["copy1"].asked(); r != 0 {
		t.Errorf("a later piece turned to the replica (%d reads)", r)
	}
	all["main"].set(func(s *faulty) { s.fail, s.offline = nil, true })
	info, o, err := s.Stat(f.ctx, checkedread.InstanceObject(in, f.series, f.path))
	if err != nil || o.Storage != "copy1" || info.Size != in.Size {
		t.Errorf("stat of an object not online: %+v from %q, %v; want the replica's", info, o.Storage, err)
	}
	src2 := s.Source(checkedread.InstanceObject(in, f.series, f.path))
	if _, err := src2.Store().Read(f.ctx, "data/other", 0, 1); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a source's store read another key: %v", err)
	}
}
