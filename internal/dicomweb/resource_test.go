package dicomweb

import (
	"net/http"
	"slices"
	"testing"
)

// TestParseResourceNamesEachServedResource guards the reading of every
// path of the search and retrieve services (PS3.18 10.4, 10.6) into its
// kind, level and UIDs, and the 404 and 400 of a path that is not one.
func TestParseResourceNamesEachServedResource(t *testing.T) {
	const st, se, in = "1.2.3", "1.2.3.4", "1.2.3.4.5"
	cases := []struct {
		path string
		want Resource
	}{
		{"studies", Resource{Kind: Search, Level: Study}},
		{"series", Resource{Kind: Search, Level: Series}},
		{"instances", Resource{Kind: Search, Level: Instance}},
		{"studies/" + st, Resource{Kind: Retrieve, Level: Study, Study: st}},
		{"studies/" + st + "/metadata", Resource{Kind: Metadata, Level: Study, Study: st}},
		{"studies/" + st + "/series", Resource{Kind: Search, Level: Series, Study: st}},
		{"studies/" + st + "/instances", Resource{Kind: Search, Level: Instance, Study: st}},
		{"studies/" + st + "/series/" + se, Resource{Kind: Retrieve, Level: Series, Study: st, Series: se}},
		{"studies/" + st + "/series/" + se + "/metadata", Resource{Kind: Metadata, Level: Series, Study: st, Series: se}},
		{"studies/" + st + "/series/" + se + "/instances", Resource{Kind: Search, Level: Instance, Study: st, Series: se}},
		{"studies/" + st + "/series/" + se + "/instances/" + in, Resource{Kind: Retrieve, Level: Instance, Study: st, Series: se, Instance: in}},
		{"studies/" + st + "/series/" + se + "/instances/" + in + "/metadata", Resource{Kind: Metadata, Level: Instance, Study: st, Series: se, Instance: in}},
		{"studies/" + st + "/series/" + se + "/instances/" + in + "/bulkdata", Resource{Kind: BulkData, Level: Instance, Study: st, Series: se, Instance: in}},
		{"studies/" + st + "/series/" + se + "/instances/" + in + "/frames/1,3,2", Resource{Kind: Frames, Level: Instance, Study: st, Series: se, Instance: in, FrameNumbers: []int{1, 3, 2}}},
	}
	for _, c := range cases {
		got, err := ParseResource(c.path)
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		if got.Kind != c.want.Kind || got.Level != c.want.Level || got.Study != c.want.Study || got.Series != c.want.Series ||
			got.Instance != c.want.Instance || !slices.Equal(got.FrameNumbers, c.want.FrameNumbers) {
			t.Errorf("%s: got %+v, want %+v", c.path, got, c.want)
		}
	}
	for path, status := range map[string]int{
		"":                     http.StatusNotFound,
		"patients":             http.StatusNotFound,
		"studies/1.2/rendered": http.StatusNotFound,
		"series/1.2":           http.StatusNotFound,
		"studies/1.02":         http.StatusBadRequest,
		"studies/abc/series":   http.StatusBadRequest,
		"studies/1.2/series/1.3/instances/1.4/frames/0":   http.StatusBadRequest,
		"studies/1.2/series/1.3/instances/1.4/frames/1,a": http.StatusBadRequest,
		"studies/1.2/series/1.3/instances/1.4/frames/+1":  http.StatusBadRequest,
		"studies/1.2/series/1.3/instances/1.4/frames":     http.StatusNotFound,
	} {
		if _, err := ParseResource(path); err == nil || err.Status != status {
			t.Errorf("%q: got %v, want status %d", path, err, status)
		}
	}
}

// TestShapeOfHoldsNoUID guards the audit entry's resource: the literal
// segments of the path with each UID and the frame list replaced, so no
// UID reaches a log, and "unknown" for anything else.
func TestShapeOfHoldsNoUID(t *testing.T) {
	for path, want := range map[string]string{
		"studies":                             "studies",
		"studies/1.2.3/series/1.2.4/metadata": "studies/{study}/series/{series}/metadata",
		"studies/1.2/series/1.3/instances/1.4/frames/1,2": "studies/{study}/series/{series}/instances/{instance}/frames/{frames}",
		"studies/1.2/patient":                             "unknown",
		"PatientName^Doe":                                 "unknown",
		"studies/1/series/2/instances/3/frames/4/5":       "unknown",
	} {
		if got := ShapeOf(path); got != want {
			t.Errorf("%q: got %q, want %q", path, got, want)
		}
	}
}
