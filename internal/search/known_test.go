package search

import "testing"

// TestKnownNamesTheAttributesTheTablesHold guards Known: every attribute of
// the registry is known by its keyword, and a keyword the tables do not hold
// is not, whatever its case or its spelling as a tag.
func TestKnownNamesTheAttributesTheTablesHold(t *testing.T) {
	for _, a := range Attributes() {
		if !Known(a.Keyword) {
			t.Errorf("%s is an attribute and is not known", a.Keyword)
		}
	}
	for _, k := range []string{"", "echotime", "00180081", "PatientBirthDate", "session.date"} {
		if Known(k) {
			t.Errorf("%q is known", k)
		}
	}
}
