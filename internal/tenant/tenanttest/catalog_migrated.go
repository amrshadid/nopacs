package tenanttest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// CheckMigratedCatalog checks, through the port, a catalog a driver's test
// wrote straight into a database at the schema before the catalog's reads
// were added, and then migrated: its rows, with times in microseconds
// since 1970, which at turns into times:
//
//   - subjects Sub-A (id 1, created 100), sub-b (2, 200), sub-c (3, 300,
//     with no session);
//   - sessions Sub-A/ses-1 (1, study 2.25.1.1, created 100), Sub-A/ses-2
//     (2, study 2.25.1.2, 150) and sub-b/ses-1 (3, study 2.25.1.3, 200),
//     all on storage "store";
//   - scans 1 (session 1, scan-1, 2 instances, 30 bytes, created 100,
//     updated 120), 2 (session 1, scan-2, 1, 5, 110, 110), 3 (session 2,
//     scan-1, 1, 7, 150, 160), 4 (session 3, scan-1, 1, 9, 200, 200);
//   - receptions rc1 (recv-x, study 2.25.1.1), rc2 and rc3 (recv-y, the
//     other two studies), rc4 (recv-z, a later arrival of 2.25.1.1);
//   - held instances 2.25.3.1 (scan 1, rc1, 10 bytes, held at 100),
//     2.25.3.2 (scan 1, rc4, 20 bytes, held at 120), 2.25.3.3 (scan 2,
//     rc1, 5), 2.25.3.4 (scan 3, rc2, 7), 2.25.3.5 (scan 4, rc3, 9), and
//     2.25.3.9 claimed by rc4.
func CheckMigratedCatalog(t *testing.T, s tenant.CatalogReads, at func(int64) time.Time) {
	t.Helper()
	ctx := context.Background()
	sub, err := s.CatalogSubject(ctx, "Sub-A")
	if err != nil {
		t.Fatal(err)
	}
	if sub != (tenant.CatalogSubject{Label: "Sub-A", Sessions: 2, Instances: 4, Bytes: 42, Created: at(100), Updated: at(160)}) {
		t.Errorf("subject Sub-A after the migration: %+v", sub)
	}
	if sub, err := s.CatalogSubject(ctx, "sub-c"); err != nil || sub.Sessions != 0 || sub.Instances != 0 || !sub.Updated.Equal(at(300)) {
		t.Errorf("a subject with no session after the migration: %+v, %v", sub, err)
	}
	ses, err := s.CatalogSession(ctx, "Sub-A", "ses-1")
	if err != nil {
		t.Fatal(err)
	}
	if ses.Receiver != "recv-x" || ses.Scans != 2 || ses.Instances != 3 || ses.Bytes != 35 || !ses.Updated.Equal(at(120)) ||
		ses.StudyDate != "" || len(ses.Modalities) != 0 {
		t.Errorf("session Sub-A/ses-1 after the migration: %+v", ses)
	}
	scan, err := s.CatalogScan(ctx, label.Path{Subject: "Sub-A", Session: "ses-1", Scan: "scan-1"})
	if err != nil {
		t.Fatal(err)
	}
	if scan.Receiver != "recv-x" || scan.Storage != "store" || scan.Instances != 2 || scan.Values != (tenant.ScanValues{}) {
		t.Errorf("scan Sub-A/ses-1/scan-1 after the migration: %+v, want the receiver of its first held instance", scan)
	}
	page, err := s.ListScans(ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, sc := range page.Items {
		got = append(got, sc.SubjectLabel+"/"+sc.SessionLabel+"/"+sc.Label+"/"+sc.Receiver)
	}
	want := []string{"Sub-A/ses-1/scan-1/recv-x", "Sub-A/ses-1/scan-2/recv-x", "Sub-A/ses-2/scan-1/recv-y", "sub-b/ses-1/scan-1/recv-y"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("scans by label after the migration: %q, want %q", got, want)
	}
	sessions, err := s.ListSessions(ctx, tenant.SessionFilter{}, master.Page{Limit: 10})
	if err != nil || len(sessions.Items) != 3 || sessions.Items[0].SubjectLabel != "sub-b" {
		t.Errorf("sessions newest first after the migration: %+v, %v", sessions.Items, err)
	}
}
