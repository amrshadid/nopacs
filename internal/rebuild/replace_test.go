package rebuild

import (
	"database/sql"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// sqliteScanIDs answers each scan's integer id in the SQLite file at path,
// by Series Instance UID.
func sqliteScanIDs(t *testing.T, path string) map[string]int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT series_uid, id FROM scans`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var s string
		var id int64
		if err := rows.Scan(&s, &id); err != nil {
			t.Fatal(err)
		}
		out[s] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRebuildReplaceNeverReusesAnID guards the ids of a replace on SQLite:
// with the scan of the largest id hidden by a committed delete and another
// scan missing from the catalog in use, the replace removes the first and
// adds the second, and the scan added takes an id no scan ever had, never
// the removed one's, which a send job's or a retry's rows may still name.
func TestRebuildReplaceNeverReusesAnID(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	ids := sqliteScanIDs(t, r.dbPath)
	var highest, lowest string
	var top int64
	for s, id := range ids {
		if highest == "" || id > ids[highest] {
			highest = s
		}
		if lowest == "" || id < ids[lowest] {
			lowest = s
		}
		top = max(top, id)
	}
	for _, p := range r.scanPaths() {
		sc, err := r.p.CatalogScan(r.ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if sc.SeriesUID == highest {
			r.putRecord(r.recordOf("01j00000000000000000000001", p, record.LevelScan, record.StateCommitted, nil))
		}
	}
	gone := itoa(ids[lowest])
	sqliteExec(t, r.dbPath, "DELETE FROM instances WHERE scan_id = "+gone, "DELETE FROM reception_scans WHERE scan_id = "+gone,
		"DELETE FROM scans WHERE id = "+gone)
	var writes atomic.Int64
	j, tallies := r.run(r.config(&writes), tenant.RebuildReplace)
	if j.State != tenant.JobPartial || tallies[tenant.TallyActivated] != 1 {
		t.Fatalf("the replace ended %+v with %v", j, tallies)
	}
	after := sqliteScanIDs(t, r.dbPath)
	if _, there := after[highest]; there {
		t.Errorf("the scan the delete hides is still in the catalog")
	}
	if got := after[lowest]; got <= top {
		t.Errorf("the scan added took the id %d; ids up to %d were given before (the removed scan's was %d)", got, top, ids[highest])
	}
}

// itoa writes n in decimal.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// catalogExec runs statements on the rig's tenant database.
func (r *rig) catalogExec(stmts ...string) {
	r.t.Helper()
	if r.address != "" {
		testkit.PostgresExec(r.t, r.address, stmts...)
		return
	}
	sqliteExec(r.t, r.dbPath, stmts...)
}

// TestRebuildReplaceTradesLabels guards a replace over a catalog in use
// whose two scans of one session hold each other's labels, as the storage
// does not: the activation, which keeps each row's id and changes its
// label in place, meets no unique index of labels between its writes, so
// it activates on its first attempt and the catalog is the storage's
// again.
func TestRebuildReplaceTradesLabels(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		want := r.catalogText()
		var a, b tenant.CatalogScan
		paths := r.scanPaths()
		for i, p := range paths {
			for _, q := range paths[i+1:] {
				if p.Subject == q.Subject && p.Session == q.Session && a.SeriesUID == "" {
					var err error
					if a, err = r.p.CatalogScan(r.ctx, p); err != nil {
						t.Fatal(err)
					}
					if b, err = r.p.CatalogScan(r.ctx, q); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if a.SeriesUID == "" {
			t.Fatal("the corpus has no session of two scans")
		}
		set := func(series, to string) string {
			return "UPDATE scans SET label = '" + to + "', folded = lower('" + to + "') WHERE series_uid = '" + series + "'"
		}
		r.catalogExec(set(a.SeriesUID, "swapping"), set(b.SeriesUID, a.Label), set(a.SeriesUID, b.Label))
		var writes atomic.Int64
		j, tallies := r.run(r.config(&writes), tenant.RebuildReplace)
		if j.State != tenant.JobDone || tallies[tenant.TallyActivated] != 1 {
			t.Fatalf("the replace over labels traded ended %+v with %v", j, tallies)
		}
		if w, err := r.p.Work(r.ctx, tenantUnit(j.ID)); err != nil || w.Attempt != 1 {
			t.Errorf("the replace took %d attempts (%v), want 1", w.Attempt, err)
		}
		if got := r.catalogText(); got != want {
			t.Errorf("the catalog after the replace is not the storage's:\n%s\nwant\n%s", got, want)
		}
	})
}
