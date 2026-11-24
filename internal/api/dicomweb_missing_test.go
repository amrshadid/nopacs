package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// tryGet makes a request to lab1's DICOMweb root as lab1's reader and
// returns what came back, or the error of a connection closed before any
// answer, which a client sees as an answer it never had.
func (w *webFixture) tryGet(t *testing.T, rest string, headers ...string) (httpAnswer, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, w.s.url+webPath(rest), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.f.lab1Reader.key)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	res, err := w.s.client.Do(req)
	if err != nil {
		return httpAnswer{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, rerr := io.ReadAll(res.Body)
	return httpAnswer{status: res.StatusCode, header: res.Header, body: body, readErr: rerr}, nil
}

// removeMetadata deletes the metadata file of the first scan by label of a
// study from lab1's storage.
func (w *webFixture) removeMetadata(t *testing.T, study string) tenant.CatalogScan {
	t.Helper()
	ctx := context.Background()
	labels, err := w.a.p.CatalogLabels(ctx, study, nil)
	if err != nil || labels.Session == nil {
		t.Fatalf("study %s is not in the catalog: %v", study, err)
	}
	got, err := w.a.p.ListScans(ctx, tenant.ScanFilter{SubjectLabel: labels.Session.SubjectLabel, SessionLabel: labels.Session.SessionLabel,
		Order: tenant.OrderLabel}, master.Page{Limit: 1})
	if err != nil || len(got.Items) == 0 {
		t.Fatalf("study %s has no scan: %v", study, err)
	}
	s := got.Items[0]
	key, err := layout.MetadataKey(label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.a.store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestDICOMwebRetrieveCutAfterItBeganEndsShort guards a retrieve that meets
// a missing instance's file after its answer began (more than the bytes a
// stream holds before its status were sent): the multipart body ends
// before its closing boundary, so the client sees it is not whole; the
// request is audited as failed with the reason, and the abort carries the
// cause, data loss, for the server's log line of the request.
func TestDICOMwebRetrieveCutAfterItBeganEndsShort(t *testing.T) {
	f := newPlaceFixture(t)
	src := corpusFile(t, "MR_small.dcm")
	files := [][]byte{src}
	sop := uidOf(t, src)
	for _, c := range []byte("0123456789") {
		if sop[len(sop)-1] != c {
			files = append(files, reUID(t, src, c))
		}
	}
	a := receiveArchive(t, f, files)
	w := &webFixture{f: f, a: a, s: serveWeb(t, f.env), files: map[string]uids{}}
	u := uidsOfFile(t, src)
	whole := w.get(t, "studies/"+u.study+"/series/"+u.series, "Accept", acceptStored)
	if n := len(partsOf(t, whole, "application/dicom")); n != len(files) || len(whole.body) < 64<<10 {
		t.Fatalf("%d instances of %d bytes, want %d of more than 64 KiB", n, len(whole.body), len(files))
	}
	scans, err := w.a.p.ListScans(context.Background(), tenant.ScanFilter{}, master.Page{Limit: 2})
	if err != nil || len(scans.Items) != 1 {
		t.Fatalf("%d scans (%v), want the one series", len(scans.Items), err)
	}
	// The last instance the answer sends, in the order of their UIDs.
	var last tenant.ScanInstance
	s := scans.Items[0]
	for _, h := range w.a.held(t, label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}) {
		if h.SOPInstanceUID > last.SOPInstanceUID {
			last = h
		}
	}
	if err := w.a.store.Delete(context.Background(), last.Key); err != nil {
		t.Fatal(err)
	}
	got, err := w.tryGet(t, "studies/"+u.study+"/series/"+u.series, "Accept", acceptStored)
	if err != nil {
		t.Fatalf("the retrieve was not answered: %v", err)
	}
	if got.status != http.StatusOK || got.readErr == nil || len(got.body) >= len(whole.body) {
		t.Errorf("a retrieve cut after it began: %d, %d bytes, read error %v; want 200 cut short", got.status, len(got.body), got.readErr)
	}
	id := got.header.Get(api.RequestIDHeader)
	en := entriesOf(w.f.tenantLog("lab1"), id)
	if len(en) != 1 || en[0].Outcome != master.OutcomeFailed || !strings.Contains(en[0].Reason, "missing") {
		t.Errorf("the cut retrieve is audited as %+v", en)
	}
	if cause := w.s.cut(id); fault.KindOf(cause) != fault.DataLoss {
		t.Errorf("the cut retrieve was aborted with %v, want data loss", cause)
	}
}

// TestDICOMwebSearchWithoutAScansMetadataAnswersAnError guards a search
// whose results need a scan's metadata file that is missing from its
// storage: the failure is known before any byte reaches the client, so the
// client is answered 500 with the reason, never a connection closed with
// no answer, with none of the Warnings a success carries (a fuzzy
// matching asked for), and the request is audited as failed with that
// reason. It
// holds whether the scan is the first result's or a later one's, whose
// earlier results were made but not yet sent.
func TestDICOMwebSearchWithoutAScansMetadataAnswersAnError(t *testing.T) {
	w := newWebFixture(t)
	whole := w.get(t, "studies?fuzzymatching=true")
	all := results(t, whole)
	if len(all) < 3 {
		t.Fatalf("%d studies, want at least 3", len(all))
	}
	if len(whole.header.Values("Warning")) != 1 {
		t.Fatalf("a whole fuzzy search carries Warnings %q, want its one", whole.header.Values("Warning"))
	}
	for _, at := range []int{len(all) - 1, 0} {
		w.removeMetadata(t, value(all[at], "0020000D"))
		got, err := w.tryGet(t, "studies?fuzzymatching=true")
		if err != nil {
			t.Fatalf("with the metadata of result %d missing, the search was not answered: %v", at, err)
		}
		if got.status != http.StatusInternalServerError || got.readErr != nil {
			t.Errorf("with the metadata of result %d missing, the search answered %d (%v): %q", at, got.status, got.readErr, clipBody(got.body))
		}
		if wn := got.header.Values("Warning"); len(wn) != 0 {
			t.Errorf("with the metadata of result %d missing, the failed search carries the Warnings of a success: %q", at, wn)
		}
		en := entriesOf(w.f.tenantLog("lab1"), got.header.Get(api.RequestIDHeader))
		if len(en) != 1 || en[0].Outcome != master.OutcomeFailed || !strings.Contains(en[0].Reason, "metadata file is missing") {
			t.Errorf("with the metadata of result %d missing, the search is audited as %+v", at, en)
		}
	}
}
