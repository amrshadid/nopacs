package dicomweb

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/tag"
)

// request returns a GET with the Accept header given, none when empty.
func request(accept string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://archive.test/x", nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	return r
}

// TestAcceptInstancesAnswersOnlyAsStored guards the negotiation of a
// retrieve (PS3.18 8.7.3.5): "*" and anything take every stored syntax,
// no transfer-syntax names Explicit VR Little Endian, a named syntax takes
// only itself, application/dicom alone asks for a bare file, and a type
// that is not DICOM is 406.
func TestAcceptInstancesAnswersOnlyAsStored(t *testing.T) {
	const jpeg = "1.2.840.10008.1.2.4.50"
	cases := []struct {
		accept  string
		ts      string
		accepts bool
		bare    bool
	}{
		{"", jpeg, true, false},
		{"*/*", jpeg, true, false},
		{`multipart/related; type="application/dicom"; transfer-syntax=*`, jpeg, true, false},
		{`multipart/related; type="application/dicom"`, jpeg, false, false},
		{`multipart/related; type="application/dicom"`, ExplicitLittle, true, false},
		{`multipart/related; type="application/dicom"; transfer-syntax=` + jpeg, jpeg, true, false},
		{`multipart/related; type="application/dicom"; transfer-syntax=` + jpeg + `, multipart/related; type="application/dicom"; transfer-syntax=1.2.840.10008.1.2`, ImplicitLittle, true, false},
		{`application/dicom; transfer-syntax=*`, jpeg, true, true},
		{`multipart/related; type="*/*"`, jpeg, true, false},
	}
	for _, c := range cases {
		w, err := AcceptInstances(request(c.accept))
		if err != nil {
			t.Errorf("%q: %v", c.accept, err)
			continue
		}
		if w.Accepts(c.ts) != c.accepts || w.Bare != c.bare {
			t.Errorf("%q for %s: accepts %v bare %v", c.accept, c.ts, w.Accepts(c.ts), w.Bare)
		}
	}
	for _, a := range []string{"application/json", `multipart/related; type="image/jpeg"`, `multipart/related; type="application/dicom"; q=0`} {
		if _, err := AcceptInstances(request(a)); err == nil || err.Status != http.StatusNotAcceptable {
			t.Errorf("%q: got %v, want 406", a, err)
		}
	}
}

// TestAcceptFramesAnswersOnlyAsStored guards a frame's media type and
// transfer syntax (PS3.18 8.7.3.3.2, 8.7.3.5): its own type or
// application/octet-stream with "*" or its syntax, octet-stream with no
// syntax only for a native little endian frame, and 406 otherwise.
func TestAcceptFramesAnswersOnlyAsStored(t *testing.T) {
	const jpeg, rle, big = "1.2.840.10008.1.2.4.50", "1.2.840.10008.1.2.5", "1.2.840.10008.1.2.2"
	cases := []struct {
		accept, ts, media, syntax string
	}{
		{"", jpeg, "image/jpeg", jpeg},
		{`multipart/related; type="application/octet-stream"; transfer-syntax=*`, jpeg, MediaOctetStream, jpeg},
		{`multipart/related; type="image/jpeg"`, jpeg, "image/jpeg", jpeg},
		{`multipart/related; type="application/octet-stream"`, ImplicitLittle, MediaOctetStream, ExplicitLittle},
		{`multipart/related; type="image/dicom-rle"; transfer-syntax=` + rle, rle, "image/dicom-rle", rle},
	}
	for _, c := range cases {
		media, syntax, err := AcceptFrames(request(c.accept), c.ts)
		if err != nil || media != c.media || syntax != c.syntax {
			t.Errorf("%q for %s: got %s %s %v", c.accept, c.ts, media, syntax, err)
		}
	}
	for _, c := range [][2]string{
		{`multipart/related; type="application/octet-stream"`, jpeg},
		{`multipart/related; type="application/octet-stream"`, big},
		{`multipart/related; type="image/jls"`, jpeg},
		{`multipart/related; type="image/jpeg"; transfer-syntax=1.2.840.10008.1.2.4.70`, jpeg},
	} {
		if _, _, err := AcceptFrames(request(c[0]), c[1]); err == nil || err.Status != http.StatusNotAcceptable {
			t.Errorf("%q for %s: got %v, want 406", c[0], c[1], err)
		}
	}
}

// TestAcceptJSONAndBulk guards the JSON answers' type (PS3.18 8.7.3.2)
// and the form of bulk data (PS3.18 8.7.3.3.1).
func TestAcceptJSONAndBulk(t *testing.T) {
	for accept, want := range map[string]string{"": MediaDICOMJSON, "application/json": MediaJSON, "application/dicom+json, application/json": MediaDICOMJSON} {
		if got, err := AcceptJSON(request(accept)); err != nil || got != want {
			t.Errorf("%q: %s %v", accept, got, err)
		}
	}
	if _, err := AcceptJSON(request("application/dicom+xml")); err == nil || err.Status != http.StatusNotAcceptable {
		t.Error("xml is taken")
	}
	r := httptest.NewRequest(http.MethodGet, "http://archive.test/x?accept=application/json", nil)
	r.Header.Set("Accept", "application/dicom+xml")
	if got, _ := AcceptJSON(r); got != MediaJSON {
		t.Error("the accept parameter does not stand for the header")
	}
	if multi, err := AcceptBulk(request("")); !multi || err != nil {
		t.Error("bulk data by default")
	}
	if multi, err := AcceptBulk(request(`multipart/related; type="*/*"`)); !multi || err != nil {
		t.Error("bulk data to any type")
	}
	if multi, err := AcceptBulk(request(MediaOctetStream)); multi || err != nil {
		t.Error("bare bulk data")
	}
	if _, err := AcceptBulk(request("image/jpeg")); err == nil {
		t.Error("an image type is taken for bulk data")
	}
}

// TestResultIsDICOMJSONInTagOrder guards a search's result as DICOM JSON
// (PS3.18 F.2): numbers of IS as numbers, several values as an array, the
// members in order of tag, the first value of an attribute kept, and an
// element of binary data left out.
func TestResultIsDICOMJSONInTagOrder(t *testing.T) {
	r := NewResult()
	r.Set(TagSeriesNumber, dataelem.IS, "5")
	r.Set(TagSeriesNumber, dataelem.IS, "6")
	r.Set(TagModalitiesInStudy, dataelem.CS, []string{"MR", "CT"})
	r.SetRaw("00100010", json.RawMessage(`{"vr":"PN","Value":[{"Alphabetic":"Doe^Jane"}]}`))
	r.FromDataset(map[string]json.RawMessage{
		"00200011": json.RawMessage(`{"vr":"IS","Value":[9]}`),
		"7FE00010": json.RawMessage(`{"vr":"OW","BulkDataURI":"x"}`),
		"00091010": json.RawMessage(`{"vr":"OB","InlineBinary":"AAAA"}`),
	}, func(string) bool { return true })
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"00080061":{"Value":["MR","CT"],"vr":"CS"},"00100010":{"vr":"PN","Value":[{"Alphabetic":"Doe^Jane"}]},"00200011":{"Value":[5],"vr":"IS"}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	if !r.Has(TagSeriesNumber) || !r.Has(tagPatientName) {
		t.Error("Has")
	}
}

// tagPatientName is Patient's Name (0010,0010).
var tagPatientName = mustTag("00100010")

// mustTag reads a tag of eight hexadecimal digits.
func mustTag(s string) tag.Tag {
	v, _ := attributeTagHex(s)
	return v
}

// TestMultipartIsRelatedWithItsType guards the body of retrieved
// resources (RFC 2387, PS3.18 8.7.3): its type and boundary parameters,
// each part's Content-Type with its transfer syntax, the parts' bytes.
func TestMultipartIsRelatedWithItsType(t *testing.T) {
	var b bytes.Buffer
	m := NewMultipart(&b, MediaDICOM)
	for _, body := range []string{"one", "two"} {
		w, err := m.Part(MediaDICOM, ExplicitLittle)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	media, params, err := mime.ParseMediaType(m.ContentType())
	if err != nil || media != MediaMultipart || params["type"] != MediaDICOM {
		t.Fatalf("%s: %v", m.ContentType(), err)
	}
	rd := multipart.NewReader(&b, params["boundary"])
	for _, want := range []string{"one", "two"} {
		p, err := rd.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		_, _ = got.ReadFrom(p)
		if got.String() != want || p.Header.Get("Content-Type") != MediaDICOM+"; transfer-syntax="+ExplicitLittle {
			t.Errorf("part %q %q", got.String(), p.Header.Get("Content-Type"))
		}
	}
}

// TestWarningIsQuoted guards a Warning header's form (RFC 7234 5.5) and
// the text of the capped limit, which states the bound.
func TestWarningIsQuoted(t *testing.T) {
	if got := Warning(request(""), `a "b"`); got != `299 archive.test "a \"b\""` {
		t.Errorf("got %s", got)
	}
	if !strings.Contains(WarnCapped, strconv.Itoa(MaxLimit)) {
		t.Error("the capped limit's text does not state the bound")
	}
}
