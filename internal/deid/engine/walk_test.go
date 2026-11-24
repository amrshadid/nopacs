package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestSequencesAndPrivateBlocks guards the walk: rules reach elements at
// any depth and by path; a private element is matched by its creator as
// its own item declares it, compared exactly; a block's creator follows
// its block; a private element no rule names is removed; an item is
// removed with its sequence and its values become needles.
func TestSequencesAndPrivateBlocks(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("names", vrSel("PN"), aReplace("ANON")),
		rule("deep", pathSel("RequestAttributesSequence[*]/RequestedProcedureID"), aRemove),
		rule("vendor", privSel("VENDOR", "0019", "10"), aKeep),
		rule("others", kw("OtherPatientIDsSequence"), aRemove),
	)
	inner := build(t,
		el{0x00190010, "LO", "VENDOR"},
		el{0x00191010, "LO", "kept"},
		el{0x00191011, "LO", "dropped"},
		el{0x00190011, "LO", "OTHER"},
		el{0x00191110, "LO", "other creator"},
		el{0x00401001, "SH", "RP1"},
		el{tPatientName, "PN", "Inner^Name"},
	)
	lower := build(t, el{0x00190010, "LO", "vendor"}, el{0x00191010, "LO", "case differs"})
	ds := build(t,
		el{tPatientName, "PN", "Doe^John"},
		el{0x00101002, "SQ", []*dataset.Dataset{build(t, el{tPatientID, "LO", "OTHER-ID"})}},
		el{0x00400275, "SQ", []*dataset.Dataset{inner, lower}},
		el{0x00401001, "SH", "RP-TOP"},
	)
	rec := apply(t, en, ds)
	got := item(t, ds, 0x00400275, 0)
	if v, _ := text(got, tPatientName); v != "ANON" {
		t.Errorf("a name in an item: %q", v)
	}
	if got.Contains(tagOf(0x00401001)) {
		t.Error("the path's element is still in the item")
	}
	if v, _ := text(ds, 0x00401001); v != "RP-TOP" {
		t.Errorf("the path reached the top level: %q", v)
	}
	if v, _ := text(got, 0x00191010); v != "kept" || !got.Contains(tagOf(0x00190010)) {
		t.Errorf("the kept private element or its creator: %q", v)
	}
	for _, gone := range []uint32{0x00191011, 0x00190011, 0x00191110} {
		if got.Contains(tagOf(gone)) {
			t.Errorf("%08X is still in the item", gone)
		}
	}
	other := item(t, ds, 0x00400275, 1)
	if other.Contains(tagOf(0x00191010)) || other.Contains(tagOf(0x00190010)) {
		t.Error("a creator that differs in case was taken as the rule's")
	}
	if ds.Contains(tagOf(0x00101002)) {
		t.Error("the removed sequence is present")
	}
	found := false
	for _, n := range rec.Needles {
		found = found || n.Value == "OTHER-ID"
	}
	if !found {
		t.Error("a value of a removed sequence is not a needle")
	}
	readFile(t, writeFile(t, ds))
}

// TestContentItemSelector guards a content item rule: it selects the
// value of the structured report content item whose concept name it
// names, at any depth of the content tree, and nothing else.
func TestContentItemSelector(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("finding", itemSel("DCM", "121071", "TEXT"), aReplace("removed")))
	concept := func(scheme, value string) []*dataset.Dataset {
		return []*dataset.Dataset{build(t, el{0x00080100, "SH", value}, el{0x00080102, "SH", scheme})}
	}
	nested := build(t, el{0x0040A040, "CS", "TEXT"}, el{0x0040A043, "SQ", concept("DCM", "121071")}, el{0x0040A160, "UT", "John has a finding"})
	other := build(t, el{0x0040A040, "CS", "TEXT"}, el{0x0040A043, "SQ", concept("DCM", "999999")}, el{0x0040A160, "UT", "kept text"})
	root := build(t, el{0x0040A730, "SQ", []*dataset.Dataset{nested, other}})
	ds := build(t, el{0x0040A730, "SQ", []*dataset.Dataset{root}})
	apply(t, en, ds)
	top := item(t, ds, 0x0040A730, 0)
	if v, _ := text(item(t, top, 0x0040A730, 0), 0x0040A160); v != "removed" {
		t.Errorf("the item's value: %q", v)
	}
	if v, _ := text(item(t, top, 0x0040A730, 1), 0x0040A160); v != "kept text" {
		t.Errorf("another item's value: %q", v)
	}
}

// TestFixedElements guards what the engine does whatever the profile
// says: pixel data kept even under a rule that removes its VR, Original
// Attributes and trailing padding removed, group lengths removed.
func TestFixedElements(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("ob", vrSel("OW"), aRemove), rule("keep-all", vrSel("SQ"), aKeep))
	ds := build(t,
		el{0x00080000, "UL", []byte{4, 0, 0, 0}},
		el{0x04000561, "SQ", []*dataset.Dataset{build(t, el{tPatientName, "PN", "Doe^John"})}},
		el{0x7FE00010, "OW", []byte{1, 2, 3, 4}},
		el{0xFFFCFFFC, "OB", []byte{0, 0}},
	)
	rec := apply(t, en, ds)
	if !ds.Contains(tagOf(0x7FE00010)) {
		t.Error("pixel data was removed")
	}
	for _, gone := range []uint32{0x00080000, 0x04000561, 0xFFFCFFFC} {
		if ds.Contains(tagOf(gone)) {
			t.Errorf("%08X is present", gone)
		}
	}
	if len(rec.Needles) != 1 || rec.Needles[0].Value != "Doe^John" {
		t.Errorf("the Original Attributes' values as needles: %v entries", len(rec.Needles))
	}
}

// TestThePipelineHandlesWhatTheLanguageRefuses guards that the engine
// treats every element the profile language refuses to name as that
// language says (profile.Pipeline), whatever the profile decides: command
// elements, the directory elements of group 0004 (a file name made of the
// patient's name, the instance's own UID) and the elements of group 7FE0
// that are not pixel data are removed, under a decider that keeps
// everything and under a profile that remaps every UID, never kept by the
// default; pixel data and the table of its frames stay; and each record
// entry gives the reason the language gives.
func TestThePipelineHandlesWhatTheLanguageRefuses(t *testing.T) {
	sample := func() *dataset.Dataset {
		return build(t,
			el{0x00001000, "UI", "1.2.3.4.5.6.7.8"},
			el{0x00041500, "CS", `DOE_JOHN\IMG0001`},
			el{0x00041511, "UI", "1.2.3.4.5.6.7.8"},
			el{tSOPClass, "UI", ctImageStorage}, el{tSOPInstance, "UI", "1.2.3.4.5.6.7.8"},
			el{0x7FE00001, "OV", make([]byte, 8)},
			el{0x7FE00002, "OV", make([]byte, 8)},
			el{0x7FE00010, "OW", []byte{1, 2, 3, 4}},
			el{0x7FE00020, "OW", []byte("DOE^JOHN")},
		)
	}
	keepAll, err := newEngine(fakeDecider{}, testKeys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, en := range map[string]*Engine{
		"keep everything": keepAll,
		"remap UIDs":      newTestEngine(t, testKeys, Options{}, rule("uids", vrSel("UI"), aUID)),
	} {
		ds := sample()
		rec := apply(t, en, ds)
		for _, e := range rec.Entries {
			h, why := profile.Pipeline(e.Tag)
			if h == profile.NotHandled {
				continue
			}
			if e.Reason != why || e.Rule != -1 {
				t.Errorf("%s: %s: rule %d, reason %q", name, e.Path, e.Rule, e.Reason)
			}
			if gone := !ds.Contains(tagOf(e.Tag)); gone != (h == profile.HandledRemoved) {
				t.Errorf("%s: %s is removed %v, handled as %v", name, e.Path, gone, h)
			}
		}
		for _, tg := range []uint32{0x00001000, 0x00041500, 0x00041511, 0x7FE00020} {
			if ds.Contains(tagOf(tg)) {
				t.Errorf("%s: %08X stayed", name, tg)
			}
		}
		for _, tg := range []uint32{0x7FE00001, 0x7FE00002, 0x7FE00010} {
			if !ds.Contains(tagOf(tg)) {
				t.Errorf("%s: %08X was removed", name, tg)
			}
		}
	}
}

// TestAVRTheRuleWasNotCheckedForFails guards an element whose VR in the
// file is not the one its rule was checked for: hashing an element the
// file holds as a date fails, removing it does not, and an element held
// as UN is treated as the dictionary's VR.
func TestAVRTheRuleWasNotCheckedForFails(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("id", kw("PatientID"), aHash))
	var f *Failure
	if _, err := en.Apply(context.Background(), build(t, el{tPatientID, "DA", "20240101"}), Reception{}); !errors.As(err, &f) || f.Cause != CauseBadValue {
		t.Errorf("a hash of an element held as DA: %v", err)
	}
	un := build(t, el{tPatientID, "UN", "PAT001"})
	apply(t, en, un)
	elem, _ := un.Get(tagOf(tPatientID))
	if got := mustText(t, un, tPatientID); got != naming.HashText(testKeys.Hash, []byte("PAT001")) || elem.GetVR() != "LO" {
		t.Errorf("an element held as UN: %q, VR %s", got, elem.GetVR())
	}
	rm := newTestEngine(t, testKeys, Options{}, rule("id", kw("PatientID"), aRemove))
	apply(t, rm, build(t, el{tPatientID, "DA", "20240101"}))
}

// TestCleanOnASequenceAndOverlayBits guards what clean does beyond free
// text: a sequence is kept and its items walked, each element by its own
// rule, as keep does; the bits of an overlay (OB or OW) are removed, not
// read for text.
func TestCleanOnASequenceAndOverlayBits(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("refs", kw("ReferencedStudySequence"), aClean),
		rule("uids", vrSel("UI"), aUID),
		rule("overlay", &v1.ProfileSelector{Kind: &v1.ProfileSelector_Tag{Tag: "60003000"}}, aClean),
	)
	ds := build(t,
		el{tRefStudySeq, "SQ", []*dataset.Dataset{build(t, el{tRefSOPInstance, "UI", "1.2.3.9"})}},
		el{0x60003000, "OW", []byte{1, 0, 2, 0}},
	)
	apply(t, en, ds)
	if v, _ := text(item(t, ds, tRefStudySeq, 0), tRefSOPInstance); v != RemapUID(testKeys.UID, "1.2.3.9") {
		t.Errorf("a UID in a cleaned sequence: %q", v)
	}
	if ds.Contains(tagOf(0x60003000)) {
		t.Error("the overlay's bits stayed")
	}
}
