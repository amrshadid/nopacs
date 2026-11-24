package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// testConcurrentPuts guards that Puts racing on one key never mix: every
// Read during the race, and the object left after it, is one Put's bytes,
// whole. The objects are small and few, so the test does not depend on how
// fast the storage is, only on what it shows.
func testConcurrentPuts(e *env) {
	const writers, rounds = 4, 6
	sums := map[[sha256.Size]byte]bool{}
	versions := map[int][]byte{}
	for w := range writers {
		for r := range rounds {
			v := w*rounds + r
			versions[v] = PatternBytes(uint64(1000+v), 20000+v*997)
			sums[sha256.Sum256(versions[v])] = true
		}
	}
	e.put("race/k", versions[0])
	ctx, stop := context.WithCancel(e.ctx)
	var wg, readers sync.WaitGroup
	errs := make(chan error, writers+2)
	for w := range writers {
		wg.Go(func() {
			for r := range rounds {
				data := versions[w*rounds+r]
				got, err := e.s.Put(e.ctx, "race/k", bytes.NewReader(data))
				if err != nil {
					errs <- fmt.Errorf("put: %w", err)
					return
				}
				if got.SHA256 != sha256.Sum256(data) {
					errs <- errors.New("put returned the checksum of other bytes than it was given")
					return
				}
			}
		})
	}
	for range 2 {
		readers.Go(func() {
			for ctx.Err() == nil {
				r, err := e.s.Read(e.ctx, "race/k", 0, -1)
				if err != nil {
					errs <- fmt.Errorf("Read during the race: %w", err)
					return
				}
				b, err := io.ReadAll(r)
				_ = r.Close() // The bytes are checked below; a close error would show as a later failure.
				if err != nil {
					errs <- fmt.Errorf("reading during the race: %w", err)
					return
				}
				if !sums[sha256.Sum256(b)] {
					errs <- fmt.Errorf("a Read during the race gave %d bytes that are no Put's whole object", len(b))
					return
				}
			}
		})
	}
	wg.Wait()
	stop()
	readers.Wait()
	close(errs)
	for err := range errs {
		e.Error(err)
	}
	final := e.read("race/k", 0, -1)
	if !sums[sha256.Sum256(final)] {
		e.Fatalf("after the race the key holds %d bytes that are no Put's whole object", len(final))
	}
	if keys, _ := e.listAll(blob.ListOptions{}); !slices.Equal(keys, []string{"race/k"}) {
		e.Fatalf("List after the race = %q, want one key", keys)
	}
}

// testReadSeesOneVersion guards that a stream from Read gives the object as
// it was when Read was called, even when a Put replaces it, or a Delete
// removes it, before the stream is drained.
func testReadSeesOneVersion(e *env) {
	old, next := PatternBytes(1, 200000), PatternBytes(2, 200000)
	for _, then := range []string{"put", "delete"} {
		e.put("v/k", old)
		r, err := e.s.Read(e.ctx, "v/k", 0, -1)
		if err != nil {
			e.Fatal(err)
		}
		head := make([]byte, 1000)
		if _, err := io.ReadFull(r, head); err != nil {
			e.Fatal(err)
		}
		if then == "put" {
			e.put("v/k", next)
		} else if err := e.s.Delete(e.ctx, "v/k"); err != nil {
			e.Fatal(err)
		}
		rest, err := io.ReadAll(r)
		closeOrFail(e.T, r)
		if err != nil {
			e.Fatal(err)
		}
		if got := append(head, rest...); !bytes.Equal(got, old) {
			e.Fatalf("after a %s during the read, the stream gave %d bytes that are not the old object whole", then, len(got))
		}
	}
}

// testPutDeleteRace guards Puts and Deletes of one key racing each other:
// every call succeeds, and the key ends holding nothing or one Put's object
// whole.
func testPutDeleteRace(e *env) {
	const rounds = 20
	versions := map[[sha256.Size]byte]bool{}
	var data [][]byte
	for i := range 4 {
		b := PatternBytes(uint64(300+i), 1000+i*313)
		data = append(data, b)
		versions[sha256.Sum256(b)] = true
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8*rounds)
	for w := range 4 {
		wg.Go(func() {
			for range rounds {
				if _, err := e.s.Put(e.ctx, "pd/k", bytes.NewReader(data[w])); err != nil {
					errs <- fmt.Errorf("put: %w", err)
				}
			}
		})
		wg.Go(func() {
			for range rounds {
				if err := e.s.Delete(e.ctx, "pd/k"); err != nil {
					errs <- fmt.Errorf("delete: %w", err)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		e.Error(err)
	}
	r, err := e.s.Read(e.ctx, "pd/k", 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return
	}
	if err != nil {
		e.Fatal(err)
	}
	b, err := io.ReadAll(r)
	closeOrFail(e.T, r)
	if err != nil || !versions[sha256.Sum256(b)] {
		e.Fatalf("after the race the key holds %d bytes that are no Put's object (%v)", len(b), err)
	}
}

// testSiblingsRace guards that what one key goes through never fails or
// changes another under the same prefix: each goroutine puts, reads back,
// and deletes its own keys, some with failing readers, while the others do
// the same beside it. Every call that should succeed does, every
// acknowledged object reads back as written until its own goroutine
// deletes it, and a key that lies untouched under the prefix throughout
// stays.
func testSiblingsRace(e *env) {
	const workers, rounds = 8, 15
	keep := PatternBytes(9, 777)
	e.put("sib/a/b/keep", keep)
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds*3)
	for w := range workers {
		wg.Go(func() {
			for i := range rounds {
				key := fmt.Sprintf("sib/a/b/w%d/k%d", w, i%3)
				if w%2 == 1 {
					key = fmt.Sprintf("sib/a/w%d", w)
				}
				if i%4 == 3 {
					if _, err := e.s.Put(e.ctx, key+"x", &failAfter{r: Pattern(1, 100), n: 10}); !errors.Is(err, errBroken) {
						errs <- fmt.Errorf("a failing put: %w", err)
					}
					continue
				}
				data := PatternBytes(uint64(w*100+i), 500+i) // #nosec G115 -- small, non-negative test counters.
				if _, err := e.s.Put(e.ctx, key, bytes.NewReader(data)); err != nil {
					errs <- fmt.Errorf("put %s: %w", key, err)
					continue
				}
				r, err := e.s.Read(e.ctx, key, 0, -1)
				if err != nil {
					errs <- fmt.Errorf("an acknowledged object is gone: read %s: %w", key, err)
					continue
				}
				got, err := io.ReadAll(r)
				_ = r.Close() // Checked through the bytes.
				if err != nil || !bytes.Equal(got, data) {
					errs <- fmt.Errorf("an acknowledged object changed: %s (%w)", key, err)
				}
				if err := e.s.Delete(e.ctx, key); err != nil {
					errs <- fmt.Errorf("delete %s: %w", key, err)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		e.Error(err)
	}
	e.wantObject("sib/a/b/keep", keep)
}

// testNewParentsAtOnce guards Puts that need the same new levels of
// prefix, started together: all succeed and all are listed.
func testNewParentsAtOnce(e *env) {
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, n)
	var want []string
	for i := range n {
		key := fmt.Sprintf("np/a/b/c/k%d", i)
		want = append(want, key)
		wg.Go(func() {
			<-start
			if _, err := e.s.Put(e.ctx, key, bytes.NewReader([]byte(key))); err != nil {
				errs <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		e.Error(err)
	}
	if keys, _ := e.listAll(blob.ListOptions{Prefix: "np/"}); !slices.Equal(keys, want) {
		e.Fatalf("List(np/) = %q, want %q", keys, want)
	}
}
