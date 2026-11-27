package correct

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestEditJudgeRefusesRemovingWhatTheProfileWrites guards the judge of a
// removal: an attribute the profile writes into every file it
// de-identifies (Patient Identity Removed, which PS3.15 E.1 has it set),
// removed by an edit, is refused, since the profile's run over the edited
// file writes it again; an attribute the profile leaves alone may be
// removed.
func TestEditJudgeRefusesRemovingWhatTheProfileWrites(t *testing.T) {
	file, err := os.ReadFile(filepath.Join(corpusDir, studyFiles[0])) // #nosec G304 -- a test's own file name.
	if err != nil {
		t.Fatal(err)
	}
	target := tenant.EditTarget{Profile: editProfile, Receiver: "dimse"}
	j := editJudge(t)
	err = j.JudgeEdit(context.Background(), EditedFile{Target: target, File: file, Removed: []uint32{0x00120062}})
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "PatientIdentityRemoved (0012,0062)") {
		t.Fatalf("removing what the profile writes answered %v, want a refusal", err)
	}
	if err := j.JudgeEdit(context.Background(), EditedFile{Target: target, File: file, Removed: []uint32{0x00181210}}); err != nil {
		t.Fatalf("removing what the profile leaves alone was refused: %v", err)
	}
}

// noCodes answers every lookup as having no code, as a table that has
// none for the value does.
type noCodes struct{}

// Code reports every value as having no code.
func (noCodes) Code(context.Context, string, string) (string, bool, error) { return "", false, nil }

// Assign answers no code for any key.
func (noCodes) Assign(_ context.Context, keys []engine.LookupKey) ([]string, error) {
	return make([]string, len(keys)), nil
}

// lookupJudge returns editJudge with a profile that replaces the Patient
// ID by its code in a table, a file without one held, its engine reading
// lookups from l.
func lookupJudge(t *testing.T, l engine.Lookups) ProfileJudge {
	t.Helper()
	doc := editProfileDoc()
	doc.Rules = append(doc.Rules, &v1.ProfileRule{Name: "subject",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "PatientID"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Lookup{Lookup: &v1.LookupAction{Table: "subjects",
			OnMissing: v1.LookupMissing_LOOKUP_MISSING_QUARANTINE}}}})
	v, err := profile.New(editProfile.Name, editProfile.Version, doc, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	en, err := engine.New(v.Expanded(), editKeys, engine.Options{Lookups: l, Method: editProfile.String()})
	if err != nil {
		t.Fatal(err)
	}
	j := editJudge(t)
	j.Engine = func(context.Context, tenant.ProfileRef) (*engine.Engine, error) { return en, nil }
	return j
}

// TestEditJudgeRefusesWhatALookupLeavesUndecided guards the check after an
// edit under a profile with a lookup table: an engine whose table holds no
// code for a value leaves the verdict undecided (the profile stopped, the
// check after never ran), and the edit is refused, never passed unjudged.
// With FixedLookups the same profile judges the file: a series description
// passes, and a Patient ID, which the lookup replaces, is refused as taken
// out.
func TestEditJudgeRefusesWhatALookupLeavesUndecided(t *testing.T) {
	file, err := os.ReadFile(filepath.Join(corpusDir, studyFiles[0])) // #nosec G304 -- a test's own file name.
	if err != nil {
		t.Fatal(err)
	}
	target := tenant.EditTarget{Profile: editProfile, Receiver: "dimse"}
	edited := func(tag uint32) EditedFile { return EditedFile{Target: target, File: file, Set: []uint32{tag}} }
	ctx := context.Background()

	err = lookupJudge(t, noCodes{}).JudgeEdit(ctx, edited(0x0008103E))
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "could not judge") {
		t.Fatalf("a judge whose lookup decided nothing answered %v, want a refusal", err)
	}
	fixed := lookupJudge(t, FixedLookups{})
	if err := fixed.JudgeEdit(ctx, edited(0x0008103E)); err != nil {
		t.Fatalf("a series description the profile keeps, judged with fixed codes, was refused: %v", err)
	}
	err = fixed.JudgeEdit(ctx, edited(0x00100020))
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "PatientID (0010,0020) out (lookup)") {
		t.Fatalf("a Patient ID the profile looks up answered %v, want it refused as taken out", err)
	}
}
