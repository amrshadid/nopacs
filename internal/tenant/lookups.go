package tenant

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// MaxLookupImport is the most entries one ImportLookupCodes takes: a
// cohort's list of chosen codes in one transaction, all or none.
const MaxLookupImport = 10000

// The details a store adds to the audit entry of an import of lookup
// codes: counts only, never a value or a code.
const (
	// DetailLookupEntries is how many distinct entries the import named.
	DetailLookupEntries = "entries"
	// DetailLookupAdded is how many of them were added.
	DetailLookupAdded = "added"
	// DetailLookupUnchanged is how many the table already held with the
	// same code.
	DetailLookupUnchanged = "unchanged"
	// DetailLookupConflicts is how many entries of a refused import
	// conflicted with the table.
	DetailLookupConflicts = "conflicts"
)

// LookupConflicts is an import refused because entries conflict with the
// table: a value it gives another code, or a code it gives another value.
// It carries how many, never which: naming the entries would make an
// import a test of whether given values are in the table, around the
// bounded forward lookup. A store returns it wrapped in a Precondition
// failure (ConflictsError).
type LookupConflicts struct {
	// Count is how many entries conflict.
	Count int
}

// Error says how many entries conflict.
func (e *LookupConflicts) Error() string {
	return strconv.Itoa(e.Count) + " entries conflict with the lookup table"
}

// ConflictsError returns the Precondition failure of an import whose n
// entries conflict with the table, with a LookupConflicts in its chain.
func ConflictsError(table string, n int) error {
	return fault.Wrap(fault.Precondition, &LookupConflicts{Count: n}, strconv.Itoa(n)+
		" of the entries conflict with the lookup table "+table+
		" (a value it gives another code, or a code it gives another value); nothing was stored")
}

// LookupEntry is one code a person chose for a value: the keyed hash of
// the value (as LookupKey.Key), never the value, and its code.
type LookupEntry struct {
	// Key is the keyed hash of the value: naming.HashLength characters of
	// lower case base32.
	Key string
	// Code is the code, of the form ValidLookupCode keeps.
	Code string
}

// LookupImport is what an import stored.
type LookupImport struct {
	// Added is how many entries were added.
	Added int
	// Unchanged is how many the table already held with the same code.
	Unchanged int
	// Codes is how many codes the table holds after the import.
	Codes int64
}

// LookupTableCount is a lookup table in a list: its name and how many
// codes it holds, never a key or a code.
type LookupTableCount struct {
	// Name is the table's name.
	Name string
	// Codes is how many codes it holds, imported and assigned together.
	Codes int64
}

// LookupImports keeps the codes a person chooses, and answers the tables'
// sizes. It is part of LookupTables.
type LookupImports interface {
	// ImportLookupCodes stores entries in the table, all in one
	// transaction or none, with the audit entry (ImportedDetails adds the
	// counts), under guard. entries is a batch ValidateLookupImport
	// returned. An entry whose key the table already holds with the same
	// code is counted unchanged; a key it holds with another code, or a
	// code it holds for another key, is a Precondition failure naming the
	// entry's position, from 1, and stores nothing. The table's counter
	// is taken as AssignLookupCodes takes it, so an import and an
	// assignment of the table never interleave, and a code assigned later
	// passes over every code imported. now is when the codes are stored.
	ImportLookupCodes(ctx context.Context, table string, entries []LookupEntry, now time.Time, audit master.AuditEntry, guard Guard) (LookupImport, error)
	// ListLookupTables returns a page of the tenant's lookup tables in
	// order of name, each with how many codes it holds.
	ListLookupTables(ctx context.Context, page master.Page) ([]LookupTableCount, string, error)
}

// ValidateLookupImport returns entries with each key once, in their
// order, or an Invalid error unless table is a table's name and entries
// is 1 to MaxLookupImport entries, each a keyed hash's text and a code of
// a code's form, no key given two codes and no code given two keys. An
// entry repeated with the same code is kept once. An error names an entry
// by its position, from 1, never its key or code.
func ValidateLookupImport(table string, entries []LookupEntry) ([]LookupEntry, error) {
	if err := ValidateName("lookup table", table); err != nil {
		return nil, err
	}
	if len(entries) == 0 || len(entries) > MaxLookupImport {
		return nil, invalid("an import holds 1 to " + strconv.Itoa(MaxLookupImport) + " entries")
	}
	byKey := make(map[string]int, len(entries))
	byCode := make(map[string]int, len(entries))
	out := make([]LookupEntry, 0, len(entries))
	for i, e := range entries {
		at := "entry " + strconv.Itoa(i+1)
		if err := (LookupKey{Table: table, Key: e.Key}).Validate(); err != nil {
			return nil, invalid(at + ": " + err.Error())
		}
		if !ValidLookupCode(e.Code) {
			return nil, invalid(at + ": a code is 1 to " + strconv.Itoa(MaxLookupCodeLength) + " of A-Z, 0-9 and _")
		}
		j, seenKey := byKey[e.Key]
		k, seenCode := byCode[e.Code]
		switch {
		case seenKey && entries[j].Code == e.Code:
			continue
		case seenKey:
			return nil, invalid(at + ": its value is entry " + strconv.Itoa(j+1) + "'s, given another code")
		case seenCode:
			return nil, invalid(at + ": its code is entry " + strconv.Itoa(k+1) + "'s, given to another value")
		}
		byKey[e.Key], byCode[e.Code] = i, i
		out = append(out, e)
	}
	return out, nil
}

// ImportedDetails returns audit with the counts of an import added: how
// many entries it named, added and found unchanged.
func ImportedDetails(audit master.AuditEntry, entries int, got LookupImport) (master.AuditEntry, error) {
	a := audit.WithDetails(DetailLookupEntries, strconv.Itoa(entries), DetailLookupAdded, strconv.Itoa(got.Added),
		DetailLookupUnchanged, strconv.Itoa(got.Unchanged))
	return a, a.Validate()
}
