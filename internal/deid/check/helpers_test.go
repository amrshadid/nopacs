package check

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// el is an element of a test data set: a value of []*dataset.Dataset is a
// sequence of those items, a string or []byte the value's bytes.
type el struct {
	tag   uint32
	vr    string
	value any
}

// build makes a data set of elements.
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

// The SOP Class and Instance of every test file: a secondary capture
// (PS3.4 B.5) and a UID under the example root of PS3.5 B.2.
const (
	testSOPClass    = "1.2.840.10008.5.1.4.1.1.7"
	testSOPInstance = "2.25.1111"
)

// base returns the elements every test file holds, then more.
func base(more ...el) []el {
	return append([]el{
		{0x00080016, "UI", testSOPClass},
		{0x00080018, "UI", testSOPInstance},
		{0x00080060, "CS", "OT"},
		{0x00100010, "PN", "SUBJECT"},
		{0x00100020, "LO", "HASHCODE"},
	}, more...)
}

// writeDS writes ds as an Explicit VR Little Endian Part 10 file with meta
// as its file meta information beside the SOP UIDs, and returns its path.
func writeDS(t testing.TB, ds *dataset.Dataset, meta filewriter.FileMetaInfo) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.dcm")
	f, err := os.Create(path) //nolint:gosec // G304: the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	meta.MediaStorageSOPClassUID = testSOPClass
	if meta.MediaStorageSOPInstanceUID == "" {
		meta.MediaStorageSOPInstanceUID = testSOPInstance
	}
	if meta.TransferSyntaxUID == "" {
		meta.TransferSyntaxUID = "1.2.840.10008.1.2.1"
	}
	w := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(f))
	w.SetFileMetaInfo(&meta)
	if err := w.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeFile writes a file of els beside the base elements.
func writeFile(t testing.TB, els ...el) string {
	t.Helper()
	return writeDS(t, build(t, base(els...)...), filewriter.FileMetaInfo{})
}

// needle makes one needle.
func needle(k engine.NeedleKind, v string) engine.Needle {
	return engine.Needle{Kind: k, Value: v, Path: "00000000"}
}

// checkPath opens the file at path and checks it, as the pipeline does
// with its temporary file.
func (c *Checker) checkPath(t testing.TB, ctx context.Context, path string, needles engine.Needles, characterSet *string) (Report, error) {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: the test's own temporary file.
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return c.Check(ctx, f, st.Size(), needles, characterSet)
}

// run checks path for needles with the default options and no source
// character set.
func run(t testing.TB, path string, needles ...engine.Needle) (Report, error) {
	t.Helper()
	return New(Options{}).checkPath(t, context.Background(), path, needles, nil)
}

// mustFind checks path for needles and fails unless the check finds a
// value of kind in want, returning the report.
func mustFind(t testing.TB, path string, kind Kind, want string, needles ...engine.Needle) Report {
	t.Helper()
	rep, err := run(t, path, needles...)
	var res *Residual
	if !errors.As(err, &res) {
		t.Fatalf("want a residual of %s at %s, got %v (report %+v)", kind, want, err, rep)
	}
	for _, f := range rep.Findings {
		if f.Kind == kind && f.Path == want {
			if res.Reason() != ReasonResidual {
				t.Errorf("reason %q", res.Reason())
			}
			return rep
		}
	}
	t.Fatalf("no finding of %s at %s: %+v", kind, want, rep.Findings)
	return rep
}

// mustPass checks path for needles and fails unless it passes.
func mustPass(t testing.TB, path string, needles ...engine.Needle) Report {
	t.Helper()
	rep, err := run(t, path, needles...)
	if err != nil {
		t.Fatalf("want a pass, got %v: %+v", err, rep.Findings)
	}
	return rep
}

// patch writes b over the file at path from offset at.
func patch(t testing.TB, path string, at int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(b, at); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
