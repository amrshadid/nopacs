package api_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestTrialShowsRedactionsAndPatterns guards what a trial answers of a
// value pattern and of redact: the parts redact cut from a value, with the
// value before and after; the matches a value pattern found in the value
// its rule removed; and nothing of either for other elements.
func TestTrialShowsRedactionsAndPatterns(t *testing.T) {
	f := newFixture(t)
	doc := basicDocument()
	doc.Rules = []*v1.ProfileRule{
		{Name: "smartscore", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "SeriesDescription"}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Redact{Redact: &v1.RedactAction{Pattern: `SmartScore - `}}}},
		{Name: "model", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_ValuePattern{ValuePattern: &v1.ValuePatternSelector{Pattern: `LightSpeed Ultra`}}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}},
	}
	res := mustCall[*v1.TryProfileResponse](t, "try", tryShowing(unsaved(doc), corpusFile(t, "ct5n-2062.dcm"))(f.as(f.lab1Operator)))
	r := res.GetResults()[0]
	if r.GetVerdict() != v1.TrialVerdict_TRIAL_VERDICT_PASSED {
		t.Fatalf("result %v", r)
	}
	series := elementAt(r, "0008103E")
	if series.GetAction() != "redact" || series.GetRedactions() != 1 || series.GetBefore() != "SmartScore - Gated 0.5 sec" ||
		series.GetAfter() != "Gated 0.5 sec" || series.GetPatternMatches() != 0 {
		t.Errorf("the series description %v", series)
	}
	model := elementAt(r, "00081090")
	if model.GetAction() != "remove" || model.GetPatternMatches() != 1 || model.GetRuleName() != "model" || model.GetPresentAfter() || model.GetRedactions() != 0 {
		t.Errorf("the model name %v", model)
	}
	if e := elementAt(r, "00080070"); e.GetPatternMatches() != 0 || e.GetRedactions() != 0 {
		t.Errorf("the manufacturer %v", e)
	}
}
