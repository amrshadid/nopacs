package rebuild

import (
	"context"
	"errors"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// rowStore answers Correction from one row, or an error; nothing else of
// the port is used by ByRow.
type rowStore struct {
	tenant.Pipeline
	row tenant.Correction
	err error
}

// Correction answers the row or the error.
func (s rowStore) Correction(context.Context, string) (tenant.Correction, error) { return s.row, s.err }

// TestByRow guards how the server's rebuilds and a restore's apply a
// correction record: as its row says, checked by what the record then
// hides from the build (hides.add). A delete hides its scan while its row
// is committed, purging or purged, and nothing while it is prepared,
// undone or abandoned, whatever the record says; a record of another kind
// than its row applies nothing; a record with no row applies as its record
// reads, by the rule a restore adopts it by: a delete committed, purging,
// purged or still prepared hides its scan, one abandoned or undone hides
// nothing, a relabel published hides its old tree and one prepared or
// abandoned its new tree, a replace nothing; a row that cannot be read
// fails; a delete adopted from its record alone, its row committed and
// unresolved, hides its scan whatever its record says; a relabel hides the
// new tree until its row is published and the old tree after; a replace
// hides nothing.
func TestByRow(t *testing.T) {
	scan := record.Scan{Subject: "sub-01", Session: "ses-01", Scan: "scan-01", SeriesUID: "1.2.3"}
	old := hideKey(label.Path{Subject: "sub-01", Session: "ses-01", Scan: "scan-01"}, "1.2.3")
	renamed := hideKey(label.Path{Subject: "sub-02", Session: "ses-01", Scan: "scan-01"}, "1.2.3")
	del := func(s record.State) record.Correction {
		return record.Correction{ID: "c1", Kind: record.KindDelete, Level: record.LevelSession, State: s, Scans: []record.Scan{scan}}
	}
	relabel := func(s record.State) record.Correction {
		return record.Correction{ID: "c1", Kind: record.KindRelabel, Level: record.LevelSubject, State: s, Scans: []record.Scan{scan},
			Relabel: &record.Relabel{Level: record.LevelSubject, From: "sub-01", To: "sub-02"}}
	}
	row := func(k record.Kind, s record.State) rowStore {
		return rowStore{row: tenant.Correction{ID: "c1", Kind: k, State: s}}
	}
	unavailable := fault.New(fault.Unavailable, "the database does not answer")
	noRow := rowStore{err: fault.New(fault.NotFound, "no such correction")}
	for _, c := range []struct {
		name    string
		rec     record.Correction
		store   rowStore
		err     bool
		applied bool
		hides   []string
	}{
		{name: "delete, row committed", rec: del(record.StatePrepared), store: row(record.KindDelete, record.StateCommitted), applied: true, hides: []string{old}},
		{name: "delete, row purging", rec: del(record.StateCommitted), store: row(record.KindDelete, record.StatePurging), applied: true, hides: []string{old}},
		{name: "delete, row purged", rec: del(record.StateCommitted), store: row(record.KindDelete, record.StatePurged), applied: true, hides: []string{old}},
		{name: "delete, row prepared", rec: del(record.StatePrepared), store: row(record.KindDelete, record.StatePrepared), applied: true},
		{name: "delete, row undone, record committed", rec: del(record.StateCommitted), store: row(record.KindDelete, record.StateUndone), applied: true},
		{name: "delete, row abandoned, record prepared", rec: del(record.StatePrepared), store: row(record.KindDelete, record.StateAbandoned), applied: true},
		{name: "record of another kind than its row", rec: del(record.StateCommitted), store: row(record.KindRelabel, record.StateCommitted)},
		{name: "no row, delete prepared", rec: del(record.StatePrepared), store: noRow, applied: true, hides: []string{old}},
		{name: "no row, delete committed", rec: del(record.StateCommitted), store: noRow, applied: true, hides: []string{old}},
		{name: "no row, delete purging", rec: del(record.StatePurging), store: noRow, applied: true, hides: []string{old}},
		{name: "no row, delete purged", rec: del(record.StatePurged), store: noRow, applied: true, hides: []string{old}},
		{name: "no row, delete abandoned", rec: del(record.StateAbandoned), store: noRow, applied: true},
		{name: "no row, delete undone", rec: del(record.StateUndone), store: noRow, applied: true},
		{name: "no row, relabel prepared", rec: relabel(record.StatePrepared), store: noRow, applied: true, hides: []string{renamed}},
		{name: "no row, relabel abandoned", rec: relabel(record.StateAbandoned), store: noRow, applied: true, hides: []string{renamed}},
		{name: "no row, relabel committed", rec: relabel(record.StateCommitted), store: noRow, applied: true, hides: []string{old}},
		{name: "no row, replace committed", rec: record.Correction{ID: "c1", Kind: record.KindReplace, Level: record.LevelInstance,
			State: record.StateCommitted, Scans: []record.Scan{scan}}, store: noRow, applied: true},
		{name: "row unreadable", rec: del(record.StateCommitted), store: rowStore{err: unavailable}, err: true},
		{name: "delete adopted from its record alone, purged", rec: del(record.StatePurged), store: rowStore{row: tenant.Correction{ID: "c1",
			Kind: record.KindDelete, State: record.StateCommitted, Unresolved: true, StateReason: tenant.StateReasonUnresolved}},
			applied: true, hides: []string{old}},
		{name: "relabel, row prepared", rec: relabel(record.StateCommitted), store: row(record.KindRelabel, record.StatePrepared), applied: true,
			hides: []string{renamed}},
		{name: "relabel, row committed", rec: relabel(record.StatePrepared), store: row(record.KindRelabel, record.StateCommitted), applied: true,
			hides: []string{old}},
		{name: "replace, row committed", rec: record.Correction{ID: "c1", Kind: record.KindReplace, Level: record.LevelInstance,
			State: record.StateCommitted, Scans: []record.Scan{scan}}, store: row(record.KindReplace, record.StateCommitted), applied: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, applied, err := ByRow(context.Background(), c.store, c.rec)
			if c.err {
				if !errors.Is(err, unavailable) {
					t.Fatalf("ByRow answered %v, want the row's read error", err)
				}
				return
			}
			if err != nil || applied != c.applied {
				t.Fatalf("ByRow answered applied %v, %v; want applied %v", applied, err, c.applied)
			}
			h := &hides{scans: map[string]bool{}, instances: map[string]map[string]bool{}}
			if applied {
				h.add(got)
			}
			if len(h.scans) != len(c.hides) || len(h.instances) != 0 {
				t.Fatalf("it hides scans %v and instances %v, want scans %v", h.scans, h.instances, c.hides)
			}
			for _, k := range c.hides {
				if !h.scans[k] {
					t.Fatalf("it hides scans %v, want %v", h.scans, c.hides)
				}
			}
		})
	}
}
