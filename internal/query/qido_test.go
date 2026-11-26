package query

import (
	"errors"
	"testing"
)

// q builds QIDO parameters from key and value pairs.
func q(kv ...string) []QIDOParam {
	var ps []QIDOParam
	for i := 0; i+1 < len(kv); i += 2 {
		ps = append(ps, QIDOParam{Key: kv[i], Value: kv[i+1]})
	}
	return ps
}

// TestFromQIDOMatching guards the mapping of each kind of matching PS3.4
// C.2.2.2 defines onto the tree, over values in the form of the
// standard's examples (PS3.18 8.3.4.1: keyword or tag, dotted sequence
// paths; PS3.4 C.2.2.2: single value, UID list, universal, wildcard,
// range, sequence), and the parameters that are not matching keys.
func TestFromQIDOMatching(t *testing.T) {
	cases := []struct {
		params []QIDOParam
		want   string
	}{
		{q("PatientID", "11235813"), `PatientID == "11235813"`},
		{q("00100020", "11235813"), `PatientID == "11235813"`},
		{q("PatientName", "SMITH^JOHN"), `PatientName = "SMITH^JOHN"`},
		{q("00100010", "SMITH*"), `PatientName nocase like "SMITH*"`},
		{q("StudyDescription", "Ab?d*"), `StudyDescription like "Ab?d*"`},
		{q("Modality", "MR"), `Modality == "MR"`},
		{q("StudyInstanceUID", "1.2.392.200036.9116.2.2.2.1762893313.1029997326.945873"), `StudyInstanceUID == "1.2.392.200036.9116.2.2.2.1762893313.1029997326.945873"`},
		{q("SeriesInstanceUID", "1.2.3,1.2.4"), `SeriesInstanceUID in ("1.2.3", "1.2.4")`},
		{q("SOPInstanceUID", `1.2.3\1.2.4\1.2.5`), `SOPInstanceUID in ("1.2.3", "1.2.4", "1.2.5")`},
		{q("StudyDate", "20130509"), `StudyDate = 2013-05-09`},
		{q("StudyDate", "20060705-20060707"), `StudyDate between 2006-07-05 and 2006-07-07`},
		{q("StudyDate", "-20060707"), `StudyDate <= 2006-07-07`},
		{q("StudyDate", "20060705-"), `StudyDate >= 2006-07-05`},
		{q("StudyTime", "1030-1200"), `StudyTime between 10:30:00 and 12:00:00`},
		{q("AcquisitionDateTime", "20060705103000-20060705120000"), `AcquisitionDateTime between 2006-07-05T10:30:00 and 2006-07-05T12:00:00`},
		{q("SeriesNumber", "4"), `SeriesNumber = 4`},
		{q("SliceThickness", "1.5"), `SliceThickness = 1.5`},
		{q("ReferencedSeriesSequence.SeriesInstanceUID", "1.2.3"), `ReferencedSeriesSequence.SeriesInstanceUID == "1.2.3"`},
		{q("00081115.0020000E", "1.2.3"), `ReferencedSeriesSequence.SeriesInstanceUID == "1.2.3"`},
		{q("00290010", "SIEMENS CSA HEADER", "00291008", "IMAGE NUM 4"), `(0029,"SIEMENS CSA HEADER",08) == "IMAGE NUM 4"`},
		{q("PatientName", "", "Modality", "*"), ``},
		{q("ReferencedSeriesSequence", ""), ``},
		{q("limit", "10", "offset", "20", "includefield", "all", "Modality", "CT"), `Modality == "CT"`},
		{q("Modality", "CT", "StudyDate", "20130509-"), `Modality == "CT" and StudyDate >= 2013-05-09`},
		{q("emptyvaluematching", "true", "AccessionNumber", `""`), `AccessionNumber absent`},
		{q("multiplevaluematching", "true", "Modality", `CT\MR`), `Modality == "CT" or Modality == "MR"`},
		{q("multiplevaluematching", "true", "Modality", `CT\*`), ``},
		{q("StudyDate", "*"), ``},
		{q("StudyTime", "*", "AcquisitionDateTime", "*"), ``},
		{q("StudyInstanceUID", "*"), ``},
		{q("AcquisitionDateTime", "20060705103000+0100"), `AcquisitionDateTime = 2006-07-05T10:30:00`},
		{q("AcquisitionDateTime", "20060705103000-0500"), `AcquisitionDateTime = 2006-07-05T10:30:00`},
		{q("AcquisitionDateTime", "20060705103000-0500-20060705120000-0500"), `AcquisitionDateTime between 2006-07-05T10:30:00 and 2006-07-05T12:00:00`},
		{q("AcquisitionDateTime", "-20060705120000+0100"), `AcquisitionDateTime <= 2006-07-05T12:00:00`},
		{q("AcquisitionDateTime", "20060705103000-0500-"), `AcquisitionDateTime >= 2006-07-05T10:30:00`},
		{q("AcquisitionDateTime", "2006-2007"), `AcquisitionDateTime between 2006-01-01T00:00:00 and 2007-01-01T00:00:00`},
		{q("AcquisitionDateTime", "20060705-20060706"), `AcquisitionDateTime between 2006-07-05T00:00:00 and 2006-07-06T00:00:00`},
	}
	for _, c := range cases {
		n, _, err := FromQIDO(c.params)
		if err != nil {
			t.Errorf("%v: %v", c.params, err)
			continue
		}
		got := ""
		if n != nil {
			got = Print(n)
			if _, err := Parse(got); err != nil {
				t.Errorf("%v: the tree printed does not read back: %v", c.params, err)
			}
		}
		if got != c.want {
			t.Errorf("%v\n got %s\nwant %s", c.params, got, c.want)
		}
	}
}

// TestFromQIDORefusals guards the queries answered 400: a key twice, an
// unknown keyword, a pattern of a UID, a malformed range, a private
// element without its creator, "" without empty value matching, "\"
// without multiple value matching, a sequence key with a value, a number
// that is not one.
func TestFromQIDORefusals(t *testing.T) {
	for _, params := range [][]QIDOParam{
		q("Modality", "CT", "00080060", "MR"),
		q("NoSuchKeyword", "x"),
		q("StudyInstanceUID", "1.2.*"),
		q("StudyDate", "2006-07-05"),
		q("StudyDate", "20060705-20060707-20060709"),
		q("StudyDate", "-"),
		q("StudyDate", "2006*"),
		q("00291008", "x"),
		q("AccessionNumber", `""`),
		q("Modality", `CT\MR`),
		q("ReferencedSeriesSequence", "x"),
		q("SeriesNumber", "four"),
		q("A.B.C.D.E.F.G.H.I", "x"),
		q("AcquisitionDateTime", "20060705103000+05"),
		q("AcquisitionDateTime", "20060705103000+0500+0100"),
	} {
		_, _, err := FromQIDO(params)
		var qe *QIDOError
		if !errors.As(err, &qe) || !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v, want a *QIDOError", params, err)
		}
	}
}

// TestFromQIDOFuzzyWarning guards the warning a server without fuzzy
// matching sends (PS3.18 8.3.4.1).
func TestFromQIDOFuzzyWarning(t *testing.T) {
	n, w, err := FromQIDO(q("fuzzymatching", "true", "PatientName", "SMITH"))
	if err != nil || len(w) != 1 || w[0] != WarningFuzzy || Print(n) != `PatientName = "SMITH"` {
		t.Errorf("fuzzy matching gave %v, %v, %v", n, w, err)
	}
	if _, w, _ := FromQIDO(q("fuzzymatching", "false", "PatientName", "SMITH")); len(w) != 0 {
		t.Errorf("fuzzymatching=false warned %v", w)
	}
}

// TestFromQIDOCompiles guards that the tree of a QIDO query compiles to
// SQL over the same registry as the archive's own search.
func TestFromQIDOCompiles(t *testing.T) {
	n, _, err := FromQIDO(q("Modality", "MR", "SeriesDescription", "*MPRAGE*"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Compiler{Registry: testRegistry(t), Dialect: Postgres}.Compile(n, LevelScan)
	want := `(t0.modality = $1 AND (t0.series_description_folded LIKE $2 ESCAPE '\' AND t0.series_description LIKE $3 ESCAPE '\'))`
	if err != nil || got.Where != want {
		t.Errorf("got %s, %v\nwant %s", got.Where, err, want)
	}
}
