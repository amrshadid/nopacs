package profile_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestShippedCatalog guards the bases the binary ships: the catalog is
// made without error; every base of PS3.15 Annex E is known by name, of
// this edition; those with attributes to change are built from the DICOM
// library's tables and expand, Clean Pixel Data expands to no rules (its
// work is the pixel rules), Clean Recognizable Visual Features is refused
// with its reason; a name
// of an earlier edition is refused as unknown; and the catalog lists each
// base as available or not.
func TestShippedCatalog(t *testing.T) {
	cat := profile.Shipped()
	names := cat.Names()
	want := []string{
		"annex-e/basic:2026d", "annex-e/clean-descriptors:2026d", "annex-e/clean-graphics:2026d",
		"annex-e/clean-pixel-data:2026d", "annex-e/clean-recognizable-visual-features:2026d",
		"annex-e/clean-structured-content:2026d", "annex-e/retain-device-identity:2026d",
		"annex-e/retain-institution-identity:2026d", "annex-e/retain-longitudinal-full-dates:2026d",
		"annex-e/retain-longitudinal-modified-dates:2026d", "annex-e/retain-patient-characteristics:2026d",
		"annex-e/retain-safe-private:2026d", "annex-e/retain-uids:2026d",
		"nopacs/longitudinal:1", "nopacs/strict:1",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("bases %v, want %v", names, want)
	}
	refused := map[string]string{
		"annex-e/clean-recognizable-visual-features:2026d": "faces",
	}
	for _, n := range names {
		b, unavailable, found := cat.Lookup(n)
		if !found || b.Description == "" {
			t.Errorf("%s: found %v, description %q", n, found, b.Description)
		}
		_, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{n}}, cat)
		if why, ok := refused[n]; ok {
			if !strings.Contains(unavailable, why) || (strings.HasPrefix(n, "annex-e/") && b.Document != nil) {
				t.Errorf("%s: unavailable %q, want it to say %q", n, unavailable, why)
			}
			if len(problems) != 1 || problems[0].Code != profile.CodeBaseUnavailable || !strings.Contains(problems[0].Message, n) {
				t.Errorf("a document naming %s: %v", n, problems)
			}
			continue
		}
		if unavailable != "" || len(problems) > 0 {
			t.Errorf("%s: unavailable %q, problems %v", n, unavailable, problems)
		}
	}
	_, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{"annex-e/basic:2026c"}}, cat)
	if len(problems) != 1 || problems[0].Code != profile.CodeUnknownBase {
		t.Errorf("a document naming an earlier edition: %v", problems)
	}
	listed := cat.Proto()
	if len(listed) != len(want) {
		t.Fatalf("%d bases listed", len(listed))
	}
	for _, b := range listed {
		_, isRefused := refused[b.GetName()]
		if b.GetAvailable() == isRefused || (b.GetUnavailableReason() == "") == isRefused || (b.GetDocument() == nil) != isRefused {
			t.Errorf("listed %s: available %v, reason %q, document %v", b.GetName(), b.GetAvailable(), b.GetUnavailableReason(), b.GetDocument() != nil)
		}
	}
	// A document of its own rules needs no base.
	if _, problems := profile.Expand(&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("PatientName"), remove())}}, cat); len(problems) > 0 {
		t.Errorf("own rules over the shipped catalog: %v", problems)
	}
}

// TestShippedCompositesAreValid guards the composite bases' own rules and
// bases: with every Annex E base available (empty ones standing in for
// the two that change pixels), each composite expands over the basic
// profile first and itself last, and nopacs/strict keeps its two rules and
// removes what no rule names.
func TestShippedCompositesAreValid(t *testing.T) {
	var bases []profile.Base
	for _, n := range profile.Shipped().Names() {
		b, unavailable, _ := profile.Shipped().Lookup(n)
		if strings.HasPrefix(n, "annex-e/") && unavailable != "" {
			b = profile.Base{Name: n, Document: &v1.ProfileDocument{}}
		}
		bases = append(bases, b)
	}
	cat, err := profile.NewCatalog(bases...)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"nopacs/longitudinal:1", "nopacs/strict:1"} {
		e := mustExpand(t, &v1.ProfileDocument{Bases: []string{n}}, cat)
		if e.GetBases()[0] != profile.AnnexE("basic") || e.GetBases()[len(e.GetBases())-1] != n {
			t.Errorf("%s expands over %v", n, e.GetBases())
		}
	}
	strict := mustExpand(t, &v1.ProfileDocument{Bases: []string{"nopacs/strict:1"}}, cat)
	own := 0
	for _, r := range strict.GetRules() {
		if r.GetOrigin() == "nopacs/strict:1" {
			own++
		}
	}
	if strict.GetUnnamed() != v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE || own != 2 {
		t.Errorf("strict: unnamed %v, %d rules of its own", strict.GetUnnamed(), own)
	}
}

// TestNewCatalogRefuses guards what a catalog refuses: a malformed or
// repeated name, a base with neither rules nor a reason, a base naming one
// the catalog does not hold, bases naming each other in a circle, and a
// base whose own document has a problem.
func TestNewCatalogRefuses(t *testing.T) {
	doc := func(bases ...string) *v1.ProfileDocument { return &v1.ProfileDocument{Bases: bases} }
	for _, c := range []struct {
		name  string
		bases []profile.Base
	}{
		{"malformed name", []profile.Base{{Name: "basic", Document: doc()}}},
		{"name without edition", []profile.Base{{Name: "test/basic", Document: doc()}}},
		{"repeated name", []profile.Base{{Name: "test/a:1", Document: doc()}, {Name: "test/a:1", Document: doc()}}},
		{"neither rules nor reason", []profile.Base{{Name: "test/a:1"}}},
		{"rules and reason", []profile.Base{{Name: "test/a:1", Document: doc(), Unavailable: "why"}}},
		{"unknown inner base", []profile.Base{{Name: "test/a:1", Document: doc("test/b:1")}}},
		{"circle", []profile.Base{{Name: "test/a:1", Document: doc("test/b:1")}, {Name: "test/b:1", Document: doc("test/a:1")}}},
		{"itself", []profile.Base{{Name: "test/a:1", Document: doc("test/a:1")}}},
		{"bad rule", []profile.Base{{Name: "test/a:1", Document: &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", kw("Nobody"), remove())}}}}},
	} {
		if _, err := profile.NewCatalog(c.bases...); err == nil {
			t.Errorf("%s: made", c.name)
		}
	}
}
