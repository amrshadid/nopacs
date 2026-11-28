package api_test

import (
	"database/sql"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestMaskedKeyReadsCatalogWithoutFileValues guards the catalog's answers
// for a session and a scan a receiver that keeps files as received
// archived: to a masked key, GetScan, ListScans, GetSession and
// ListSessions answer the labels, counts, sizes and times but none of the
// values read from the files (the Study and Series Instance UIDs, the
// study date, the modality, the description and number of the series), and
// say so with values_withheld; a plain reader is answered every value and
// no flag.
func TestMaskedKeyReadsCatalogWithoutFileValues(t *testing.T) {
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{corpusFile(t, "ct5n-2062.dcm")})
	path := a.scans(t)[0]
	if !a.held(t, path)[0].Profile.IsZero() {
		t.Fatal("the instance was de-identified; the test needs one archived as received")
	}
	sess, err := a.p.CatalogSession(ctx, path.Subject, path.Session)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := a.p.CatalogScan(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if sess.StudyUID == "" || sc.SeriesUID == "" || sess.StudyDate == "" || sc.Values.Modality == "" {
		t.Fatalf("the file holds no values to withhold: %+v %+v", sess, sc)
	}
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleReader))
	scanReq := &v1.GetScanRequest{Tenant: "lab1", SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan}
	sessReq := &v1.GetSessionRequest{Tenant: "lab1", SubjectLabel: path.Subject, SessionLabel: path.Session}
	scans := func(c clients) []*v1.Scan {
		return append(
			[]*v1.Scan{mustCall[*v1.GetScanResponse](t, "GetScan", call(c.catalog.GetScan, scanReq)).GetScan()},
			mustCall[*v1.ListScansResponse](t, "ListScans", call(c.catalog.ListScans, &v1.ListScansRequest{Tenant: "lab1"})).GetScans()...)
	}
	sessions := func(c clients) []*v1.Session {
		return append(
			[]*v1.Session{mustCall[*v1.GetSessionResponse](t, "GetSession", call(c.catalog.GetSession, sessReq)).GetSession()},
			mustCall[*v1.ListSessionsResponse](t, "ListSessions", call(c.catalog.ListSessions, &v1.ListSessionsRequest{Tenant: "lab1"})).GetSessions()...)
	}

	for _, m := range scans(f.as(masked)) {
		if m.GetSeriesInstanceUid() != "" || m.GetModality() != "" || m.GetSeriesDescription() != "" || m.SeriesNumber != nil || !m.GetValuesWithheld() {
			t.Errorf("a masked key's scan holds a file value or lacks the flag: %v", m)
		}
		if m.GetLabel() != path.Scan || m.GetInstanceCount() != 1 || m.GetSizeBytes() == 0 || m.GetArchiveTime() == nil {
			t.Errorf("a masked key's scan lost what is not a file value: %v", m)
		}
	}
	for _, m := range sessions(f.as(masked)) {
		if m.GetStudyInstanceUid() != "" || m.GetStudyDate() != "" || len(m.GetModalities()) != 0 || !m.GetValuesWithheld() {
			t.Errorf("a masked key's session holds a file value or lacks the flag: %v", m)
		}
		if m.GetLabel() != path.Session || m.GetScanCount() != 1 || m.GetInstanceCount() != 1 || m.GetArchiveTime() == nil {
			t.Errorf("a masked key's session lost what is not a file value: %v", m)
		}
	}
	for _, m := range scans(f.as(f.lab1Reader)) {
		if m.GetSeriesInstanceUid() != sc.SeriesUID || m.GetModality() != sc.Values.Modality || m.GetValuesWithheld() {
			t.Errorf("a plain reader's scan: %v", m)
		}
	}
	for _, m := range sessions(f.as(f.lab1Reader)) {
		if m.GetStudyInstanceUid() != sess.StudyUID || m.GetStudyDate() != sess.StudyDate || m.GetValuesWithheld() {
			t.Errorf("a plain reader's session: %v", m)
		}
	}
}

// TestMaskedKeyFiltersMatchNoAsReceivedRow guards the catalog's filters
// and orders by a value read from the files, which would tell a masked key
// the withheld value by which rows they keep: for a scan and a session
// archived as received, a masked key's ListScans by modality and by series
// description prefix, and ListSessions by modality and by a range of study
// dates, keep no row, while a plain reader's keep it; an order by study
// date, or by series description, still lists the row (last, in label
// order), and a plain reader's is unchanged. That the rows archived
// de-identified are kept is guarded by the tenant contract suite.
func TestMaskedKeyFiltersMatchNoAsReceivedRow(t *testing.T) {
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{corpusFile(t, "ct5n-2062.dcm")})
	path := a.scans(t)[0]
	sc, err := a.p.CatalogScan(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.p.CatalogSession(ctx, path.Subject, path.Session)
	if err != nil {
		t.Fatal(err)
	}
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleReader))
	scansOf := func(as caller, req *v1.ListScansRequest) int {
		req.Tenant = "lab1"
		return len(mustCall[*v1.ListScansResponse](t, "ListScans", call(f.as(as).catalog.ListScans, req)).GetScans())
	}
	sessionsOf := func(as caller, req *v1.ListSessionsRequest) int {
		req.Tenant = "lab1"
		return len(mustCall[*v1.ListSessionsResponse](t, "ListSessions", call(f.as(as).catalog.ListSessions, req)).GetSessions())
	}
	prefix := strings.ToLower(sc.Values.SeriesDescription[:3])
	for name, c := range map[string]struct {
		masked, plain int
		count         func(caller) int
	}{
		"scans by modality": {0, 1, func(as caller) int {
			return scansOf(as, &v1.ListScansRequest{Modality: sc.Values.Modality})
		}},
		"scans by description prefix": {0, 1, func(as caller) int {
			return scansOf(as, &v1.ListScansRequest{SeriesDescriptionPrefix: prefix, Order: v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION})
		}},
		"scans in order of description": {1, 1, func(as caller) int {
			return scansOf(as, &v1.ListScansRequest{Order: v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION})
		}},
		"sessions by modality": {0, 1, func(as caller) int {
			return sessionsOf(as, &v1.ListSessionsRequest{Modality: sc.Values.Modality})
		}},
		"sessions by a range of study dates": {0, 1, func(as caller) int {
			return sessionsOf(as, &v1.ListSessionsRequest{StudyDateStart: sess.StudyDate, StudyDateEnd: sess.StudyDate, Order: v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE})
		}},
		"sessions in order of study date": {1, 1, func(as caller) int {
			return sessionsOf(as, &v1.ListSessionsRequest{Order: v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE})
		}},
	} {
		if got := c.count(masked); got != c.masked {
			t.Errorf("%s, masked: %d rows, want %d", name, got, c.masked)
		}
		if got := c.count(f.lab1Reader); got != c.plain {
			t.Errorf("%s, plain reader: %d rows, want %d", name, got, c.plain)
		}
	}
}

// TestMaskedCatalogListsReadNoInstances guards that a masked key's pages of
// the catalog say which scans and sessions were archived as received from
// the rows themselves and read no instance: with the instances table set
// aside in the tenant's database, so that any statement reading it fails,
// ListScans (unfiltered, by modality, by description), GetScan, ListSessions
// and GetSession still answer, withholding the values of every scan, and a
// plain reader's lists answer with them.
func TestMaskedCatalogListsReadNoInstances(t *testing.T) {
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{corpusFile(t, "ct5n-2062.dcm"), corpusFile(t, "MR_small.dcm"), corpusFile(t, "ct2n-6293.dcm")})
	paths := a.scans(t)
	if len(paths) < 2 {
		t.Fatalf("the archive holds %d scans, want several", len(paths))
	}
	file, err := driver.SQLiteTenants{DataDir: f.data}.Path("lab1")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{`PRAGMA busy_timeout = 10000`, `PRAGMA legacy_alter_table = ON`, `ALTER TABLE instances RENAME TO instances_set_aside`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if _, err := db.Exec(`ALTER TABLE instances_set_aside RENAME TO instances`); err != nil {
			t.Error(err)
		}
	}()

	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleReader))
	mc := f.as(masked)
	scans := mustCall[*v1.ListScansResponse](t, "ListScans", call(mc.catalog.ListScans, &v1.ListScansRequest{Tenant: "lab1"})).GetScans()
	if len(scans) != len(paths) {
		t.Fatalf("a masked key's ListScans answers %d scans, want %d", len(scans), len(paths))
	}
	for _, m := range scans {
		if !m.GetValuesWithheld() || m.GetModality() != "" {
			t.Errorf("a masked key's scan is not withheld: %v", m)
		}
	}
	for name, req := range map[string]*v1.ListScansRequest{
		"by modality":    {Tenant: "lab1", Modality: "CT"},
		"by description": {Tenant: "lab1", Order: v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION},
	} {
		mustCall[*v1.ListScansResponse](t, "ListScans "+name, call(mc.catalog.ListScans, req))
	}
	sessions := mustCall[*v1.ListSessionsResponse](t, "ListSessions", call(mc.catalog.ListSessions, &v1.ListSessionsRequest{Tenant: "lab1"})).GetSessions()
	if len(sessions) == 0 {
		t.Fatal("a masked key's ListSessions answers none")
	}
	for _, m := range sessions {
		if !m.GetValuesWithheld() || m.GetStudyInstanceUid() != "" {
			t.Errorf("a masked key's session is not withheld: %v", m)
		}
	}
	p := paths[0]
	if m := mustCall[*v1.GetScanResponse](t, "GetScan", call(mc.catalog.GetScan, &v1.GetScanRequest{Tenant: "lab1", SubjectLabel: p.Subject,
		SessionLabel: p.Session, ScanLabel: p.Scan})).GetScan(); !m.GetValuesWithheld() {
		t.Errorf("a masked key's GetScan is not withheld: %v", m)
	}
	if m := mustCall[*v1.GetSessionResponse](t, "GetSession", call(mc.catalog.GetSession, &v1.GetSessionRequest{Tenant: "lab1", SubjectLabel: p.Subject,
		SessionLabel: p.Session})).GetSession(); !m.GetValuesWithheld() {
		t.Errorf("a masked key's GetSession is not withheld: %v", m)
	}
	for _, m := range mustCall[*v1.ListScansResponse](t, "ListScans", call(f.as(f.lab1Reader).catalog.ListScans, &v1.ListScansRequest{Tenant: "lab1"})).GetScans() {
		if m.GetValuesWithheld() || m.GetSeriesInstanceUid() == "" {
			t.Errorf("a plain reader's scan: %v", m)
		}
	}
}
