package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestTheMethodRecordsTheAnnexECodes guards the record of the method in a
// file made under PS3.15 Annex E bases: De-identification Method Code
// Sequence (0012,0064) holds the CID 7050 code of each base in the order
// of the profile and its options, with the DCM scheme; De-identification
// Method (0012,0063) holds their meanings, then the caller's method and
// the software's versions; Patient Identity Removed (0012,0062) is YES and
// Longitudinal Temporal Information Modified (0028,0303) MODIFIED, since
// the dates option shifted dates; and all of it reads back from the
// written file. A profile on no Annex E base writes no code sequence.
func TestTheMethodRecordsTheAnnexECodes(t *testing.T) {
	e, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{
		profile.AnnexE("basic"), profile.AnnexE("clean-descriptors"), profile.AnnexE("retain-longitudinal-modified-dates"),
	}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	en, err := New(e, testKeys, Options{Method: "research@3"})
	if err != nil {
		t.Fatal(err)
	}
	ds := sample(t)
	apply(t, en, ds)
	back := readFile(t, writeFile(t, ds))
	var codes []string
	for i := 0; ; i++ {
		it, err := back.GetSequenceDataset(tagOf(tagDeidMethodCodes), i)
		if err != nil {
			break
		}
		v, _ := text(it, 0x00080100)
		s, _ := text(it, 0x00080102)
		codes = append(codes, v+"/"+s)
	}
	if got := strings.Join(codes, " "); got != "113100/DCM 113105/DCM 113107/DCM" {
		t.Errorf("the codes: %q", got)
	}
	want := map[uint32]string{
		tagDeidMethod: `Basic Application Confidentiality Profile\Clean Descriptors Option\` +
			`Retain Longitudinal Temporal Information Modified Dates Option\research@3\NoPACs unknown\go-dicom unknown`,
		tagIdentityRemoved:  "YES",
		tagTemporalModified: "MODIFIED",
	}
	for tg, w := range want {
		if got, _ := text(back, tg); got != w {
			t.Errorf("%08X = %q, want %q", tg, got, w)
		}
	}
	if got, _ := text(back, tPatientName); got == "Doe^John" {
		t.Error("the basic profile kept the patient's name")
	}

	own := newTestEngine(t, testKeys, Options{}, rule("name", kw("PatientName"), aRemove))
	plain := sample(t)
	apply(t, own, plain)
	if plain.Contains(tagOf(tagDeidMethodCodes)) || plain.Contains(tagOf(tagDeidMethod)) {
		t.Error("a profile on no Annex E base recorded a method")
	}
	if got, _ := text(plain, tagIdentityRemoved); got != "YES" {
		t.Errorf("Patient Identity Removed: %q", got)
	}
}

// safePrivateEngine returns an engine on the basic profile and Retain
// Safe Private, with the rules given.
func safePrivateEngine(t *testing.T, rules ...*v1.ProfileRule) *Engine {
	t.Helper()
	e, problems := profile.Expand(&v1.ProfileDocument{
		Bases: []string{profile.AnnexE("basic"), profile.AnnexE("retain-safe-private")}, Rules: rules,
	}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	en, err := New(e, testKeys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// implicitFile returns ds written in Implicit VR Little Endian and read
// back, as a file of that syntax arrives: every private element without a
// VR, which the reader gives as UN with its bytes as written.
func implicitFile(t *testing.T, els ...el) *dataset.Dataset {
	t.Helper()
	base := []el{{tSOPClass, "UI", ctImageStorage}, {tSOPInstance, "UI", "1.2.3.4"}, {tPatientID, "LO", "PAT001"}}
	return readFile(t, writeFileAs(t, build(t, append(base, els...)...), implicitVR))
}

// TestSafePrivateValuesTakeTheTablesVR guards how a private element held
// as UN that PS3.15 Table E.3.10-1 lists is judged under the Retain Safe
// Private base, from a file written in Implicit VR Little Endian. The
// table says which attributes are safe, not what a file put in them: a
// value that reads as a person's name ("Roe^Richard" in GEMS_ACQU_01
// (0019,xx9E), LO in the table; "SAG^FIESTA" too, whose form is a name's)
// is removed by the floor and kept as a needle, whatever the table's VR;
// so is one written into GEMS_HELIOS_01 (0045,xx02), FL in the table, that
// is not a whole number of FL values. A value that is what the table says
// is kept with its VR: "FIESTA 2D" as LO, four bytes as FL. A value not in
// the table that reads as a name is removed as before.
func TestSafePrivateValuesTakeTheTablesVR(t *testing.T) {
	en := safePrivateEngine(t, rule("other", privSel("OTHER", "0021", "xx"), aKeep))
	ds := implicitFile(t,
		el{0x00190010, "LO", "GEMS_ACQU_01"}, el{0x0019109E, "UN", []byte("Roe^Richard ")},
		el{0x00210010, "LO", "OTHER"}, el{0x00211001, "UN", []byte("Doe^John")},
		el{0x00450010, "LO", "GEMS_HELIOS_01"}, el{0x00451002, "UN", []byte("Roe^Richard ")},
	)
	rec := apply(t, en, ds)
	for _, gone := range []uint32{0x0019109E, 0x00211001, 0x00451002} {
		if got, _ := text(ds, gone); ds.Contains(tagOf(gone)) && strings.Contains(got, "^") {
			t.Errorf("%08X kept a name: %q", gone, got)
		}
	}
	needles := map[string]bool{}
	for _, n := range rec.Needles {
		needles[n.Value] = true
	}
	if !needles["Roe^Richard"] || !needles["Doe^John"] {
		t.Errorf("the names are not needles: %v", len(rec.Needles))
	}
	sag := implicitFile(t, el{0x00190010, "LO", "GEMS_ACQU_01"}, el{0x0019109E, "UN", []byte("SAG^FIESTA")})
	apply(t, en, sag)
	if got, _ := text(sag, 0x0019109E); got == "SAG^FIESTA" {
		t.Error("a listed value that reads as a name was kept")
	}
	ok := implicitFile(t, el{0x00190010, "LO", "GEMS_ACQU_01"}, el{0x0019109E, "UN", []byte("FIESTA 2D ")},
		el{0x00450010, "LO", "GEMS_HELIOS_01"}, el{0x00451002, "UN", []byte{0, 0, 0x80, 0x3f}})
	apply(t, en, ok)
	if e, found := ok.Get(tagOf(0x0019109E)); !found || e.GetVR() != "LO" || mustText(t, ok, 0x0019109E) != "FIESTA 2D" {
		t.Errorf("a listed text value: present %v, %v", found, e)
	}
	if e, found := ok.Get(tagOf(0x00451002)); !found || e.GetVR() != "FL" {
		t.Errorf("a listed FL value: present %v, %v", found, e)
	}
}

// TestListedTextInAnotherCharacterSet guards a file whose text is not
// UTF-8 (ISO_IR 100) holding a private text element Table E.3.10-1 lists,
// held as UN (its bytes as the file wrote them): under a profile that
// removes it, or under the basic profile alone, it is removed and the file
// is not refused; under Retain Safe Private, told the file's character
// set, it is kept decoded into UTF-8 with the table's VR; not told, it is
// kept as the bytes it was, with no VR (UN), and the file is not refused either.
func TestListedTextInAnotherCharacterSet(t *testing.T) {
	file := func() *dataset.Dataset {
		return implicitFile(t, el{tagSpecificCharacterSet, "CS", "ISO_IR 100"},
			el{0x00190010, "LO", "GEMS_ACQU_01"}, el{0x0019109E, "UN", []byte("Sch\xe4del ")})
	}
	for name, en := range map[string]*Engine{
		"no bases": newTestEngine(t, testKeys, Options{}),
		"basic":    newBasesEngine(t, profile.AnnexE("basic")),
	} {
		ds := file()
		if _, err := en.Apply(context.Background(), ds, Reception{}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if ds.Contains(tagOf(0x0019109E)) {
			t.Errorf("%s: the private value stayed", name)
		}
	}
	latin1 := "ISO_IR 100"
	ds := file()
	if _, err := safePrivateEngine(t).Apply(context.Background(), ds, Reception{CharacterSet: &latin1}); err != nil {
		t.Fatal(err)
	}
	if e, found := ds.Get(tagOf(0x0019109E)); !found || e.GetVR() != "LO" || mustText(t, ds, 0x0019109E) != "Schädel" {
		t.Errorf("told the character set: present %v, %v", found, e)
	}
	ds = file()
	if _, err := safePrivateEngine(t).Apply(context.Background(), ds, Reception{}); err != nil {
		t.Fatal(err)
	}
	if e, found := ds.Get(tagOf(0x0019109E)); !found || (e.GetVR() != "UN" && e.GetVR() != "") {
		t.Errorf("not told the character set: present %v, %v", found, e)
	}
}

// newBasesEngine returns an engine on the bases named.
func newBasesEngine(t *testing.T, bases ...string) *Engine {
	t.Helper()
	e, problems := profile.Expand(&v1.ProfileDocument{Bases: bases}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	en, err := New(e, testKeys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// TestTheMethodRecordFollowsTheProfile guards what the method record may
// claim. A version whose own rule keeps what its basic profile removes
// records no code, since the codes would claim a profile the file was not
// made by, and still names itself in (0012,0063). Longitudinal Temporal
// Information Modified (0028,0303) is what PS3.15 E.3.6 says of each dates
// option: UNMODIFIED under full dates, MODIFIED under modified dates even
// for a file that holds no date. A version naming an Annex E base of
// another edition than this build's is refused.
func TestTheMethodRecordFollowsTheProfile(t *testing.T) {
	plain := func() *dataset.Dataset {
		return build(t, el{tSOPClass, "UI", ctImageStorage}, el{tSOPInstance, "UI", "1.2.3.4"}, el{tPatientID, "LO", "PAT001"},
			el{tStudyDesc, "LO", "Head CT"})
	}
	engine := func(rules []*v1.ProfileRule, bases ...string) *Engine {
		t.Helper()
		e, problems := profile.Expand(&v1.ProfileDocument{Bases: bases, Rules: rules}, profile.Shipped())
		if problems != nil {
			t.Fatal(problems)
		}
		en, err := New(e, testKeys, Options{Method: "own@1"})
		if err != nil {
			t.Fatal(err)
		}
		return en
	}
	ds := plain()
	apply(t, engine([]*v1.ProfileRule{rule("keep-description", kw("StudyDescription"), aKeep)}, profile.AnnexE("basic")), ds)
	if ds.Contains(tagOf(tagDeidMethodCodes)) {
		t.Error("a version that keeps what its base removes recorded the base's code")
	}
	if got, _ := text(ds, tagDeidMethod); got != `own@1\NoPACs unknown\go-dicom unknown` {
		t.Errorf("(0012,0063) = %q", got)
	}
	for base, want := range map[string]string{
		profile.AnnexE("retain-longitudinal-full-dates"):     "UNMODIFIED",
		profile.AnnexE("retain-longitudinal-modified-dates"): "MODIFIED",
		profile.AnnexE("clean-descriptors"):                  "",
	} {
		ds := plain()
		apply(t, engine(nil, profile.AnnexE("basic"), base), ds)
		if got, _ := text(ds, tagTemporalModified); got != want {
			t.Errorf("%s: (0028,0303) = %q, want %q", base, got, want)
		}
	}
	e, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	e.Bases = []string{"annex-e/basic:2026c"}
	if _, err := New(e, testKeys, Options{}); err == nil || !strings.Contains(err.Error(), "2026c") {
		t.Errorf("a version of another edition: %v", err)
	}
}
