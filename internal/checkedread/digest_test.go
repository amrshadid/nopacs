package checkedread_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// putMetadata puts b as the scan's metadata file on the storage named and
// returns its key.
func (f *fixture) putMetadata(storage string, b []byte) string {
	f.t.Helper()
	key, err := layout.MetadataKey(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.dirOf(storage).Put(f.ctx, key, bytes.NewReader(b)); err != nil {
		f.t.Fatal(err)
	}
	return key
}

// TestMetadataReadWholeFromAReplicaIsChecked guards a whole read of a
// metadata file whose digest the catalog keeps: served by a replica in
// step, the file the digest names reads to its end and another fails at
// its end with ErrOtherFile, a mismatch; served by the primary, a newer
// file (a reception's, put before its commit) reads as it is.
func TestMetadataReadWholeFromAReplicaIsChecked(t *testing.T) {
	f := newFixture(t, 1, 64)
	good := []byte("{\"the metadata file\":1}\n")
	key := f.putMetadata("main", good)
	f.attach("copy1")
	f.inStep("copy1")
	obj := checkedread.Object{Storage: "main", Key: key, SeriesUID: f.series, Path: f.path, SHA256: tenant.SHA256(sha256.Sum256(good))}
	read := func(s *checkedread.Stores) ([]byte, checkedread.Opened, error) {
		o, err := s.Open(f.ctx, obj, 0, -1)
		if err != nil {
			return nil, o, err
		}
		defer func() { _ = o.Body.Close() }()
		b, err := io.ReadAll(o.Body)
		return b, o, err
	}
	s, all := f.stores()
	all["main"].set(func(s *faulty) { s.fail = notAnswering })
	if b, o, err := read(s); err != nil || !bytes.Equal(b, good) || o.Storage != "copy1" {
		t.Fatalf("the replica's file the digest names read %q from %s, %v", b, o.Storage, err)
	}
	f.putMetadata("copy1", []byte("{\"another metadata file\":2}\n"))
	s, all = f.stores()
	all["main"].set(func(s *faulty) { s.fail = notAnswering })
	if _, _, err := read(s); !errors.Is(err, checkedread.ErrOtherFile) || !checkedread.Mismatched(err) {
		t.Errorf("another file read whole from the replica = %v; want ErrOtherFile, a mismatch", err)
	}
	newer := []byte("{\"a reception's next metadata file\":3}\n")
	f.putMetadata("main", newer)
	s, _ = f.stores()
	if b, o, err := read(s); err != nil || !bytes.Equal(b, newer) || o.Replica {
		t.Errorf("a newer file on the primary read %q (replica %v), %v; want it read as it is", b, o.Replica, err)
	}
}

// TestMetadataOtherFileTurnsToTheReplica guards a metadata file whose
// digest the catalog keeps, its storage keeping its own SHA-256: a primary
// that keeps another file turns the read, before its first byte, to a
// replica in step that keeps the catalog's; a replica in step that keeps
// another file is recorded as the replica's damage of the scan, and with
// no copy keeping the catalog's file the primary's is answered.
func TestMetadataOtherFileTurnsToTheReplica(t *testing.T) {
	f := newFixture(t, 1, 64)
	good := []byte("{\"the metadata file\":1}\n")
	digest := tenant.SHA256(sha256.Sum256(good))
	other := tenant.SHA256(sha256.Sum256([]byte("another")))
	key := f.putMetadata("main", good)
	f.recordDigest(digest)
	f.attach("copy1")
	f.inStep("copy1")
	s, all := f.stores()
	all["main"].set(func(s *faulty) { s.sum = &other })
	all["copy1"].set(func(s *faulty) { s.sum = &digest })
	obj := checkedread.Object{Storage: "main", Key: key, SeriesUID: f.series, Path: f.path, SHA256: digest}
	src := s.Source(obj)
	if _, err := src.Stat(f.ctx); err != nil {
		t.Fatalf("Stat of a metadata file the primary keeps otherwise = %v; want the replica's", err)
	}
	if o, _ := src.Opened(); o.Storage != "copy1" || !o.Replica {
		t.Errorf("the metadata file was served by %+v; want the replica in step", o)
	}

	s2, all2 := f.stores()
	all2["main"].set(func(s *faulty) { s.sum = &other })
	all2["copy1"].set(func(s *faulty) { s.sum = &other })
	src2 := s2.Source(obj)
	if _, err := src2.Stat(f.ctx); err != nil {
		t.Errorf("Stat with every copy keeping another file = %v; want the primary's file answered", err)
	}
	if o, _ := src2.Opened(); o.Storage != "main" || o.Replica {
		t.Errorf("with every copy keeping another file the read was served by %+v; want the primary", o)
	}
	row, err := f.p.ReplicaScan(f.ctx, "copy1", f.series)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("the replica keeping another metadata file is %s with %d damaged; want damaged with 1", row.State, row.Damaged)
	}
}

// recordDigest records sum as the catalog's digest of the scan's metadata
// file on "main".
func (f *fixture) recordDigest(sum tenant.SHA256) {
	f.t.Helper()
	if ok, err := f.p.RecordSidecarRewritten(f.ctx, f.series, "main", sum, time.Now()); err != nil || !ok {
		f.t.Fatalf("RecordSidecarRewritten = %v, %v", ok, err)
	}
}

// TestMetadataRewrittenSinceTheReaderReadTheCatalog guards a reader that
// holds the digest of a metadata file the catalog has since replaced (the
// file rewritten, its new digest recorded, the replica copied again): the
// replica in step holds the catalog's new file, other than the reader's
// digest, and the read records no damage of it.
func TestMetadataRewrittenSinceTheReaderReadTheCatalog(t *testing.T) {
	f := newFixture(t, 1, 64)
	old := tenant.SHA256(sha256.Sum256([]byte("{\"the metadata file\":1}\n")))
	rewritten := tenant.SHA256(sha256.Sum256([]byte("{\"the metadata file rewritten\":2}\n")))
	other := tenant.SHA256(sha256.Sum256([]byte("another")))
	key := f.putMetadata("main", []byte("{\"the metadata file\":1}\n"))
	f.recordDigest(old)
	f.attach("copy1")
	f.inStep("copy1")
	obj := checkedread.Object{Storage: "main", Key: key, SeriesUID: f.series, Path: f.path, SHA256: old}
	f.recordDigest(rewritten)
	f.inStep("copy1")
	s, all := f.stores()
	all["main"].set(func(s *faulty) { s.sum = &other })
	all["copy1"].set(func(s *faulty) { s.sum = &rewritten })
	if _, err := s.Source(obj).Stat(f.ctx); err != nil {
		t.Fatalf("Stat = %v; want the primary's file answered", err)
	}
	row, err := f.p.ReplicaScan(f.ctx, "copy1", f.series)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("the replica holding the catalog's new metadata file is %s with %d damaged; want in step with none", row.State, row.Damaged)
	}
}
