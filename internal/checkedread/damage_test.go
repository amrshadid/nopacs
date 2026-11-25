package checkedread_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// inStepStill fails the test unless the replica's row of the fixture's scan
// is in step with no object recorded damaged.
func inStepStill(t *testing.T, f *fixture, replica, after string) {
	t.Helper()
	row, err := f.p.ReplicaScan(f.ctx, replica, f.series)
	if err != nil || row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after %s the replica's row is %+v, %v; want in step, nothing damaged", after, row, err)
	}
}

// TestMissingMetadataFileIsNotTheReplicasDamage guards a scan's metadata
// file that neither the primary nor the replica in step holds (a scan
// archived before metadata files were written, which the replicator does
// not copy): its read fails as not found, the replica's row stays in step,
// so its instances are still served from it, and one read of the file in
// two steps (a Stat, then a Read) asks each storage once.
func TestMissingMetadataFileIsNotTheReplicasDamage(t *testing.T) {
	f := newFixture(t, 1, 5000)
	f.attach("copy1")
	f.inStep("copy1")
	s, all := f.stores()
	key, err := layout.MetadataKey(f.path)
	if err != nil {
		t.Fatal(err)
	}
	src := s.Source(checkedread.Object{Storage: "main", Key: key, SeriesUID: f.series, Path: f.path})
	if _, err := src.Stat(f.ctx); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("stat of a metadata file held nowhere: %v, want not found", err)
	}
	if _, err := src.Read(f.ctx, 0, -1); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("read of a metadata file held nowhere: %v, want not found", err)
	}
	for _, name := range []string{"main", "copy1"} {
		if r, st := all[name].asked(); r+st != 1 {
			t.Errorf("%s was asked %d reads and %d stats for one read of the file, want one in all", name, r, st)
		}
	}
	inStepStill(t, f, "copy1", "the metadata file's read")
	in := f.held(uid(1))
	if err := f.dirOf("main").Delete(f.ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	if got, o, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in); err != nil || o.Storage != "copy1" || !bytes.Equal(got, f.files[uid(1)]) {
		t.Errorf("the instance after: from %q, %v; want the replica's bytes", o.Storage, err)
	}
}

// staleCatalog answers an instance's row with another key, as the catalog
// does once a correction published a new revision after the reader read
// the old one.
type staleCatalog struct{ checkedread.Replicas }

// HeldInstance answers the row with the key of the next revision.
func (s staleCatalog) HeldInstance(ctx context.Context, p label.Path, uid string) (tenant.ScanInstance, error) {
	in, err := s.Replicas.HeldInstance(ctx, p, uid)
	in.Key += ".r2"
	return in, err
}

// TestReplicaWithoutAReplacedKeyRecordsNothing guards a read with a key the
// catalog no longer holds (a correction published a new revision, whose
// purge took the old key from the replica): the replica's not found fails
// the read as it is, and nothing is recorded against a replica that is
// right; with the catalog's key still current, the same miss is recorded.
func TestReplicaWithoutAReplacedKeyRecordsNothing(t *testing.T) {
	f := newFixture(t, 1, 5000)
	f.attach("copy1")
	f.inStep("copy1")
	in := f.held(uid(1))
	for _, name := range []string{"main", "copy1"} {
		if err := f.dirOf(name).Delete(f.ctx, in.Key); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := f.stores()
	checkedread.UseReplicas(s, staleCatalog{f.p})
	if _, _, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("a replaced key: %v, want not found", err)
	}
	inStepStill(t, f, "copy1", "a read of a replaced key")
	s2, _ := f.stores()
	if _, _, err := readAll(f.ctx, s2, checkedread.InstanceObject(in, f.series, f.path), in); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("a current key: %v, want not found", err)
	}
	if row, err := f.p.ReplicaScan(f.ctx, "copy1", f.series); err != nil || row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("a current key's miss: the row is %+v, %v; want it recorded", row, err)
	}
}

// TestReplicaNotAnsweringRecordsNothing guards a replica in step that does
// not answer: the read fails with the primary's error and nothing is
// recorded, since a storage that does not answer says nothing of what it
// holds.
func TestReplicaNotAnsweringRecordsNothing(t *testing.T) {
	f := newFixture(t, 1, 5000)
	f.attach("copy1")
	f.inStep("copy1")
	in := f.held(uid(1))
	if err := f.dirOf("main").Delete(f.ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	s, all := f.stores()
	if _, err := s.Get(f.ctx, "copy1"); err != nil {
		t.Fatal(err)
	}
	all["copy1"].set(func(s *faulty) { s.fail = notAnswering })
	if _, _, err := readAll(f.ctx, s, checkedread.InstanceObject(in, f.series, f.path), in); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("the read: %v, want the primary's not found", err)
	}
	if r, _ := all["copy1"].asked(); r != 1 {
		t.Errorf("the replica was read %d times, want once", r)
	}
	inStepStill(t, f, "copy1", "a replica not answering")
}

// TestStatRecordsNothing guards a Stat, which answers a request for no
// byte: a replica in step without the object fails it as not found and is
// not recorded; the read of the bytes that follows records it.
func TestStatRecordsNothing(t *testing.T) {
	f := newFixture(t, 1, 5000)
	f.attach("copy1")
	f.inStep("copy1")
	in := f.held(uid(1))
	for _, name := range []string{"main", "copy1"} {
		if err := f.dirOf(name).Delete(f.ctx, in.Key); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := f.stores()
	obj := checkedread.InstanceObject(in, f.series, f.path)
	if _, _, err := s.Stat(f.ctx, obj); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("the stat: %v, want not found", err)
	}
	inStepStill(t, f, "copy1", "a stat")
	if _, _, err := readAll(f.ctx, s, obj, in); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("the read: %v, want not found", err)
	}
	if row, err := f.p.ReplicaScan(f.ctx, "copy1", f.series); err != nil || row.Damaged != 1 {
		t.Errorf("after the read the row is %+v, %v; want the miss recorded", row, err)
	}
}

// TestTurnedReadsWarnOnceAMinute guards the warning of reads a replica
// serves because the primary cannot: the first is logged with the tenant,
// the primary storage and the kind of its failure, and no key; the next
// within a minute is not; the first a minute later is.
func TestTurnedReadsWarnOnceAMinute(t *testing.T) {
	f := newFixture(t, 1, 5000)
	f.attach("copy1")
	f.inStep("copy1")
	var out bytes.Buffer
	counts := &checkedread.Counts{Log: slog.New(slog.NewTextHandler(&out, nil))}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	checkedread.CountsClock(counts, func() time.Time { return at })
	in := f.held(uid(1))
	obj := checkedread.InstanceObject(in, f.series, f.path)
	read := func() {
		t.Helper()
		s, all := f.stores()
		s.WithCounts(counts)
		if _, err := s.Get(f.ctx, "main"); err != nil {
			t.Fatal(err)
		}
		all["main"].set(func(s *faulty) { s.fail = notAnswering })
		if _, o, err := readAll(f.ctx, s, obj, in); err != nil || o.Storage != "copy1" {
			t.Fatalf("served by %q, %v; want copy1", o.Storage, err)
		}
	}
	read()
	at = at.Add(30 * time.Second)
	read()
	if n := strings.Count(out.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings within a minute, want one:\n%s", n, out.String())
	}
	line := out.String()
	for _, want := range []string{"tenant=lab", "storage=main", "fault=unavailable"} {
		if !strings.Contains(line, want) {
			t.Errorf("the warning lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, in.Key) || strings.Contains(line, in.SOPInstanceUID) {
		t.Errorf("the warning names the object: %s", line)
	}
	at = at.Add(31 * time.Second)
	read()
	if n := strings.Count(out.String(), "level=WARN"); n != 2 {
		t.Errorf("%d warnings after a minute, want two:\n%s", n, out.String())
	}
}
