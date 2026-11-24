package blobtest

import (
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// testPurge guards Purge: the key answers not found after it and no other
// key is touched; each removal names a disposition, and a store that keeps
// no versions answers exactly one, removed; a key that holds nothing
// answers none and no error; a bad key is refused, touching nothing.
func testPurge(e *env) {
	e.put("p/a/1.dcm", []byte("one"))
	e.put("p/a/2.dcm", []byte("two"))
	e.put("p/a/1.dcm.x", []byte("near"))
	removals, err := e.s.Purge(e.ctx, "p/a/1.dcm")
	if err != nil {
		e.Fatalf("Purge: %v", err)
	}
	if len(removals) == 0 {
		e.Fatal("Purge of a held key answered no removal")
	}
	for _, r := range removals {
		switch r.Disposition {
		case blob.Removed, blob.Hidden, blob.Retained, blob.Unknown:
		default:
			e.Fatalf("Purge answered the disposition %q", r.Disposition)
		}
		if r.Disposition != blob.Retained && !r.RetainUntil.IsZero() {
			e.Fatalf("a removal %+v names a retention it was not refused for", r)
		}
	}
	if !e.d.Versioned && (len(removals) != 1 || removals[0].Disposition != blob.Removed || removals[0].Version != "") {
		e.Fatalf("Purge on a store that keeps no versions answered %+v, want one removal, removed", removals)
	}
	e.wantAbsent("p/a/1.dcm")
	e.wantObject("p/a/2.dcm", []byte("two"))
	e.wantObject("p/a/1.dcm.x", []byte("near"))
	again, err := e.s.Purge(e.ctx, "p/a/1.dcm")
	if err != nil || (!e.d.Versioned && len(again) != 0) {
		e.Fatalf("Purge of a key that holds nothing answered %+v, %v", again, err)
	}
	if _, err := e.s.Purge(e.ctx, "p/./x"); fault.KindOf(err) != fault.Invalid {
		e.Fatalf("Purge of a bad key answered %v, want Invalid", err)
	}
}
