package tenanttest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
)

// This file holds the tests that pin what a second driver could otherwise
// get subtly wrong and still pass, the tenant port's share of the master
// suite's guards (mastertest/guards.go): cursors bound to their list and
// filter, error kinds, both or neither for failures, byte-exact text, the
// edges of time and of every size bound, paging across deletes, ties, and
// two instances on one database.

// wantDuplicateAudit fails unless err is master.ErrDuplicateAuditID of kind
// Internal: a duplicate audit id is a caller's bug, not a Conflict.
func (e *env) wantDuplicateAudit(what string, err error) {
	e.Helper()
	if !errors.Is(err, master.ErrDuplicateAuditID) || fault.KindOf(err) != fault.Internal {
		e.Errorf("%s: %v (kind %v), want ErrDuplicateAuditID of kind Internal", what, err, fault.KindOf(err))
	}
}

// altered returns c with one character in its middle changed.
func altered(c string) string {
	i := len(c) / 2
	r := byte('A')
	if c[i] == 'A' {
		r = 'B'
	}
	return c[:i] + string(r) + c[i+1:]
}

// testFailuresWriteNoAudit guards both or neither in the other direction:
// for every failure of every changing operation, the audit entry it was
// handed is absent afterwards.
func testFailuresWriteNoAudit(e *env) {
	big := strings.Repeat("v", master.MaxSettingValueLength+1)
	calls := []struct {
		name string
		run  func(a master.AuditEntry) error
	}{
		{"SetSetting, bad name", func(a master.AuditEntry) error { return e.s.SetSetting(e.ctx, "Bad Name", "v", a, nil) }},
		{"SetSetting, value too big", func(a master.AuditEntry) error { return e.s.SetSetting(e.ctx, "fine", big, a, nil) }},
		{"SetSetting, value not UTF-8", func(a master.AuditEntry) error { return e.s.SetSetting(e.ctx, "fine", "\xff", a, nil) }},
		{"UnsetSetting, bad name", func(a master.AuditEntry) error { _, err := e.s.UnsetSetting(e.ctx, "Bad Name", a, nil); return err }},
		{"AppendAudit, an invalid entry in the batch", func(a master.AuditEntry) error {
			bad := e.audit()
			bad.Outcome = "maybe"
			return e.s.AppendAudit(e.ctx, a, bad)
		}},
	}
	for _, c := range calls {
		a := e.audit()
		e.wantKind(c.name, c.run(a), fault.Invalid)
		if e.auditIDs()[a.ID] {
			e.Errorf("%s failed but wrote its audit entry", c.name)
		}
	}
	if _, err := e.s.Setting(e.ctx, "fine"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a refused SetSetting left a value (%v)", err)
	}
}

// testCursorsBound guards that a cursor is accepted only by the list, and
// the filter, it was made for: the other list, an audit filter with any
// field changed, an altered cursor and a cut one are all Invalid (the API
// contract's page token rule).
func testCursorsBound(e *env) {
	for i := range 3 {
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("s%d", i), "v", e.audit(), nil))
	}
	filter := master.AuditFilter{TenantName: "lab", KeyID: "KA", Outcome: master.OutcomeDone, Since: T0, Until: T0.Add(time.Hour)}
	for range 3 {
		a := e.audit()
		a.KeyID = "KA"
		e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a))
	}
	page := func(c string) master.Page { return master.Page{Limit: 1, Cursor: c} }
	lists := map[string]func(cursor string) (string, error){
		"ListSettings": func(c string) (string, error) { _, n, err := e.s.ListSettings(e.ctx, page(c)); return n, err },
		"ListAudit":    func(c string) (string, error) { _, n, err := e.s.ListAudit(e.ctx, filter, page(c)); return n, err },
	}
	cursors := map[string]string{}
	for name, call := range lists {
		c, err := call("")
		e.ok(name, err)
		if c == "" {
			e.Fatalf("%s returned no cursor with more to come", name)
		}
		cursors[name] = c
		if _, err := call(c); err != nil {
			e.Errorf("%s refuses its own cursor: %v", name, err)
		}
		_, err = call(altered(c))
		e.wantKind(name+" with an altered cursor", err, fault.Invalid)
		_, err = call(c[:len(c)-1])
		e.wantKind(name+" with a cut cursor", err, fault.Invalid)
	}
	_, err := lists["ListAudit"](cursors["ListSettings"])
	e.wantKind("ListAudit with the settings list's cursor", err, fault.Invalid)
	_, err = lists["ListSettings"](cursors["ListAudit"])
	e.wantKind("ListSettings with the audit list's cursor", err, fault.Invalid)

	changed := map[string]func(*master.AuditFilter){
		"tenant":     func(f *master.AuditFilter) { f.TenantName = "clinic" },
		"key":        func(f *master.AuditFilter) { f.KeyID = "KB" },
		"outcome":    func(f *master.AuditFilter) { f.Outcome = master.OutcomeFailed },
		"start time": func(f *master.AuditFilter) { f.Since = T0.Add(-time.Second) },
		"end time":   func(f *master.AuditFilter) { f.Until = T0.Add(2 * time.Hour) },
		"no filter":  func(f *master.AuditFilter) { *f = master.AuditFilter{} },
	}
	for name, change := range changed {
		f := filter
		change(&f)
		_, _, err := e.s.ListAudit(e.ctx, f, page(cursors["ListAudit"]))
		e.wantKind("ListAudit cursor with another "+name, err, fault.Invalid)
	}
}

// testErrorKinds guards the kinds of the failures a caller acts on, and that
// a call that returned an error committed nothing: a context already
// canceled, a deadline already passed, and contexts canceled while calls
// are in flight.
func testErrorKinds(e *env) {
	canceled, cancel := context.WithCancel(e.ctx)
	cancel()
	a := e.audit()
	err := e.s.SetSetting(canceled, "early", "v", a, nil)
	e.wantKind("SetSetting with a canceled context", err, fault.Unavailable)
	if !errors.Is(err, context.Canceled) {
		e.Errorf("SetSetting with a canceled context: %v does not keep context.Canceled", err)
	}
	_, err = e.s.Setting(canceled, "anything")
	e.wantKind("Setting with a canceled context", err, fault.Unavailable)
	_, _, err = e.s.ListAudit(canceled, master.AuditFilter{}, master.Page{Limit: 5})
	e.wantKind("ListAudit with a canceled context", err, fault.Unavailable)
	_, _, err = e.s.ListSettings(canceled, master.Page{Limit: 5})
	e.wantKind("ListSettings with a canceled context", err, fault.Unavailable)

	past, cancelPast := context.WithDeadline(e.ctx, time.Now().Add(-time.Second))
	defer cancelPast()
	a2 := e.audit()
	err = e.s.AppendAudit(past, a2)
	e.wantKind("AppendAudit past its deadline", err, fault.Unavailable)
	if !errors.Is(err, context.DeadlineExceeded) {
		e.Errorf("AppendAudit past its deadline: %v does not keep context.DeadlineExceeded", err)
	}
	if _, err := e.s.Setting(e.ctx, "early"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("the setting of the canceled SetSetting exists (%v)", err)
	}
	ids := e.auditIDs()
	if ids[a.ID] || ids[a2.ID] {
		e.Error("an audit entry of a call that failed was written")
	}

	// Contexts canceled at varied moments: whatever each call returned, it
	// committed exactly when it returned nil.
	type result struct {
		name  string
		entry master.AuditEntry
		err   error
	}
	results := make([]result, 60)
	var wg sync.WaitGroup
	for i := range results {
		results[i].name, results[i].entry = fmt.Sprintf("raced.%02d", i), e.audit()
		wg.Go(func() {
			ctx, cancel := context.WithCancel(e.ctx)
			go func() { time.Sleep(time.Duration(i%6) * 300 * time.Microsecond); cancel() }()
			results[i].err = e.s.SetSetting(ctx, results[i].name, "v", results[i].entry, nil)
			cancel()
		})
	}
	wg.Wait()
	ids = e.auditIDs()
	for _, r := range results {
		_, serr := e.s.Setting(e.ctx, r.name)
		committed := serr == nil
		if r.err != nil && fault.KindOf(r.err) != fault.Unavailable {
			e.Errorf("SetSetting under a canceled context: kind %v (%v), want Unavailable", fault.KindOf(r.err), r.err)
		}
		if committed != (r.err == nil) || ids[r.entry.ID] != (r.err == nil) {
			e.Errorf("SetSetting returned %v but the setting present = %v, its audit entry present = %v", r.err, committed, ids[r.entry.ID])
		}
	}
}

// testCloseInFlight guards that calls in flight while the store is closed
// fail as Unavailable, never with another kind or a panic: the registry
// closes a tenant's store that requests may still be using.
func testCloseInFlight(e *env) {
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	stop := make(chan struct{})
	for i := range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				var err error
				if i%2 == 0 {
					_, _, err = e.s.ListSettings(e.ctx, master.Page{Limit: 10})
				} else {
					err = e.s.SetSetting(e.ctx, "busy", "v", e.audit(), nil)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	e.ok("Close", e.s.Close())
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		if fault.KindOf(err) != fault.Unavailable {
			e.Errorf("a call in flight during Close: kind %v (%v), want Unavailable", fault.KindOf(err), err)
		}
	}
}

// testIDsCaseAndOrder guards that the tenant port's ordered and compared text
// is kept whole, by case, and listed in byte order: the audit entries' ids
// (A-Z, a-z, 0-9, "_" and "-") and key ids that differ only in case or mix it
// with "_" and "-", and the setting names ("a-z", 0-9, "." "_" and "-", which
// the name rule allows in lowercase only) that mix those three. A database
// that orders text by a locale's collation puts "a1" before "a-b" and "B"
// after "a", and fails here. Every list is read whole and paged by small pages,
// since the cursor compares text too.
func testIDsCaseAndOrder(e *env) {
	// Audit entry ids, all at one time, so the list orders them by id alone,
	// newest first: the larger id first.
	ids := []string{"a_b", "a-b", "A-b", "B", "a", "_x", "-x", "Z", "z9", "Z_", "AB", "Ab", "aB"}
	var batch []master.AuditEntry
	for _, id := range ids {
		a := e.audit()
		a.ID = id
		batch = append(batch, a)
	}
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, batch...))
	want := slices.Clone(ids)
	slices.Sort(want) // byte order
	slices.Reverse(want)
	var got []string
	for _, a := range e.allAudit(master.AuditFilter{}) {
		got = append(got, a.ID)
	}
	if !slices.Equal(got, want) {
		e.Errorf("audit ids in order %q, want descending byte order %q", got, want)
	}
	got = got[:0]
	for page := (master.Page{Limit: 3}); ; {
		entries, next, err := e.s.ListAudit(e.ctx, master.AuditFilter{}, page)
		e.ok("ListAudit", err)
		for _, a := range entries {
			got = append(got, a.ID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if !slices.Equal(got, want) {
		e.Errorf("audit ids paged by 3 in order %q, want %q", got, want)
	}

	// Key ids in the entries: compared whole and by case by the key filter.
	keys := []string{"CaseKey", "casekey", "Case_Key", "case-key", "CASEKEY"}
	for _, k := range keys {
		a := e.audit()
		a.KeyID = k
		e.ok("AppendAudit "+k, e.s.AppendAudit(e.ctx, a))
	}
	for _, k := range keys {
		entries := e.allAudit(master.AuditFilter{KeyID: k})
		if len(entries) != 1 || entries[0].KeyID != k {
			e.Errorf("filter on key %q matched %d entries, want exactly its own", k, len(entries))
		}
	}
	for _, k := range []string{"caseKey", "CaseKe", "CaseKeyX"} {
		if n := len(e.allAudit(master.AuditFilter{KeyID: k})); n != 0 {
			e.Errorf("filter on key %q matched %d entries; ids are compared whole and by case", k, n)
		}
	}

	// Setting names mixing ".", "-" and "_", each with a value of its own.
	names := []string{"a.b", "a-b", "a_b", "a0", "ab", "a", "a.", "a-", "a_", ".a", "-a", "_a", "a.-_b", "a_-.b", "0a"}
	for i, n := range names {
		e.ok("SetSetting "+n, e.s.SetSetting(e.ctx, n, fmt.Sprintf("value-%d", i), e.audit(), nil))
	}
	for i, n := range names {
		if v, err := e.s.Setting(e.ctx, n); err != nil || v != fmt.Sprintf("value-%d", i) {
			e.Errorf("Setting(%q) = %q, %v; want its own value", n, v, err)
		}
	}
	wantNames := slices.Clone(names)
	slices.Sort(wantNames)
	for _, limit := range []int{master.MaxPageLimit, 2} {
		got = got[:0]
		for page := (master.Page{Limit: limit}); ; {
			list, next, err := e.s.ListSettings(e.ctx, page)
			e.ok("ListSettings", err)
			for _, st := range list {
				got = append(got, st.Name)
			}
			if next == "" {
				break
			}
			page.Cursor = next
		}
		if !slices.Equal(got, wantNames) {
			e.Errorf("settings paged by %d in order %q, want byte order %q", limit, got, wantNames)
		}
	}
}

// testExactText guards that text is kept and matched byte for byte: the
// audit tenant-name filter is exact (case, spaces, Unicode normal forms),
// and a setting's value comes back exactly.
func testExactText(e *env) {
	const nfc, nfd = "l\u00e4b", "la\u0308b" // the same text to a reader, different bytes
	names := []string{"lab", "Lab", "lab ", " lab", nfc, nfd}
	for _, n := range names {
		a := e.audit()
		a.TenantName = n
		e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a))
	}
	for _, n := range names {
		got := e.allAudit(master.AuditFilter{TenantName: n})
		if len(got) != 1 || got[0].TenantName != n {
			var seen []string
			for _, a := range got {
				seen = append(seen, a.TenantName)
			}
			e.Errorf("filter %q matched %q, want exactly that name once", n, seen)
		}
	}
	for i, v := range []string{"value  ", "  value", "Zo\u00e9", "Zoe\u0301", "tab\there", "line\nbreak"} {
		name := fmt.Sprintf("exact.%d", i)
		e.ok("SetSetting", e.s.SetSetting(e.ctx, name, v, e.audit(), nil))
		got, err := e.s.Setting(e.ctx, name)
		if err != nil || got != v {
			e.Errorf("value %q came back as %q (%v)", v, got, err)
		}
	}
}

// testTimeEdges guards the first and the last time the store keeps, read back
// exactly, and their order in the audit list.
func testTimeEdges(e *env) {
	first := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	a1, a2 := e.audit(), e.audit()
	a1.Time, a2.Time = first, last
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a1, a2))
	all := e.allAudit(master.AuditFilter{})
	if len(all) != 2 || !mastertest.SameTime(all[0].Time, last) || !mastertest.SameTime(all[1].Time, first) {
		e.Fatalf("audit entries at the edges of time read back as %+v", all)
	}
	since := e.allAudit(master.AuditFilter{Since: last})
	if len(since) != 1 || since[0].ID != a2.ID {
		e.Errorf("entries since the last time = %d, want the one at it", len(since))
	}
}

// testSizeBounds guards every size bound through the store: a value at
// exactly its bound is stored and read back, and one byte over is Invalid.
func testSizeBounds(e *env) {
	name := strings.Repeat("s", master.MaxSettingNameLength)
	value := strings.Repeat("v", master.MaxSettingValueLength)
	e.ok("SetSetting at both bounds", e.s.SetSetting(e.ctx, name, value, e.audit(), nil))
	if got, err := e.s.Setting(e.ctx, name); err != nil || got != value {
		e.Errorf("a setting at both bounds read back with %d bytes (%v)", len(got), err)
	}
	e.wantKind("SetSetting, name one byte over", e.s.SetSetting(e.ctx, name+"s", "v", e.audit(), nil), fault.Invalid)
	e.wantKind("SetSetting, value one byte over", e.s.SetSetting(e.ctx, "x", value+"v", e.audit(), nil), fault.Invalid)

	a := e.audit()
	a.ID = strings.Repeat("A", master.MaxIDLength)
	a.KeyID = ""
	a.Method = strings.Repeat("m", master.MaxAuditFieldLength)
	a.Object = strings.Repeat("o", master.MaxAuditFieldLength)
	a.RemoteAddress = strings.Repeat("r", master.MaxAuditFieldLength)
	a.RequestID = strings.Repeat("q", master.MaxAuditFieldLength)
	a.TenantName = strings.Repeat("t", master.MaxAuditFieldLength)
	a.LocalUser = strings.Repeat("u", master.MaxAuditFieldLength)
	a.Reason = strings.Repeat("x", master.MaxAuditReasonLength)
	e.ok("AppendAudit at every bound", e.s.AppendAudit(e.ctx, a))
	all := e.allAudit(master.AuditFilter{TenantName: a.TenantName})
	if len(all) != 1 || mastertest.DiffAudit(all[0], a) != "" {
		e.Errorf("audit entry at every bound did not read back exactly")
	}
	fields := map[string]func(*master.AuditEntry){
		"id":             func(x *master.AuditEntry) { x.ID += "A" },
		"method":         func(x *master.AuditEntry) { x.Method += "m" },
		"object":         func(x *master.AuditEntry) { x.Object += "o" },
		"remote address": func(x *master.AuditEntry) { x.RemoteAddress += "r" },
		"request id":     func(x *master.AuditEntry) { x.RequestID += "q" },
		"tenant name":    func(x *master.AuditEntry) { x.TenantName += "t" },
		"local user":     func(x *master.AuditEntry) { x.LocalUser += "u" },
		"reason":         func(x *master.AuditEntry) { x.Reason += "x" },
	}
	for name, change := range fields {
		x := a
		if name != "id" {
			x.ID = e.id("a")
		}
		change(&x)
		e.wantKind("AppendAudit, "+name+" one byte over", e.s.AppendAudit(e.ctx, x), fault.Invalid)
	}
}
