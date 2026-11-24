package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/receive/header"
)

// panicDecider panics deciding the element at tag.
type panicDecider struct {
	fakeDecider
	at uint32
}

func (p panicDecider) Decide(loc profile.Location, f profile.Facts) Decision {
	if loc.Tag == p.at {
		panic("a fault deciding " + formatTag(loc.Tag))
	}
	return p.fakeDecider.Decide(loc, f)
}

// TestAPanicIsARefusalAndChangesNothing guards that a fault of the engine
// is a Failure of cause internal, never a half-changed data set: a panic
// while planning leaves the data set untouched, and one part way through
// making the changes undoes those made.
func TestAPanicIsARefusalAndChangesNothing(t *testing.T) {
	mk := func() *dataset.Dataset { return sample(t) }
	en, err := newEngine(panicDecider{at: tStudyDesc}, testKeys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ds := mk()
	before := writeFile(t, ds)
	_, err = en.Apply(context.Background(), ds, Reception{})
	var f *Failure
	if !errors.As(err, &f) || f.Cause != CauseInternal || f.Reason() != ReasonFailed {
		t.Fatalf("a panic planning: %v", err)
	}
	if strings.Contains(err.Error(), "deciding") {
		t.Errorf("the panic's text reached the error: %v", err)
	}
	if !bytes.Equal(before, writeFile(t, ds)) {
		t.Error("a panic planning changed the data set")
	}

	half := newTestEngine(t, testKeys, Options{Lookups: &mapLookups{codes: map[string]string{}}, Method: "m@1"}, sampleRules()...)
	half.seams.beforeOp = func(i int) {
		if i == 6 {
			panic("a fault applying")
		}
	}
	ds = mk()
	before = writeFile(t, ds)
	if _, err := half.Apply(context.Background(), ds, Reception{}); !errors.As(err, &f) || f.Cause != CauseInternal {
		t.Fatalf("a panic applying: %v", err)
	}
	if !bytes.Equal(before, writeFile(t, ds)) {
		t.Error("a panic applying left the data set half changed")
	}
}

// countingLookups is Lookups that counts the codes it is asked to assign
// and the calls that ask.
type countingLookups struct {
	mapLookups
	assigned, calls int
}

func (c *countingLookups) Assign(ctx context.Context, keys []LookupKey) ([]string, error) {
	c.assigned += len(keys)
	c.calls++
	return c.mapLookups.Assign(ctx, keys)
}

// TestLookupsAssignOnlyAfterThePlan guards that a table assigns a code
// only for a file the engine goes on to change: a file refused by a later
// rule, or a later value, takes none; one that succeeds takes one for each
// value, once though it appears twice, all in one call, so a file takes
// all its codes or none.
func TestLookupsAssignOnlyAfterThePlan(t *testing.T) {
	lk := &countingLookups{mapLookups: mapLookups{codes: map[string]string{}}}
	en := newTestEngine(t, testKeys, Options{Lookups: lk},
		rule("id", kw("PatientID"), aLookup("subjects", true)),
		rule("other", kw("OtherPatientIDs"), aLookup("subjects", true)),
		rule("no-sr", kw("ContentSequence"), aQuarantine),
		rule("date", kw("StudyDate"), aShift),
	)
	var f *Failure
	refused := build(t, el{tPatientID, "LO", "PAT001"}, el{tContentSequence, "SQ", []*dataset.Dataset{build(t)}})
	if _, err := en.Apply(context.Background(), refused, Reception{}); !errors.As(err, &f) || f.Cause != CauseRuleQuarantine {
		t.Fatalf("refused: %v", err)
	}
	bad := build(t, el{tPatientID, "LO", "PAT001"}, el{tStudyDate, "DA", "20241345"})
	if _, err := en.Apply(context.Background(), bad, Reception{}); !errors.As(err, &f) || f.Cause != CauseBadValue {
		t.Fatalf("a bad date: %v", err)
	}
	if lk.assigned != 0 {
		t.Fatalf("refused files took %d codes", lk.assigned)
	}
	ok := build(t, el{tPatientID, "LO", "PAT001"}, el{0x00101000, "LO", "PAT001"})
	apply(t, en, ok)
	two := build(t, el{tPatientID, "LO", "PAT002"}, el{0x00101000, "LO", "PAT003"})
	apply(t, en, two)
	if lk.calls != 2 || lk.assigned != 3 || mustText(t, two, tPatientID) != "A0002" || mustText(t, two, 0x00101000) != "A0003" {
		t.Errorf("two values of one file: %d calls for %d codes", lk.calls, lk.assigned)
	}
	if lk.assigned != 3 || mustText(t, ok, tPatientID) != "A0001" || mustText(t, ok, 0x00101000) != "A0001" {
		t.Errorf("assigned %d: %q %q", lk.assigned, mustText(t, ok, tPatientID), mustText(t, ok, 0x00101000))
	}
}

// TestKeysAndSetRulesAreChecked guards New's and the plan's checks: two
// equal keys are refused; a set rule whose VR is not one VR that holds
// text refuses the file.
func TestKeysAndSetRulesAreChecked(t *testing.T) {
	e, problems := profile.Expand(&v1.ProfileDocument{}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	for _, keys := range []Keys{
		{UID: key('a'), Date: key('a'), Hash: key('h')},
		{UID: key('u'), Date: key('d'), Hash: key('u')},
		{UID: key('u'), Date: key('d'), Hash: key('d')},
	} {
		if _, err := New(e, keys, Options{}); err == nil {
			t.Error("equal keys were taken")
		}
	}
	for _, vr := range []string{"US or SS", "US", ""} {
		dec := setDecider{SetRule{Rule: 0, Tag: 0x00280106, VR: vr}}
		en, err := newEngine(dec, testKeys, Options{})
		if err != nil {
			t.Fatal(err)
		}
		var f *Failure
		if _, err := en.Apply(context.Background(), build(t), Reception{}); !errors.As(err, &f) || f.Cause != CauseBadValue {
			t.Errorf("a set rule of VR %q: %v", vr, err)
		}
	}
}

// setDecider is a decider with one set rule, which decides its element.
type setDecider struct{ s SetRule }

func (d setDecider) Decide(loc profile.Location, _ profile.Facts) Decision {
	if loc.Tag == d.s.Tag {
		return Decision{Rule: d.s.Rule, Action: Action{Kind: ActSet, Constant: "1"}}
	}
	return Decision{Rule: -1, Action: Action{Kind: ActKeep}}
}
func (d setDecider) Sets() []SetRule    { return []SetRule{d.s} }
func (setDecider) Templates() []string  { return nil }
func (setDecider) DateShiftDays() int32 { return 365 }

// TestUnknownSequencesAreWalked guards a sequence the DICOM library kept
// as UN bytes (a private sequence of defined length, PS3.5 6.2.2): kept by
// a rule, it is parsed and walked, the name inside removed by the floor
// and kept as a needle, and the element becomes a sequence that writes and
// reads back without the name, in explicit and implicit VR; a standard
// element unknown to the dictionary holding items is walked the same; one
// that begins with an item and is not items refuses the file when kept
// and is removed when a rule removes it; one no rule keeps is removed; and
// a UN value of text is kept. Each case reads its input from a file
// written in Explicit and in Implicit VR Little Endian.
func TestUnknownSequencesAreWalked(t *testing.T) {
	items := append([]byte{0xFE, 0xFF, 0x00, 0xE0, 0x10, 0, 0, 0}, []byte("\x10\x00\x10\x00\x08\x00\x00\x00Doe^John")...)
	keep := newTestEngine(t, testKeys, Options{}, rule("vendor", privSel("VENDOR", "0029", "xx"), aKeep))
	for _, ts := range []string{"1.2.840.10008.1.2.1", implicitVR} {
		// The input is a file of the syntax, as it arrives: in explicit VR
		// the element says UN, in implicit VR it says nothing, and the
		// reader keeps its bytes either way.
		ds := readFile(t, writeFileAs(t, build(t, el{0x00080016, "UI", ctImageStorage}, el{0x00080018, "UI", "1.2.3.4"},
			el{0x00290010, "LO", "VENDOR"}, el{0x00291010, "UN", items}), ts))
		if e, _ := ds.Get(tagOf(0x00291010)); e.GetVR() == "SQ" {
			t.Fatalf("%s: the reader parsed the sequence itself; the test reads nothing", ts)
		}
		rec := apply(t, keep, ds)
		elem, ok := ds.Get(tagOf(0x00291010))
		if !ok || elem.GetVR() != "SQ" {
			t.Fatalf("%s: the private sequence: present %v, VR %v", ts, ok, elem.GetVR())
		}
		if got, _ := text(item(t, ds, 0x00291010, 0), tPatientName); got == "Doe^John" || got == "" {
			t.Errorf("%s: the name inside the sequence: %q", ts, got)
		}
		found := false
		for _, n := range rec.Needles {
			found = found || (n.Value == "Doe^John" && n.Path == "00291010[0]/00100010")
		}
		if !found {
			t.Errorf("%s: the name inside is not a needle: %v", ts, len(rec.Needles))
		}
		out := writeFileAs(t, ds, ts)
		if bytes.Contains(out, []byte("Doe^John")) || bytes.Contains(out, []byte("Doe")) {
			t.Errorf("%s: the written file holds the name", ts)
		}
		back := readFile(t, out)
		if !back.Contains(tagOf(0x00291010)) {
			t.Errorf("%s: the kept sequence did not read back", ts)
		}
	}
	unknown := build(t, el{0x00089998, "UN", items})
	apply(t, keep, unknown)
	if got, _ := text(item(t, unknown, 0x00089998, 0), tPatientName); got == "Doe^John" {
		t.Error("an unknown standard element holding items kept the name inside")
	}
	broken := append([]byte{0xFE, 0xFF, 0x00, 0xE0, 0x20, 0, 0, 0}, []byte("\x10\x00\x10\x00\x08\x00\x00\x00Doe^John")...)
	var f *Failure
	ds := build(t, el{0x00290010, "LO", "VENDOR"}, el{0x00291010, "UN", broken})
	if _, err := keep.Apply(context.Background(), ds, Reception{}); !errors.As(err, &f) || f.Cause != CauseBadValue || f.Path != "00291010" {
		t.Errorf("a UN value beginning with an item that is not items, kept: %v", err)
	}
	drop := newTestEngine(t, testKeys, Options{}, rule("vendor", privSel("VENDOR", "0029", "xx"), aRemove))
	ds = build(t, el{0x00290010, "LO", "VENDOR"}, el{0x00291010, "UN", broken})
	apply(t, drop, ds)
	if ds.Contains(tagOf(0x00291010)) {
		t.Error("a UN value that is not items, removed by a rule, stayed")
	}
	rm := newTestEngine(t, testKeys, Options{})
	gone := build(t, el{0x00290010, "LO", "VENDOR"}, el{0x00291010, "UN", items})
	apply(t, rm, gone)
	if gone.Contains(tagOf(0x00291010)) {
		t.Error("the private UN sequence stayed")
	}
	plain := build(t, el{0x00290010, "LO", "VENDOR"}, el{0x00291010, "UN", "plain"})
	apply(t, keep, plain)
	if !plain.Contains(tagOf(0x00291010)) {
		t.Error("a UN value of text was not kept")
	}
}

// TestCreatorsAndContentItemsAnywhere guards two readings of a data set: a
// private creator padded with a NUL or spaces is its creator; and a
// content item is any item with a Value Type, here one of the Acquisition
// Context Sequence (0040,0555), which a content item rule selects.
func TestCreatorsAndContentItemsAnywhere(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("vendor", privSel("VENDOR", "0029", "10"), aKeep),
		rule("operator", itemSel("DCM", "113870", "PNAME"), aReplace("OPERATOR")),
		rule("note", itemSel("DCM", "121106", "TEXT"), aRemove),
	)
	concept := func(value string) []*dataset.Dataset {
		return []*dataset.Dataset{build(t, el{0x00080100, "SH", value}, el{0x00080102, "SH", "DCM"})}
	}
	ds := build(t,
		el{0x00290010, "LO", " VENDOR\x00"},
		el{0x00291010, "LO", "kept"},
		el{0x00400555, "SQ", []*dataset.Dataset{
			build(t, el{0x0040A040, "CS", "PNAME "}, el{0x0040A043, "SQ", concept("113870")}, el{0x0040A123, "PN", "Doe^John"}),
			build(t, el{0x0040A040, "CS", "TEXT"}, el{0x0040A043, "SQ", concept("121106")}, el{0x0040A160, "UT", "John Doe was here"}),
		}},
	)
	apply(t, en, ds)
	if v, _ := text(ds, 0x00291010); v != "kept" {
		t.Errorf("the padded creator's element: %q", v)
	}
	if v, _ := text(item(t, ds, 0x00400555, 0), 0x0040A123); v != "OPERATOR" {
		t.Errorf("a person name content item in the acquisition context: %q", v)
	}
	if item(t, ds, 0x00400555, 1).Contains(tagOf(0x0040A160)) {
		t.Error("a text content item in the acquisition context stayed")
	}
}

// TestHashedPatientIDIsTheSubjectsCode guards that the engine's hash of
// Patient ID and the subject label's code, made by the naming rule from
// the header the receiver reads, are made of the same text and so are
// equal, padding and leading spaces included (PS3.5 6.2: an LO's leading
// and trailing spaces are not part of its value).
func TestHashedPatientIDIsTheSubjectsCode(t *testing.T) {
	for _, id := range []string{"PAT001", " PAT001  ", "PAT 001", "pat-001\x00"} {
		ds := build(t,
			el{tSOPClass, "UI", ctImageStorage}, el{tSOPInstance, "UI", "1.2.3.4"},
			el{tStudyUID, "UI", "1.2.3"}, el{0x0020000E, "UI", "1.2.3.5"},
			el{tPatientID, "LO", id},
		)
		b := writeFile(t, ds)
		h, err := header.Read(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		hasher, err := naming.NewHasher(testKeys.Hash)
		if err != nil {
			t.Fatal(err)
		}
		labels, err := naming.Default().Labels(h, hasher)
		if err != nil {
			t.Fatal(err)
		}
		en := newTestEngine(t, testKeys, Options{}, rule("id", kw("PatientID"), aHash))
		got := readFile(t, b)
		apply(t, en, got)
		if want := strings.TrimPrefix(labels.Subject, "sub-"); mustText(t, got, tPatientID) != want {
			t.Errorf("%q: hashed %q, the subject's code %q", id, mustText(t, got, tPatientID), want)
		}
	}
}

// TestASubjectsLabelAndShiftAgree guards that the date shift is keyed by
// exactly what the subject label is: files the default naming rule puts
// under one subject (one Patient ID, its padding and leading spaces
// aside, whatever the Issuer of Patient ID) get one shift, and a file of
// another subject another label.
func TestASubjectsLabelAndShiftAgree(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("dates", vrSel("DA"), aShift))
	hasher, err := naming.NewHasher(testKeys.Hash)
	if err != nil {
		t.Fatal(err)
	}
	shifts := map[string]map[string]bool{}
	for _, f := range []struct{ id, issuer string }{
		{"PAT001", ""}, {" PAT001  ", "HOSPITAL A"}, {"PAT001", "HOSPITAL B"}, {"PAT002", "HOSPITAL A"},
	} {
		els := []el{
			{tSOPClass, "UI", ctImageStorage}, {tSOPInstance, "UI", "1.2.3.4"},
			{tStudyUID, "UI", "1.2.3"}, {0x0020000E, "UI", "1.2.3.5"},
			{tStudyDate, "DA", "20200610"}, {tPatientID, "LO", f.id},
		}
		if f.issuer != "" {
			els = append(els, el{0x00100021, "LO", f.issuer})
		}
		b := writeFile(t, build(t, els...))
		h, err := header.Read(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		labels, err := naming.Default().Labels(h, hasher)
		if err != nil {
			t.Fatal(err)
		}
		got := readFile(t, b)
		apply(t, en, got)
		if shifts[labels.Subject] == nil {
			shifts[labels.Subject] = map[string]bool{}
		}
		shifts[labels.Subject][mustText(t, got, tStudyDate)] = true
	}
	if len(shifts) != 2 {
		t.Fatalf("%d subjects, want 2", len(shifts))
	}
	for subject, dates := range shifts {
		if len(dates) != 1 {
			t.Errorf("subject %s has %d shifts", subject, len(dates))
		}
	}
}

// iso2022Item is an item, in Implicit VR Little Endian, whose Protocol
// Name (0018,1030) is 山田太郎 written in ISO 2022 IR 87 (JIS X 0208 after
// ESC $ B, back to ASCII after ESC ( B, PS3.5 6.1.2.5.3).
var iso2022Item = append([]byte{0xFE, 0xFF, 0x00, 0xE0, 0x16, 0, 0, 0, 0x18, 0x00, 0x30, 0x10, 0x0E, 0, 0, 0},
	"\x1b$B;3EDB@O:\x1b(B"...)

// TestUnknownSequenceTextIsDecoded guards text inside a sequence held as
// UN bytes, which the DICOM library leaves as the file wrote it: in a file
// of ISO 2022 IR 87 whose patient is 山田太郎, a kept private sequence
// whose Protocol Name says the name in that set's 7-bit bytes, which pass
// for ASCII, is decoded when the engine is told the file's character set,
// so clean finds the name and removes it; not told, the file is refused,
// never kept with bytes no one read.
func TestUnknownSequenceTextIsDecoded(t *testing.T) {
	file := func() *dataset.Dataset {
		return readFile(t, writeFile(t, build(t,
			el{tagSpecificCharacterSet, "CS", `\ISO 2022 IR 87`},
			el{tSOPClass, "UI", ctImageStorage}, el{tSOPInstance, "UI", "1.2.3.4"},
			el{tPatientName, "PN", []byte("Yamada^Tarou=\x1b$B;3ED\x1b(B^\x1b$BB@O:\x1b(B")},
			el{0x00290010, "LO", "VENDOR"}, el{0x00291010, "UN", iso2022Item})))
	}
	en := newTestEngine(t, testKeys, Options{},
		rule("vendor", privSel("VENDOR", "0029", "xx"), aKeep), rule("protocol", kw("ProtocolName"), aClean))
	ds := file()
	var f *Failure
	if _, err := en.Apply(context.Background(), ds, Reception{}); !errors.As(err, &f) || f.Cause != CauseBadValue {
		t.Errorf("not told the character set: %v", err)
	}
	cs := `\ISO 2022 IR 87`
	ds = file()
	if _, err := en.Apply(context.Background(), ds, Reception{CharacterSet: &cs}); err != nil {
		t.Fatal(err)
	}
	got, _ := text(item(t, ds, 0x00291010, 0), 0x00181030)
	if strings.Contains(got, "山田") || strings.Contains(got, "太郎") || strings.ContainsRune(got, 0x1b) {
		t.Errorf("the Protocol Name inside the sequence: %q", got)
	}
	if out := writeFile(t, ds); bytes.Contains(out, []byte(";3ED")) || bytes.Contains(out, []byte("B@O:")) {
		t.Error("the written file holds the name's ISO 2022 bytes")
	}
}

// TestLookupsPerFileAreBounded guards the bound of a file's lookups: a
// file whose values need more than MaxLookups codes is refused as too
// large before any table is asked, its data set left as it was; one at the
// bound is assigned.
func TestLookupsPerFileAreBounded(t *testing.T) {
	file := func(n int) *dataset.Dataset {
		items := make([]*dataset.Dataset, n)
		for i := range items {
			items[i] = build(t, el{tAccession, "SH", fmt.Sprintf("ACC%06d", i)})
		}
		return build(t, el{tPatientID, "LO", "PAT001"}, el{0x00081115, "SQ", items})
	}
	lk := &countingLookups{mapLookups: mapLookups{codes: map[string]string{}}}
	en := newTestEngine(t, testKeys, Options{Lookups: lk}, rule("accession", kw("AccessionNumber"), aLookup("acc", true)))
	ds := file(MaxLookups + 1)
	before := writeFile(t, ds)
	var f *Failure
	if _, err := en.Apply(context.Background(), ds, Reception{}); !errors.As(err, &f) || f.Cause != CauseTooLarge {
		t.Fatalf("%d lookups: %v", MaxLookups+1, err)
	}
	if lk.calls != 0 || !bytes.Equal(before, writeFile(t, ds)) {
		t.Errorf("a refused file asked the tables %d times, or changed", lk.calls)
	}
	apply(t, en, file(MaxLookups))
	if lk.calls != 1 || lk.assigned != MaxLookups {
		t.Errorf("at the bound: %d calls for %d codes", lk.calls, lk.assigned)
	}
}
