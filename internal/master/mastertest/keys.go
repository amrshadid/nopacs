package mastertest

import (
	"fmt"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// testKeys guards each key operation: the normal case read back exactly,
// revocation once, and each failure kind.
func testKeys(e *env) {
	k := e.key()
	e.ok("CreateKey", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
	got, err := e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if d := diffKey(got, k); d != "" {
		e.Errorf("Key: %s", d)
	}
	never := e.key()
	never.Expires = time.Time{}
	e.ok("CreateKey, no expiry", e.s.CreateKey(e.ctx, never, e.audit(), master.LocalOperator()))
	got, err = e.s.Key(e.ctx, never.ID)
	e.ok("Key", err)
	if !got.Expires.IsZero() {
		e.Errorf("a key with no expiry reads back expiring %v", got.Expires)
	}

	dup := e.key()
	dup.ID = k.ID
	e.wantKind("CreateKey, id taken", e.s.CreateKey(e.ctx, dup, e.audit(), master.LocalOperator()), fault.Conflict)
	short := e.key()
	short.Hash = short.Hash[:16]
	e.wantKind("CreateKey, short hash", e.s.CreateKey(e.ctx, short, e.audit(), master.LocalOperator()), fault.Invalid)
	_, err = e.s.Key(e.ctx, "nope")
	e.wantKind("Key, unknown", err, fault.NotFound)

	revoked := T0.Add(time.Hour)
	at, err := e.s.RevokeKey(e.ctx, k.ID, revoked, e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	if !SameTime(at, revoked) {
		e.Errorf("RevokeKey returned %v, want %v", at, revoked)
	}
	again := e.audit()
	at, err = e.s.RevokeKey(e.ctx, k.ID, revoked.Add(time.Hour), again, master.LocalOperator())
	e.ok("RevokeKey again", err)
	if !SameTime(at, revoked) {
		e.Errorf("RevokeKey again returned %v, want the first revocation %v", at, revoked)
	}
	if !e.auditIDs()[again.ID] {
		e.Error("revoking a revoked key wrote no audit entry; the request is still recorded")
	}
	got, err = e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if !SameTime(got.Revoked, revoked) {
		e.Errorf("revoked = %v, want the first revocation %v", got.Revoked, revoked)
	}
	_, err = e.s.RevokeKey(e.ctx, "nope", revoked, e.audit(), master.LocalOperator())
	e.wantKind("RevokeKey, unknown", err, fault.NotFound)
	_, err = e.s.RevokeKey(e.ctx, never.ID, time.Time{}, e.audit(), master.LocalOperator())
	e.wantKind("RevokeKey, zero time", err, fault.Invalid)
}

// testKeyHolderUnicode guards that a holder's name is any UTF-8 up to 256
// bytes, kept exactly, and that longer or invalid UTF-8 is refused
// (master.MaxHolderLength).
func testKeyHolderUnicode(e *env) {
	holders := []string{
		"Zoë Ångström-Øster",
		"山田 太郎 (研究室)",
		"د. أمل",
		"🧠 imaging bot",
		strings.Repeat("ß", 128), // 256 bytes
	}
	for _, h := range holders {
		k := e.key()
		k.Holder = h
		e.ok("CreateKey "+h, e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
		got, err := e.s.Key(e.ctx, k.ID)
		e.ok("Key", err)
		if got.Holder != h {
			e.Errorf("holder = %q, want %q", got.Holder, h)
		}
	}
	long := e.key()
	long.Holder = strings.Repeat("ß", 128) + "x"
	e.wantKind("holder of 257 bytes", e.s.CreateKey(e.ctx, long, e.audit(), master.LocalOperator()), fault.Invalid)
	broken := e.key()
	broken.Holder = "caf\xe9"
	e.wantKind("holder not UTF-8", e.s.CreateKey(e.ctx, broken, e.audit(), master.LocalOperator()), fault.Invalid)
}

// testMarkKeyUsed guards the throttle: a thousand calls within one minute
// write once, the first; a call more than a minute after the stored time
// writes again; an unknown key writes nothing and is not an error.
func testMarkKeyUsed(e *env) {
	k := e.mustKey()
	writes := 0
	for i := range 1000 {
		at := T0.Add(time.Duration(i) * 59 * time.Millisecond) // the last is at 58.941 s
		wrote, err := e.s.MarkKeyUsed(e.ctx, k.ID, at, "")
		e.ok("MarkKeyUsed", err)
		if wrote {
			writes++
		}
	}
	if writes != 1 {
		e.Errorf("1000 calls within a minute wrote %d times, want 1", writes)
	}
	got, err := e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if !SameTime(got.LastUsed, T0) {
		e.Errorf("last used = %v, want the first call's %v", got.LastUsed, T0)
	}
	wrote, err := e.s.MarkKeyUsed(e.ctx, k.ID, T0.Add(time.Minute), "")
	e.ok("MarkKeyUsed", err)
	if wrote {
		e.Error("a call exactly one minute later wrote; the stored time must be more than a minute older")
	}
	later := T0.Add(time.Minute + time.Microsecond)
	wrote, err = e.s.MarkKeyUsed(e.ctx, k.ID, later, "")
	e.ok("MarkKeyUsed", err)
	if !wrote {
		e.Error("a call more than a minute later did not write")
	}
	got, err = e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if !SameTime(got.LastUsed, later) {
		e.Errorf("last used = %v, want %v", got.LastUsed, later)
	}
	wrote, err = e.s.MarkKeyUsed(e.ctx, "nope", T0, "")
	if err != nil || wrote {
		e.Errorf("MarkKeyUsed(unknown) = %v, %v; want false, nil", wrote, err)
	}
	_, err = e.s.MarkKeyUsed(e.ctx, k.ID, time.Time{}, "")
	e.wantKind("MarkKeyUsed, zero time", err, fault.Invalid)
}

// testMarkKeyUsedConcurrent guards that many requests with the same key at
// the same moment write its last use once.
func testMarkKeyUsedConcurrent(e *env) {
	k := e.mustKey()
	const callers = 50
	results := make(chan error, callers)
	wrote := make(chan bool, callers)
	for range callers {
		go func() {
			w, err := e.s.MarkKeyUsed(e.ctx, k.ID, T0, "")
			wrote <- w
			results <- err
		}()
	}
	writes := 0
	for range callers {
		if err := <-results; err != nil {
			e.Errorf("MarkKeyUsed: %v", err)
		}
		if <-wrote {
			writes++
		}
	}
	if writes != 1 {
		e.Errorf("%d concurrent calls wrote %d times, want 1", callers, writes)
	}
}

// testKeysListed guards the key list: by creation then id, paged across
// inserts, with creation times that tie.
func testKeysListed(e *env) {
	for i := range 40 {
		k := e.key()
		k.Created = T0.Add(time.Duration(i/4) * time.Second)
		k.Expires = T0.Add(time.Hour)
		e.ok("CreateKey", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
	}
	n := 0
	order := func(k master.Key) string {
		return fmt.Sprintf("%020d/%s", k.Created.UnixMicro(), k.ID)
	}
	checkPaging(e, "ListKeys", func(p master.Page) ([]master.Key, string, error) { return e.s.ListKeys(e.ctx, p) }, order,
		func() {
			n++
			k := e.key()
			k.Created = T0.Add(time.Duration(n%10) * time.Second)
			k.Expires = T0.Add(time.Hour)
			e.ok("CreateKey", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
		},
		40, 7)
}

// testMarkKeyUsedEdges guards the throttle at its exact edges and that the
// stored time never moves backward.
func testMarkKeyUsedEdges(e *env) {
	k := e.mustKey()
	at := func(t time.Time) bool {
		e.Helper()
		w, err := e.s.MarkKeyUsed(e.ctx, k.ID, t, "")
		e.ok("MarkKeyUsed", err)
		return w
	}
	if !at(T0) {
		e.Fatal("the first use did not write")
	}
	if at(T0.Add(-time.Hour)) || at(T0.Add(-time.Microsecond)) {
		e.Error("a use older than the stored time wrote")
	}
	if at(T0) {
		e.Error("a use at the stored time wrote")
	}
	if at(T0.Add(time.Minute)) {
		e.Error("a use exactly one minute later wrote")
	}
	if !at(T0.Add(time.Minute + time.Microsecond)) {
		e.Error("a use one minute and one microsecond later did not write")
	}
	if at(T0) {
		e.Error("after a newer use, an older one wrote")
	}
	got, err := e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if !SameTime(got.LastUsed, T0.Add(time.Minute+time.Microsecond)) {
		e.Errorf("last used = %v, want the newest use", got.LastUsed)
	}
}
