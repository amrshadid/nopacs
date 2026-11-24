package blobtest

import (
	"bytes"
	"fmt"
	"slices"
	"sync"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// listKeys is a set of keys whose byte order is easy to get wrong: "." and
// "-" sort before "/", "0" after it, upper case before "_" and "_" before
// lower case, so a lister that walks directories in name order without care
// returns them out of order. No two differ only in case, which a volume
// that ignores case would hold as one.
var listKeys = []string{
	"a-", "a.b", "a/b", "a/c/d", "a/c/e", "a/c.x", "a0/z",
	"b/x/y/z", "data/s1/x", "data/s10/x", "data/s1-/x", "data/s2/y",
	"z", "Z_", "_u/x",
}

// putAll puts each key with its own name as its bytes.
func (e *env) putAll(keys []string) {
	for _, k := range keys {
		e.put(k, []byte(k))
	}
}

// testList guards a listing without a delimiter: every key, in byte order,
// the same whatever the page size, with and without a prefix.
func testList(e *env) {
	e.putAll(listKeys)
	want := slices.Sorted(slices.Values(listKeys))
	for _, limit := range []int{1, 2, 3, 7, 1000, 0} {
		if keys, _ := e.listAll(blob.ListOptions{Limit: limit}); !slices.Equal(keys, want) {
			e.Fatalf("List with limit %d = %q, want %q", limit, keys, want)
		}
	}
	for _, c := range []struct {
		prefix string
		want   []string
	}{
		{"a/", []string{"a/b", "a/c.x", "a/c/d", "a/c/e"}},
		{"a/c", []string{"a/c.x", "a/c/d", "a/c/e"}},
		{"a/c/", []string{"a/c/d", "a/c/e"}},
		{"a/c/d", []string{"a/c/d"}},
		{"data/s1", []string{"data/s1-/x", "data/s1/x", "data/s10/x"}},
		{"nothing", nil},
		{"a/c/d/", nil},
	} {
		for _, limit := range []int{1, 2, 0} {
			if keys, _ := e.listAll(blob.ListOptions{Prefix: c.prefix, Limit: limit}); !slices.Equal(keys, c.want) {
				e.Errorf("List(%q, limit %d) = %q, want %q", c.prefix, limit, keys, c.want)
			}
		}
	}
}

// testListDelimiter guards a listing of one level: the keys at that level
// and one prefix for each group below it, in byte order together, across
// pages of every size.
func testListDelimiter(e *env) {
	e.putAll(listKeys)
	for _, c := range []struct {
		prefix   string
		keys     []string
		prefixes []string
	}{
		{"", []string{"Z_", "a-", "a.b", "z"}, []string{"_u/", "a/", "a0/", "b/", "data/"}},
		{"a/", []string{"a/b", "a/c.x"}, []string{"a/c/"}},
		{"a/c/", []string{"a/c/d", "a/c/e"}, nil},
		{"data/", nil, []string{"data/s1-/", "data/s1/", "data/s10/", "data/s2/"}},
		{"data/s1", nil, []string{"data/s1-/", "data/s1/", "data/s10/"}},
		{"b/", nil, []string{"b/x/"}},
		{"a", []string{"a-", "a.b"}, []string{"a/", "a0/"}},
		{"q/", nil, nil},
	} {
		for _, limit := range []int{1, 2, 3, 0} {
			keys, prefixes := e.listAll(blob.ListOptions{Prefix: c.prefix, Delimiter: "/", Limit: limit})
			if !slices.Equal(keys, c.keys) || !slices.Equal(prefixes, c.prefixes) {
				e.Errorf("List(%q, /, limit %d) = %q %q; want %q %q", c.prefix, limit, keys, prefixes, c.keys, c.prefixes)
			}
		}
	}
}

// testListPartialPrefix guards that a prefix ending inside a segment
// matches every name that begins with it, at the top level and below, and
// nothing that does not.
func testListPartialPrefix(e *env) {
	e.putAll([]string{"sub-", "subject", "sub/x", "su", "s", "subz/y/z", "t"})
	keys, _ := e.listAll(blob.ListOptions{Prefix: "sub"})
	if want := []string{"sub-", "sub/x", "subject", "subz/y/z"}; !slices.Equal(keys, want) {
		e.Fatalf("List(sub) = %q, want %q", keys, want)
	}
	keys, prefixes := e.listAll(blob.ListOptions{Prefix: "sub", Delimiter: "/", Limit: 1})
	if !slices.Equal(keys, []string{"sub-", "subject"}) || !slices.Equal(prefixes, []string{"sub/", "subz/"}) {
		e.Fatalf("List(sub, /) = %q %q", keys, prefixes)
	}
}

// testListDefaultLimit guards that a limit of 0 means pages of
// blob.MaxListLimit, and that a listing longer than one page continues on
// the next.
func testListDefaultLimit(e *env) {
	const n = blob.MaxListLimit + 3
	var wg sync.WaitGroup
	errs := make(chan error, n)
	sem := make(chan struct{}, 8)
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if _, err := e.s.Put(e.ctx, fmt.Sprintf("many/k%05d", i), bytes.NewReader(nil)); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		e.Fatal(err)
	}
	p, err := e.s.List(e.ctx, blob.ListOptions{Prefix: "many/"})
	if err != nil {
		e.Fatal(err)
	}
	if len(p.Keys) != blob.MaxListLimit || p.Next == "" {
		e.Fatalf("first page holds %d keys (next %q), want %d and more", len(p.Keys), p.Next, blob.MaxListLimit)
	}
	p, err = e.s.List(e.ctx, blob.ListOptions{Prefix: "many/", Token: p.Next})
	if err != nil {
		e.Fatal(err)
	}
	if want := []string{"many/k01000", "many/k01001", "many/k01002"}; !slices.Equal(p.Keys, want) || p.Next != "" {
		e.Fatalf("second page = %q (next %q), want %q and the end", p.Keys, p.Next, want)
	}
	if keys, _ := e.listAll(blob.ListOptions{Prefix: "many/", Delimiter: "/"}); len(keys) != n {
		e.Fatalf("List(many/, /) = %d keys, want %d", len(keys), n)
	}
}

// testListStable guards that a listing continues exactly where the page
// before ended while keys change: a key put behind that point is not seen,
// one put ahead of it is, and one deleted ahead of it is not, with and
// without a delimiter.
func testListStable(e *env) {
	for _, delim := range []string{"", "/"} {
		prefix := "stable" + map[string]string{"": "flat", "/": "level"}[delim] + "/"
		var keys []string
		for i := range 20 {
			keys = append(keys, fmt.Sprintf("%sk%02d", prefix, i))
		}
		e.putAll(keys)
		opts := blob.ListOptions{Prefix: prefix, Delimiter: delim, Limit: 5}
		p, err := e.s.List(e.ctx, opts)
		if err != nil {
			e.Fatal(err)
		}
		if !slices.Equal(p.Keys, keys[:5]) {
			e.Fatalf("first page = %q", p.Keys)
		}
		e.put(prefix+"k02a", nil)
		e.put(prefix+"k10a", nil)
		if err := e.s.Delete(e.ctx, prefix+"k07"); err != nil {
			e.Fatal(err)
		}
		opts.Token = p.Next
		rest, _ := e.listAll(opts)
		want := append(slices.Clone(keys[5:7]), keys[8:11]...)
		want = append(want, prefix+"k10a")
		want = append(want, keys[11:]...)
		if !slices.Equal(rest, want) {
			e.Fatalf("delimiter %q: the rest = %q, want %q", delim, rest, want)
		}
	}
}

// testListRefused guards that List refuses a bad prefix, delimiter, limit
// or token as Invalid.
func testListRefused(e *env) {
	e.put("x/y", nil)
	for _, opts := range []blob.ListOptions{
		{Prefix: "/x"},
		{Prefix: "../"},
		{Prefix: "x//"},
		{Prefix: `x\`},
		{Delimiter: "|"},
		{Limit: -1},
		{Limit: blob.MaxListLimit + 1},
		{Token: "\x00 not a token"},
	} {
		if _, err := e.s.List(e.ctx, opts); fault.KindOf(err) != fault.Invalid {
			e.Errorf("List(%+v) = %v, want Invalid", opts, err)
		}
	}
}

// testNextOnlyWhenMore guards that a page carries a Next only when another
// entry follows it: a last page that is exactly full, with or without a
// delimiter, ends the listing.
func testNextOnlyWhenMore(e *env) {
	e.putAll([]string{"n/a", "n/b", "n/c", "n/d/x", "n/e/y", "n/f"})
	for _, c := range []struct {
		delim string
		limit int
		pages int
	}{
		{"", 6, 1}, {"", 3, 2}, {"", 2, 3}, {"", 1, 6},
		{"/", 6, 1}, {"/", 3, 2}, {"/", 2, 3}, {"/", 1, 6},
	} {
		opts := blob.ListOptions{Prefix: "n/", Delimiter: c.delim, Limit: c.limit}
		for page := 1; ; page++ {
			p, err := e.s.List(e.ctx, opts)
			if err != nil {
				e.Fatal(err)
			}
			if page == c.pages {
				if p.Next != "" {
					e.Fatalf("delimiter %q, limit %d: the last page, exactly full, has Next %q", c.delim, c.limit, p.Next)
				}
				break
			}
			if p.Next == "" {
				e.Fatalf("delimiter %q, limit %d: the listing ended at page %d, want %d pages", c.delim, c.limit, page, c.pages)
			}
			opts.Token = p.Next
		}
	}
}
