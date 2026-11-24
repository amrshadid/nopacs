package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	coreuid "github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// Tags of the test data sets.
const (
	tSOPClass        = 0x00080016
	tSOPInstance     = 0x00080018
	tStudyDate       = 0x00080020
	tStudyTime       = 0x00080030
	tAcqDateTime     = 0x0008002A
	tAccession       = 0x00080050
	tModality        = 0x00080060
	tManufacturer    = 0x00080070
	tInstitution     = 0x00080080
	tReferring       = 0x00080090
	tStudyDesc       = 0x00081030
	tRefStudySeq     = 0x00081110
	tRefSOPClass     = 0x00081150
	tRefSOPInstance  = 0x00081155
	tPatientName     = 0x00100010
	tPatientID       = 0x00100020
	tBirthDate       = 0x00100030
	tPatientSex      = 0x00100040
	tPatientAge      = 0x00101010
	tPatientComments = 0x00104000
	tStudyUID        = 0x0020000D
	ctImageStorage   = "1.2.840.10008.5.1.4.1.1.2"
)

// mapLookups is Lookups over a map, assigning codes in order.
type mapLookups struct {
	codes map[string]string
	next  int
}

func (m *mapLookups) Code(_ context.Context, table, key string) (string, bool, error) {
	c, ok := m.codes[table+"/"+key]
	return c, ok, nil
}

func (m *mapLookups) Assign(_ context.Context, keys []LookupKey) ([]string, error) {
	out := make([]string, len(keys))
	for i, k := range keys {
		c, ok := m.codes[k.Table+"/"+k.Key]
		if !ok {
			m.next++
			c = fmt.Sprintf("A%04d", m.next)
			m.codes[k.Table+"/"+k.Key] = c
		}
		out[i] = c
	}
	return out, nil
}

// sample is a data set with an identifying value of each kind.
func sample(t testing.TB) *dataset.Dataset {
	return build(t,
		el{0x00090010, "LO", "ACME 1.0"},
		el{0x00091001, "LO", "Doe private"},
		el{0x00110010, "LO", "KEEPME"},
		el{0x00111002, "LO", "kept value"},
		el{tSOPClass, "UI", ctImageStorage},
		el{tSOPInstance, "UI", "1.2.3.4.5.6.7"},
		el{tStudyDate, "DA", "20240315"},
		el{tStudyTime, "TM", "101500"},
		el{tAcqDateTime, "DT", "20240315101500.123456+0100"},
		el{tAccession, "SH", "ACC12345"},
		el{tModality, "CS", "CT"},
		el{tManufacturer, "LO", "Acme"},
		el{tInstitution, "LO", "General Hospital"},
		el{tReferring, "PN", "Who^Doctor"},
		el{tStudyDesc, "LO", "Head CT for John Doe"},
		el{tRefStudySeq, "SQ", []*dataset.Dataset{build(t,
			el{tRefSOPClass, "UI", ctImageStorage},
			el{tRefSOPInstance, "UI", "1.2.3.4.9"},
		)}},
		el{tPatientName, "PN", "Doe^John"},
		el{tPatientID, "LO", "PAT001"},
		el{tBirthDate, "DA", "19500101"},
		el{tPatientAge, "AS", "095Y"},
		el{tPatientComments, "LT", "comments"},
		el{tStudyUID, "UI", "1.2.3.4.5"},
	)
}

// sampleRules is a profile with every action the language has but
// quarantine.
func sampleRules() []*v1.ProfileRule {
	return []*v1.ProfileRule{
		rule("name", kw("PatientName"), aTemplate("SUBJ-{#PatientID}")),
		rule("id", kw("PatientID"), aHash),
		rule("accession", kw("AccessionNumber"), aLookup("acc", true)),
		rule("institution", kw("InstitutionName"), aRemove),
		rule("referring", kw("ReferringPhysicianName"), aEmpty),
		rule("description", kw("StudyDescription"), aClean),
		rule("uids", vrSel("UI"), aUID),
		rule("dates", vrSel("DA"), aShift),
		rule("datetimes", vrSel("DT"), aShift),
		rule("age", kw("PatientAge"), aGeneralize),
		rule("comments", kw("PatientComments"), aReplace("none")),
		rule("keep-acme", privSel("KEEPME", "0011", "xx"), aKeep),
		rule("sex", kw("PatientSex"), aSet("O")),
	}
}

// TestEachActionChangesItsElement guards what every action does to the
// element it decides: the value written, the needle kept (and none of a
// comment replaced), the element
// made, the private block kept with its creator and the one removed with
// its creator, and the record of the method: the caller's text, then this
// build's version and the DICOM library's, "unknown" in a test binary,
// which records no dependencies (TestTheMethodNamesTheLibrarysVersion).
func TestEachActionChangesItsElement(t *testing.T) {
	lk := &mapLookups{codes: map[string]string{}}
	en := newTestEngine(t, testKeys, Options{Lookups: lk, Method: "research@1"}, sampleRules()...)
	ds := sample(t)
	rec := apply(t, en, ds)

	hashID := naming.HashText(testKeys.Hash, []byte("PAT001"))
	days := ShiftDays(testKeys.Date, "PAT001", 365)
	shifted, _ := shiftDate("20240315", days)
	birth, _ := shiftDate("19500101", days)
	want := map[uint32]string{
		tPatientName:     "SUBJ-" + hashID,
		tPatientID:       hashID,
		tAccession:       "A0001",
		tReferring:       "",
		tStudyDesc:       "Head CT for",
		tSOPClass:        ctImageStorage,
		tSOPInstance:     RemapUID(testKeys.UID, "1.2.3.4.5.6.7"),
		tStudyUID:        RemapUID(testKeys.UID, "1.2.3.4.5"),
		tStudyDate:       shifted,
		tBirthDate:       birth,
		tStudyTime:       "101500",
		tAcqDateTime:     shifted + "101500.123456+0100",
		tPatientAge:      "090Y",
		tPatientComments: "none",
		tPatientSex:      "O",
		tModality:        "CT",
		0x00111002:       "kept value",
		0x00110010:       "KEEPME",
		0x00120062:       "YES",
		0x00120063:       `research@1\NoPACs unknown\go-dicom unknown`,
		0x00280303:       "MODIFIED",
	}
	for tg, w := range want {
		got, ok := text(ds, tg)
		if !ok || got != w {
			t.Errorf("%08X = %q (present %v), want %q", tg, got, ok, w)
		}
	}
	for _, gone := range []uint32{tInstitution, 0x00090010, 0x00091001} {
		if ds.Contains(tagOf(gone)) {
			t.Errorf("%08X is still present", gone)
		}
	}
	ref := item(t, ds, tRefStudySeq, 0)
	if got, _ := text(ref, tRefSOPInstance); got != RemapUID(testKeys.UID, "1.2.3.4.9") {
		t.Errorf("a UID in an item: %q", got)
	}
	if got, _ := text(ref, tRefSOPClass); got != ctImageStorage {
		t.Errorf("a registered UID in an item was changed: %q", got)
	}
	if !rec.DatesShifted {
		t.Errorf("record: dates shifted %v", rec.DatesShifted)
	}
	if days < 1 || days > 365 {
		t.Errorf("the shift is %d days", days)
	}
	values := map[string]bool{}
	for _, n := range rec.Needles {
		values[n.Value] = true
	}
	for _, v := range []string{"Doe^John", "PAT001", "ACC12345", "General Hospital", "Who^Doctor", "1.2.3.4.5", "1.2.3.4.9", "20240315", "19500101"} {
		if !values[v] {
			t.Errorf("the needles lack %q", v)
		}
	}
	// Patient Comments is free text, replaced only because it might hold
	// an identifier: its value identifies no one by itself.
	if values["comments"] {
		t.Error("a replaced comment is a needle")
	}
	if values[ctImageStorage] {
		t.Error("a registered UID is a needle")
	}
	if values["Doe private"] {
		t.Error("a value a default removed is a needle")
	}
	if s := fmt.Sprintf("%v %+v %#v %s", rec.Needles, rec.Needles, rec.Needles, rec.Needles); strings.Contains(s, "PAT001") {
		t.Errorf("printing the needles shows a value: %s", s)
	}
	// Every result is a file the library reads back the same.
	back := readFile(t, writeFile(t, ds))
	for tg, w := range want {
		if got, _ := text(back, tg); got != w {
			t.Errorf("read back, %08X = %q, want %q", tg, got, w)
		}
	}
	for _, s := range allText(back) {
		for _, v := range []string{"Doe", "PAT001", "ACC12345", "Hospital", "1.2.3.4.5"} {
			if strings.Contains(s, v) {
				t.Errorf("the file read back holds %q in %q", v, s)
			}
		}
	}
}

// TestEveryUIDRemappedIsValidAndKeyed guards RemapUID: deterministic, a
// valid UID of at most 44 characters under 2.25, different under another
// tenant's key and for another UID, and registered UIDs never remapped.
func TestEveryUIDRemappedIsValidAndKeyed(t *testing.T) {
	seen := map[string]bool{}
	for i := range 2000 {
		orig := fmt.Sprintf("1.2.826.0.1.3680043.%d.%d", i, i*7)
		a, b := RemapUID(testKeys.UID, orig), RemapUID(testKeys.UID, orig)
		if a != b {
			t.Fatalf("not deterministic: %s, %s", a, b)
		}
		if err := coreuid.Check(a); err != nil || len(a) > 44 || !strings.HasPrefix(a, "2.25.") {
			t.Fatalf("%s: %v, length %d", a, err, len(a))
		}
		if a == RemapUID(otherKeys.UID, orig) {
			t.Fatalf("two tenants' keys give one UID for %s", orig)
		}
		if seen[a] {
			t.Fatalf("two UIDs map to %s", a)
		}
		seen[a] = true
	}
	for _, u := range []string{ctImageStorage, "1.2.840.10008.1.2.1", "1.2.840.10008.1.1",
		// Secondary Capture, RT Dose, RT Plan and Segmentation: standard
		// classes the library's registry does not list.
		"1.2.840.10008.5.1.4.1.1.7", "1.2.840.10008.5.1.4.1.1.481.2",
		"1.2.840.10008.5.1.4.1.1.481.5", "1.2.840.10008.5.1.4.1.1.66.4"} {
		if !Registered(u) {
			t.Errorf("%s is not taken as registered", u)
		}
		if out, replaced := remapUIDValue(testKeys.UID, u); out != u || replaced != nil {
			t.Errorf("registered %s became %s", u, out)
		}
	}
	out, replaced := remapUIDValue(testKeys.UID, `1.2.3\`+ctImageStorage+`\\1.2.4`)
	if want := RemapUID(testKeys.UID, "1.2.3") + `\` + ctImageStorage + `\\` + RemapUID(testKeys.UID, "1.2.4"); out != want || len(replaced) != 2 {
		t.Errorf("a multi-valued UID: %q, %v", out, replaced)
	}
}

// TestDatesShiftConsistentlyPerSubject guards the date shift: one subject
// gets one shift in every file, within the profile's bound and never zero;
// DT keeps its time, fraction and offset; partial dates keep their
// precision and are shifted from the middle of their period, so a year is
// not always the year before, and one the shift leaves as it was is
// emptied rather than kept as received; an impossible date fails; and a
// file without a Patient ID fails as no_subject.
func TestDatesShiftConsistentlyPerSubject(t *testing.T) {
	for _, most := range []int32{1, 7, 365, 3650} {
		for i := range 500 {
			d := ShiftDays(testKeys.Date, fmt.Sprint("P", i), most)
			if d < 1 || d > int(most) {
				t.Fatalf("shift %d outside 1..%d", d, most)
			}
		}
	}
	if ShiftDays(testKeys.Date, "P1", 3650) == ShiftDays(otherKeys.Date, "P1", 3650) &&
		ShiftDays(testKeys.Date, "P2", 3650) == ShiftDays(otherKeys.Date, "P2", 3650) {
		t.Error("two tenants' keys give one shift")
	}
	cases := []struct {
		in   string
		days int
		want string
	}{
		{"20240301", 1, "20240229"},
		{"20240101", 1, "20231231"},
		{"2024", 1, ""},
		{"2024", 184, "2023"},
		{"2024", 183, ""},
		{"202403", 1, ""},
		{"202403", 15, "202402"},
		{"20240301101500.5-0500", 1, "20240229101500.5-0500"},
		{`20240301\\20240302`, 1, `20240229\\20240301`},
	}
	for _, c := range cases {
		f := shiftDT
		if !strings.ContainsAny(c.in, ".-") && len(strings.Split(c.in, `\`)[0]) == 8 {
			f = shiftDA
		}
		if got, err := f(c.in, c.days); err != nil || got != c.want {
			t.Errorf("shift %q by %d = %q, %v; want %q", c.in, c.days, got, err, c.want)
		}
	}
	// A year is the year before for about half the shifts, and emptied for
	// the others, never kept as received: its original cannot be told from
	// the result alone.
	years := map[string]int{}
	for days := 1; days <= 365; days++ {
		out, _ := shiftDT("2024", days)
		years[out]++
	}
	if len(years) != 2 || years[""] < 150 || years["2023"] < 150 {
		t.Errorf("a year shifted by 1 to 365 days: %v", years)
	}
	for _, bad := range []string{"20240230", "2024-03-01", "0001010", "00010101", "2024030", "20240301101500.1234567"} {
		if _, err := shiftDT(bad, 1); err == nil && len(bad) != 8 {
			t.Errorf("shifting %q did not fail", bad)
		}
		if len(bad) == 8 {
			if _, err := shiftDA(bad, 1); err == nil {
				t.Errorf("shifting %q did not fail", bad)
			}
		}
	}

	en := newTestEngine(t, testKeys, Options{}, rule("dates", vrSel("DA"), aShift))
	a := build(t, el{tPatientID, "LO", "PAT9"}, el{tStudyDate, "DA", "20200610"})
	b := build(t, el{tPatientID, "LO", "PAT9 "}, el{tStudyDate, "DA", "20200610"})
	apply(t, en, a)
	apply(t, en, b)
	if x, y := mustText(t, a, tStudyDate), mustText(t, b, tStudyDate); x != y || x == "20200610" {
		t.Errorf("one subject's dates: %s and %s", x, y)
	}
	noID := build(t, el{tStudyDate, "DA", "20200610"})
	_, err := en.Apply(context.Background(), noID, Reception{})
	var f *Failure
	if !errors.As(err, &f) || f.Cause != CauseNoSubject || f.Reason() != ReasonFailed || f.Detail() != "no_subject" {
		t.Fatalf("no Patient ID: %v", err)
	}
	if got := mustText(t, noID, tStudyDate); got != "20200610" {
		t.Errorf("a failed file was changed: %s", got)
	}
}

// TestAgesAndDatesGeneralize guards generalize: ages over 89 years become
// 090Y, younger ones and those in days, weeks and months stay, a date
// becomes the first of January of its year, and a malformed age fails.
func TestAgesAndDatesGeneralize(t *testing.T) {
	for in, want := range map[string]string{"090Y": "090Y", "089Y": "089Y", "120Y": "090Y", "999M": "999M", "030D": "030D", "": ""} {
		if got, err := generalizeAge(in); err != nil || got != want {
			t.Errorf("age %q = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"90Y", "090X", "-90Y"} {
		if _, err := generalizeAge(bad); err == nil {
			t.Errorf("age %q did not fail", bad)
		}
	}
	if got, _ := generalizeDate(`20240315\19991231`); got != `20240101\19990101` {
		t.Errorf("dates: %q", got)
	}
}

// TestHashEqualsTheLabelCode guards that a hashed identifier is the code
// the naming rule's {#...} gives the same value, padding removed, so a
// file's Patient ID equals its subject's label; and that a CS value's
// code is in upper case, as CS requires.
func TestHashEqualsTheLabelCode(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("id", kw("PatientID"), aHash), rule("mod", kw("Modality"), aHash))
	ds := build(t, el{tPatientID, "LO", " PAT001 "}, el{tModality, "CS", "MR"})
	apply(t, en, ds)
	if got := mustText(t, ds, tPatientID); got != naming.HashText(testKeys.Hash, []byte("PAT001")) {
		t.Errorf("hash %q", got)
	}
	if got := mustText(t, ds, tModality); got != strings.ToUpper(naming.HashText(testKeys.Hash, []byte("MR"))) || !validText("CS", got) {
		t.Errorf("CS hash %q", got)
	}
}

// TestTemplatesAndSets guards templates (literal braces, the hash of a
// value as received even when its element is changed too, failure when
// the placeholder's element is absent) and set (made at the top level
// when absent, replaced where present, not made where another rule wins).
func TestTemplatesAndSets(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("id", kw("PatientID"), aHash),
		rule("name", kw("PatientName"), aTemplate("{{S}}-{#PatientID}")),
		rule("sex", kw("PatientSex"), aSet("O")),
	)
	ds := build(t, el{tPatientID, "LO", "PAT001"}, el{tPatientName, "PN", "Doe^John"})
	apply(t, en, ds)
	if got, want := mustText(t, ds, tPatientName), "{S}-"+naming.HashText(testKeys.Hash, []byte("PAT001")); got != want {
		t.Errorf("template %q, want %q", got, want)
	}
	if got := mustText(t, ds, tPatientSex); got != "O" {
		t.Errorf("set made %q", got)
	}
	present := build(t, el{tPatientID, "LO", "P"}, el{tPatientSex, "CS", "M"})
	apply(t, en, present)
	if got := mustText(t, present, tPatientSex); got != "O" {
		t.Errorf("set replaced with %q", got)
	}
	noID := build(t, el{tPatientName, "PN", "Doe^John"})
	_, err := en.Apply(context.Background(), noID, Reception{})
	var f *Failure
	if !errors.As(err, &f) || f.Cause != CauseBadValue || f.Rule != "name" {
		t.Errorf("a template over an absent element: %v", err)
	}
	if got := mustText(t, noID, tPatientName); got != "Doe^John" {
		t.Errorf("a failed file was changed: %q", got)
	}
}

// TestQuarantineRuleRefusesTheFile guards the quarantine action: the file
// fails with the rule's name as its detail, only when the element is
// present, and nothing is changed.
func TestQuarantineRuleRefusesTheFile(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("no-sr", kw("ContentSequence"), aQuarantine), rule("id", kw("PatientID"), aHash))
	ds := build(t, el{tPatientID, "LO", "PAT001"}, el{0x0040A730, "SQ", []*dataset.Dataset{build(t)}})
	_, err := en.Apply(context.Background(), ds, Reception{})
	var f *Failure
	if !errors.As(err, &f) || f.Reason() != ReasonRefused || f.Detail() != "no-sr" {
		t.Fatalf("quarantine: %v", err)
	}
	if got := mustText(t, ds, tPatientID); got != "PAT001" {
		t.Errorf("a refused file was changed: %q", got)
	}
	apply(t, en, build(t, el{tPatientID, "LO", "PAT001"}))
}

// TestLookupsGiveCodesOrFail guards lookup: the table is asked by the
// value's keyed hash, never the value; a missing code is assigned or
// fails as lookup_missing by the rule's policy; a code that is not a value
// of the VR fails; and no tables at all is an error that is not a file's
// failure.
func TestLookupsGiveCodesOrFail(t *testing.T) {
	lk := &mapLookups{codes: map[string]string{}}
	assign := newTestEngine(t, testKeys, Options{Lookups: lk}, rule("id", kw("PatientID"), aLookup("subjects", true)))
	strict := newTestEngine(t, testKeys, Options{Lookups: lk}, rule("id", kw("PatientID"), aLookup("subjects", false)))
	a := build(t, el{tPatientID, "LO", "PAT001"})
	apply(t, assign, a)
	if got := mustText(t, a, tPatientID); got != "A0001" {
		t.Errorf("assigned %q", got)
	}
	for k := range lk.codes {
		if strings.Contains(k, "PAT001") {
			t.Errorf("a table key holds the value: %s", k)
		}
	}
	b := build(t, el{tPatientID, "LO", "PAT001"})
	apply(t, strict, b)
	if got := mustText(t, b, tPatientID); got != "A0001" {
		t.Errorf("looked up %q", got)
	}
	var f *Failure
	if _, err := strict.Apply(context.Background(), build(t, el{tPatientID, "LO", "PAT002"}), Reception{}); !errors.As(err, &f) || f.Cause != CauseLookupMissing {
		t.Errorf("a missing code: %v", err)
	}
	lk.codes["subjects/"+naming.HashText(testKeys.Hash, []byte("PAT003"))] = strings.Repeat("x", 65)
	if _, err := strict.Apply(context.Background(), build(t, el{tPatientID, "LO", "PAT003"}), Reception{}); !errors.As(err, &f) || f.Cause != CauseBadValue {
		t.Errorf("a code too long for LO: %v", err)
	}
	none := newTestEngine(t, testKeys, Options{}, rule("id", kw("PatientID"), aLookup("subjects", true)))
	if _, err := none.Apply(context.Background(), build(t, el{tPatientID, "LO", "PAT001"}), Reception{}); err == nil || errors.As(err, &f) {
		t.Errorf("no lookup tables: %v", err)
	}
}

// TestCleanRemovesTheFilesValues guards clean: every value the profile
// removes from the file, and each part of a removed name, is cut from free
// text in any case, wherever the free text is in the walk; a short part of
// a name is cut only as a whole word ("Al", not the "al" of "Alpha").
func TestCleanRemovesTheFilesValues(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("desc", kw("StudyDescription"), aClean),
		rule("name", kw("PatientName"), aRemove),
		rule("id", kw("PatientID"), aRemove),
	)
	ds := build(t,
		el{tStudyDesc, "LO", "MRI of JOHN doe, id pat001, by Al Alpha"},
		el{tPatientName, "PN", "Doe^John^Al"},
		el{tPatientID, "LO", "PAT001"},
	)
	apply(t, en, ds)
	if got := mustText(t, ds, tStudyDesc); got != "MRI of , id , by  Alpha" {
		t.Errorf("cleaned %q", got)
	}
}

// TestPixelRulesAreTaken guards that a profile with pixel rules makes an
// engine, and that a short key is still refused.
func TestPixelRulesAreTaken(t *testing.T) {
	e, problems := profile.Expand(&v1.ProfileDocument{PixelRules: []*v1.PixelRule{{
		Name: "accept", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "reviewed later",
	}}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	if _, err := New(e, testKeys, Options{}); err != nil {
		t.Errorf("pixel rules: %v", err)
	}
	if _, err := New(e, Keys{UID: []byte("short"), Date: key(1), Hash: key(2)}, Options{}); err == nil {
		t.Error("a short key was taken")
	}
}

// mustText returns an element's text, failing the test when absent.
func mustText(t testing.TB, ds *dataset.Dataset, tg uint32) string {
	t.Helper()
	v, ok := text(ds, tg)
	if !ok {
		t.Fatalf("%08X is absent", tg)
	}
	return v
}

// tagOf is a tag of the DICOM library.
func tagOf(t uint32) tag.Tag { return tag.Tag(t) }

// TestTheMethodNamesTheLibrarysVersion guards how the DICOM library's
// version is read from build information for De-identification Method
// (0012,0063): its module's version, the version that replaces it, and
// "unknown" without one; and that a value too long for an LO, or not of
// the default repertoire, is written as unknown.
func TestTheMethodNamesTheLibrarysVersion(t *testing.T) {
	lib := func(deps ...*debug.Module) *debug.BuildInfo { return &debug.BuildInfo{Deps: deps} }
	for want, bi := range map[string]*debug.BuildInfo{
		"v1.11.0": lib(&debug.Module{Path: "example.org/other", Version: "v9"}, &debug.Module{Path: dicomLibrary, Version: "v1.11.0"}),
		"v1.12.0": lib(&debug.Module{Path: dicomLibrary, Version: "v1.11.0", Replace: &debug.Module{Path: "fork", Version: "v1.12.0"}}),
		"unknown": lib(&debug.Module{Path: "example.org/other", Version: "v9"}),
		"(devel)": lib(&debug.Module{Path: dicomLibrary, Version: "(devel)"}),
	} {
		if got := libraryVersion(bi); got != want {
			t.Errorf("libraryVersion = %q, want %q", got, want)
		}
	}
	if got := libraryVersion(nil); got != "unknown" {
		t.Errorf("no build information: %q", got)
	}
	for in, want := range map[string]string{
		"v1.11.0":               "go-dicom v1.11.0",
		strings.Repeat("9", 60): "go-dicom unknown",
		`v1\v2`:                 "go-dicom unknown",
		"v1.0-é":                "go-dicom unknown",
	} {
		if got := methodValue("go-dicom", in); got != want {
			t.Errorf("methodValue(%q) = %q, want %q", in, got, want)
		}
	}
	if v := buildVersions(); len(v) != 2 || !strings.HasPrefix(v[0], "NoPACs ") || !strings.HasPrefix(v[1], "go-dicom ") {
		t.Errorf("the versions written: %q", v)
	}
}
