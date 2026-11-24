package engine_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// basicEngine is the engine of PS3.15 Annex E's basic profile, with the
// rules given after it, under fixed test keys.
func basicEngine(t *testing.T, rules ...*v1.ProfileRule) *engine.Engine {
	t.Helper()
	exp, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, Rules: rules}, profile.Shipped())
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

// elem is one element of a synthetic data set.
type elem struct {
	tag uint32
	vr  string
	v   string
}

// dataSet builds a data set of the elements.
func dataSet(t *testing.T, es ...elem) *dataset.Dataset {
	t.Helper()
	ds := dataset.NewDataset()
	for _, e := range es {
		if err := ds.Add(dataelem.NewDataElement(tag.Tag(e.tag), dataelem.VR(e.vr), []byte(e.v))); err != nil {
			t.Fatal(err)
		}
	}
	return ds
}

// part10 writes ds as an Explicit VR Little Endian Part 10 file.
func part10(t *testing.T, ds *dataset.Dataset) []byte {
	t.Helper()
	return part10As(t, ds, "1.2.840.10008.1.2.1")
}

// part10As writes ds as a Part 10 file in the transfer syntax ts.
func part10As(t *testing.T, ds *dataset.Dataset, ts string) []byte {
	t.Helper()
	sop, inst := "1.2.840.10008.5.1.4.1.1.2", "2.25.1"
	if e, ok := ds.Get(tag.New(0x0008, 0x0018)); ok {
		if b, ok := e.Bytes(); ok {
			inst = string(bytes.TrimRight(b, " \x00"))
		}
	}
	path := filepath.Join(t.TempDir(), "out.dcm")
	f, err := os.Create(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	w := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(f))
	w.SetFileMetaInfo(&filewriter.FileMetaInfo{MediaStorageSOPClassUID: sop, MediaStorageSOPInstanceUID: inst, TransferSyntaxUID: ts})
	if err := w.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// deidentify runs en over the file b and returns the result and its
// needles.
func deidentify(t *testing.T, en *engine.Engine, b []byte) ([]byte, engine.Needles) {
	t.Helper()
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(b)), filereader.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ds := df.GetDataset()
	rec, err := en.Apply(context.Background(), ds, engine.Reception{})
	if err != nil {
		t.Fatal(err)
	}
	return part10(t, ds), rec.Needles
}

// checkAfter runs the check after de-identification over b.
func checkAfter(b []byte, needles engine.Needles) error {
	_, err := check.New(check.Options{}).Check(context.Background(), bytes.NewReader(b), int64(len(b)), needles, nil)
	return err
}

// hasValue reports whether needles hold value as a needle of kind.
func hasValue(needles engine.Needles, kind engine.NeedleKind, value string) bool {
	for _, n := range needles {
		if n.Kind == kind && n.Value == value {
			return true
		}
	}
	return false
}

// valueIsNeedle reports whether any needle holds value, of any kind.
func valueIsNeedle(needles engine.Needles, value string) bool {
	for _, n := range needles {
		if n.Value == value {
			return true
		}
	}
	return false
}

// TestARemovedWeightIsNoNeedle guards the false hold of a CT whose
// Patient's Weight, which the basic profile removes, is "0.000000" and
// whose Gantry/Detector Tilt, which it keeps, is the same decimal (as in
// pydicom's CT_small.dcm): a value of a numeric VR identifies no one, so
// it is removed but is no needle, and the check after passes the file.
// With the public corpus named, CT_small.dcm itself passes too.
func TestARemovedWeightIsNoNeedle(t *testing.T) {
	en := basicEngine(t)
	b := part10(t, dataSet(t,
		elem{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, elem{0x00080018, "UI", "2.25.4321"},
		elem{0x00100020, "LO", "PID12345"}, elem{0x00101030, "DS", "0.000000"}, elem{0x00181120, "DS", "0.000000"},
	))
	out, needles := deidentify(t, en, b)
	if valueIsNeedle(needles, "0.000000") {
		t.Error("a removed weight is a needle")
	}
	if !bytes.Contains(out, []byte("0.000000")) {
		t.Fatal("the result does not keep the gantry tilt the test needs")
	}
	if err := checkAfter(out, needles); err != nil {
		t.Errorf("the check after holds the file: %v", err)
	}
	dir := os.Getenv("NOPACS_PYDICOM_CORPUS")
	if dir == "" {
		return
	}
	ct, err := os.ReadFile(filepath.Join(dir, "CT_small.dcm")) // #nosec G304 G703 -- a public corpus the test is pointed at.
	if err != nil {
		t.Fatal(err)
	}
	out, needles = deidentify(t, en, ct)
	if err := checkAfter(out, needles); err != nil {
		t.Errorf("CT_small.dcm: the check after holds the file: %v", err)
	}
}

// TestAScoutSeriesPassesTheCheck guards the false hold of the receiver's
// scout CT files, whose Series Description "Scout", which the basic
// profile removes, is found again in Scan Options "SCOUT MODE", which it
// keeps: a description is removed because it might hold an identifier,
// not because it is one, so it is no needle and the files pass.
func TestAScoutSeriesPassesTheCheck(t *testing.T) {
	en := basicEngine(t)
	for _, name := range []string{"ct2n-6293.dcm", "ct2n-6924.dcm"} {
		b, err := os.ReadFile(filepath.Join("../../receive/testdata", name)) // #nosec G304 -- the receiver's public test file.
		if err != nil {
			t.Fatal(err)
		}
		out, needles := deidentify(t, en, b)
		if valueIsNeedle(needles, "Scout") {
			t.Errorf("%s: a removed series description is a needle", name)
		}
		if !bytes.Contains(out, []byte("SCOUT MODE")) {
			t.Fatalf("%s: the result does not keep the scan options the test needs", name)
		}
		if err := checkAfter(out, needles); err != nil {
			t.Errorf("%s: the check after holds the file: %v", name, err)
		}
	}
}

// TestANameInADescriptionIsFoundThroughItsName guards what is still
// caught once a description is no needle: a Study Description holding the
// patient's name is no needle itself, but the name is, so the name is
// found wherever it is left. Under the basic profile the description is
// removed and the file passes; with a rule keeping it, the check after
// finds the name in it and holds the file.
func TestANameInADescriptionIsFoundThroughItsName(t *testing.T) {
	file := part10(t, dataSet(t,
		elem{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, elem{0x00080018, "UI", "2.25.4322"},
		elem{0x00081030, "LO", "Head CT for Jane Doe"},
		elem{0x00100010, "PN", "Doe^Jane"}, elem{0x00100020, "LO", "PID12345"},
	))
	out, needles := deidentify(t, basicEngine(t), file)
	if valueIsNeedle(needles, "Head CT for Jane Doe") {
		t.Error("a removed description is a needle")
	}
	if !hasValue(needles, engine.NeedleName, "Doe^Jane") {
		t.Fatal("the patient's name is not a needle")
	}
	if err := checkAfter(out, needles); err != nil {
		t.Errorf("the check after holds a file whose description was removed: %v", err)
	}
	keep := &v1.ProfileRule{Name: "keep-description",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyDescription"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}}
	out, needles = deidentify(t, basicEngine(t, keep), file)
	var res *check.Residual
	if err := checkAfter(out, needles); !errors.As(err, &res) || res.First.Kind != check.KindName || res.First.Path != "00081030" {
		t.Errorf("the name kept in a description: %v, want a name found at 00081030", err)
	}
}

// TestEachKindOfNeedleIsMade guards the needles the basic profile makes:
// a name, the patient's identifier, an accession number, an institution's
// name and address, a date and a UID are each a needle of their kind; a
// weight, an age, a sex (CS), a description and a protocol's name, which
// it removes too, are not.
func TestEachKindOfNeedleIsMade(t *testing.T) {
	file := part10(t, dataSet(t,
		elem{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, elem{0x00080018, "UI", "1.2.3.4.5.6.7"},
		elem{0x00080020, "DA", "20240315"}, elem{0x00080050, "SH", "ACC12345"},
		elem{0x00080080, "LO", "General Hospital"}, elem{0x00080081, "ST", "1 Main Street, Springfield"},
		elem{0x00081030, "LO", "CT HEAD"}, elem{0x00100010, "PN", "Doe^Jane"}, elem{0x00100020, "LO", "PID12345"},
		elem{0x00100040, "CS", "FEMALE"}, elem{0x00101010, "AS", "045Y"}, elem{0x00101030, "DS", "72.5"},
		elem{0x00181030, "LO", "ROUTINE BRAIN"},
	))
	_, needles := deidentify(t, basicEngine(t), file)
	for _, want := range []struct {
		kind  engine.NeedleKind
		value string
	}{
		{engine.NeedleName, "Doe^Jane"}, {engine.NeedleText, "PID12345"}, {engine.NeedleText, "ACC12345"},
		{engine.NeedleText, "General Hospital"}, {engine.NeedleText, "1 Main Street, Springfield"},
		{engine.NeedleDate, "20240315"}, {engine.NeedleUID, "1.2.3.4.5.6.7"},
	} {
		if !hasValue(needles, want.kind, want.value) {
			t.Errorf("%q is not a needle of kind %d", want.value, want.kind)
		}
	}
	for _, not := range []string{"CT HEAD", "FEMALE", "045Y", "72.5", "ROUTINE BRAIN"} {
		if valueIsNeedle(needles, not) {
			t.Errorf("%q is a needle", not)
		}
	}
}
