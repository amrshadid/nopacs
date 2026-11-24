package backup

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// macField matches a record's MAC field, the last of its fields.
var macField = regexp.MustCompile(`,"mac":"[0-9a-f]{64}"`)

// TestRestoreTreatsAnUnauthenticatedRecordAsAbsent guards that a record on
// a storage whose MAC is missing or does not verify for the tenant under
// the key file, here deletes made after the backup and rewritten without a
// MAC, with another tenant's, and with another installation's, is neither
// adopted nor applied: no correction is made of it, the subject it names
// stays in the rebuilt catalog, the rebuild tallies it, and the report
// counts and names it, on each database type.
func TestRestoreTreatsAnUnauthenticatedRecordAsAbsent(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		path, _ := r.backup()
		other := testkit.InstallationKeys(t)
		forgeries := []func(c record.Correction) []byte{
			func(c record.Correction) []byte {
				b, err := record.EncodeCorrection(c, tenantID, r.keys)
				if err != nil {
					t.Fatal(err)
				}
				return macField.ReplaceAll(b, nil)
			},
			func(c record.Correction) []byte {
				b, err := record.EncodeCorrection(c, "T0000000000002", r.keys)
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
			func(c record.Correction) []byte {
				b, err := record.EncodeCorrection(c, tenantID, other)
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
		}
		n := min(len(all), len(forgeries))
		if n < 2 {
			t.Fatalf("the corpus made %d subjects, want at least 2", len(all))
		}
		var ids []string
		e := r.engine()
		for i := range n {
			c := r.deleteSubject(e, all[i])
			r.rewriteRecord(c.ID, forgeries[i])
			ids = append(ids, c.ID)
		}
		rep, err := r.restoreFresh(path)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if got := r.subjects(r.p); !slices.Equal(got, all) {
			t.Fatalf("the restored catalog holds %v, want every subject %v", got, all)
		}
		for _, id := range ids {
			if c, err := r.p.Correction(r.ctx, id); fault.KindOf(err) != fault.NotFound {
				t.Errorf("a correction was made of the record that does not verify: %+v (%v)", c, err)
			}
		}
		res := rep.Tenants[0]
		slices.Sort(ids)
		got := slices.Sorted(slices.Values(res.UnauthenticatedIDs))
		if res.Unauthenticated != n || !slices.Equal(got, ids) || res.Added != 0 || res.Disagreed != 0 || res.Records != n {
			t.Fatalf("the tenant's reconcile is %+v, want %d records unauthenticated (%v) and none adopted", res, n, ids)
		}
		var tallied int64
		for _, x := range res.Tallies {
			if x.Key == tenant.TallyUnauthenticatedCorrection {
				tallied = x.Count
			}
		}
		if tallied != int64(n) {
			t.Errorf("the rebuild tallied %d records unauthenticated, want %d (%v)", tallied, n, res.Tallies)
		}
		var out strings.Builder
		if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), "that do not verify under this key file") ||
			!strings.Contains(out.String(), ids[0]) || !strings.Contains(out.String(), tenant.TallyUnauthenticatedCorrection) {
			t.Fatalf("the report reads %q (%v)", out.String(), err)
		}
	})
}
