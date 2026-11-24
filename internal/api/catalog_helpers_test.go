package api_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
)

// catalogData is an archive made by real reception of public corpus files
// (a segmentation, two CT series of one study and an MR series of four
// instances, of three subjects), with every row of its catalog read once,
// in label order, through the service under test.
type catalogData struct {
	f        *placeFixture
	a        *archive
	cat      nopacsv1connect.CatalogServiceClient
	subjects []*v1.Subject
	sessions []*v1.Session
	scans    []*v1.Scan
}

// newCatalogData receives the files and reads the whole catalog.
func newCatalogData(t *testing.T) *catalogData {
	t.Helper()
	files, _ := metaFiles(t)
	return newCatalogDataOf(t, files)
}

// newCatalogDataOf receives the files and reads the whole catalog.
func newCatalogDataOf(t *testing.T, files [][]byte) *catalogData {
	t.Helper()
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, files)
	d := &catalogData{f: f, a: a, cat: f.as(f.lab1Reader).catalog}
	label := v1.CatalogOrder_CATALOG_ORDER_LABEL
	d.subjects = mustCall[*v1.ListSubjectsResponse](t, "subjects", call(d.cat.ListSubjects, &v1.ListSubjectsRequest{Tenant: "lab1", Order: label, PageSize: 1000})).GetSubjects()
	d.sessions = mustCall[*v1.ListSessionsResponse](t, "sessions", call(d.cat.ListSessions, &v1.ListSessionsRequest{Tenant: "lab1", Order: label, PageSize: 1000})).GetSessions()
	d.scans = mustCall[*v1.ListScansResponse](t, "scans", call(d.cat.ListScans, &v1.ListScansRequest{Tenant: "lab1", Order: label, PageSize: 1000})).GetScans()
	if len(d.subjects) != 3 || len(d.sessions) != 3 || len(d.scans) != 4 {
		t.Fatalf("the archive holds %d subjects, %d sessions and %d scans, want 3, 3 and 4", len(d.subjects), len(d.sessions), len(d.scans))
	}
	return d
}

// sessionWith returns the one session that has a scan of this modality.
func (d *catalogData) sessionWith(t *testing.T, modality string) *v1.Session {
	t.Helper()
	var out *v1.Session
	for _, s := range d.sessions {
		if slices.Contains(s.GetModalities(), modality) {
			if out != nil {
				t.Fatalf("two sessions have a %s scan", modality)
			}
			out = s
		}
	}
	if out == nil {
		t.Fatalf("no session has a %s scan", modality)
	}
	return out
}

// sessionKey, scanKey and subjectKey name a row by its labels.
func subjectKey(s *v1.Subject) string { return s.GetLabel() }
func sessionKey(s *v1.Session) string { return s.GetSubjectLabel() + "/" + s.GetLabel() }
func scanKey(s *v1.Scan) string {
	return s.GetSubjectLabel() + "/" + s.GetSessionLabel() + "/" + s.GetLabel()
}

// keys returns the keys of rows, sorted, for a comparison that does not
// depend on order.
func keys[T any](rows []T, key func(T) string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, key(r))
	}
	slices.Sort(out)
	return out
}

// filtered returns the rows that keep returns true for.
func filtered[T any](rows []T, keep func(T) bool) []T {
	var out []T
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// pageAll reads a list to its end one row at a time (page_size 1), failing
// unless every page but the last holds one row and carries a token, the
// last carries none, and the total is the same on every page. It returns
// the rows in the order answered and that total.
func pageAll[R, S proto.Message, T any](t *testing.T, name string, base R,
	rpc func(context.Context, R) (S, error), set func(R, int32, string), read func(S) ([]T, string, *int64)) ([]T, *int64) {
	t.Helper()
	var (
		all   []T
		total *int64
		token string
	)
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatalf("%s: more than 100 pages of one row", name)
		}
		r := proto.Clone(base).(R)
		set(r, 1, token)
		res, err := rpc(context.Background(), r)
		if err != nil {
			t.Fatalf("%s: page %d: %v", name, page, err)
		}
		items, next, tot := read(res)
		if len(items) > 1 {
			t.Fatalf("%s: page %d holds %d rows, want at most 1", name, page, len(items))
		}
		if page == 0 {
			total = tot
		} else if (tot == nil) != (total == nil) || (tot != nil && *tot != *total) {
			t.Fatalf("%s: page %d total differs from the first page's", name, page)
		}
		all = append(all, items...)
		if next == "" {
			return all, total
		}
		if len(items) != 1 {
			t.Fatalf("%s: page %d holds no row but a token", name, page)
		}
		token = next
	}
}

// checkTotal fails unless got is absent (want < 0) or equals want.
func checkTotal(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	switch {
	case want < 0 && got != nil:
		t.Errorf("%s: total_count %d, want it absent (not known)", name, *got)
	case want >= 0 && (got == nil || *got != want):
		t.Errorf("%s: total_count %v, want %d", name, got, want)
	}
}

// lowerPrefix reports whether s starts with p, ASCII letters compared in
// lower case.
func lowerPrefix(s, p string) bool {
	return strings.HasPrefix(strings.ToLower(s), strings.ToLower(p))
}
