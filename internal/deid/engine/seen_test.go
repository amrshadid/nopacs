package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataset"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestUnnamedElementsAreReported guards the report of what no rule
// named: each standard element and each (private creator, element) a
// default decided, once with its VR and its count at any depth, the block
// number written 00; never an element a rule named, a private creator
// element or an element the pipeline handles; never a value.
func TestUnnamedElementsAreReported(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("names", vrSel("PN"), aRemove),
		rule("uids", vrSel("UI"), aUID),
		rule("acme-10", privSel("ACME 1.0", "0009", "10"), aKeep),
	)
	item := func(v string) *dataset.Dataset {
		return build(t, el{0x00080100, "SH", v}, el{0x00090010, "LO", "ACME 1.0"}, el{0x00091011, "LO", "secret-" + v})
	}
	ds := build(t,
		el{tSOPInstance, "UI", "1.2.3.4"},
		el{tStudyDesc, "LO", "Head CT of Jane"},
		el{tPatientName, "PN", "Doe^Jane"},
		el{0x00090010, "LO", "ACME 1.0"},
		el{0x00091010, "LO", "kept by a rule"},
		el{0x00091012, "LO", "orphan-value"},
		el{0x00090020, "LO", "OTHER"},
		el{0x00092012, "LO", "other-value"},
		el{0x00081032, "SQ", []*dataset.Dataset{item("CODE1"), item("CODE2")}},
	)
	rec := apply(t, en, ds)
	got := map[string]int{}
	for _, u := range rec.Unnamed {
		got[u.String()] = u.Count
	}
	want := map[string]int{
		"0008,1030 LO":            1,
		"0008,1032 SQ":            1,
		"0008,0100 SH":            2,
		`0009,xx11 LO "ACME 1.0"`: 2,
		`0009,xx12 LO "ACME 1.0"`: 1,
		`0009,xx12 LO "OTHER"`:    1,
	}
	if len(got) != len(want) {
		t.Errorf("unnamed %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	if rec.UnnamedOver != 0 {
		t.Errorf("%d over", rec.UnnamedOver)
	}
	printed := fmt.Sprintf("%v %+v", rec.Unnamed, rec.Unnamed)
	for _, v := range []string{"Head CT", "Jane", "secret", "orphan", "other-value", "CODE1"} {
		if strings.Contains(printed, v) {
			t.Errorf("the report holds a value, %q: %s", v, printed)
		}
	}
}

// TestUnnamedHidesACreatorHoldingARemovedValue guards the report against a
// private creator that is the patient's name, or holds the patient's ID,
// which the profile removes: such a creator is withheld (marked
// unreadable), its element still counted, and a vendor's creator beside it
// is given.
func TestUnnamedHidesACreatorHoldingARemovedValue(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("names", vrSel("PN"), aRemove), rule("uids", vrSel("UI"), aUID),
		rule("pid", kw("PatientID"), aRemove))
	ds := build(t,
		el{tSOPInstance, "UI", "1.2.3.4"},
		el{0x00090010, "LO", "DOE^JANE"},
		el{0x00090011, "LO", "SITE MRN778812"},
		el{0x00090012, "LO", "ACME 1.0"},
		el{0x00091011, "LO", "x"},
		el{0x00091111, "LO", "y"},
		el{0x00091211, "LO", "z"},
		el{tPatientName, "PN", "Doe^Jane"},
		el{tPatientID, "LO", "MRN778812"},
	)
	rec := apply(t, en, ds)
	printed := strings.ToUpper(fmt.Sprintf("%v %+v", rec.Unnamed, rec.Unnamed))
	if strings.Contains(printed, "DOE") || strings.Contains(printed, "MRN778812") {
		t.Errorf("the unnamed report names a removed value: %s", printed)
	}
	unreadable, vendor := 0, false
	for _, u := range rec.Unnamed {
		if u.CreatorUnreadable {
			unreadable += u.Count
		}
		vendor = vendor || u.Creator == "ACME 1.0"
	}
	if unreadable != 2 || !vendor {
		t.Errorf("withheld %d, vendor given %v: %v", unreadable, vendor, rec.Unnamed)
	}
}

// TestUnnamedHidingIsBoundedInTime guards the search of the report's
// creators: a file of 1,000 private blocks, each of its own creator, and
// 2,000 names in a sequence is de-identified in well under a second (the
// creators are searched for every name in one pass each), and a creator
// holding a part of the patient's name among other words is withheld.
func TestUnnamedHidingIsBoundedInTime(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("names", vrSel("PN"), aRemove), rule("uids", vrSel("UI"), aUID))
	els := []el{{tSOPInstance, "UI", "1.2.3.4"}, {tPatientName, "PN", "Doe^Jane"}}
	n := 0
	for g := uint32(0x0009); n < 1000; g += 2 {
		for b := uint32(0x10); b <= 0xFF && n < 1000; b++ {
			creator := fmt.Sprintf("VENDOR CREATOR NUMBER %06d", n)
			if n == 500 {
				creator = "ACME JANE DOE"
			}
			els = append(els, el{g<<16 | b, "LO", creator}, el{g<<16 | b<<8 | 0x01, "LO", "x"})
			n++
		}
	}
	items := make([]*dataset.Dataset, 2000)
	for i := range items {
		items[i] = build(t, el{0x0040A123, "PN", fmt.Sprintf("Person%05d^Given%05d", i, i)})
	}
	els = append(els, el{0x0040A730, "SQ", items})
	ds := build(t, els...)
	start := time.Now()
	rec := apply(t, en, ds)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("de-identifying took %v", d)
	}
	withheld := 0
	for _, u := range rec.Unnamed {
		if u.CreatorUnreadable {
			withheld += u.Count
		}
		if strings.Contains(u.Creator, "DOE") {
			t.Errorf("the report names the patient: %s", u)
		}
	}
	if withheld != 1 {
		t.Errorf("%d withheld, want 1", withheld)
	}
}

// TestUnnamedIsBounded guards the report's memory: a file of more distinct
// private elements than MaxUnnamed lists MaxUnnamed and counts the rest.
func TestUnnamedIsBounded(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{})
	var els []el
	n := 0
	for g := uint32(0x0009); n < MaxUnnamed+100; g += 2 {
		els = append(els, el{g<<16 | 0x0010, "LO", fmt.Sprintf("CREATOR %04X", g)})
		for e := uint32(0x00); e < 0xF0 && n < MaxUnnamed+100; e++ {
			els = append(els, el{g<<16 | 0x1000 | e, "LO", "v"})
			n++
		}
	}
	rec := apply(t, en, build(t, els...))
	if len(rec.Unnamed) != MaxUnnamed || rec.UnnamedOver != 100 {
		t.Errorf("%d listed, %d over", len(rec.Unnamed), rec.UnnamedOver)
	}
}

// TestUnnamedOnCorpusFiles guards the report over the receiver's public
// files under the corpus profile: each file de-identified lists some
// element no rule named; every one listed is standard and named by no
// rule of the profile (its tag is in the record, decided by a default), or
// private; and a profile with a rule for each element listed lists none
// of them again.
func TestUnnamedOnCorpusFiles(t *testing.T) {
	en := newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{
		Rules:      corpusRules(),
		PixelRules: []*v1.PixelRule{{Name: "accept-all", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "the attributes are judged here"}},
	})
	files := 0
	for _, path := range corpusFiles(t, receiveTestdata) {
		ds, err := readPath(t, path)
		if err != nil {
			continue
		}
		rec, err := en.Apply(context.Background(), ds, Reception{})
		var f *Failure
		if errors.As(err, &f) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		files++
		if len(rec.Unnamed) == 0 {
			t.Errorf("%s: nothing unnamed", filepath.Base(path))
		}
		byDefault := map[uint32]bool{}
		for _, e := range rec.Entries {
			if e.Rule < 0 {
				byDefault[e.Tag] = true
			}
		}
		var named []*v1.ProfileRule
		for _, u := range rec.Unnamed {
			if u.Count < 1 {
				t.Errorf("%s: %s counted %d", filepath.Base(path), u, u.Count)
			}
			if u.Private() {
				continue
			}
			if !byDefault[u.Tag] {
				t.Errorf("%s: %s is listed and no entry of it was decided by a default", filepath.Base(path), u)
			}
			named = append(named, rule("", &v1.ProfileSelector{Kind: &v1.ProfileSelector_Tag{Tag: formatTag(u.Tag)}}, aKeep))
		}
		again, err := readPath(t, path)
		if err != nil {
			t.Fatal(err)
		}
		full := newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{
			Rules:      append(corpusRules(), named...),
			PixelRules: []*v1.PixelRule{{Name: "accept-all", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "the attributes are judged here"}},
		})
		rec2, err := full.Apply(context.Background(), again, Reception{})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, u := range rec2.Unnamed {
			if !u.Private() {
				t.Errorf("%s: %s is listed though a rule names it", filepath.Base(path), u)
			}
		}
	}
	if files < 10 {
		t.Fatalf("only %d files", files)
	}
}
