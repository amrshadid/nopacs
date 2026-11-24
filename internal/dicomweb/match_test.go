package dicomweb

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestMatchesFollowsEachKindOfMatching guards matching (PS3.4 C.2.2.2):
// single values case sensitive with padding ignored, numbers by value,
// UID lists, wild cards, and date and time ranges open at either end.
func TestMatchesFollowsEachKindOfMatching(t *testing.T) {
	cases := []struct {
		k    Key
		v    string
		want bool
	}{
		{Key{VR: "CS", Match: MatchSingle, Value: "MR"}, "MR ", true},
		{Key{VR: "CS", Match: MatchSingle, Value: "MR"}, "mr", false},
		{Key{VR: "IS", Match: MatchSingle, Value: "007"}, "7", true},
		{Key{VR: "IS", Match: MatchSingle, Value: "-0"}, "0", true},
		{Key{VR: "IS", Match: MatchSingle, Value: "7"}, "70", false},
		{Key{VR: "UI", Match: MatchList, UIDs: []string{"1.2", "1.3"}}, "1.3", true},
		{Key{VR: "UI", Match: MatchList, UIDs: []string{"1.2", "1.3"}}, "1.4", false},
		{Key{VR: "LO", Match: MatchWildcard, Value: "T1*"}, "T1 MPRAGE", true},
		{Key{VR: "LO", Match: MatchWildcard, Value: "*RAGE"}, "T1 MPRAGE", true},
		{Key{VR: "LO", Match: MatchWildcard, Value: "T?_*E"}, "T1_MPRAGE", true},
		{Key{VR: "LO", Match: MatchWildcard, Value: "T?_*E"}, "T1_MPRAGES", false},
		{Key{VR: "LO", Match: MatchWildcard, Value: "?"}, "é", true},
		{Key{VR: "DA", Match: MatchRange, Low: "20200101", High: "20201231"}, "20200615", true},
		{Key{VR: "DA", Match: MatchRange, Low: "20200101", High: "20201231"}, "20210101", false},
		{Key{VR: "DA", Match: MatchRange, High: "20201231"}, "19991231", true},
		{Key{VR: "DA", Match: MatchRange, Low: "20200101"}, "", false},
		{Key{VR: "TM", Match: MatchRange, High: "1200"}, "120059.5", true},
		{Key{VR: "TM", Match: MatchRange, High: "1200"}, "1201", false},
	}
	for _, c := range cases {
		if got := c.k.Matches(c.v); got != c.want {
			t.Errorf("%+v against %q: got %v", c.k, c.v, got)
		}
	}
	if (Key{VR: "CS", Match: MatchSingle, Value: "CT"}).Matches("MR", "CT") != true {
		t.Error("a value among several does not match")
	}
	if LiteralPrefix("T1*x?") != "T1" || LiteralPrefix("abc") != "abc" {
		t.Error("literal prefix")
	}
}

// TestElementValuesReadsDICOMJSON guards the text a key is matched
// against, read from an element of DICOM JSON (PS3.18 F.2.2).
func TestElementValuesReadsDICOMJSON(t *testing.T) {
	for raw, want := range map[string][]string{
		`{"vr":"CS","Value":["MR","CT"]}`:                 {"MR", "CT"},
		`{"vr":"IS","Value":[12]}`:                        {"12"},
		`{"vr":"PN","Value":[{"Alphabetic":"Doe^Jane"}]}`: {"Doe^Jane"},
		`{"vr":"OB","InlineBinary":"AAAA"}`:               {},
		`not json`:                                        nil,
	} {
		got := ElementValues(json.RawMessage(raw))
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
}
