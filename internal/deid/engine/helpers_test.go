package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// testKeys are a tenant's keys for the tests; otherKeys another tenant's.
var (
	testKeys  = Keys{UID: key('u'), Date: key('d'), Hash: key('h')}
	otherKeys = Keys{UID: key('U'), Date: key('D'), Hash: key('H')}
)

// key is a 32-byte key of one repeated byte.
func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// Selectors and actions of the profile language, for writing test
// profiles.
func kw(k string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}
}
func vrSel(v string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Vr{Vr: v}}
}
func pathSel(p string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Path{Path: p}}
}
func privSel(creator, group, elem string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_PrivateElement{PrivateElement: &v1.PrivateElement{Creator: creator, Group: group, Element: elem}}}
}
func itemSel(scheme, value, valueType string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_ContentItem{ContentItem: &v1.ContentItemSelector{Scheme: scheme, Value: value, ValueType: valueType}}}
}

var (
	aKeep       = &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}
	aRemove     = &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	aEmpty      = &v1.ProfileAction{Kind: &v1.ProfileAction_Empty{Empty: &v1.EmptyAction{}}}
	aHash       = &v1.ProfileAction{Kind: &v1.ProfileAction_Hash{Hash: &v1.HashAction{}}}
	aUID        = &v1.ProfileAction{Kind: &v1.ProfileAction_Uid{Uid: &v1.UidAction{}}}
	aShift      = &v1.ProfileAction{Kind: &v1.ProfileAction_Shift{Shift: &v1.ShiftAction{}}}
	aGeneralize = &v1.ProfileAction{Kind: &v1.ProfileAction_Generalize{Generalize: &v1.GeneralizeAction{}}}
	aClean      = &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{}}}
	aQuarantine = &v1.ProfileAction{Kind: &v1.ProfileAction_Quarantine{Quarantine: &v1.QuarantineAction{}}}
	aDummy      = &v1.ProfileAction{Kind: &v1.ProfileAction_Dummy{Dummy: &v1.DummyAction{}}}
)

func aRemoveUnless(f v1.RequiredFallback) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_RemoveUnlessRequired{RemoveUnlessRequired: &v1.RemoveUnlessRequiredAction{Fallback: f}}}
}

func aReplace(c string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Replace{Replace: &v1.ReplaceAction{Value: &v1.ReplaceAction_Constant{Constant: c}}}}
}
func aTemplate(t string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Replace{Replace: &v1.ReplaceAction{Value: &v1.ReplaceAction_Template{Template: t}}}}
}
func aSet(c string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Set{Set: &v1.SetAction{Value: &v1.SetAction_Constant{Constant: c}}}}
}
func aLookup(table string, assign bool) *v1.ProfileAction {
	m := v1.LookupMissing_LOOKUP_MISSING_QUARANTINE
	if assign {
		m = v1.LookupMissing_LOOKUP_MISSING_ASSIGN
	}
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Lookup{Lookup: &v1.LookupAction{Table: table, OnMissing: m}}}
}

// rule makes a rule.
func rule(name string, sel *v1.ProfileSelector, act *v1.ProfileAction, when ...*v1.ProfileCondition) *v1.ProfileRule {
	return &v1.ProfileRule{Name: name, Selector: sel, Action: act, When: when}
}

// newTestEngine expands a document of rules and makes its Engine under
// keys, failing the test when either is refused.
func newTestEngine(t testing.TB, keys Keys, opts Options, rules ...*v1.ProfileRule) *Engine {
	t.Helper()
	return newDocEngine(t, keys, opts, &v1.ProfileDocument{Rules: rules})
}

// newDocEngine returns an Engine over a document, expanded over the
// shipped bases.
func newDocEngine(t testing.TB, keys Keys, opts Options, doc *v1.ProfileDocument) *Engine {
	t.Helper()
	e, problems := profile.Expand(doc, profile.Shipped())
	if problems != nil {
		t.Fatalf("expanding the test profile: %v", problems)
	}
	en, err := New(e, keys, opts)
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// el is an element of a test data set.
type el struct {
	tag   uint32
	vr    string
	value any
}

// build makes a data set of elements; a value of []*dataset.Dataset is a
// sequence of those items.
func build(t testing.TB, els ...el) *dataset.Dataset {
	t.Helper()
	ds := dataset.NewDataset()
	for _, e := range els {
		if items, ok := e.value.([]*dataset.Dataset); ok {
			seq := sequence.New()
			for _, it := range items {
				if err := seq.Append(it); err != nil {
					t.Fatal(err)
				}
			}
			if err := ds.AddSequence(tag.Tag(e.tag), seq); err != nil {
				t.Fatal(err)
			}
			continue
		}
		v := e.value
		if s, ok := v.(string); ok {
			v = []byte(s)
		}
		if err := ds.Add(dataelem.NewDataElement(tag.Tag(e.tag), dataelem.VR(e.vr), v)); err != nil {
			t.Fatal(err)
		}
	}
	return ds
}

// text returns an element's value as text and whether it is present.
func text(ds *dataset.Dataset, t uint32) (string, bool) {
	v, ok := elementText(ds, tag.Tag(t))
	if !ok {
		return "", ds.Contains(tag.Tag(t))
	}
	return strings.TrimRight(v, " \x00"), true
}

// item returns item i of the sequence t of ds.
func item(t testing.TB, ds *dataset.Dataset, seqTag uint32, i int) *dataset.Dataset {
	t.Helper()
	it, err := ds.GetSequenceDataset(tag.Tag(seqTag), i)
	if err != nil {
		t.Fatalf("item %d of %08X: %v", i, seqTag, err)
	}
	return it
}

// apply runs en over ds with no reception, failing the test on an error.
func apply(t testing.TB, en *Engine, ds *dataset.Dataset) *Record {
	t.Helper()
	rec, err := en.Apply(context.Background(), ds, Reception{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return rec
}

// writeFile writes ds as a Part 10 file, Explicit VR Little Endian unless
// its pixel data is encapsulated, and returns its bytes.
func writeFile(t testing.TB, ds *dataset.Dataset) []byte {
	t.Helper()
	return writeFileAs(t, ds, filewriter.StorageTransferSyntax(ds))
}

// implicitVR is Implicit VR Little Endian (PS3.5 A.1), in which no
// element carries its VR: a reader gives a private element none.
const implicitVR = "1.2.840.10008.1.2"

// writeFileAs writes ds as a Part 10 file in the transfer syntax ts and
// returns its bytes.
func writeFileAs(t testing.TB, ds *dataset.Dataset, ts string) []byte {
	t.Helper()
	sop, _ := text(ds, 0x00080016)
	inst, _ := text(ds, 0x00080018)
	path := filepath.Join(t.TempDir(), "out.dcm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(f))
	w.SetFileMetaInfo(&filewriter.FileMetaInfo{
		MediaStorageSOPClassUID:    sop,
		MediaStorageSOPInstanceUID: inst,
		TransferSyntaxUID:          ts,
	})
	if err := w.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// readFile reads a Part 10 file's data set with the DICOM library.
func readFile(t testing.TB, b []byte) *dataset.Dataset {
	t.Helper()
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(b)), filereader.ReadOptions{})
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	return df.GetDataset()
}

// readPath reads a file from disk with the DICOM library, whole.
func readPath(t testing.TB, path string) (*dataset.Dataset, error) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a corpus file the test names.
	if err != nil {
		return nil, err
	}
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(b)), filereader.ReadOptions{})
	if err != nil {
		return nil, err
	}
	return df.GetDataset(), nil
}

// allText returns every text value of ds at any depth, each element's
// bytes read as text, private and unknown elements included.
func allText(ds *dataset.Dataset) []string {
	var out []string
	for _, t := range ds.Tags() {
		elem, ok := ds.Get(t)
		if !ok {
			continue
		}
		if seq, isSeq := elem.GetValue().(*sequence.Sequence); isSeq {
			for _, it := range seq.Items() {
				if inner, ok := it.(*dataset.Dataset); ok {
					out = append(out, allText(inner)...)
				}
			}
			continue
		}
		if h, _ := profile.Pipeline(uint32(t)); h == profile.HandledKept {
			continue
		}
		if b, ok := elem.Bytes(); ok {
			out = append(out, string(b))
		}
	}
	return out
}

// receiveTestdata is the directory of public test files the receiver's
// tests use, which these tests read too.
const receiveTestdata = "../../receive/testdata"
