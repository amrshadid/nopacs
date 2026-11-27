package engine

import (
	"errors"
	"fmt"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// ErrKeepsPlacingUIDs is returned by ForReception for a profile that leaves
// a Study, Series or SOP Instance UID other than remapped (RemapUID): a
// receiver places each file by the remapping of those UIDs, computed when
// the file lands, so a file the profile left otherwise could never be
// archived where its rows say.
var ErrKeepsPlacingUIDs = errors.New("engine: the profile does not remap the UIDs a file is placed by")

// placingUIDs are the attributes a receiver places a file by (PS3.6):
// Study Instance UID, Series Instance UID and SOP Instance UID.
var placingUIDs = []struct {
	tag     uint32
	keyword string
}{{0x0020000D, "StudyInstanceUID"}, {0x0020000E, "SeriesInstanceUID"}, {0x00080018, "SOPInstanceUID"}}

// noFacts answers every condition subject as absent.
type noFacts struct{}

// Value reports every subject absent.
func (noFacts) Value(v1.ConditionSubject) (string, bool) { return "", false }

// ForReception returns why an expanded profile cannot de-identify the files
// of a receiver, or nil: it is built on Annex E bases of another edition
// than this build's; or a rule that applies whatever the file says keeps,
// removes or changes a placing UID other than by remapping it
// (ErrKeepsPlacingUIDs, the rule named), such as the Retain UIDs option. A
// rule with a condition is judged as for a file the condition does not
// hold for; one that does hold is refused when the file is archived, as
// uid_not_remapped.
func ForReception(e *v1.ExpandedProfile) error {
	for _, b := range e.GetBases() {
		if _, edition, ok := profile.AnnexEName(b); ok && edition != profile.AnnexEEdition {
			return fmt.Errorf("engine: the profile is built on %s, and this build runs the Annex E bases of edition %s only", b, profile.AnnexEEdition)
		}
	}
	// A check of a version before a receiver points at it: its lists are
	// not kept for it.
	dec, err := newProgramDecider(e, true)
	if err != nil {
		return err
	}
	for _, u := range placingUIDs {
		d := dec.Decide(profile.Location{Tag: u.tag, VR: "UI"}, noFacts{})
		a := d.Action
		if a.Kind == ActUID || (a.Kind == ActRemoveUnlessRequired && a.Fallback == FallbackUID) {
			continue
		}
		by := "the default for an element no rule names"
		if d.Name != "" {
			by = "rule " + d.Name
		} else if d.Rule >= 0 {
			by = fmt.Sprintf("rule %d", d.Rule)
		}
		if d.Origin != "" {
			by += " of " + d.Origin
		}
		return fmt.Errorf("%w: %s is given %s by %s", ErrKeepsPlacingUIDs, u.keyword, a.Kind, by)
	}
	return nil
}
