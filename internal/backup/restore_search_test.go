package backup

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestRestoreAnswersTheSameSearches guards a backup of a tenant with
// searchable scans: the search tables' trigram indexes (virtual tables on
// SQLite, with their shadow tables) are derived data a snapshot leaves out,
// so the backup is made, and the restored tenant answers the searches the
// live one did, a contains that the trigram index serves among them.
func TestRestoreAnswersTheSameSearches(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		ctx := r.ctx
		r.archive(studyFiles...)
		page, err := r.p.ListScans(ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		conds := []string{`Modality present`}
		for _, s := range page.Items {
			d := s.Values.SeriesDescription
			if len([]rune(d)) >= 5 {
				conds = append(conds, `SeriesDescription contains "`+strings.ToLower(string([]rune(d)[1:4]))+`"`, `SeriesDescription like "`+string([]rune(d)[:3])+`*"`)
				break
			}
		}
		if len(conds) < 3 {
			t.Fatal("no scan of the corpus has a series description to search inside")
		}
		answers := func() map[string][]string {
			out := map[string][]string{}
			for _, c := range conds {
				n, err := query.Parse(c)
				if err != nil {
					t.Fatal(err)
				}
				res, err := r.p.Search(ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: n, Page: master.Page{Limit: 100}, Now: time.Now()})
				if err != nil {
					t.Fatalf("Search %s: %v", c, err)
				}
				for _, s := range res.Scans {
					out[c] = append(out[c], s.SubjectLabel+"/"+s.SessionLabel+"/"+s.Label)
				}
				slices.Sort(out[c])
			}
			return out
		}
		want := answers()
		for _, c := range conds {
			if len(want[c]) == 0 {
				t.Fatalf("the live tenant answers nothing to %s", c)
			}
		}
		path, _ := r.backup()
		_ = r.st.Close()
		_ = r.m.Close()
		target := filepath.Join(filepath.Dir(r.dataDir), "restored")
		cfg := RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
			MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")}
		if pg {
			r.emptyPostgres()
			cfg.MasterAddress = r.masterAddr
			cfg.Scratch, _ = testkit.PostgresScratch(t)
		}
		if _, err := Restore(ctx, cfg); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		r.dataDir, r.masterAddr = target, cfg.MasterAddress
		r.openMaster()
		r.openTenant()
		got := answers()
		for _, c := range conds {
			if !slices.Equal(got[c], want[c]) {
				t.Errorf("%s: the restored tenant answers %v, the live one %v", c, got[c], want[c])
			}
		}
	})
}
