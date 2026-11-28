package sqlkit

import (
	"regexp"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// readsInstances matches a statement that reads the instances table.
var readsInstances = regexp.MustCompile(`(?i)\b(from|join)\s+instances\b`)

// TestMaskedListsReadNoInstances guards that the catalog's lists for a key
// that may not learn original values decide which sessions and scans were
// archived as received from the rows' own as_received column: no statement
// of any filter or order, under either treatment of those rows, reads the
// instances table, which for a page of a thousand scans would be a read of
// every instance beneath them.
func TestMaskedListsReadNoInstances(t *testing.T) {
	c := CatalogSQL{
		Param: func(int) string { return "?" },
		Time:  func(t time.Time) any { return t.UnixMicro() },
		InIDs: func(column, p string) string { return column + " IN (SELECT value FROM json_each(" + p + "))" },
		IDs:   func(ids []int64) (any, error) { return ids, nil },
	}
	for _, o := range []tenant.Originals{tenant.OriginalsHidden, tenant.OriginalsOnly} {
		var reads []CatalogRead
		for _, f := range []tenant.SessionFilter{
			{Order: tenant.OrderLabel}, {Order: tenant.OrderStudyDate}, {Modality: "CT"}, {SubjectLabel: "s"}, {Receiver: "r"}, {},
		} {
			f.Originals = o
			reads = append(reads, c.Sessions(f, 1, nil, 101))
		}
		for _, f := range []tenant.ScanFilter{
			{Order: tenant.OrderLabel}, {Order: tenant.OrderLabel, SubjectLabel: "s"}, {Order: tenant.OrderSeriesDescription},
			{SessionLabel: "x"}, {Modality: "CT"}, {Receiver: "r"}, {},
		} {
			f.Originals = o
			reads = append(reads, c.Scans(f, 1, nil, 101))
		}
		for i, r := range reads {
			if readsInstances.MatchString(r.SQL) {
				t.Errorf("originals %d, read %d reads the instances: %s", o, i, r.SQL)
			}
		}
	}
	for _, level := range []query.Level{query.LevelScan, query.LevelSession} {
		if got := Hidden(level, "x"); got == "" || readsInstances.MatchString(got) {
			t.Errorf("the search's condition for level %v is %q, want one that reads no instances", level, got)
		}
	}
}
