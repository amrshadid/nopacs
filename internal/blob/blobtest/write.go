package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// testPutRead guards that Put returns the size and SHA-256 of what it was
// given, and that Read and Stat give back exactly that, for sizes at and
// around the buffer sizes a driver is likely to use.
func testPutRead(e *env) {
	for i, n := range []int{1, 4095, 4096, 4097, 65536, 3<<20 + 1} {
		key := fmt.Sprintf("put/%d/object.dcm", n)
		data := PatternBytes(uint64(i), n)
		e.put(key, data)
		e.wantObject(key, data)
	}
	// The deepest key there may be.
	deep := strings.Repeat("d/", blob.MaxKeySegments-1) + "object"
	e.put(deep, []byte("deep"))
	e.wantObject(deep, []byte("deep"))
	// A reader that gives one byte at a time and a reader of unknown
	// length are the same to Put.
	data := PatternBytes(99, 1000)
	w, err := e.s.Put(e.ctx, "put/slow", io.LimitReader(&oneByte{r: bytes.NewReader(data)}, 1<<40))
	if err != nil || w.Size != 1000 || w.SHA256 != sha256.Sum256(data) {
		e.Fatalf("Put from a one-byte reader = %+v, %v", w, err)
	}
	e.wantObject("put/slow", data)
}

type oneByte struct{ r io.Reader }

func (o *oneByte) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return o.r.Read(b)
}

// testEmpty guards an object of no bytes: it exists, it is listed, its size
// is 0, and its SHA-256 is that of nothing.
func testEmpty(e *env) {
	w := e.put("empty", nil)
	if w.SHA256 != sha256.Sum256(nil) {
		e.Fatalf("SHA-256 of nothing = %x", w.SHA256)
	}
	e.wantObject("empty", nil)
	if keys, _ := e.listAll(blob.ListOptions{}); !slices.Equal(keys, []string{"empty"}) {
		e.Fatalf("List = %q, want the empty object", keys)
	}
	for _, r := range [][2]int64{{0, 0}, {0, -1}, {0, 5}} {
		if got := e.read("empty", r[0], r[1]); len(got) != 0 {
			e.Fatalf("Read(empty, %d, %d) gave %d bytes", r[0], r[1], len(got))
		}
	}
	if _, err := e.s.Read(e.ctx, "empty", 1, 0); fault.KindOf(err) != fault.Invalid {
		e.Fatalf("Read(empty, 1, 0) = %v, want Invalid (past the end)", err)
	}
}

// testRanges guards Read's ranges at every edge: the first and last byte, a
// length that runs past the end and is cut there, an offset at the end
// (empty), an offset past it and negative values (Invalid), a missing key
// (NotFound), and a stream closed before it is drained.
func testRanges(e *env) {
	const n = 10007
	data := PatternBytes(7, n)
	e.put("r/x", data)
	for _, c := range []struct{ off, length, from, to int64 }{
		{0, -1, 0, n},
		{0, 0, 0, 0},
		{0, 1, 0, 1},
		{0, n, 0, n},
		{0, n + 1000, 0, n},
		{1, -1, 1, n},
		{1, n - 2, 1, n - 1},
		{n - 1, 1, n - 1, n},
		{n - 1, -1, n - 1, n},
		{n - 1, 100, n - 1, n},
		{n, 0, n, n},
		{n, -1, n, n},
		{n, 10, n, n},
		{4096, 4096, 4096, 8192},
		{5000, 1 << 40, 5000, n},
	} {
		if got := e.read("r/x", c.off, c.length); !bytes.Equal(got, data[c.from:c.to]) {
			e.Errorf("Read(%d, %d) gave %d bytes, want bytes %d to %d", c.off, c.length, len(got), c.from, c.to)
		}
	}
	for _, c := range [][2]int64{{n + 1, 0}, {n + 1, -1}, {1 << 40, 1}, {-1, 1}, {0, -2}} {
		if r, err := e.s.Read(e.ctx, "r/x", c[0], c[1]); fault.KindOf(err) != fault.Invalid {
			if err == nil {
				closeOrFail(e.T, r)
			}
			e.Errorf("Read(%d, %d) = %v, want Invalid", c[0], c[1], err)
		}
	}
	if _, err := e.s.Read(e.ctx, "r/missing", 0, 0); fault.KindOf(err) != fault.NotFound {
		e.Errorf("Read of a missing key with length 0 = %v, want NotFound", err)
	}
	r, err := e.s.Read(e.ctx, "r/x", 0, -1)
	if err != nil {
		e.Fatal(err)
	}
	if _, err := io.ReadFull(r, make([]byte, 10)); err != nil {
		e.Fatal(err)
	}
	closeOrFail(e.T, r)
}

// testReplace guards that a Put to a key that holds an object replaces it
// whole: larger by smaller leaves no tail of the old bytes, smaller by
// larger leaves nothing short.
func testReplace(e *env) {
	big, small := PatternBytes(1, 300000), PatternBytes(2, 10)
	e.put("k", big)
	e.put("k", small)
	e.wantObject("k", small)
	e.put("k", big)
	e.wantObject("k", big)
	e.put("k", nil)
	e.wantObject("k", nil)
	if keys, _ := e.listAll(blob.ListOptions{}); !slices.Equal(keys, []string{"k"}) {
		e.Fatalf("List after replacing = %q, want one key", keys)
	}
}

// testFailedPut guards that a Put whose reader fails midway leaves nothing:
// not the key, not its prefix in a listing, and for a key that held an
// object, the old object untouched. The reader's error comes back through
// Put.
func testFailedPut(e *env) {
	for _, n := range []int64{0, 1, 70000} {
		_, err := e.s.Put(e.ctx, "fail/a/b/new", &failAfter{r: Pattern(3, 1<<20), n: n})
		if !errors.Is(err, errBroken) {
			e.Fatalf("Put from a reader that fails after %d bytes = %v, want its error", n, err)
		}
		e.wantAbsent("fail/a/b/new")
	}
	if keys, prefixes := e.listAll(blob.ListOptions{Delimiter: "/"}); len(keys)+len(prefixes) != 0 {
		e.Fatalf("after failed Puts, List shows %q %q, want nothing", keys, prefixes)
	}
	old := PatternBytes(4, 5000)
	e.put("fail/old", old)
	_, err := e.s.Put(e.ctx, "fail/old", &failAfter{r: Pattern(5, 1<<20), n: 100000})
	if !errors.Is(err, errBroken) {
		e.Fatalf("replacing Put from a failing reader = %v, want its error", err)
	}
	e.wantObject("fail/old", old)
}

// testCanceledPut guards that a Put whose context ends, before it starts
// or midway, returns an error that matches the context's and leaves the
// key as it was.
func testCanceledPut(e *env) {
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if _, err := e.s.Put(ctx, "cancel/before", Pattern(1, 100)); !errors.Is(err, context.Canceled) {
		e.Fatalf("Put with a canceled context = %v, want context.Canceled", err)
	}
	e.wantAbsent("cancel/before")

	ctx, cancel = context.WithCancel(e.ctx)
	defer cancel()
	_, err := e.s.Put(ctx, "cancel/midway", &cancelAfter{r: Pattern(2, 8<<20), n: 1 << 20, cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		e.Fatalf("Put canceled midway = %v, want context.Canceled", err)
	}
	e.wantAbsent("cancel/midway")

	old := PatternBytes(6, 1234)
	e.put("cancel/old", old)
	ctx, cancel = context.WithCancel(e.ctx)
	defer cancel()
	if _, err := e.s.Put(ctx, "cancel/old", &cancelAfter{r: Pattern(7, 8<<20), n: 1 << 20, cancel: cancel}); !errors.Is(err, context.Canceled) {
		e.Fatalf("replacing Put canceled midway = %v, want context.Canceled", err)
	}
	e.wantObject("cancel/old", old)
}

// testRefusedKeys guards that every operation refuses a bad key as Invalid
// before doing anything: Put does not read its input, and nothing is
// written.
func testRefusedKeys(e *env) {
	for _, key := range BadKeys() {
		u := &untouched{}
		if _, err := e.s.Put(e.ctx, key, u); fault.KindOf(err) != fault.Invalid {
			e.Errorf("Put(%q) = %v, want Invalid", key, err)
		}
		if u.read {
			e.Errorf("Put(%q) read its input before refusing the key", key)
		}
		if r, err := e.s.Read(e.ctx, key, 0, -1); fault.KindOf(err) != fault.Invalid {
			if err == nil {
				closeOrFail(e.T, r)
			}
			e.Errorf("Read(%q) = %v, want Invalid", key, err)
		}
		if _, err := e.s.Stat(e.ctx, key); fault.KindOf(err) != fault.Invalid {
			e.Errorf("Stat(%q) = %v, want Invalid", key, err)
		}
		if err := e.s.Delete(e.ctx, key); fault.KindOf(err) != fault.Invalid {
			e.Errorf("Delete(%q) = %v, want Invalid", key, err)
		}
	}
	if keys, _ := e.listAll(blob.ListOptions{}); len(keys) != 0 {
		e.Fatalf("after refused keys, List shows %q", keys)
	}
}

// testPrefixIsNotAnObject guards that a key that is only a prefix of
// others holds nothing: Stat and Read say NotFound, and Delete of it is a
// harmless no-op that leaves the keys under it.
func testPrefixIsNotAnObject(e *env) {
	e.put("p/q/r", []byte("x"))
	for _, k := range []string{"p", "p/q"} {
		e.wantAbsent(k)
		if err := e.s.Delete(e.ctx, k); err != nil {
			e.Fatalf("Delete(%q), a prefix only: %v", k, err)
		}
	}
	e.wantObject("p/q/r", []byte("x"))
}

// testKeyAndPrefix guards what the driver declared for a key that is also
// the prefix of another: both kept, or the second refused as Conflict with
// the first untouched, in either order.
func testKeyAndPrefix(e *env) {
	for _, pair := range [][2]string{{"kp/a", "kp/a/b"}, {"pk/a/b", "pk/a"}} {
		e.put(pair[0], []byte("first"))
		_, err := e.s.Put(e.ctx, pair[1], bytes.NewReader([]byte("second")))
		if e.d.KeyAndPrefix {
			if err != nil {
				e.Fatalf("Put(%q) after %q: %v; the driver declared it holds both", pair[1], pair[0], err)
			}
			e.wantObject(pair[1], []byte("second"))
		} else {
			if fault.KindOf(err) != fault.Conflict {
				e.Fatalf("Put(%q) after %q = %v; want Conflict", pair[1], pair[0], err)
			}
			e.wantAbsent(pair[1])
		}
		e.wantObject(pair[0], []byte("first"))
	}
}

// testDelete guards that Delete removes one object and nothing else, that
// its prefix disappears from a listing when the last key under it goes, and
// that deleting a key that holds nothing is not an error.
func testDelete(e *env) {
	e.put("d/a/1", []byte("1"))
	e.put("d/a/2", []byte("2"))
	e.put("d/b", []byte("b"))
	if err := e.s.Delete(e.ctx, "d/a/1"); err != nil {
		e.Fatal(err)
	}
	e.wantAbsent("d/a/1")
	e.wantObject("d/a/2", []byte("2"))
	if err := e.s.Delete(e.ctx, "d/a/2"); err != nil {
		e.Fatal(err)
	}
	if keys, prefixes := e.listAll(blob.ListOptions{Prefix: "d/", Delimiter: "/"}); !slices.Equal(keys, []string{"d/b"}) || len(prefixes) != 0 {
		e.Fatalf("after deleting every key under d/a/, List(d/) = %q %q; want only d/b", keys, prefixes)
	}
	for _, k := range []string{"d/a/2", "never/was", "d/b/below"} {
		if err := e.s.Delete(e.ctx, k); err != nil {
			e.Fatalf("Delete(%q), which holds nothing: %v", k, err)
		}
	}
	e.wantObject("d/b", []byte("b"))
}
