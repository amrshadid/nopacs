package tenanttest

import (
	"context"
	"errors"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// entry returns an import entry of the table's key n and code.
func entry(n int, code string) tenant.LookupEntry {
	return tenant.LookupEntry{Key: lookupKey("t", n).Key, Code: code}
}

// testLookupImport guards an import: its codes are what LookupCode
// answers; an entry given twice with its code counts once; importing
// again counts each unchanged and stores nothing new; the audit entry
// carries the counts and neither a key nor a code; a table assigning in
// order afterwards passes over an imported code; the list answers each
// table's count.
func testLookupImport(e *penv) {
	got, err := e.p.ImportLookupCodes(e.ctx, "cohort", []tenant.LookupEntry{entry(1, "P001"), entry(2, "2"), entry(1, "P001")}, at(1), e.audit(), nil)
	e.ok("ImportLookupCodes", err)
	if got != (tenant.LookupImport{Added: 2, Codes: 2}) {
		e.Errorf("the first import: %+v", got)
	}
	for n, want := range map[int]string{1: "P001", 2: "2"} {
		code, found, err := e.p.LookupCode(e.ctx, tenant.LookupKey{Table: "cohort", Key: lookupKey("t", n).Key})
		if err != nil || !found || code != want {
			e.Errorf("LookupCode of entry %d = %q, %v, %v; want %q", n, code, found, err, want)
		}
	}
	got, err = e.p.ImportLookupCodes(e.ctx, "cohort", []tenant.LookupEntry{entry(2, "2"), entry(3, "P003")}, at(2), e.audit(), nil)
	e.ok("ImportLookupCodes again", err)
	if got != (tenant.LookupImport{Added: 1, Unchanged: 1, Codes: 3}) {
		e.Errorf("the second import: %+v", got)
	}
	codes, err := e.p.AssignLookupCodes(e.ctx, []tenant.LookupKey{{Table: "cohort", Key: lookupKey("t", 9).Key}}, at(3))
	e.ok("AssignLookupCodes after an import", err)
	if len(codes) != 1 || codes[0] != "1" {
		e.Errorf("the first code assigned after an import: %q", codes)
	}
	codes, err = e.p.AssignLookupCodes(e.ctx, []tenant.LookupKey{{Table: "cohort", Key: lookupKey("t", 10).Key}}, at(3))
	e.ok("AssignLookupCodes after an import", err)
	if len(codes) != 1 || codes[0] != "3" {
		e.Errorf("the code assigned after the imported 2: %q, want 3", codes)
	}
	entries := e.allAudit(master.AuditFilter{})
	if len(entries) != 2 {
		e.Fatalf("%d audit entries, want one per import", len(entries))
	}
	for i, want := range []string{"2 1 1", "2 2 0"} { // newest first
		d := entries[i].Details
		if got := d[tenant.DetailLookupEntries] + " " + d[tenant.DetailLookupAdded] + " " + d[tenant.DetailLookupUnchanged]; got != want {
			e.Errorf("audit entry %d: %v, want %s", i, d, want)
		}
		for _, v := range d {
			if strings.Contains(v, "P00") || strings.Contains(v, lookupKey("t", 1).Key) {
				e.Errorf("audit entry %d carries a code or a key: %v", i, d)
			}
		}
	}
	_, err = e.p.ImportLookupCodes(e.ctx, "other", []tenant.LookupEntry{entry(1, "X")}, at(4), e.audit(), nil)
	e.ok("ImportLookupCodes into another table", err)
	tables, next, err := e.p.ListLookupTables(e.ctx, master.Page{Limit: 1})
	e.ok("ListLookupTables", err)
	if len(tables) != 1 || tables[0] != (tenant.LookupTableCount{Name: "cohort", Codes: 5}) || next == "" {
		e.Fatalf("the first page of tables: %+v, next %q", tables, next)
	}
	tables, next, err = e.p.ListLookupTables(e.ctx, master.Page{Limit: 1, Cursor: next})
	e.ok("ListLookupTables page 2", err)
	if len(tables) != 1 || tables[0] != (tenant.LookupTableCount{Name: "other", Codes: 1}) || next != "" {
		e.Errorf("the second page of tables: %+v, next %q", tables, next)
	}
}

// testLookupImportRefusals guards what an import refuses, storing nothing
// and writing no audit entry: keys the table gives another code and codes
// it gives another key (a precondition failure counting them, every entry
// judged, naming none: an import must not tell which values the table
// holds); a
// key given two codes or a code two keys in one import, a malformed code
// or key, none or too many entries (each invalid); and a refusal by the
// guard. A refused import does not store its valid entries before the bad
// one.
func testLookupImportRefusals(e *penv) {
	_, err := e.p.ImportLookupCodes(e.ctx, "cohort", []tenant.LookupEntry{entry(1, "A")}, at(1), e.audit(), nil)
	e.ok("ImportLookupCodes", err)
	for _, c := range []struct {
		what    string
		entries []tenant.LookupEntry
		kind    fault.Kind
		message string
	}{
		{"a value coded otherwise", []tenant.LookupEntry{entry(2, "B"), entry(1, "Z")}, fault.Precondition, "1 of the entries"},
		{"a code taken", []tenant.LookupEntry{entry(2, "B"), entry(3, "A")}, fault.Precondition, "1 of the entries"},
		{"both", []tenant.LookupEntry{entry(1, "Z"), entry(2, "B"), entry(3, "A")}, fault.Precondition, "2 of the entries"},
		{"a value given two codes", []tenant.LookupEntry{entry(2, "B"), entry(2, "C")}, fault.Invalid, "entry 2"},
		{"a code given two values", []tenant.LookupEntry{entry(2, "B"), entry(3, "B")}, fault.Invalid, "entry 2"},
		{"a code in lower case", []tenant.LookupEntry{entry(2, "b")}, fault.Invalid, "entry 1"},
		{"a code too long", []tenant.LookupEntry{entry(2, strings.Repeat("A", 17))}, fault.Invalid, "entry 1"},
		{"a key that is not a hash", []tenant.LookupEntry{{Key: "plainvalue", Code: "B"}}, fault.Invalid, "entry 1"},
		{"no entry", nil, fault.Invalid, ""},
		{"too many entries", make([]tenant.LookupEntry, tenant.MaxLookupImport+1), fault.Invalid, ""},
	} {
		_, err := e.p.ImportLookupCodes(e.ctx, "cohort", c.entries, at(2), e.audit(), nil)
		e.wantKind("ImportLookupCodes of "+c.what, err, c.kind)
		if err != nil && !strings.Contains(err.Error(), c.message) {
			e.Errorf("ImportLookupCodes of %s: %v does not name %q", c.what, err, c.message)
		}
		var lc *tenant.LookupConflicts
		if c.kind == fault.Precondition && (!errors.As(err, &lc) || strings.Contains(fault.Message(err), "entry")) {
			e.Errorf("ImportLookupCodes of %s: %v names an entry, or carries no count", c.what, err)
		}
	}
	_, err = e.p.ImportLookupCodes(e.ctx, "Cohort", []tenant.LookupEntry{entry(2, "B")}, at(2), e.audit(), nil)
	e.wantKind("ImportLookupCodes into a malformed table", err, fault.Invalid)
	refuse := func(context.Context) error { return fault.New(fault.Denied, "revoked") }
	_, err = e.p.ImportLookupCodes(e.ctx, "cohort", []tenant.LookupEntry{entry(2, "B")}, at(2), e.audit(), refuse)
	e.wantKind("a refused import", err, fault.Denied)
	if _, found, _ := e.p.LookupCode(e.ctx, tenant.LookupKey{Table: "cohort", Key: lookupKey("t", 2).Key}); found {
		e.Error("a refused import stored a code")
	}
	if n := len(e.allAudit(master.AuditFilter{})); n != 1 {
		e.Errorf("%d audit entries; only the first import's should be written", n)
	}
	tables, _, err := e.p.ListLookupTables(e.ctx, master.Page{Limit: 10})
	e.ok("ListLookupTables", err)
	if len(tables) != 1 || tables[0].Codes != 1 {
		e.Errorf("after the refusals the tables are %+v", tables)
	}
}
