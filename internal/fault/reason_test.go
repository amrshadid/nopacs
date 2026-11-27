package fault

import (
	"regexp"
	"testing"
)

// TestCases guards the table of reasons: every reason is a fixed word in
// capitals and underscores (the form google.rpc.ErrorInfo asks of a
// reason), appears once, is a case of a kind other than Internal (whose
// message says nothing of its cause), and is found by CaseOf with its kind
// and metadata keys; a word the table does not hold is not found.
func TestCases(t *testing.T) {
	word := regexp.MustCompile(`^[A-Z][A-Z0-9_]+[A-Z0-9]$`)
	seen := map[string]bool{}
	for _, c := range Cases() {
		if !word.MatchString(c.Reason) {
			t.Errorf("reason %q is not UPPER_SNAKE_CASE", c.Reason)
		}
		if seen[c.Reason] {
			t.Errorf("reason %s appears twice", c.Reason)
		}
		seen[c.Reason] = true
		if c.Kind == Internal {
			t.Errorf("reason %s is a case of Internal", c.Reason)
		}
		got, ok := CaseOf(c.Reason)
		if !ok || got.Kind != c.Kind || len(got.Metadata) != len(c.Metadata) {
			t.Errorf("CaseOf(%s) = %+v %v, want %+v", c.Reason, got, ok, c)
		}
	}
	if _, ok := CaseOf("NOT_A_REASON"); ok {
		t.Error("CaseOf found a word the table does not hold")
	}
	if c, _ := CaseOf(ReasonNotOnline); c.Kind != Unavailable || len(c.Metadata) != 1 || c.Metadata[0] != "recall_state" {
		t.Errorf("NOT_ONLINE is %+v, want unavailable with recall_state", c)
	}
}

// TestCasesIsACopy guards that a caller changing what Cases or CaseOf
// returned does not change the table.
func TestCasesIsACopy(t *testing.T) {
	cs := Cases()
	for i := range cs {
		if len(cs[i].Metadata) > 0 {
			cs[i].Metadata[0] = "changed"
		}
		cs[i].Reason = "CHANGED"
	}
	c, _ := CaseOf(ReasonHeld)
	c.Metadata[0] = "changed"
	if c, ok := CaseOf(ReasonHeld); !ok || c.Metadata[0] != "hold" {
		t.Errorf("the table changed through a copy: %+v %v", c, ok)
	}
}
