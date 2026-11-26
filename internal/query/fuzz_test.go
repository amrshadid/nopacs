package query

import (
	"reflect"
	"testing"
)

// FuzzParse guards the parser against any input: it never panics, a tree it
// returns is within every bound (Check), and the canonical form of that
// tree reads back to the same tree and prints the same.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`Modality == "MR" and (SliceThickness <= 1 or not SeriesDescription contains "loc")`,
		`(0029,"SIEMENS CSA HEADER",10) present`,
		`OtherPatientIDsSequence.PatientID nocase in ("a", "b")`,
		`ImageType[2] like "AX*"`,
		`StudyDate last 30 days and StudyTime between 10:00 and 12:00`,
		`has scan (every instance (AcquisitionDateTime >= 2024-01-31T10:30:15))`,
		`calling_ae == "X" or session.date = today`,
		`Manufacturer matches "^(GE|SIEMENS)$"`,
		`SliceThickness > -1.5e-3`,
		`((((Modality present))))`,
		`Modality != "a\"b\\c"`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := Parse(s)
		if err != nil {
			return
		}
		if err := Check(n); err != nil {
			t.Fatalf("Parse(%q) returned a tree Check refuses: %v", s, err)
		}
		p := Print(n)
		again, err := Parse(p)
		if err != nil {
			t.Fatalf("Parse(%q) printed %q, which does not read: %v", s, p, err)
		}
		if !reflect.DeepEqual(n, again) {
			t.Fatalf("Parse(%q) printed %q, which reads to another tree", s, p)
		}
		if Print(again) != p {
			t.Fatalf("printing %q again gave %q", p, Print(again))
		}
	})
}
