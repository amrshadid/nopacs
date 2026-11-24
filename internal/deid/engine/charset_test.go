package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
)

// TestTheEngineDeclaresTheCharacterSet guards Specific Character Set
// (0008,0005) as the engine's alone: ISO_IR 192 at the top level when any
// text kept, at any depth, is outside the default repertoire; none when
// every text left is ASCII, though the file declared one; an item's own
// declaration removed, the top level's covering it; a decider that
// removes the element cannot; and the file reads back with its text.
func TestTheEngineDeclaresTheCharacterSet(t *testing.T) {
	dec := fakeDecider{tSpecificCharacterSet: {Kind: ActRemove}}
	en, err := newEngine(dec, testKeys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ds := build(t,
		el{tStudyDesc, "LO", "Kopf"},
		el{tRefStudySeq, "SQ", []*dataset.Dataset{build(t,
			el{tSpecificCharacterSet, "CS", "ISO_IR 192"},
			el{tInstitution, "LO", "Klinik Köln"},
		)}},
	)
	rec := apply(t, en, ds)
	if v, _ := text(ds, tSpecificCharacterSet); v != "ISO_IR 192" {
		t.Errorf("the top level declares %q", v)
	}
	if item(t, ds, tRefStudySeq, 0).Contains(tagOf(tSpecificCharacterSet)) {
		t.Error("the item's own declaration is still there")
	}
	declared := false
	for _, e := range rec.Entries {
		declared = declared || (e.Tag == tSpecificCharacterSet && e.Path == "00080005" && e.Action == "set" && e.Rule == -1)
	}
	if !declared {
		t.Error("the record does not say the engine declared the character set")
	}
	back := readFile(t, writeFile(t, ds))
	if v, _ := text(item(t, back, tRefStudySeq, 0), tInstitution); v != "Klinik Köln" {
		t.Errorf("read back %q", v)
	}

	ascii := build(t, el{tSpecificCharacterSet, "CS", "ISO_IR 192"}, el{tStudyDesc, "LO", "Head"})
	apply(t, en, ascii)
	if ascii.Contains(tagOf(tSpecificCharacterSet)) {
		t.Error("an ASCII data set still declares a character set")
	}

	// A name outside the default repertoire removed leaves only ASCII.
	rm := newTestEngine(t, testKeys, Options{}, rule("name", kw("PatientName"), aRemove))
	gone := build(t, el{tSpecificCharacterSet, "CS", "ISO_IR 192"}, el{tPatientName, "PN", "Müller^Jürgen"}, el{tStudyDesc, "LO", "Kopf"})
	apply(t, rm, gone)
	if gone.Contains(tagOf(tSpecificCharacterSet)) {
		t.Error("the declaration stayed after the only non-ASCII text was removed")
	}
}

// TestTextTheEngineCannotReadIsRefused guards the text the engine could
// not search or clean: a value outside the default repertoire under a
// declaration the library did not decode it from (it declares ISO_IR 192
// over all it decodes), and a value that is not UTF-8, each refuse the
// file as bad_value and leave it as it was.
func TestTextTheEngineCannotReadIsRefused(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("desc", kw("StudyDescription"), aClean), rule("name", kw("PatientName"), aRemove))
	for name, ds := range map[string]*dataset.Dataset{
		"latin-1 bytes under ISO_IR 100": build(t, el{tSpecificCharacterSet, "CS", "ISO_IR 100"}, el{tStudyDesc, "LO", "M\xfcller"}),
		"not UTF-8, none declared":       build(t, el{tStudyDesc, "LO", "M\xfcller"}),
		"not UTF-8 under ISO_IR 192":     build(t, el{tSpecificCharacterSet, "CS", "ISO_IR 192"}, el{tStudyDesc, "LO", "ab\xc3("}),
	} {
		before := writeFile(t, ds)
		_, err := en.Apply(context.Background(), ds, Reception{})
		var f *Failure
		if !errors.As(err, &f) || f.Cause != CauseBadValue {
			t.Errorf("%s: %v", name, err)
		}
		if !bytes.Equal(before, writeFile(t, ds)) {
			t.Errorf("%s: a refused file was changed", name)
		}
	}
	// ASCII under another declaration is the default repertoire in any.
	apply(t, en, build(t, el{tSpecificCharacterSet, "CS", "ISO_IR 100"}, el{tStudyDesc, "LO", "Head"}))
}

// tSpecificCharacterSet is Specific Character Set (0008,0005).
const tSpecificCharacterSet = 0x00080005
