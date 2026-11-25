package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// memStore is the least of a Store PutConfirmed needs: a Put that reads its
// input to the end and keeps the object only then, as every driver does,
// and a Stat. The port's tests import no driver; the drivers run the same
// checks in the shared suite.
type memStore struct {
	blob.Store // Nil: any other method is not called here.
	mu         sync.Mutex
	m          map[string][]byte
}

func (s *memStore) Put(_ context.Context, key string, r io.Reader) (blob.Written, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return blob.Written{}, err
	}
	s.mu.Lock()
	s.m[key] = b
	s.mu.Unlock()
	return blob.Written{Size: int64(len(b)), SHA256: sha256.Sum256(b)}, nil
}

func (s *memStore) Stat(_ context.Context, key string) (blob.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[key]
	if !ok {
		return blob.Info{}, fault.New(fault.NotFound, "no object")
	}
	return blob.Info{Size: int64(len(b))}, nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
	return nil
}

// openMem returns an empty memStore.
func openMem(*testing.T) blob.Store { return &memStore{m: map[string][]byte{}} }

// TestPutConfirmedReaderShapes guards that the check holds whatever shape
// the input's reads take: one byte at a time, the last bytes arriving with
// the end of input in one call, and an input that yields nothing but its
// end. A check that missed the bytes of the call that ends the input would
// keep an object whose last bytes were never hashed.
func TestPutConfirmedReaderShapes(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	data := []byte("the bytes a replica must hold")
	sum := sha256.Sum256(data)
	changed := bytes.Clone(data)
	changed[len(changed)-1] ^= 1
	shapes := map[string]func([]byte) io.Reader{
		"one byte":      func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) },
		"data with EOF": func(b []byte) io.Reader { return iotest.DataErrReader(bytes.NewReader(b)) },
		"half reads":    func(b []byte) io.Reader { return iotest.HalfReader(bytes.NewReader(b)) },
	}
	for name, shape := range shapes {
		if _, err := blob.PutConfirmed(ctx, s, "k/"+name[:1]+"bad", shape(changed), int64(len(data)), sum); !errors.Is(err, blob.ErrNotTheBytesExpected) {
			t.Fatalf("%s: PutConfirmed of a changed last byte = %v, want ErrNotTheBytesExpected", name, err)
		}
		if _, err := s.Stat(ctx, "k/"+name[:1]+"bad"); fault.KindOf(err) != fault.NotFound {
			t.Fatalf("%s: a refused put left an object (%v)", name, err)
		}
		w, err := blob.PutConfirmed(ctx, s, "k/"+name[:1]+"good", shape(data), int64(len(data)), sum)
		if err != nil || w.SHA256 != sum || w.Size != int64(len(data)) {
			t.Fatalf("%s: PutConfirmed of the bytes expected = %+v, %v", name, w, err)
		}
	}
	empty := sha256.Sum256(nil)
	if _, err := blob.PutConfirmed(ctx, s, "k/empty", bytes.NewReader(nil), 0, empty); err != nil {
		t.Fatalf("PutConfirmed of an empty object: %v", err)
	}
}

// counting counts the bytes read from it.
type counting struct {
	r io.Reader
	n int64
}

func (c *counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestPutConfirmedStopsPastSize guards that an input longer than the size
// expected is refused at the first byte past it, without the rest being
// read: a source that runs on (a damaged length, a stream that never ends)
// must not be read to its end before it is refused.
func TestPutConfirmedStopsPastSize(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	long := &counting{r: bytes.NewReader(make([]byte, 10<<20))}
	_, err := blob.PutConfirmed(ctx, s, "k/long", long, 100, [sha256.Size]byte{})
	if fault.KindOf(err) != fault.DataLoss || !errors.Is(err, blob.ErrNotTheBytesExpected) {
		t.Fatalf("PutConfirmed of a longer input = %v, want DataLoss with ErrNotTheBytesExpected", err)
	}
	if long.n > 101 {
		t.Fatalf("PutConfirmed read %d bytes of an input it should refuse after 101", long.n)
	}
}

// liar is a store whose Put reports other bytes than it was given.
type liar struct{ blob.Store }

func (l liar) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	w, err := l.Store.Put(ctx, key, r)
	w.SHA256[0] ^= 1
	return w, err
}

// TestPutConfirmedDistrustsTheDriver guards that a driver reporting another
// checksum than that of the bytes it was handed fails PutConfirmed, as the
// server's fault, rather than passing as a confirmed copy, and that what
// such a driver kept is removed: no object it cannot vouch for stays.
func TestPutConfirmedDistrustsTheDriver(t *testing.T) {
	ctx := context.Background()
	data := []byte("abc")
	s := openMem(t)
	_, err := blob.PutConfirmed(ctx, liar{s}, "k/x", bytes.NewReader(data), 3, sha256.Sum256(data))
	if fault.KindOf(err) != fault.Internal {
		t.Fatalf("PutConfirmed through a driver that misreports = %v, want Internal", err)
	}
	if _, err := s.Stat(ctx, "k/x"); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("the object a misreporting driver kept is still there (%v)", err)
	}
}
