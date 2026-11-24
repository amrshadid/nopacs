package settings

import (
	"slices"
	"testing"
)

// TestImportValuesKeepTheirBounds guards the forms of the import settings:
// the bounds the contract gives import.max-units (1 to 64),
// import.server-max-units (1 to 256) and import.max-dir-entries (1000 to
// 10000000), and import.allowed-roots as root names separated by commas,
// each once.
func TestImportValuesKeepTheirBounds(t *testing.T) {
	for _, c := range []struct {
		parse func(string) (int, error)
		ok    map[string]int
		bad   []string
	}{
		{ParseImportMaxUnits, map[string]int{"1": 1, "4": 4, "64": 64}, []string{"0", "65", "-1", "04", "", "four"}},
		{ParseImportServerMaxUnits, map[string]int{"1": 1, "8": 8, "256": 256}, []string{"0", "257", " 8"}},
		{ParseMaxDirEntries, map[string]int{"1000": 1000, "200000": 200000, "10000000": 10000000}, []string{"999", "10000001", "1e6"}},
	} {
		for v, want := range c.ok {
			if got, err := c.parse(v); err != nil || got != want {
				t.Errorf("%q: %d, %v; want %d", v, got, err, want)
			}
		}
		for _, v := range c.bad {
			if _, err := c.parse(v); err == nil {
				t.Errorf("%q was taken", v)
			}
		}
	}
	if got, err := ParseAllowedRoots("old,lab-two"); err != nil || !slices.Equal(got, []string{"old", "lab-two"}) {
		t.Errorf("allowed roots %v, %v", got, err)
	}
	if got, err := ParseAllowedRoots(""); err != nil || got != nil {
		t.Errorf("no allowed roots read as %v, %v", got, err)
	}
	for _, v := range []string{"old,old", "old, new", "Old", "old,", ",old", "1old"} {
		if _, err := ParseAllowedRoots(v); err == nil {
			t.Errorf("allowed roots %q were taken", v)
		}
	}
	if _, err := ParseImportRoots("old=/srv/old\nnew=/srv/new"); err != nil {
		t.Errorf("import roots refused: %v", err)
	}
	if _, err := ParseImportRoots("old=/srv/old\nnew=/srv/old/inner"); err == nil {
		t.Error("two import roots one inside the other were taken")
	}
}
