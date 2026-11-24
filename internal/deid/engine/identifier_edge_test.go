package engine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// longitudinalEngine is the engine of nopacs/longitudinal:1, which keeps
// the device's identity (PS3.15 E.3.4) and removes the institution's.
func longitudinalEngine(t *testing.T) *engine.Engine {
	t.Helper()
	exp, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{"nopacs/longitudinal:1"}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	k := func(c byte) []byte { return bytes.Repeat([]byte{c}, 32) }
	en, err := engine.New(exp, engine.Keys{UID: k('u'), Date: k('d'), Hash: k('h')}, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// TestAnInstitutionInsideAMakersNamePasses guards the false hold of an MR
// whose Institution Name "TOSHIBA", which a profile removes, is found
// again inside Manufacturer "TOSHIBA_MEC", which a profile that keeps the
// device's identity keeps (as in the receiver's MR_small.dcm): an
// identifier that begins or ends with a letter is taken only as a whole
// word, "_" part of a word, so the file passes; and the same name
// standing alone in a kept element is still found.
func TestAnInstitutionInsideAMakersNamePasses(t *testing.T) {
	en := longitudinalEngine(t)
	b, err := os.ReadFile(filepath.Join("../../receive/testdata", "MR_small.dcm")) // #nosec G304 -- the receiver's public test file.
	if err != nil {
		t.Fatal(err)
	}
	out, needles := deidentify(t, en, b)
	if !hasValue(needles, engine.NeedleText, "TOSHIBA") || !bytes.Contains(out, []byte("TOSHIBA_MEC")) {
		t.Fatal("the file does not keep the maker's name and remove the institution's, as the test needs")
	}
	if err := checkAfter(out, needles); err != nil {
		t.Errorf("the check after holds the file: %v", err)
	}

	for _, c := range []struct {
		station string
		found   bool
	}{
		{"TOSHIBA", true}, {"TOSHIBA 3", true}, {"(toshiba)", true}, {"TOSHIBA-MR", true},
		{"TOSHIBA_MEC", false}, {"MY_TOSHIBA", false}, {"TOSHIBAS", false}, {"XTOSHIBA", false}, {"TOSHIBA1", true},
	} {
		en := basicEngine(t, &v1.ProfileRule{Name: "station", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StationName"}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}})
		out, needles := deidentify(t, en, part10(t, dataSet(t,
			elem{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, elem{0x00080018, "UI", "2.25.4321"},
			elem{0x00080080, "LO", "TOSHIBA"}, elem{0x00081010, "SH", c.station},
		)))
		if err := checkAfter(out, needles); (err != nil) != c.found {
			t.Errorf("institution TOSHIBA, station %q kept: check after %v, want found %v", c.station, err, c.found)
		}
	}
}

// TestCleanTakesAnIdentifierAsAWord guards that clean removes an
// identifier as the check after looks for it. A value of letters alone,
// written with spaces, not an attribute that identifies the patient (an
// institution "Mercy"), is cut standing alone but not inside another word
// or a name joined by "_". Every other identifier is cut wherever it
// stands: a Patient ID holding digits glued to letters, "_", an accented
// letter or Han ("MRNAB778812", "sub_AB778812", "ÉAB778812",
// "患者AB778812様"), cut again when cutting it joins it anew; a Patient ID
// of letters alone ("QXJVKL"), which a word edge never protects since a
// copy of it glued to other text is how it leaks; and an institution in a
// script written without spaces inside a longer name.
func TestCleanTakesAnIdentifierAsAWord(t *testing.T) {
	en := basicEngine(t, &v1.ProfileRule{Name: "clean", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "ImageComments"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{}}}})
	for _, c := range []struct {
		institution, id, text, want string
	}{
		{"Mercy", "AB778812", "at Mercy; MERCY_SITE; Mercyful; scan AB778812_T1; XAB778812; 9AB778812; AB778AB778812812",
			"at ; MERCY_SITE; Mercyful; scan _T1; X; 9;"},
		{"Mercy", "AB778812", "series sub_AB778812 T1; MRNAB778812; ÉAB778812; 患者AB778812様", "series sub_ T1; MRN; É; 患者様"},
		{"Mercy", "QXJVKL", "MRN_QXJVKL; patQXJVKLx", "MRN_; patx"},
		{"東京中央病院", "AB778812", "東京中央病院放射線科", "放射線科"},
	} {
		ds := dataSet(t,
			elem{0x00080005, "CS", "ISO_IR 192"},
			elem{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, elem{0x00080018, "UI", "2.25.4321"},
			elem{0x00080080, "LO", c.institution}, elem{0x00100020, "LO", c.id},
			elem{0x00204000, "LT", c.text},
		)
		if _, err := en.Apply(context.Background(), ds, engine.Reception{}); err != nil {
			t.Fatal(err)
		}
		e, _ := ds.Get(0x00204000)
		v, _ := e.Bytes()
		if got := strings.TrimRight(string(v), " \x00"); got != c.want {
			t.Errorf("clean of %q left %q, want %q", c.text, got, c.want)
		}
	}
}

// TestAPatientIDGluedToTextIsFound guards the check after on what clean
// does not touch: a kept Station Name holding a Patient ID glued to other
// text, or an institution in a script written without spaces inside a
// longer name, holds the file; a Patient ID of letters alone is found
// glued to "_" too, since a word edge is never given to an attribute that
// identifies the patient.
func TestAPatientIDGluedToTextIsFound(t *testing.T) {
	en := basicEngine(t, &v1.ProfileRule{Name: "station", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StationName"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}})
	for _, c := range []struct{ institution, id, station string }{
		{"Mercy", "AB778812", "sub_AB778812 T1"},
		{"Mercy", "AB778812", "MRNAB778812"},
		{"Mercy", "AB778812", "ÉAB778812"},
		{"Mercy", "AB778812", "患者AB778812様"},
		{"Mercy", "QXJVKL", "MRN_QXJVKL"},
		{"東京中央病院", "AB778812", "東京中央病院放射線科"},
	} {
		out, needles := deidentify(t, en, part10(t, dataSet(t,
			elem{0x00080005, "CS", "ISO_IR 192"},
			elem{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, elem{0x00080018, "UI", "2.25.4321"},
			elem{0x00080080, "LO", c.institution}, elem{0x00100020, "LO", c.id}, elem{0x00081010, "SH", c.station},
		)))
		if err := checkAfter(out, needles); err == nil {
			t.Errorf("institution %q, Patient ID %q, station %q kept: the check after found nothing", c.institution, c.id, c.station)
		}
	}
}
