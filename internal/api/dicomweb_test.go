package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The Accept header of a retrieve of instances as they are stored.
const acceptStored = `multipart/related; type="application/dicom"; transfer-syntax=*`

// webFixture is lab1 after a real reception of the metadata tests' files
// (native and encapsulated, several subjects, studies and series, private
// bulk values) and a multi-frame JPEG file, served through the stand-ins
// of the download's and DICOMweb's pattern routes.
type webFixture struct {
	f *placeFixture
	a *archive
	s *dataServer
	// files are the files received, by SOP Instance UID, with their UIDs.
	files map[string]uids
}

// newWebFixture receives the files.
func newWebFixture(t *testing.T) *webFixture {
	t.Helper()
	f := newPlaceFixture(t)
	files, _ := metaFiles(t)
	files = append(files, corpusFile(t, "jpeg_3frame_nobot.dcm"))
	a := receiveArchive(t, f, files)
	w := &webFixture{f: f, a: a, s: serveWeb(t, f.env), files: map[string]uids{}}
	for _, b := range files {
		u := uidsOfFile(t, b)
		w.files[u.sop] = u
	}
	return w
}

// get makes a request to lab1's DICOMweb root as lab1's reader.
func (w *webFixture) get(t *testing.T, rest string, headers ...string) httpAnswer {
	t.Helper()
	return w.s.get(t, http.MethodGet, webPath(rest), w.f.lab1Reader, headers...)
}

// studies returns the studies of the files, each with its series, each
// with its instances.
func (w *webFixture) studies() map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for _, u := range w.files {
		if out[u.study] == nil {
			out[u.study] = map[string][]string{}
		}
		out[u.study][u.series] = append(out[u.study][u.series], u.sop)
	}
	return out
}

// TestDICOMwebSearchAnswersTheCatalog guards QIDO-RS on an archive made by
// reception, compared with the UIDs of the files received: every study,
// its series, their instances; the counts, Patient ID as the subject's
// label, attributes from the metadata files; each key the catalog matches
// by index, and a key at the instance level matched from the metadata
// files; real paging with the Warning of more results; 204 for no match;
// 400 for a key not served; the Warning of fuzzy matching.
func TestDICOMwebSearchAnswersTheCatalog(t *testing.T) {
	w := newWebFixture(t)
	want := w.studies()
	ctx := context.Background()

	got := results(t, w.get(t, "studies"))
	var studyUIDs []string
	for _, r := range got {
		st := value(r, "0020000D")
		studyUIDs = append(studyUIDs, st)
		labels, err := w.a.p.CatalogLabels(ctx, st, nil)
		if err != nil || labels.Session == nil {
			t.Fatalf("study %s is not in the catalog: %v", st, err)
		}
		n := 0
		for _, sops := range want[st] {
			n += len(sops)
		}
		if value(r, "00100020") != labels.Session.SubjectLabel || value(r, "00201208") != strconv.Itoa(n) ||
			value(r, "00201206") != strconv.Itoa(len(want[st])) || value(r, "00080056") != "ONLINE" ||
			value(r, "00081190") != w.s.url+webPath("studies/"+st) {
			t.Errorf("study %s answered %v", st, r)
		}
		if _, ok := r["00100010"]; !ok {
			t.Errorf("study %s has no Patient's Name from its metadata file", st)
		}
	}
	slices.Sort(studyUIDs)
	if len(studyUIDs) != len(want) {
		t.Fatalf("%d studies answered, %d received", len(studyUIDs), len(want))
	}

	total := 0
	for st, series := range want {
		got := results(t, w.get(t, "studies/"+st+"/series"))
		if len(got) != len(series) {
			t.Errorf("study %s: %d series, want %d", st, len(got), len(series))
		}
		for _, r := range got {
			se := value(r, "0020000E")
			sops := series[se]
			if sops == nil || value(r, "00201209") != strconv.Itoa(len(sops)) || value(r, "00080060") == "" || value(r, "0020000D") != st {
				t.Errorf("series %s answered %v", se, r)
			}
			if _, ok := r["00100020"]; ok {
				t.Errorf("a series of a study named by the path holds the study's Patient ID")
			}
			inst := results(t, w.get(t, "studies/"+st+"/series/"+se+"/instances"))
			var gotSOP []string
			for _, ir := range inst {
				gotSOP = append(gotSOP, value(ir, "00080018"))
				if value(ir, "00080016") == "" || value(ir, "00081190") != w.s.url+webPath("studies/"+st+"/series/"+se+"/instances/"+value(ir, "00080018")) {
					t.Errorf("instance answered %v", ir)
				}
			}
			slices.Sort(gotSOP)
			wantSOP := slices.Clone(sops)
			slices.Sort(wantSOP)
			if !slices.Equal(gotSOP, wantSOP) {
				t.Errorf("series %s: instances %v, want %v", se, gotSOP, wantSOP)
			}
			total += len(sops)
		}
	}
	if all := results(t, w.get(t, "instances?limit=1000")); len(all) != total {
		t.Errorf("the tenant's instances: %d, want %d", len(all), total)
	}
	if all := results(t, w.get(t, "series")); len(all) != len(w.a.scans(t)) {
		t.Errorf("the tenant's series: %d, want %d", len(all), len(w.a.scans(t)))
	}

	// One instance by its UIDs; by a series UID alone; by an attribute of
	// its metadata file.
	var one uids
	for _, u := range w.files {
		one = u
		break
	}
	byUID := results(t, w.get(t, "instances?StudyInstanceUID="+one.study+"&SeriesInstanceUID="+one.series+"&SOPInstanceUID="+one.sop))
	if len(byUID) != 1 || value(byUID[0], "00080018") != one.sop {
		t.Errorf("an instance by its UIDs: %v", byUID)
	}
	if got := results(t, w.get(t, "series?SeriesInstanceUID="+one.series)); len(got) != 1 || value(got[0], "0020000D") != one.study {
		t.Errorf("a series by its UID: %v", got)
	}
	class := value(byUID[0], "00080016")
	byClass := results(t, w.get(t, "studies/"+one.study+"/series/"+one.series+"/instances?SOPClassUID="+class))
	if len(byClass) == 0 {
		t.Error("an instance by its SOP Class UID from its metadata file is not found")
	}
	for _, r := range byClass {
		if value(r, "00080016") != class {
			t.Errorf("SOPClassUID=%s answered %v", class, r)
		}
	}

	// Keys by index: a subject, a range of study dates, a modality, a
	// description's prefix.
	sess, err := w.a.p.ListSessions(ctx, tenant.SessionFilter{}, master.Page{Limit: 1})
	if err != nil || len(sess.Items) == 0 {
		t.Fatal(err)
	}
	s0 := sess.Items[0]
	for _, r := range results(t, w.get(t, "studies?PatientID="+s0.SubjectLabel)) {
		if value(r, "00100020") != s0.SubjectLabel {
			t.Errorf("PatientID=%s answered %v", s0.SubjectLabel, r)
		}
	}
	if s0.StudyDate != "" {
		found := false
		for _, r := range results(t, w.get(t, "studies?StudyDate="+s0.StudyDate+"-"+s0.StudyDate)) {
			found = found || value(r, "0020000D") == s0.StudyUID
			if value(r, "00080020") != s0.StudyDate {
				t.Errorf("a study of another date: %v", r)
			}
		}
		if !found {
			t.Error("a study is not found in the range of its own date")
		}
	}
	for _, r := range results(t, w.get(t, "series?Modality=MR")) {
		if value(r, "00080060") != "MR" {
			t.Errorf("Modality=MR answered %v", r)
		}
	}
	for _, r := range results(t, w.get(t, "studies?ModalitiesInStudy=CT")) {
		ms, _ := r["00080061"]["Value"].([]any)
		if !slices.Contains(ms, any("CT")) {
			t.Errorf("ModalitiesInStudy=CT answered %v", r)
		}
	}

	// Paging.
	first := w.get(t, "studies?limit=1")
	if len(results(t, first)) != 1 || !strings.Contains(first.header.Get("Warning"), "additional results") {
		t.Errorf("a first page of one: %d results, Warning %q", len(results(t, first)), first.header.Get("Warning"))
	}
	var paged []string
	for i := range len(want) {
		page := results(t, w.get(t, "studies?limit=1&offset="+strconv.Itoa(i)))
		if len(page) != 1 {
			t.Fatalf("page %d holds %d", i, len(page))
		}
		paged = append(paged, value(page[0], "0020000D"))
	}
	slices.Sort(paged)
	if !slices.Equal(paged, studyUIDs) {
		t.Errorf("paged one by one: %v, want %v", paged, studyUIDs)
	}
	last := w.get(t, "studies?offset="+strconv.Itoa(len(want)))
	if last.status != http.StatusNoContent || len(last.body) != 0 {
		t.Errorf("past the last study: %d %q", last.status, last.body)
	}

	// No match, a key not served, fuzzy matching.
	if a := w.get(t, "studies?StudyInstanceUID=1.2.3.999"); a.status != http.StatusNoContent {
		t.Errorf("no match: %d", a.status)
	}
	if a := w.get(t, "studies?AccessionNumber=A1"); a.status != http.StatusBadRequest || !strings.Contains(string(a.body), "AccessionNumber") {
		t.Errorf("a key not served: %d %q", a.status, a.body)
	}
	if a := w.get(t, "studies?fuzzymatching=true"); !strings.Contains(a.header.Get("Warning"), "fuzzymatching") {
		t.Errorf("fuzzy matching: Warning %q", a.header.Get("Warning"))
	}
	if a := w.get(t, "studies", "Accept", "application/dicom+xml"); a.status != http.StatusNotAcceptable {
		t.Errorf("an XML search: %d", a.status)
	}
}

// TestDICOMwebRetrieveAnswersTheArchivedBytes guards WADO-RS of instances:
// a study, a series and an instance as multipart/related; type=
// "application/dicom", each part the bytes received, with its transfer
// syntax; the transfer syntax negotiated as stored only, 206 when some
// instances are left out and 406 when none is kept; the bare form for one
// instance only; 404 for what the tenant does not hold.
func TestDICOMwebRetrieveAnswersTheArchivedBytes(t *testing.T) {
	w := newWebFixture(t)
	for st, series := range w.studies() {
		n := 0
		for _, sops := range series {
			n += len(sops)
		}
		parts := partsOf(t, w.get(t, "studies/"+st, "Accept", acceptStored), "application/dicom")
		if len(parts) != n {
			t.Errorf("study %s: %d parts, want %d", st, len(parts), n)
		}
		for _, p := range parts {
			u := uidsOfFile(t, p.body)
			if !bytes.Equal(p.body, w.a.sent[u.sop]) || u.study != st {
				t.Errorf("study %s: a part is not the bytes received", st)
			}
			h, _ := header.Read(bytes.NewReader(p.body), int64(len(p.body)))
			if p.contentType != "application/dicom; transfer-syntax="+h.TransferSyntaxUID {
				t.Errorf("a part's Content-Type %q, its file is %s", p.contentType, h.TransferSyntaxUID)
			}
		}
		for se, sops := range series {
			if got := partsOf(t, w.get(t, "studies/"+st+"/series/"+se, "Accept", acceptStored), "application/dicom"); len(got) != len(sops) {
				t.Errorf("series %s: %d parts, want %d", se, len(got), len(sops))
			}
			one := partsOf(t, w.get(t, "studies/"+st+"/series/"+se+"/instances/"+sops[0]), "application/dicom")
			if len(one) != 1 || !bytes.Equal(one[0].body, w.a.sent[sops[0]]) {
				t.Errorf("instance %s is not the bytes received", sops[0])
			}
			bare := w.get(t, "studies/"+st+"/series/"+se+"/instances/"+sops[0], "Accept", "application/dicom; transfer-syntax=*")
			if bare.status != http.StatusOK || !bytes.Equal(bare.body, w.a.sent[sops[0]]) || !strings.HasPrefix(bare.header.Get("Content-Type"), "application/dicom; transfer-syntax=") {
				t.Errorf("a bare instance: %d %q", bare.status, bare.header.Get("Content-Type"))
			}
		}
	}

	// The MR study holds Explicit VR Little Endian, RLE and JPEG 2000
	// instances of one series.
	mr := uidsOfFile(t, corpusFile(t, "MR_small.dcm"))
	def := w.get(t, "studies/"+mr.study, "Accept", `multipart/related; type="application/dicom"`)
	if def.status != http.StatusPartialContent {
		t.Errorf("Explicit VR Little Endian of a mixed study: %d, want 206", def.status)
	}
	for _, p := range partsOf(t, def, "application/dicom") {
		if p.contentType != "application/dicom; transfer-syntax=1.2.840.10008.1.2.1" {
			t.Errorf("a part of another syntax: %q", p.contentType)
		}
	}
	if a := w.get(t, "studies/"+mr.study, "Accept", `multipart/related; type="application/dicom"; transfer-syntax=1.2.840.10008.1.2.4.80`); a.status != http.StatusNotAcceptable {
		t.Errorf("a syntax none is held in: %d, want 406", a.status)
	}
	if a := w.get(t, "studies/"+mr.study, "Accept", "application/dicom"); a.status != http.StatusNotAcceptable {
		t.Errorf("a bare study: %d, want 406", a.status)
	}
	for _, rest := range []string{"studies/1.2.3.999", "studies/" + mr.study + "/series/1.2.3.999", "studies/" + mr.study + "/series/" + mr.series + "/instances/1.2.3.999"} {
		if a := w.get(t, rest, "Accept", acceptStored); a.status != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", rest, a.status)
		}
	}
}

// TestDICOMwebMetadataAndBulkData guards WADO-RS metadata: each
// instance's DICOM JSON is the metadata service's, each BulkDataURI the
// absolute URI of the instance's bulk data resource at the same place,
// which answers the bytes of the received file at that place, as a
// multipart body or bare.
func TestDICOMwebMetadataAndBulkData(t *testing.T) {
	w := newWebFixture(t)
	c := w.f.as(w.f.lab1Reader)
	checked := 0
	for st, series := range w.studies() {
		for se, sops := range series {
			got := results(t, w.get(t, "studies/"+st+"/series/"+se+"/metadata"))
			if len(got) != len(sops) {
				t.Fatalf("series %s: %d instances of metadata, want %d", se, len(got), len(sops))
			}
			labels, err := w.a.p.CatalogLabels(context.Background(), st, []string{se})
			if err != nil {
				t.Fatal(err)
			}
			path := label.Path{Subject: labels.Session.SubjectLabel, Session: labels.Session.SessionLabel, Scan: labels.Scans[se]}
			for _, r := range got {
				sop := value(r, "00080018")
				res, err := c.metadata.GetInstanceMetadata(context.Background(), instanceRequest(path, sop))
				if err != nil {
					t.Fatal(err)
				}
				var service map[string]map[string]any
				if err := json.Unmarshal([]byte(res.GetInstance().GetDicomJson()), &service); err != nil {
					t.Fatal(err)
				}
				prefix := w.s.url + webPath("studies/"+st+"/series/"+se+"/instances/"+sop+"/bulkdata?")
				for key, el := range r {
					uri, isBulk := el["BulkDataURI"].(string)
					if !isBulk {
						continue
					}
					if !strings.HasPrefix(uri, prefix) {
						t.Fatalf("BulkDataURI %q, want under %q", uri, prefix)
					}
					query := strings.TrimPrefix(uri, prefix)
					want := service[key]["BulkDataURI"].(string)
					if !strings.HasSuffix(want, "?"+query) {
						t.Errorf("BulkDataURI %q names another place than the metadata service's %q", uri, want)
					}
					el["BulkDataURI"] = want
					u := parseURI(t, want, path, sop)
					file := w.a.sent[sop][u.offset : u.offset+u.length]
					rest := strings.TrimPrefix(uri, w.s.url+"/tenants/lab1/dicom-web/")
					bare := w.get(t, rest, "Accept", "application/octet-stream")
					if bare.status != http.StatusOK || !bytes.Equal(bare.body, file) {
						t.Errorf("bulk data %s bare: %d, %d bytes, want %d", key, bare.status, len(bare.body), len(file))
					}
					multi := partsOf(t, w.get(t, rest), "application/octet-stream")
					if len(multi) != 1 || !bytes.Equal(multi[0].body, file) {
						t.Errorf("bulk data %s as multipart is not the file's bytes", key)
					}
					checked++
				}
				a, _ := json.Marshal(r)
				b, _ := json.Marshal(service)
				if !bytes.Equal(a, b) {
					t.Errorf("instance %s: the metadata differs from the metadata service's", sop)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no bulk data was checked")
	}
	if a := w.get(t, "studies/1.2.3.999/metadata"); a.status != http.StatusNotFound {
		t.Errorf("the metadata of a study not held: %d", a.status)
	}
}

// TestDICOMwebFramesAreTheStoredBytes guards WADO-RS frames: each frame
// asked for, in the order asked, one part of the stored bytes its frame
// table names, compared with go-dicom's division of the received file;
// the media type of its compression; 404 for a frame past the last; 406
// for a form other than the stored one.
func TestDICOMwebFramesAreTheStoredBytes(t *testing.T) {
	w := newWebFixture(t)
	file := corpusFile(t, "jpeg_3frame_nobot.dcm")
	u := uidsOfFile(t, file)
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	table, err := frames.Table(h, bytes.NewReader(file))
	if err != nil || len(table) != 3 {
		t.Fatalf("the file's frame table: %d frames, %v", len(table), err)
	}
	frame := func(n int) []byte {
		var b []byte
		for _, r := range table[n-1] {
			b = append(b, file[r.Offset:r.Offset+r.Length]...)
		}
		return b
	}
	base := "studies/" + u.study + "/series/" + u.series + "/instances/" + u.sop + "/frames/"
	parts := partsOf(t, w.get(t, base+"3,1"), "image/jpeg")
	if len(parts) != 2 || !bytes.Equal(parts[0].body, frame(3)) || !bytes.Equal(parts[1].body, frame(1)) {
		t.Fatalf("frames 3,1: %d parts", len(parts))
	}
	if !strings.HasPrefix(parts[0].contentType, "image/jpeg; transfer-syntax=1.2.840.10008.1.2.4.") {
		t.Errorf("a frame's Content-Type %q", parts[0].contentType)
	}
	octet := partsOf(t, w.get(t, base+"2", "Accept", `multipart/related; type="application/octet-stream"; transfer-syntax=*`), "application/octet-stream")
	if len(octet) != 1 || !bytes.Equal(octet[0].body, frame(2)) {
		t.Error("frame 2 as octet-stream is not its stored bytes")
	}
	if a := w.get(t, base+"4"); a.status != http.StatusNotFound {
		t.Errorf("a frame past the last: %d", a.status)
	}
	if a := w.get(t, base+"1", "Accept", `multipart/related; type="application/octet-stream"`); a.status != http.StatusNotAcceptable {
		t.Errorf("a JPEG frame asked decoded: %d", a.status)
	}
}

// TestDICOMwebIsClosedAndAudited guards the check of every DICOMweb
// request: no key, a wrong key and a key without a grant are refused, as
// the download is; an allowed request is recorded in the tenant's log
// with the route as its method and the resource's shape, and no entry of
// either log holds a UID the request named.
func TestDICOMwebIsClosedAndAudited(t *testing.T) {
	w := newWebFixture(t)
	var one uids
	for _, u := range w.files {
		one = u
		break
	}
	rest := "studies/" + one.study + "/series/" + one.series + "/instances/" + one.sop + "/metadata"
	for _, c := range []struct {
		name   string
		as     caller
		status int
	}{
		{"no key", nobody, http.StatusUnauthorized},
		{"a wrong key", caller{key: "nopacs_wrong"}, http.StatusUnauthorized},
		{"a key without a grant", w.f.noGrant, http.StatusForbidden},
	} {
		if a := w.s.get(t, http.MethodGet, webPath(rest), c.as); a.status != c.status || bytes.Contains(a.body, []byte(one.sop)) {
			t.Errorf("%s: %d, want %d", c.name, a.status, c.status)
		}
	}
	ok := w.get(t, rest)
	search := w.get(t, "studies?StudyInstanceUID="+one.study+"&ModalitiesInStudy=MR")
	log := w.f.tenantLog("lab1")
	for _, c := range []struct {
		answer         httpAnswer
		resource, keys string
	}{
		{ok, "studies/{study}/series/{series}/instances/{instance}/metadata", ""},
		{search, "studies", "ModalitiesInStudy,StudyInstanceUID"},
	} {
		en := entriesOf(log, c.answer.header.Get(api.RequestIDHeader))
		if len(en) != 1 {
			t.Fatalf("%d entries for %s, want 1", len(en), c.resource)
		}
		e := en[0]
		if e.Method != api.DICOMwebPattern || e.Outcome != master.OutcomeDone || e.Details["resource"] != c.resource || e.Details["keys"] != c.keys {
			t.Errorf("entry %+v", e)
		}
	}
	for _, e := range append(log, w.f.masterLog()...) {
		text := e.Method + e.Object + e.Reason
		for _, v := range e.Details {
			text += v
		}
		for _, u := range []string{one.study, one.series, one.sop} {
			if strings.Contains(text, u) {
				t.Errorf("an entry holds a UID: %+v", e)
			}
		}
	}
}

// TestDICOMwebNeedReadsThePath guards the requirement of a DICOMweb
// request as the server computes it: reader on the tenant the path names,
// on the installation when it names none, and the resource's shape with
// no UID in the details.
func TestDICOMwebNeedReadsThePath(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.SetPathValue("tenant", "lab1")
	r.SetPathValue("rest", "studies/1.2.3/series")
	need, f := api.DICOMwebNeed(r)
	if need != access.OnTenant("lab1", master.RoleReader) || f.Details["resource"] != "studies/{study}/series" || f.Object != "" {
		t.Errorf("need %+v, facts %+v", need, f)
	}
	r.SetPathValue("tenant", "")
	if need, _ := api.DICOMwebNeed(r); need != access.OnInstallation(master.RoleReader) {
		t.Errorf("no tenant: %+v", need)
	}
}

// TestDICOMwebCutsChangedBytes guards that a retrieve never passes on as
// whole an instance whose stored bytes differ from its manifest: the
// multipart body is cut before its closing boundary.
func TestDICOMwebCutsChangedBytes(t *testing.T) {
	w := newWebFixture(t)
	mr := uidsOfFile(t, corpusFile(t, "MR_small.dcm"))
	held := w.a.held(t, scanOf(t, w.a, mr))
	var in tenant.ScanInstance
	for _, h := range held {
		if h.SOPInstanceUID == mr.sop {
			in = h
		}
	}
	changed := slices.Clone(w.a.sent[mr.sop])
	changed[len(changed)-1] ^= 0xFF
	if _, err := w.a.store.Put(context.Background(), in.Key, bytes.NewReader(changed)); err != nil {
		t.Fatal(err)
	}
	a := w.get(t, "studies/"+mr.study+"/series/"+mr.series, "Accept", acceptStored)
	if a.status == http.StatusOK && a.readErr == nil && bytes.HasSuffix(bytes.TrimSpace(a.body), []byte("--")) {
		t.Error("a changed instance was sent in a body that looks whole")
	}
	if bytes.Contains(a.body, changed[len(changed)-64:]) {
		t.Error("the changed bytes reached the client")
	}
}

// scanOf returns the labels of the scan of a file's series.
func scanOf(t *testing.T, a *archive, u uids) label.Path {
	t.Helper()
	labels, err := a.p.CatalogLabels(context.Background(), u.study, []string{u.series})
	if err != nil || labels.Session == nil {
		t.Fatalf("the series is not held: %v", err)
	}
	return label.Path{Subject: labels.Session.SubjectLabel, Session: labels.Session.SessionLabel, Scan: labels.Scans[u.series]}
}
