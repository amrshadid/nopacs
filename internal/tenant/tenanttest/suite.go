package tenanttest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Open returns a fresh, empty, migrated store for the tenant whose id is
// tenantID, in a database of its own that no other test uses, and closes it
// when tb ends (tb.Cleanup). Closing a store twice must be harmless, since a
// test may close it itself. The store binds its cursors to tenantID, so a
// page token from one tenant's list is refused by another's.
type Open func(tb testing.TB, tenantID string) tenant.Store

// SuiteTenant is the tenant id the suite opens its stores for; OtherTenant
// is a second tenant's, for the tests that need two databases.
const (
	SuiteTenant = "suite-tenant"
	OtherTenant = "other-tenant"
)

// T0 is the time the suite's data starts at: a time with microseconds, so a
// driver that drops them fails.
var T0 = time.Date(2026, 11, 18, 9, 30, 0, 123456000, time.UTC)

// runStore runs the store groups against the stores open makes. Each
// test has its own store and runs in parallel with the others.
func runStore(t *testing.T, open Open) {
	tests := []struct {
		name string
		fn   func(*env)
	}{
		{"Schema", testSchema},
		{"Closed", testClosed},
		{"Settings", testSettings},
		{"SettingsListed", testSettingsListed},
		{"Audit", testAudit},
		{"AuditFilters", testAuditFilters},
		{"AuditPaging10000", testAuditPaging10000},
		{"PageLimits", testPageLimits},
		{"BothOrNeither", testBothOrNeither},
		{"ConcurrentChanges", testConcurrentChanges},
		{"FailuresWriteNoAudit", testFailuresWriteNoAudit},
		{"CursorsBound", testCursorsBound},
		{"ErrorKinds", testErrorKinds},
		{"CloseInFlight", testCloseInFlight},
		{"ExactText", testExactText},
		{"IDsCaseAndOrder", testIDsCaseAndOrder},
		{"TimeEdges", testTimeEdges},
		{"PagingWithDeletes", testPagingWithDeletes},
		{"Ties", testTies},
		{"SizeBounds", testSizeBounds},
		{"TenantsApart", testTenantsApart},
		{"Guards", testGuards},
		{"GuardRunsLast", testGuardRunsLast},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.fn(&env{T: t, s: open(t, SuiteTenant), open: open, ctx: context.Background()})
		})
	}
	// The audit seal's rules are the master's, so the master's tests run
	// here unchanged, on the tenant's log.
	t.Run("AuditSeal", func(t *testing.T) {
		mastertest.RunAuditSeal(t, func(tb testing.TB) mastertest.SealLog { return open(tb, SuiteTenant) })
	})
}

// env is one test's store and the helpers that make its data.
type env struct {
	*testing.T
	s    tenant.Store
	open Open // opens another tenant's database, for testTenantsApart
	ctx  context.Context
	mu   sync.Mutex
	seq  int
}

// id returns a new id with the prefix, unique within the test. It is safe
// for concurrent use, since testConcurrentChanges makes ids from many
// goroutines.
func (e *env) id(prefix string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	return fmt.Sprintf("%s%06d", prefix, e.seq)
}

// audit returns a new audit entry with a unique id, for a change.
func (e *env) audit() master.AuditEntry {
	return master.AuditEntry{
		ID:            e.id("a"),
		Time:          T0,
		KeyID:         "K1234567890a",
		RemoteAddress: "192.0.2.10:50000",
		Method:        "Test",
		TenantName:    "lab",
		Outcome:       master.OutcomeDone,
		RequestID:     "r1",
	}
}

// allAudit returns every audit entry the filter keeps, newest first, by
// paging through.
func (e *env) allAudit(filter master.AuditFilter) []master.AuditEntry {
	e.Helper()
	var out []master.AuditEntry
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := e.s.ListAudit(e.ctx, filter, page)
		if err != nil {
			e.Fatalf("ListAudit: %v", err)
		}
		out = append(out, entries...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// auditIDs returns the set of every audit entry's id.
func (e *env) auditIDs() map[string]bool {
	e.Helper()
	ids := map[string]bool{}
	for _, a := range e.allAudit(master.AuditFilter{}) {
		ids[a.ID] = true
	}
	return ids
}

// wantKind fails unless err has the kind want.
func (e *env) wantKind(what string, err error, want fault.Kind) {
	e.Helper()
	if err == nil {
		e.Errorf("%s: no error, want %v", what, want)
		return
	}
	if got := fault.KindOf(err); got != want {
		e.Errorf("%s: kind %v (%v), want %v", what, got, err, want)
	}
}

// ok fails the test now when err is not nil.
func (e *env) ok(what string, err error) {
	e.Helper()
	if err != nil {
		e.Fatalf("%s: %v", what, err)
	}
}

// testSchema guards Migrate and Version: a migrated store is at a version
// above 0, migrating again changes nothing, and the store answers a ping.
func testSchema(e *env) {
	v, err := e.s.Version(e.ctx)
	e.ok("Version", err)
	if v < 1 {
		e.Errorf("Version = %d after Migrate, want at least 1", v)
	}
	e.ok("Migrate again", e.s.Migrate(e.ctx))
	again, err := e.s.Version(e.ctx)
	e.ok("Version", err)
	if again != v {
		e.Errorf("Version after a second Migrate = %d, want %d", again, v)
	}
	e.ok("Ping", e.s.Ping(e.ctx))
}

// testClosed guards that a closed store fails every call as Unavailable
// instead of panicking, and that Close may be called again: the registry
// closes a store that a request may still be holding.
func testClosed(e *env) {
	e.ok("SetSetting", e.s.SetSetting(e.ctx, "a", "b", e.audit(), nil))
	e.ok("Close", e.s.Close())
	e.ok("second Close", e.s.Close())
	_, err := e.s.Setting(e.ctx, "a")
	e.wantKind("Setting after Close", err, fault.Unavailable)
	e.wantKind("SetSetting after Close", e.s.SetSetting(e.ctx, "a", "c", e.audit(), nil), fault.Unavailable)
	e.wantKind("AppendAudit after Close", e.s.AppendAudit(e.ctx, e.audit()), fault.Unavailable)
	_, _, err = e.s.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: 1})
	e.wantKind("ListAudit after Close", err, fault.Unavailable)
	e.wantKind("Ping after Close", e.s.Ping(e.ctx), fault.Unavailable)
	e.wantKind("Migrate after Close", e.s.Migrate(e.ctx), fault.Unavailable)
}

// testSettings guards each setting operation, the name and value bounds the
// master's settings have, and that unsetting an unset name succeeds and is
// still audited.
func testSettings(e *env) {
	_, err := e.s.Setting(e.ctx, "api.limit")
	e.wantKind("Setting, unset", err, fault.NotFound)
	set := e.audit()
	e.ok("SetSetting", e.s.SetSetting(e.ctx, "api.limit", "4 MiB", set, nil))
	v, err := e.s.Setting(e.ctx, "api.limit")
	if err != nil || v != "4 MiB" {
		e.Errorf("Setting = %q, %v; want 4 MiB", v, err)
	}
	if !e.auditIDs()[set.ID] {
		e.Error("SetSetting wrote no audit entry")
	}
	e.ok("SetSetting again", e.s.SetSetting(e.ctx, "api.limit", "", e.audit(), nil))
	v, err = e.s.Setting(e.ctx, "api.limit")
	if err != nil || v != "" {
		e.Errorf("Setting = %q, %v; want the empty value, which is a value", v, err)
	}
	big := strings.Repeat("v", master.MaxSettingValueLength-len("é")) + "é"
	e.ok("SetSetting 64 KiB", e.s.SetSetting(e.ctx, "big", big, e.audit(), nil))
	v, err = e.s.Setting(e.ctx, "big")
	if err != nil || v != big {
		e.Errorf("a 64 KiB value read back with %d bytes, %v", len(v), err)
	}
	e.wantKind("SetSetting, value too big", e.s.SetSetting(e.ctx, "big", big+"x", e.audit(), nil), fault.Invalid)
	e.wantKind("SetSetting, value not UTF-8", e.s.SetSetting(e.ctx, "big", "\xc3", e.audit(), nil), fault.Invalid)
	e.wantKind("SetSetting, bad name", e.s.SetSetting(e.ctx, "API Limit", "x", e.audit(), nil), fault.Invalid)
	e.wantKind("SetSetting, long name", e.s.SetSetting(e.ctx, strings.Repeat("a", master.MaxSettingNameLength+1), "x", e.audit(), nil), fault.Invalid)
	bad := e.audit()
	bad.Outcome = "maybe"
	e.wantKind("SetSetting, bad audit entry", e.s.SetSetting(e.ctx, "fine", "x", bad, nil), fault.Invalid)
	_, err = e.s.Setting(e.ctx, "Bad Name")
	e.wantKind("Setting, bad name", err, fault.Invalid)

	removed, err := e.s.UnsetSetting(e.ctx, "api.limit", e.audit(), nil)
	if err != nil || !removed {
		e.Errorf("UnsetSetting = %v, %v; want true", removed, err)
	}
	_, err = e.s.Setting(e.ctx, "api.limit")
	e.wantKind("Setting after UnsetSetting", err, fault.NotFound)
	again := e.audit()
	removed, err = e.s.UnsetSetting(e.ctx, "api.limit", again, nil)
	if err != nil || removed {
		e.Errorf("UnsetSetting of an unset name = %v, %v; want false and success", removed, err)
	}
	if !e.auditIDs()[again.ID] {
		e.Error("unsetting an unset name wrote no audit entry; the request is still recorded")
	}
	_, err = e.s.UnsetSetting(e.ctx, "Bad Name", e.audit(), nil)
	e.wantKind("UnsetSetting, bad name", err, fault.Invalid)
}

// testSettingsListed guards the settings list: by name, paged across
// inserts before and after the position reached, each setting present
// before the listing returned exactly once.
func testSettingsListed(e *env) {
	for i := range 30 {
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("m.%02d", i), "v", e.audit(), nil))
	}
	before := map[string]bool{}
	all, _, err := e.s.ListSettings(e.ctx, master.Page{Limit: master.MaxPageLimit})
	e.ok("ListSettings", err)
	for _, s := range all {
		before[s.Name] = true
	}
	if len(before) != 30 {
		e.Fatalf("%d settings before paging, want 30", len(before))
	}
	seen := map[string]int{}
	var names []string
	page := master.Page{Limit: 4}
	for n := 1; n < 1000; n++ {
		items, next, err := e.s.ListSettings(e.ctx, page)
		e.ok("ListSettings", err)
		if len(items) > page.Limit {
			e.Fatalf("a page of %d with limit %d", len(items), page.Limit)
		}
		for _, s := range items {
			seen[s.Name]++
			names = append(names, s.Name)
		}
		if next == "" {
			break
		}
		page.Cursor = next
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("a.%02d", n), "v", e.audit(), nil))
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("z.%02d", n), "v", e.audit(), nil))
	}
	for name := range before {
		if seen[name] != 1 {
			e.Errorf("setting %q returned %d times, want once", name, seen[name])
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i] <= names[i-1] {
			e.Errorf("%q after %q, out of order", names[i], names[i-1])
		}
	}
}

// testAudit guards that an entry reads back exactly, with Unicode and every
// field at its bound, that an id is unique, and that a batch is written all
// or none and bounded.
func testAudit(e *env) {
	full := master.AuditEntry{
		ID:            e.id("a"),
		Time:          T0.Add(time.Second),
		KeyID:         "K1234567890a",
		RemoteAddress: "[2001:db8::1]:51234",
		Method:        "/nopacs.v1.SettingsService/SetSetting",
		TenantName:    "lab",
		Object:        "setting/api.limit",
		Outcome:       master.OutcomeRefused,
		Reason:        strings.Repeat("ü", master.MaxAuditReasonLength/2),
		Details:       map[string]string{},
		RequestID:     "req-0001",
	}
	for i := range master.MaxAuditDetails {
		full.Details[fmt.Sprintf("key-%02d-%s", i, strings.Repeat("k", master.MaxAuditDetailKeyLength-7))] =
			strings.Repeat("値", master.MaxAuditDetailValueLength/3)
	}
	local := e.audit()
	local.KeyID, local.LocalUser, local.RemoteAddress = "", "501", ""
	local.Details = nil
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, full, local))
	got := e.allAudit(master.AuditFilter{})
	if len(got) != 2 {
		e.Fatalf("%d entries, want 2", len(got))
	}
	if d := mastertest.DiffAudit(got[0], full); d != "" {
		e.Errorf("entry: %s", d)
	}
	if d := mastertest.DiffAudit(got[1], local); d != "" {
		e.Errorf("entry: %s", d)
	}

	e.wantDuplicateAudit("AppendAudit, id taken", e.s.AppendAudit(e.ctx, full))
	fresh := e.audit()
	e.wantDuplicateAudit("AppendAudit, batch with an id taken", e.s.AppendAudit(e.ctx, fresh, full))
	if e.auditIDs()[fresh.ID] {
		e.Error("the batch's first entry was written although the batch failed")
	}
	tooMany := e.audit()
	tooMany.Details = map[string]string{}
	for i := range master.MaxAuditDetails + 1 {
		tooMany.Details[fmt.Sprint(i)] = "v"
	}
	e.wantKind("AppendAudit, 17 details", e.s.AppendAudit(e.ctx, tooMany), fault.Invalid)
	e.ok("AppendAudit, nothing", e.s.AppendAudit(e.ctx))
	batch := make([]master.AuditEntry, master.MaxAuditBatch+1)
	for i := range batch {
		batch[i] = e.audit()
	}
	e.wantKind("AppendAudit, too many", e.s.AppendAudit(e.ctx, batch...), fault.Invalid)
}

// testAuditFilters guards each filter of the audit listing, alone and
// together, and the order newest first with the id breaking ties.
func testAuditFilters(e *env) {
	var all []master.AuditEntry
	tenants := []string{"lab", "clinic", ""}
	keys := []string{"KA", "KB", ""}
	outcomes := []master.Outcome{master.OutcomeDone, master.OutcomeRefused, master.OutcomeFailed}
	for i := range 90 {
		a := e.audit()
		a.Time = T0.Add(time.Duration(i/2) * time.Second) // pairs share a time
		a.TenantName = tenants[i%3]
		a.KeyID = keys[(i/3)%3]
		a.Outcome = outcomes[(i/9)%3]
		all = append(all, a)
	}
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, all...))

	filters := []master.AuditFilter{
		{},
		{TenantName: "lab"},
		{KeyID: "KB"},
		{Outcome: master.OutcomeRefused},
		{Since: T0.Add(10 * time.Second)},
		{Until: T0.Add(20 * time.Second)},
		{Since: T0.Add(5 * time.Second), Until: T0.Add(25 * time.Second)},
		{TenantName: "clinic", KeyID: "KA"},
		{TenantName: "lab", Outcome: master.OutcomeFailed, Since: T0.Add(30 * time.Second)},
		{KeyID: "KA", Since: T0.Add(3 * time.Second), Until: T0.Add(40 * time.Second)},
		{TenantName: "nobody"},
	}
	for _, f := range filters {
		var want []master.AuditEntry
		// The ids grow with i, so walking all backward is newest first with
		// the larger id first within a time.
		for i := len(all) - 1; i >= 0; i-- {
			a := all[i]
			if (f.TenantName == "" || a.TenantName == f.TenantName) &&
				(f.KeyID == "" || a.KeyID == f.KeyID) &&
				(f.Outcome == "" || a.Outcome == f.Outcome) &&
				(f.Since.IsZero() || !a.Time.Before(f.Since)) &&
				(f.Until.IsZero() || a.Time.Before(f.Until)) {
				want = append(want, a)
			}
		}
		var got []master.AuditEntry
		page := master.Page{Limit: 7}
		for {
			entries, next, err := e.s.ListAudit(e.ctx, f, page)
			e.ok("ListAudit", err)
			got = append(got, entries...)
			if next == "" {
				break
			}
			page.Cursor = next
		}
		if len(got) != len(want) {
			e.Errorf("filter %+v: %d entries, want %d", f, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				e.Errorf("filter %+v: entry %d is %s, want %s", f, i, got[i].ID, want[i].ID)
				break
			}
		}
	}
	_, _, err := e.s.ListAudit(e.ctx, master.AuditFilter{Since: T0, Until: T0}, master.Page{Limit: 5})
	e.wantKind("ListAudit, empty range", err, fault.Invalid)
}

// testAuditPaging10000 guards that a listing of 10,000 entries, paged, with
// entries inserted between pages before, after and at the position reached,
// returns each of the 10,000 exactly once, in order.
func testAuditPaging10000(e *env) {
	const n = 10000
	originals := make([]master.AuditEntry, n)
	for i := range originals {
		a := e.audit()
		a.ID = fmt.Sprintf("o%05d", i)
		a.Time = T0.Add(time.Duration(i/3) * time.Millisecond) // threes share a time
		originals[i] = a
	}
	e.ok("AppendAudit 10,000", e.s.AppendAudit(e.ctx, originals...))

	seen := map[string]int{}
	var order []master.AuditEntry
	page := master.Page{Limit: 97}
	for pages := 1; ; pages++ {
		entries, next, err := e.s.ListAudit(e.ctx, master.AuditFilter{}, page)
		e.ok("ListAudit", err)
		for _, a := range entries {
			seen[a.ID]++
		}
		order = append(order, entries...)
		if next == "" {
			break
		}
		page.Cursor = next
		last := entries[len(entries)-1]
		ins := []master.AuditEntry{e.audit(), e.audit(), e.audit(), e.audit()}
		ins[0].ID, ins[0].Time = fmt.Sprintf("new-%04d", pages), T0.Add(time.Hour)
		ins[1].ID, ins[1].Time = fmt.Sprintf("old-%04d", pages), T0.Add(-time.Hour)
		ins[2].ID, ins[2].Time = last.ID+"-after", last.Time
		ins[3].ID, ins[3].Time = "a"+fmt.Sprintf("%04d", pages), last.Time
		e.ok("AppendAudit between pages", e.s.AppendAudit(e.ctx, ins...))
		if pages > 2*n {
			e.Fatal("the listing does not end")
		}
	}
	for _, a := range originals {
		if seen[a.ID] != 1 {
			e.Fatalf("entry %s returned %d times, want once", a.ID, seen[a.ID])
		}
	}
	for id, count := range seen {
		if count != 1 {
			e.Errorf("entry %s returned %d times", id, count)
		}
	}
	for i := 1; i < len(order); i++ {
		p, c := order[i-1], order[i]
		if c.Time.After(p.Time) || (c.Time.Equal(p.Time) && c.ID >= p.ID) {
			e.Fatalf("entry %d (%s at %v) is not before entry %d (%s at %v)", i, c.ID, c.Time, i-1, p.ID, p.Time)
		}
	}
}

// testPageLimits guards that both lists refuse a limit of 0 or above 1000,
// and a cursor they did not make, as Invalid.
func testPageLimits(e *env) {
	for i := range 3 {
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("s%d", i), "v", e.audit(), nil))
	}
	_, settingsCursor, err := e.s.ListSettings(e.ctx, master.Page{Limit: 1})
	e.ok("ListSettings", err)
	if settingsCursor == "" {
		e.Fatal("ListSettings with more to come returned no cursor")
	}
	lists := map[string]func(master.Page) error{
		"ListSettings": func(p master.Page) error { _, _, err := e.s.ListSettings(e.ctx, p); return err },
		"ListAudit": func(p master.Page) error {
			_, _, err := e.s.ListAudit(e.ctx, master.AuditFilter{}, p)
			return err
		},
	}
	for name, list := range lists {
		e.ok(name+" limit 1", list(master.Page{Limit: 1}))
		e.ok(name+" limit 1000", list(master.Page{Limit: 1000}))
		e.wantKind(name+" limit 0", list(master.Page{Limit: 0}), fault.Invalid)
		e.wantKind(name+" limit 1001", list(master.Page{Limit: 1001}), fault.Invalid)
		e.wantKind(name+" garbage cursor", list(master.Page{Limit: 5, Cursor: "%%%not-a-cursor"}), fault.Invalid)
	}
	e.wantKind("ListAudit with the settings list's cursor", lists["ListAudit"](master.Page{Limit: 5, Cursor: settingsCursor}), fault.Invalid)
}

// testBothOrNeither guards the rule that a change and its audit entry are
// written both or neither. For every changing operation the audit entry is
// made to fail by giving it an id already taken; the operation must fail
// with master.ErrDuplicateAuditID (kind Internal) and leave the change
// unmade. A driver that writes the change
// first and the entry second, outside one transaction, fails here.
func testBothOrNeither(e *env) {
	taken := e.audit()
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, taken))
	e.ok("SetSetting", e.s.SetSetting(e.ctx, "kept", "old", e.audit(), nil))
	dup := func() master.AuditEntry {
		a := e.audit()
		a.ID = taken.ID
		return a
	}
	before := len(e.auditIDs())
	check := func(op string, err error, unchanged func() string) {
		e.Helper()
		e.wantDuplicateAudit(op+" with a taken audit id", err)
		if d := unchanged(); d != "" {
			e.Errorf("%s with a failed audit entry changed the database: %s", op, d)
		}
		if n := len(e.auditIDs()); n != before {
			e.Errorf("%s with a failed audit entry: %d audit entries, want %d", op, n, before)
		}
	}
	check("SetSetting (new)", e.s.SetSetting(e.ctx, "fresh", "v", dup(), nil), func() string {
		if _, err := e.s.Setting(e.ctx, "fresh"); fault.KindOf(err) != fault.NotFound {
			return fmt.Sprintf("the setting exists (%v)", err)
		}
		return ""
	})
	kept := func() string {
		v, err := e.s.Setting(e.ctx, "kept")
		if err != nil || v != "old" {
			return fmt.Sprintf("value %q (%v)", v, err)
		}
		return ""
	}
	check("SetSetting (replace)", e.s.SetSetting(e.ctx, "kept", "new", dup(), nil), kept)
	_, err := e.s.UnsetSetting(e.ctx, "kept", dup(), nil)
	check("UnsetSetting", err, kept)
	first := e.audit()
	check("AppendAudit (batch)", e.s.AppendAudit(e.ctx, first, dup()), func() string {
		if e.auditIDs()[first.ID] {
			return "the batch's first entry was written"
		}
		return ""
	})
}

// testConcurrentChanges guards that changes made from many goroutines at
// once are each written with their own entry: none is lost, none fails with
// a lock error, and every entry matches its change.
func testConcurrentChanges(e *env) {
	const writers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				name := fmt.Sprintf("w%d.s%02d", w, i)
				a := e.audit()
				a.Object = "setting/" + name
				errs <- e.s.SetSetting(e.ctx, name, "v", a, nil)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			e.Errorf("concurrent SetSetting: %v", err)
		}
	}
	all, next, err := e.s.ListSettings(e.ctx, master.Page{Limit: master.MaxPageLimit})
	e.ok("ListSettings", err)
	if len(all) != writers*each || next != "" {
		e.Errorf("%d settings after %d concurrent sets, want %d", len(all), writers*each, writers*each)
	}
	objects := map[string]bool{}
	for _, a := range e.allAudit(master.AuditFilter{}) {
		objects[a.Object] = true
	}
	for _, s := range all {
		if !objects["setting/"+s.Name] {
			e.Errorf("setting %s has no audit entry", s.Name)
		}
	}
}
