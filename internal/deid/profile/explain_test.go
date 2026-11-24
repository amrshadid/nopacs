package profile_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// expandFor expands doc with the test catalog, failing the test on a
// problem.
func expandFor(t *testing.T, doc *v1.ProfileDocument) *v1.ExpandedProfile {
	t.Helper()
	e, ps := profile.Expand(doc, testCatalog(t))
	if len(ps) > 0 {
		t.Fatal(ps)
	}
	return e
}

// TestMatchesFollowPrecedence guards the list an explanation is made of:
// every rule whose selector selects the element, the most specific tier
// first and the later rule first within a tier, each saying whether its
// conditions hold; the first that holds is the one Decide chooses.
func TestMatchesFollowPrecedence(t *testing.T) {
	e := expandFor(t, &v1.ProfileDocument{Bases: []string{baseA}, Rules: []*v1.ProfileRule{
		rl("v", vr("PN"), hash()),
		rl("t-mr", kw("PatientName"), keep(), equals(modality, "MR")),
		rl("t", kw("PatientName"), empty()),
	}})
	prog, err := profile.Compile(e)
	if err != nil {
		t.Fatal(err)
	}
	loc := profile.Location{Tag: patientName, VR: "PN"}
	got := prog.Matches(loc, facts{modality: "CT"})
	names := make([]string, len(got))
	for i, m := range got {
		names[i] = e.GetRules()[m.Index].GetRule().GetName() + "/" + m.Tier.String()
		if want := names[i] != "t-mr/tag"; m.Holds != want {
			t.Errorf("%s holds %v", names[i], m.Holds)
		}
	}
	want := []string{"t/tag", "t-mr/tag", "a-name/tag", "v/vr"}
	if len(names) != len(want) {
		t.Fatalf("matches %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("matches %v, want %v", names, want)
			break
		}
	}
	if d := prog.Decide(loc, facts{modality: "CT"}); d.Index != got[0].Index {
		t.Errorf("Decide chose rule %d, the first match is %d", d.Index, got[0].Index)
	}
}
