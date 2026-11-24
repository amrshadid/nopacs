package engine

import (
	"errors"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestForReception guards which profiles a receiver may de-identify with:
// the basic profile remaps the placing UIDs and is taken; the Retain UIDs
// option, and an own rule that keeps SOP Instance UID, are refused with
// ErrKeepsPlacingUIDs naming the attribute and what decided; a rule with a
// condition is not taken for one that always applies; and a profile with
// pixel rules is taken.
func TestForReception(t *testing.T) {
	expand := func(doc *v1.ProfileDocument) *v1.ExpandedProfile {
		t.Helper()
		e, problems := profile.Expand(doc, profile.Shipped())
		if problems != nil {
			t.Fatalf("expanding: %v", problems)
		}
		return e
	}
	basic := []string{profile.AnnexE("basic")}
	if err := ForReception(expand(&v1.ProfileDocument{Bases: basic})); err != nil {
		t.Errorf("the basic profile: %v", err)
	}
	err := ForReception(expand(&v1.ProfileDocument{Bases: append(basic, profile.AnnexE("retain-uids"))}))
	if !errors.Is(err, ErrKeepsPlacingUIDs) || !strings.Contains(err.Error(), "StudyInstanceUID") || !strings.Contains(err.Error(), "retain-uids") {
		t.Errorf("Retain UIDs: %v", err)
	}
	err = ForReception(expand(&v1.ProfileDocument{Bases: basic, Rules: []*v1.ProfileRule{rule("keep-sop", kw("SOPInstanceUID"), aKeep)}}))
	if !errors.Is(err, ErrKeepsPlacingUIDs) || !strings.Contains(err.Error(), "SOPInstanceUID is given keep by rule keep-sop") {
		t.Errorf("an own rule keeping SOP Instance UID: %v", err)
	}
	ct := &v1.ProfileCondition{Subject: v1.ConditionSubject_CONDITION_SUBJECT_MODALITY, Test: &v1.ProfileCondition_Equals{Equals: "CT"}}
	if err := ForReception(expand(&v1.ProfileDocument{Bases: basic, Rules: []*v1.ProfileRule{rule("keep-ct", kw("SOPInstanceUID"), aKeep, ct)}})); err != nil {
		t.Errorf("a rule with a condition: %v", err)
	}
	withPixels := expand(&v1.ProfileDocument{Bases: basic, PixelRules: []*v1.PixelRule{{
		Name: "accept", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "reviewed later",
	}}})
	if err := ForReception(withPixels); err != nil {
		t.Errorf("pixel rules: %v", err)
	}
}
