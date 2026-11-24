package mastertest

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// suspendAll suspends every key, as a restore does (master.KeySuspension).
func (e *env) suspendAll() {
	e.Helper()
	ks, ok := e.s.(master.KeySuspension)
	if !ok {
		e.Fatal("the store cannot suspend keys (master.KeySuspension)")
	}
	if _, err := ks.SuspendKeys(e.ctx, T0, e.audit()); err != nil {
		e.Fatalf("SuspendKeys: %v", err)
	}
}

// suspended reports whether the key id reads as suspended.
func (e *env) suspended(id string) bool {
	e.Helper()
	k, err := e.s.Key(e.ctx, id)
	e.ok("Key "+id, err)
	return !k.Suspended.IsZero()
}

// reinstatedEntries returns the objects of the entries a reinstatement
// with audit wrote, by the number ReinstatedEntry gave each, and fails
// the test on an entry that does not read as audit made it.
func (e *env) reinstatedEntries(audit master.AuditEntry) map[int]string {
	e.Helper()
	out := map[int]string{}
	for _, a := range e.allAudit(master.AuditFilter{}) {
		rest, ok := strings.CutPrefix(a.ID, audit.ID+"-")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil {
			e.Errorf("entry %s: not numbered as ReinstatedEntry numbers", a.ID)
			continue
		}
		if a.Method != audit.Method || a.LocalUser != audit.LocalUser || a.Outcome != audit.Outcome || a.RequestID != audit.RequestID || !SameTime(a.Time, audit.Time) {
			e.Errorf("entry %s reads %+v, want the request's entry %+v with its own id and object", a.ID, a, audit)
		}
		out[n] = a.Object
	}
	return out
}

// wantEntries fails unless the entries a reinstatement with audit wrote
// name exactly the keys ids, numbered from 1 in that order.
func (e *env) wantEntries(what string, audit master.AuditEntry, ids ...string) {
	e.Helper()
	got := e.reinstatedEntries(audit)
	want := map[int]string{}
	for i, id := range ids {
		want[i+1] = id
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		e.Errorf("%s: entries %v, want %v", what, got, want)
	}
}

// keyIDs returns the ids of keys, in their order.
func keyIDs(keys []master.Key) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.ID)
	}
	return out
}

// testReinstateKeys guards reinstatement by id: only the suspended keys
// named change, they come back ordered by id, without their hash and read
// as not suspended, an id named twice counts once, a key named that is not
// suspended is neither listed nor audited, and each key reinstated has its
// own entry with its id as the object.
func testReinstateKeys(e *env) {
	a, b, c, d := e.mustKey(), e.mustKey(), e.mustKey(), e.mustKey()
	e.suspendAll()

	audit := e.audit()
	keys, n, err := e.s.ReinstateKeys(e.ctx, []string{c.ID, a.ID, c.ID}, false, audit, master.LocalOperator())
	e.ok("ReinstateKeys", err)
	if got := keyIDs(keys); n != 2 || fmt.Sprint(got) != fmt.Sprint([]string{a.ID, c.ID}) {
		e.Errorf("ReinstateKeys answered %v, %d; want [%s %s], 2", got, n, a.ID, c.ID)
	}
	for _, k := range keys {
		if !k.Suspended.IsZero() || k.Hash != nil || k.Holder != "holder of "+k.ID || !SameTime(k.Created, T0) {
			e.Errorf("a reinstated key reads %+v; want it whole, not suspended and without its hash", k)
		}
	}
	for id, want := range map[string]bool{a.ID: false, b.ID: true, c.ID: false, d.ID: true} {
		if got := e.suspended(id); got != want {
			e.Errorf("key %s suspended = %v, want %v", id, got, want)
		}
	}
	e.wantEntries("by id", audit, a.ID, c.ID)

	// a is no longer suspended: it is left alone, not listed, not audited.
	again := e.audit()
	keys, n, err = e.s.ReinstateKeys(e.ctx, []string{a.ID, b.ID}, false, again, master.LocalOperator())
	e.ok("ReinstateKeys, one not suspended", err)
	if got := keyIDs(keys); n != 1 || fmt.Sprint(got) != fmt.Sprint([]string{b.ID}) {
		e.Errorf("ReinstateKeys answered %v, %d; want [%s], 1", got, n, b.ID)
	}
	e.wantEntries("one not suspended", again, b.ID)

	none := e.audit()
	keys, n, err = e.s.ReinstateKeys(e.ctx, []string{a.ID, b.ID}, false, none, master.LocalOperator())
	e.ok("ReinstateKeys, none suspended", err)
	if len(keys) != 0 || n != 0 {
		e.Errorf("reinstating keys not suspended answered %v, %d; want nothing", keyIDs(keys), n)
	}
	e.wantEntries("none suspended", none)
	if !e.suspended(d.ID) {
		e.Error("a key never named was reinstated")
	}
}

// testReinstateAllKeys guards reinstatement of every key: every suspended
// key changes, the answer is the count alone, one entry is written per key
// in the order of their ids, and asking again changes and writes nothing.
func testReinstateAllKeys(e *env) {
	a, b := e.mustKey(), e.mustKey()
	e.suspendAll()
	c := e.mustKey() // made after the suspension: never suspended
	keys, n, err := e.s.ReinstateKeys(e.ctx, []string{a.ID}, false, e.audit(), master.LocalOperator())
	e.ok("ReinstateKeys", err)
	if n != 1 || len(keys) != 1 {
		e.Fatalf("ReinstateKeys answered %v, %d", keyIDs(keys), n)
	}

	audit := e.audit()
	keys, n, err = e.s.ReinstateKeys(e.ctx, nil, true, audit, master.LocalOperator())
	e.ok("ReinstateKeys, all", err)
	if n != 1 || len(keys) != 0 {
		e.Errorf("ReinstateKeys, all answered %v, %d; want no keys and a count of 1", keyIDs(keys), n)
	}
	for _, id := range []string{a.ID, b.ID, c.ID} {
		if e.suspended(id) {
			e.Errorf("key %s is still suspended after all were reinstated", id)
		}
	}
	e.wantEntries("all", audit, b.ID)

	e.suspendAll()
	audit = e.audit()
	_, n, err = e.s.ReinstateKeys(e.ctx, nil, true, audit, master.LocalOperator())
	e.ok("ReinstateKeys, all of three", err)
	if n != 3 {
		e.Errorf("ReinstateKeys, all answered %d; want 3", n)
	}
	e.wantEntries("all of three", audit, a.ID, b.ID, c.ID)

	none := e.audit()
	_, n, err = e.s.ReinstateKeys(e.ctx, nil, true, none, master.LocalOperator())
	e.ok("ReinstateKeys, all again", err)
	if n != 0 {
		e.Errorf("ReinstateKeys, all again answered %d; want 0", n)
	}
	e.wantEntries("all again", none)
}

// testReinstateRevoked guards that reinstating clears only the suspension:
// a revoked key stays revoked and an expired key keeps its expiry, so each
// stays refused for what it is.
func testReinstateRevoked(e *env) {
	revoked := e.mustKey()
	at := T0.Add(time.Hour)
	_, err := e.s.RevokeKey(e.ctx, revoked.ID, at, e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	expired := e.key()
	expired.Expires = T0.Add(time.Minute)
	e.ok("CreateKey", e.s.CreateKey(e.ctx, expired, e.audit(), master.LocalOperator()))
	e.suspendAll()

	keys, n, err := e.s.ReinstateKeys(e.ctx, []string{revoked.ID, expired.ID}, false, e.audit(), master.LocalOperator())
	e.ok("ReinstateKeys", err)
	if n != 2 || len(keys) != 2 {
		e.Fatalf("ReinstateKeys answered %v, %d; want both keys", keyIDs(keys), n)
	}
	for _, k := range keys {
		want := map[string]master.Key{revoked.ID: {Revoked: at}, expired.ID: {Expires: expired.Expires}}[k.ID]
		if !SameTime(k.Revoked, want.Revoked) || (k.ID == expired.ID && !SameTime(k.Expires, want.Expires)) {
			e.Errorf("the answer reads key %s revoked %v, expiring %v; want %v, %v", k.ID, k.Revoked, k.Expires, want.Revoked, want.Expires)
		}
	}
	got, err := e.s.Key(e.ctx, revoked.ID)
	e.ok("Key", err)
	if !got.Suspended.IsZero() || !SameTime(got.Revoked, at) {
		e.Errorf("the revoked key reads suspended %v, revoked %v; want not suspended and still revoked at %v", got.Suspended, got.Revoked, at)
	}
	got, err = e.s.Key(e.ctx, expired.ID)
	e.ok("Key", err)
	if !got.Suspended.IsZero() || !SameTime(got.Expires, expired.Expires) {
		e.Errorf("the expired key reads suspended %v, expiring %v; want not suspended and the same expiry", got.Suspended, got.Expires)
	}
}

// testReinstateRefused guards every refusal of ReinstateKeys, and that each
// changes nothing and writes no entry: an unknown id among known ones is
// NotFound; ids and all together or neither, more than MaxReinstateKeys ids,
// an id that is not valid, a whole key or anything longer than a key id
// given as an id (refused without repeating it), an audit id with no room
// for each key's entry,
// and the zero Actor are Invalid; a key, however high its grants, and the
// server are Denied. MaxReinstateKeys ids are accepted.
func testReinstateRefused(e *env) {
	k := e.mustKey()
	_, admin := e.adminKey()
	e.suspendAll()
	before := len(e.allAudit(master.AuditFilter{}))
	local := master.LocalOperator()

	atLimit := make([]string, 0, master.MaxReinstateKeys)
	atLimit = append(atLimit, k.ID)
	for i := 1; i < master.MaxReinstateKeys; i++ {
		atLimit = append(atLimit, fmt.Sprintf("unknown%04d", i))
	}
	long := e.audit()
	long.ID = strings.Repeat("a", master.MaxIDLength-10)
	for _, c := range []struct {
		what string
		ids  []string
		all  bool
		by   master.Actor
		ae   master.AuditEntry
		kind fault.Kind
	}{
		{"an unknown id", []string{k.ID, "unknown"}, false, local, e.audit(), fault.NotFound},
		{"1000 ids, one known", atLimit, false, local, e.audit(), fault.NotFound},
		{"1001 ids", append(atLimit, "unknown1000"), false, local, e.audit(), fault.Invalid},
		{"ids and all", []string{k.ID}, true, local, e.audit(), fault.Invalid},
		{"neither ids nor all", nil, false, local, e.audit(), fault.Invalid},
		{"an id not valid", []string{k.ID, "not valid"}, false, local, e.audit(), fault.Invalid},
		{"an audit id with no room", []string{k.ID}, false, local, long, fault.Invalid},
		{"the zero actor", []string{k.ID}, false, master.Actor{}, e.audit(), fault.Invalid},
		{"an admin key", []string{k.ID}, false, admin, e.audit(), fault.Denied},
		{"an admin key, all", nil, true, admin, e.audit(), fault.Denied},
		{"the server", nil, true, master.Server(), e.audit(), fault.Denied},
	} {
		keys, n, err := e.s.ReinstateKeys(e.ctx, c.ids, c.all, c.ae, c.by)
		e.wantKind(c.what, err, c.kind)
		if len(keys) != 0 || n != 0 {
			e.Errorf("%s: answered %v, %d alongside its error", c.what, keyIDs(keys), n)
		}
	}
	// A whole key pasted where its id goes: 64 characters an id may hold,
	// refused as Invalid without repeating it, so the secret reaches
	// neither the caller's error nor, through it, the audit log.
	whole := "nopacs_" + strings.Repeat("A", 12) + "_" + strings.Repeat("s", 44)
	for _, id := range []string{whole, "nopacs_short", strings.Repeat("k", 13)} {
		_, _, err := e.s.ReinstateKeys(e.ctx, []string{k.ID, id}, false, e.audit(), local)
		e.wantKind("a whole key or too long an id ("+id[:7]+"...)", err, fault.Invalid)
		if err != nil && strings.Contains(err.Error()+fault.Message(err), id) {
			e.Errorf("the refusal of a value given as a key id repeats it: %q", fault.Message(err))
		}
	}
	for _, a := range e.allAudit(master.AuditFilter{}) {
		if strings.Contains(fmt.Sprint(a), "ssssssssssssssssssss") {
			e.Errorf("an audit entry holds the whole key: %+v", a)
		}
	}
	if !e.suspended(k.ID) {
		e.Error("a refused reinstatement changed a key")
	}
	if after := len(e.allAudit(master.AuditFilter{})); after != before {
		e.Errorf("refused reinstatements wrote %d audit entries; want none", after-before)
	}
}

// testReinstateDuplicateAudit guards that a key's entry whose id is taken
// fails the whole reinstatement: no key changes and no entry is written,
// the both-or-neither rule of every change.
func testReinstateDuplicateAudit(e *env) {
	a, b := e.mustKey(), e.mustKey()
	e.suspendAll()
	audit := e.audit()
	taken := master.ReinstatedEntry(audit, 2, "elsewhere")
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, taken))
	_, _, err := e.s.ReinstateKeys(e.ctx, []string{a.ID, b.ID}, false, audit, master.LocalOperator())
	e.wantDuplicateAudit("ReinstateKeys, an entry's id taken", err)
	if !e.suspended(a.ID) || !e.suspended(b.ID) {
		e.Error("a reinstatement whose entry could not be written changed a key")
	}
	if got := e.reinstatedEntries(audit); len(got) != 1 || got[2] != "elsewhere" {
		e.Errorf("entries %v; want only the one written before", got)
	}
}
