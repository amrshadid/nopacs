package api_test

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/trial"
)

// TestTryLocal guards a trial run with no server: a corpus file tried
// under the basic profile passes on de-identification alone, element by
// element, with Patient ID removed or replaced and its value before
// answered; a lookup that assigns a missing code reads no table and is
// answered UNASSIGNED, and counted; a file that is not DICOM is
// held as a receiver would hold it; the summary counts the files; a file
// that cannot be read stops the run with its error.
func TestTryLocal(t *testing.T) {
	ct := corpusFile(t, "ct5n-2062.dcm")
	doc := basicDocument()
	doc.Rules = []*v1.ProfileRule{{Name: "code-study", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyID"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Lookup{Lookup: &v1.LookupAction{Table: "studies", OnMissing: v1.LookupMissing_LOOKUP_MISSING_ASSIGN}}}}}
	e, ps := profile.Expand(doc, profile.Shipped())
	if len(ps) > 0 {
		t.Fatal(ps)
	}
	_, sum, err := profile.Checksum(e)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"ct.dcm": ct, "notes.txt": []byte("not a DICOM file at all, only text")}
	read := func(name string) ([]byte, error) {
		b, ok := files[name]
		if !ok {
			return nil, errors.New("no such file")
		}
		return b, nil
	}
	res, err := trial.TryLocal(context.Background(), trial.LocalTrial{Expanded: e, Checksum: sum, Method: "unsaved " + sum[:16],
		Names: []string{"ct.dcm", "notes.txt"}, Read: read})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetResults()) != 2 || res.GetSummary().GetFiles() != 2 || res.GetChecksum() != sum {
		t.Fatalf("results %d, summary %v", len(res.GetResults()), res.GetSummary())
	}
	r := res.GetResults()[0]
	if r.GetName() != "ct.dcm" || r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_PASSED || !r.GetDeidentificationOnly() || r.GetLookupsUnassigned() != 1 {
		t.Errorf("the CT file: %v %s %s, %d unassigned", r.GetVerdict(), r.GetReason(), r.GetCause(), r.GetLookupsUnassigned())
	}
	if id := elementAt(r, "00100020"); id.GetBefore() != ctID || id.GetAfter() == ctID {
		t.Errorf("Patient ID: %v", id)
	}
	if s := elementAt(r, "00200010"); s.GetAfter() != "UNASSIGNED" {
		t.Errorf("Study ID: %v", s)
	}
	if r := res.GetResults()[1]; r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_HELD || r.GetReason() != "not_dicom" {
		t.Errorf("a text file: %v %s", r.GetVerdict(), r.GetReason())
	}
	_, err = trial.TryLocal(context.Background(), trial.LocalTrial{Expanded: e, Checksum: sum, Names: []string{"gone.dcm"}, Read: read})
	if err == nil {
		t.Error("a file that cannot be read did not stop the run")
	}
}
