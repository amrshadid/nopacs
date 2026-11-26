package query

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"
)

// evalCase is a condition and its value over one file.
type evalCase struct {
	cond string
	want Truth
}

// runEval evaluates each case over ds.
func runEval(t *testing.T, name string, ds *dataset.Dataset, cases []evalCase) {
	t.Helper()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for _, c := range cases {
		r, err := Eval(mustParse(t, c.cond), Subject{Dataset: ds, Now: now})
		if err != nil {
			t.Errorf("%s: Eval(%q): %v", name, c.cond, err)
			continue
		}
		if r.Truth != c.want {
			t.Errorf("%s: %q = %v, want %v", name, c.cond, r.Truth, c.want)
		}
	}
}

// TestEvalMRSmall guards comparisons of each kind over a public file whose
// values pydicom reads as: Modality MR, Manufacturer TOSHIBA_MEC,
// SliceThickness 0.8, StudyDate 20040826, StudyTime 185059, Rows 64 (US),
// ImageType DERIVED\SECONDARY\OTHER, EchoTime 240.0000, PixelSpacing
// 0.3125\0.3125, SoftwareVersions V3.51*P25, InstitutionName TOSHIBA, and
// an empty ReferringPhysicianName.
func TestEvalMRSmall(t *testing.T) {
	ds := readFile(t, filepath.Join(receiveTestdata, "MR_small.dcm"))
	runEval(t, "MR_small", ds, []evalCase{
		{`Modality == "MR"`, True},
		{`Modality == "mr"`, False},
		{`Modality = "mr"`, True},
		{`Modality != "CT"`, True},
		{`Modality in ("CT", "MR")`, True},
		{`Modality in ("CT", "PT")`, False},
		{`Manufacturer prefix "TOSH"`, True},
		{`Manufacturer prefix "tosh"`, False},
		{`Manufacturer nocase prefix "tosh"`, True},
		{`Manufacturer like "TOSHIBA_*"`, True},
		{`Manufacturer like "TOSHIBA?MEC"`, True},
		{`Manufacturer like "TOSHIBA%MEC"`, False},
		{`Manufacturer contains "iba_m"`, True},
		{`Manufacturer words "mec toshiba"`, True},
		{`Manufacturer words "mec siemens"`, False},
		{`Manufacturer matches "^TOSHIBA_[A-Z]+$"`, True},
		{`Manufacturer matches "^toshiba"`, False},
		{`Manufacturer matches "(?i)^toshiba"`, True},
		{`SoftwareVersions == "V3.51*P25"`, True},
		{`SliceThickness = 0.8`, True},
		{`SliceThickness < 1`, True},
		{`SliceThickness between 0.5 and 0.8`, True},
		{`SliceThickness > 0.8`, False},
		{`EchoTime = 240`, True},
		{`Rows = 64`, True},
		{`Rows >= 65`, False},
		{`PixelSpacing = 0.3125`, True},
		{`PixelSpacing[1] = 0.3125`, True},
		{`PixelSpacing[2] = 0.3125`, False},
		{`ImageType == "SECONDARY"`, True},
		{`ImageType[0] == "SECONDARY"`, False},
		{`ImageType[1] == "SECONDARY"`, True},
		{`StudyDate = 2004-08-26`, True},
		{`StudyDate between 2004-01-01 and 2004-12-31`, True},
		{`StudyDate >= -30d`, False},
		{`StudyDate < -20y`, True},
		{`StudyTime > 18:00`, True},
		{`StudyTime = 18:50:59`, True},
		{`StudyDate = "20040826"`, True},
		{`StudyDate = "2004-08-26"`, False},
		{`InstitutionName present`, True},
		{`ReferringPhysicianName present`, False},
		{`ReferringPhysicianName absent`, True},
		{`ReferringPhysicianName = ""`, False},
		{`SeriesDescription present`, False},
		{`SeriesDescription == "x"`, False},
		{`not SeriesDescription == "x"`, True},
		{`SeriesDescription absent and Modality == "MR"`, True},
		{`Modality == "CT" or (Rows = 64 and not Manufacturer prefix "GE")`, True},
	})
}

// TestEvalSequencesAndAttributeTags guards paths through nested sequences
// and binary values, over a public segmentation whose values pydicom reads
// as: SegmentSequence one item, SegmentLabel Liver; the shared functional
// groups' PixelMeasuresSequence SliceThickness 1.0; DimensionIndexSequence
// two items, DimensionIndexPointer (0062,000B) then (0020,0032); ImageType
// DERIVED\PRIMARY; Rows 512.
func TestEvalSequencesAndAttributeTags(t *testing.T) {
	ds := readFile(t, filepath.Join(receiveTestdata, "liver_1frame.dcm"))
	runEval(t, "liver_1frame", ds, []evalCase{
		{`SegmentSequence.SegmentLabel == "Liver"`, True},
		{`SegmentSequence.SegmentLabel = "liver"`, True},
		{`SegmentSequence.SegmentLabel == "Spleen"`, False},
		{`SegmentSequence present`, True},
		{`SegmentSequence.SegmentDescription present`, False},
		{`SharedFunctionalGroupsSequence.PixelMeasuresSequence.SliceThickness = 1`, True},
		{`SharedFunctionalGroupsSequence.PixelMeasuresSequence.SliceThickness > 1`, False},
		{`DimensionIndexSequence.DimensionIndexPointer == "00200032"`, True},
		{`DimensionIndexSequence.DimensionIndexPointer == "0062000B"`, True},
		{`DimensionIndexSequence.DimensionDescriptionLabel == "ImagePositionPatient"`, True},
		{`SegmentLabel present`, False},
		{`Modality.SegmentLabel present`, False},
	})
}

// TestEvalPrivateCreatorAndAnyItem guards the private tag by creator and a
// match in a sequence's second item, over pydicom's CT_small.dcm, whose
// values pydicom reads as: (0009,0010) GEMS_IDEN_01 with (0009,1004)
// "HiSpeed CT/i"; (0043,0010) GEMS_PARM_01 with (0043,101A) SL 7 and
// (0043,1040) FL 178.07992553710938; (0043,1012) SS 14\2\3;
// OtherPatientIDsSequence items PatientID ABCD1234 then 1234ABCD;
// SliceThickness 5.0; Manufacturer GE MEDICAL SYSTEMS.
func TestEvalPrivateCreatorAndAnyItem(t *testing.T) {
	ds := corpusFile(t, "CT_small.dcm")
	runEval(t, "CT_small", ds, []evalCase{
		{`(0009,"GEMS_IDEN_01",04) == "HiSpeed CT/i"`, True},
		{`(0009,1004) == "HiSpeed CT/i"`, True},
		{`(0009,"GEMS_IDEN_01",04) contains "speed"`, True},
		{`(0009,"GEMS_ACQU_01",04) present`, False},
		{`(0009,"NO SUCH CREATOR",04) present`, False},
		{`(0043,"GEMS_PARM_01",1A) = 7`, True},
		{`(0043,"GEMS_PARM_01",40) between 178.07 and 178.08`, True},
		{`(0043,"GEMS_PARM_01",12) = 3`, True},
		{`(0043,"GEMS_PARM_01",12)[0] = 14`, True},
		{`(0043,"GEMS_PARM_01",12)[0] = 3`, False},
		{`OtherPatientIDsSequence.PatientID == "1234ABCD"`, True},
		{`OtherPatientIDsSequence.PatientID == "ABCD1234"`, True},
		{`OtherPatientIDsSequence.PatientID == "XXXX"`, False},
		{`SliceThickness = 5`, True},
		{`Manufacturer words "medical ge"`, True},
	})
}

// TestEvalImplicitNestedSequences guards an implicit-VR file, whose values
// the dictionary types, and a path two sequences deep, over pydicom's
// rtplan.dcm, whose values pydicom reads as: DoseReferenceSequence items
// TargetPrescriptionDose absent then 30.826203;
// BeamSequence.ControlPointSequence.NominalBeamEnergy 6; BeamName Field 1.
func TestEvalImplicitNestedSequences(t *testing.T) {
	ds := corpusFile(t, "rtplan.dcm")
	runEval(t, "rtplan", ds, []evalCase{
		{`DoseReferenceSequence.TargetPrescriptionDose > 30`, True},
		{`DoseReferenceSequence.TargetPrescriptionDose present`, True},
		{`DoseReferenceSequence.DoseReferenceStructureType == "COORDINATES"`, True},
		{`BeamSequence.ControlPointSequence.NominalBeamEnergy = 6`, True},
		{`BeamSequence.ControlPointSequence.NominalBeamEnergy = 18`, False},
		{`BeamSequence.BeamName == "Field 1"`, True},
		{`Modality == "RTPLAN"`, True},
	})
}

// synthetic returns a data set holding Modality CT, Manufacturer
// " SIEMENS " (padded), a private block of creator "ACME 1" at (0011,10xx)
// with (0011,1001) "abc", Rows 256 in the given byte order, a long
// ImageComments, and an empty sequence.
func synthetic(bigEndian bool) *dataset.Dataset {
	ds := dataset.NewDataset()
	add := func(g, e uint16, vr dataelem.VR, v any) {
		_ = ds.Add(dataelem.NewDataElement(tag.New(g, e), vr, v))
	}
	add(0x0008, 0x0060, dataelem.CS, []byte("CT"))
	add(0x0008, 0x0070, dataelem.LO, []byte(" SIEMENS "))
	add(0x0011, 0x0010, dataelem.LO, []byte("ACME 1 "))
	add(0x0011, 0x1001, dataelem.UN, []byte("abc "))
	add(0x0011, 0x1002, dataelem.UN, []byte{0x00, 0x01, 0x02, 0x03})
	rows := []byte{0x00, 0x01}
	if bigEndian {
		rows = []byte{0x01, 0x00}
	}
	add(0x0028, 0x0010, dataelem.US, rows)
	add(0x0020, 0x4000, dataelem.LT, []byte(strings.Repeat("x", MaxReadLength+1)))
	_ = ds.Add(dataelem.NewDataElement(tag.New(0x0008, 0x1115), dataelem.SQ, sequence.New()))
	return ds
}

// TestEvalSynthetic guards padding, a private value held as UN, the byte
// order of binary values, and an empty sequence.
func TestEvalSynthetic(t *testing.T) {
	runEval(t, "little", synthetic(false), []evalCase{
		{`Manufacturer == "SIEMENS"`, True},
		{`(0011,"ACME 1",01) == "abc"`, True},
		{`(0011,"ACME 1",02) present`, False},
		{`Rows = 256`, True},
		{`ReferencedSeriesSequence present`, False},
		{`ReferencedSeriesSequence absent`, True},
	})
	ds := synthetic(true)
	r, err := Eval(mustParse(t, `Rows = 256`), Subject{Dataset: ds, BigEndian: true})
	if err != nil || r.Truth != True {
		t.Errorf("a big-endian US read as %v, %v", r.Truth, err)
	}
}

// TestEvalKleene guards the three-valued logic of a dry run: a leaf on an
// attribute the profile removed is unknown, false wins an "and", true wins
// an "or", "not" keeps unknown, and the leaf that decided is reported.
func TestEvalKleene(t *testing.T) {
	ds := synthetic(false)
	removed := func(f Field) bool { return FieldString(f) == "PatientName" }
	cases := []evalCase{
		{`PatientName == "X"`, Unknown},
		{`PatientName absent`, Unknown},
		{`not PatientName == "X"`, Unknown},
		{`PatientName == "X" and Modality == "CT"`, Unknown},
		{`PatientName == "X" and Modality == "MR"`, False},
		{`Modality == "MR" and PatientName == "X"`, False},
		{`PatientName == "X" or Modality == "CT"`, True},
		{`PatientName == "X" or Modality == "MR"`, Unknown},
		{`not (PatientName == "X" or Modality == "MR")`, Unknown},
		{`Modality == "CT"`, True},
	}
	for _, c := range cases {
		r, err := Eval(mustParse(t, c.cond), Subject{Dataset: ds, Removed: removed})
		if err != nil || r.Truth != c.want {
			t.Errorf("%q = %v, %v; want %v", c.cond, r.Truth, err, c.want)
		}
	}
	n := mustParse(t, `Modality == "CT" and Manufacturer == "GE" and PatientName == "X"`)
	r, _ := Eval(n, Subject{Dataset: ds, Removed: removed})
	if r.Truth != False || r.Decided != n.(*And).Children[1] {
		t.Errorf("the deciding leaf of a false and is %v (%v), want the second child", r.Decided, r.Truth)
	}
	n = mustParse(t, `Modality == "MR" or PatientName == "X" or Modality == "CT"`)
	r, _ = Eval(n, Subject{Dataset: ds, Removed: removed})
	if r.Truth != True || r.Decided != n.(*Or).Children[2] {
		t.Errorf("the deciding leaf of a true or is %v (%v), want the third child", r.Decided, r.Truth)
	}
}

// TestEvalReadBound guards the bound on a value read: a value longer than
// MaxReadLength makes the condition one that cannot be evaluated, naming
// the field and not the value.
func TestEvalReadBound(t *testing.T) {
	_, err := Eval(mustParse(t, `ImageComments contains "xxx"`), Subject{Dataset: synthetic(false)})
	var re *ReadError
	if !errors.As(err, &re) || re.Field != "ImageComments" || strings.Contains(err.Error(), "xxx") {
		t.Fatalf("a long value gave %v, want a *ReadError naming ImageComments", err)
	}
}

// TestEvalCatalogAndSearchOnly guards catalog fields (given, absent,
// unknown) and the refusal of "has" in routing.
func TestEvalCatalogAndSearchOnly(t *testing.T) {
	ds := synthetic(false)
	catalog := func(name string) (string, bool, error) {
		switch name {
		case "calling_ae":
			return "SCANNER1", true, nil
		case "called_ae":
			return "", false, nil
		}
		return "", false, errors.New("unknown")
	}
	runCat := func(cond string) (Truth, error) {
		r, err := Eval(mustParse(t, cond), Subject{Dataset: ds, Catalog: catalog})
		return r.Truth, err
	}
	if v, err := runCat(`calling_ae == "SCANNER1"`); v != True || err != nil {
		t.Errorf("calling_ae: %v %v", v, err)
	}
	if v, err := runCat(`called_ae absent`); v != True || err != nil {
		t.Errorf("called_ae: %v %v", v, err)
	}
	var re *ReadError
	if _, err := runCat(`receiver_x present`); !errors.As(err, &re) {
		t.Errorf("an unknown catalog field gave %v", err)
	}
	if _, err := Eval(mustParse(t, `calling_ae present`), Subject{Dataset: ds}); !errors.As(err, &re) {
		t.Errorf("a catalog field without a catalog gave %v", err)
	}
	if _, err := runCat(`has scan (Modality present)`); !errors.Is(err, ErrSearchOnly) {
		t.Errorf("has in routing gave %v", err)
	}
}

// TestGlob guards the pattern match of "like": "*" and "?" only, any
// other character literal, and no blow-up on patterns made to backtrack.
func TestGlob(t *testing.T) {
	cases := []struct {
		s, p string
		want bool
	}{
		{"T1_MPRAGE", "T1*", true}, {"T1_MPRAGE", "*MPRAGE", true}, {"T1_MPRAGE", "T1?MPRAGE", true},
		{"T1_MPRAGE", "T1%", false}, {"T1XMPRAGE", "T1_MPRAGE", false}, {"", "*", true}, {"", "?", false},
		{"abc", "a*b*c", true}, {"abc", "a*c*", true}, {"ab", "a*b*c", false}, {"Ünï", "?n?", true},
	}
	for _, c := range cases {
		if got := glob(c.s, c.p); got != c.want {
			t.Errorf("glob(%q, %q) = %v", c.s, c.p, got)
		}
	}
	long := strings.Repeat("a", 2000)
	start := time.Now()
	if glob(long, strings.Repeat("*a", 50)+"b") {
		t.Errorf("a pattern ending in b matched")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("a backtracking pattern took %v", d)
	}
}

// TestFold guards folding: case folded in full and accents ignored, the
// ends trimmed, decomposed text folded as composed text is. A final sigma
// folds as a sigma, as FTS5's own folding takes it, so a search's trigram
// table and routing agree on Greek text; "ß" folds to "ss".
func TestFold(t *testing.T) {
	for in, want := range map[string]string{
		"Müller": "muller", "MÜLLER ": "muller", "Müller": "muller", "  T1 MPRAGE": "t1 mprage",
		"Ångström": "angstrom", "ß": "ss", "Straße": "strasse", "ΟΔΟΣ": "οδοσ", "οδος": "οδοσ",
		"İstanbul": "istanbul", "́ x": "x", "山田^太郎": "山田^太郎",
	} {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}

// FuzzFold guards that folding a folded text changes nothing: a search
// table keeps folded text and a condition's value is folded again when
// compared, so a second fold that changed the text would lose rows.
func FuzzFold(f *testing.F) {
	for _, s := range []string{"Müller", "ΟΔΟΣ", "ß", "İ", "́ x", "ǰ", "ͅ", "Ⅻ", "ﬃ"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := Fold(s)
		if twice := Fold(once); twice != once {
			t.Fatalf("Fold(%q) = %q, and folding it again gives %q", s, once, twice)
		}
	})
}

// TestEvalPersonNameGroups guards that a comparison on a person's name
// matches it by any of its component groups (PS3.4 C.2.2.2.1, PS3.5
// 6.2.1.2), in each of its values, and that Values gives the same groups a
// search keeps.
func TestEvalPersonNameGroups(t *testing.T) {
	ds := dataset.NewDataset()
	_ = ds.Add(dataelem.NewDataElement(tag.New(0x0008, 0x1070), dataelem.PN, []byte(`Doe^John=山田^太郎=やまだ^たろう\Roe^Ann`)))
	runEval(t, "person name", ds, []evalCase{
		{`OperatorsName == "Doe^John"`, True},
		{`OperatorsName == "山田^太郎"`, True},
		{`OperatorsName == "やまだ^たろう"`, True},
		{`OperatorsName = "roe^ann"`, True},
		{`OperatorsName prefix "山田"`, True},
		{`OperatorsName == "Doe^John=山田^太郎=やまだ^たろう"`, False},
		{`OperatorsName contains "=山"`, False},
	})
	got, err := Values(ds, Field{Path: []Step{{Tag: 0x00081070}}}, false)
	if want := []string{"Doe^John", "山田^太郎", "やまだ^たろう", "Roe^Ann"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("Values = %q, %v, want %q", got, err, want)
	}
}

// TestRelativeDate guards the day a relative date names.
func TestRelativeDate(t *testing.T) {
	now := time.Date(2026, 3, 31, 23, 0, 0, 0, time.UTC)
	for v, want := range map[Value]string{
		{Kind: KindRelative, Unit: 'd'}:               "20260331",
		{Kind: KindRelative, Relative: 30, Unit: 'd'}: "20260301",
		{Kind: KindRelative, Relative: 2, Unit: 'w'}:  "20260317",
		{Kind: KindRelative, Relative: 1, Unit: 'm'}:  "20260303",
		{Kind: KindRelative, Relative: 1, Unit: 'y'}:  "20250331",
	} {
		if got := RelativeDate(v, now); got != want {
			t.Errorf("RelativeDate(%v) = %s, want %s", v, got, want)
		}
	}
}

// TestNormalValues guards the reading of DA, TM and DT values, the old
// forms with separators included.
func TestNormalValues(t *testing.T) {
	for in, want := range map[string]string{"20040826": "20040826", "2004.08.26": "20040826", "20040826101500": "20040826", "2004082": ""} {
		got, ok := normalDate(in)
		if got != want || ok != (want != "") {
			t.Errorf("normalDate(%q) = %q, %v", in, got, ok)
		}
	}
	for in, want := range map[string]string{"10": "100000", "1012": "101200", "101233.123": "101233", "10:12:33": "101233", "1": ""} {
		got, ok := normalTime(in)
		if got != want || ok != (want != "") {
			t.Errorf("normalTime(%q) = %q, %v", in, got, ok)
		}
	}
	for in, want := range map[string]string{"2004": "20040101000000", "20040826": "20040826000000", "2004082610+0100": "20040826100000", "20040826101233.5": "20040826101233", "2004.08.26": "20040826000000", "x": ""} {
		got, ok := normalDateTime(in)
		if got != want || ok != (want != "") {
			t.Errorf("normalDateTime(%q) = %q, %v", in, got, ok)
		}
	}
}
