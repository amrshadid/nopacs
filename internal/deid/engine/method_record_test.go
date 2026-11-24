package engine

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestMethodIsWhatAFileRecords guards Engine.Method, which a description
// of a profile answers: its codes, its De-identification Method values
// and its Longitudinal Temporal Information Modified are exactly what a
// file the engine makes records, with no pixel work; a profile on no
// Annex E base has neither codes nor meanings, only the method and the
// versions.
func TestMethodIsWhatAFileRecords(t *testing.T) {
	e, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{
		profile.AnnexE("basic"), profile.AnnexE("retain-longitudinal-full-dates"),
	}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	en, err := New(e, testKeys, Options{Method: "research@3"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := en.Method()
	if err != nil {
		t.Fatal(err)
	}
	ds := sample(t)
	apply(t, en, ds)
	back := readFile(t, writeFile(t, ds))
	if got, _ := text(back, tagDeidMethod); got != strings.Join(m.Values, `\`) {
		t.Errorf("the file records %q; Method says %q", got, m.Values)
	}
	if got, _ := text(back, tagTemporalModified); got != m.Temporal || m.Temporal != "UNMODIFIED" {
		t.Errorf("the file records %q; Method says %q", got, m.Temporal)
	}
	var codes []string
	for i := 0; ; i++ {
		it, err := back.GetSequenceDataset(tagOf(tagDeidMethodCodes), i)
		if err != nil {
			break
		}
		v, _ := text(it, 0x00080100)
		codes = append(codes, v)
	}
	if len(codes) != len(m.Codes) || len(codes) != 2 {
		t.Fatalf("the file records the codes %q; Method says %v", codes, m.Codes)
	}
	for i, c := range m.Codes {
		if c.Value != codes[i] || !strings.Contains(m.Values[i], c.Meaning) {
			t.Errorf("code %d: %v, file %q, value %q", i, c, codes[i], m.Values[i])
		}
	}

	own := newTestEngine(t, testKeys, Options{Method: "own@1"}, rule("name", kw("PatientName"), aRemove))
	m, err = own.Method()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Codes) != 0 || len(m.Values) != 3 || m.Values[0] != "own@1" {
		t.Errorf("a profile on no Annex E base: %+v", m)
	}
}
