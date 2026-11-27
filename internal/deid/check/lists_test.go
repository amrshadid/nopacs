package check

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/filewriter"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// listTerm is a term no other text of the tests holds.
const listTerm = "Zebulonquist"

// listNeedle returns the needle of lists holding listTerm, "Ann Lee" and
// a name of a script written without spaces.
func listNeedle(t testing.TB) engine.Needle {
	t.Helper()
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{listTerm, "Ann Lee", "田中"}}})
	if problems != nil {
		t.Fatal(problems)
	}
	return engine.Needle{Kind: engine.NeedleList, Lists: l}
}

// TestListTermFoundAgainHoldsTheFile guards the check of a profile's
// lists: a term in a kept value, in any case, width or Unicode form, or
// with a run of white space for its space, is a residual_value of kind
// list_term at its path, the detail naming neither term nor list; a term
// inside a longer word is not, as clean leaves it; a term of a script
// written without spaces is found inside other text.
func TestListTermFoundAgainHoldsTheFile(t *testing.T) {
	n := listNeedle(t)
	for _, v := range []string{
		"seen by " + listTerm,
		"seen by " + strings.ToUpper(listTerm),
		"seen by ＺＥＢＵＬＯＮＱＵＩＳＴ",
		"seen by ann \t LEE.",
		"田中先生",
	} {
		file := writeFile(t, el{0x00080005, "CS", "ISO_IR 192"}, el{0x00081030, "LO", v})
		rep := mustFind(t, file, KindListTerm, "00081030", n)
		_, err := run(t, file, n)
		var res *Residual
		if !errors.As(err, &res) || strings.Contains(strings.ToLower(res.Detail()+err.Error()+fmt.Sprintf("%+v", rep)), "zebulon") ||
			strings.Contains(res.Detail(), "staff") {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"Ann Leeds review", "x" + listTerm, listTerm + "son", "Ann_Lee2"} {
		mustPass(t, writeFile(t, el{0x00080005, "CS", "ISO_IR 192"}, el{0x00081030, "LO", v}), n)
	}
	// Composed and decomposed: a term written with a combining mark.
	l, _ := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{"Müller"}}})
	mustFind(t, writeFile(t, el{0x00080005, "CS", "ISO_IR 192"}, el{0x00204000, "LT", "by Müller"}), KindListTerm, "00204000", engine.Needle{Kind: engine.NeedleList, Lists: l})
}

// TestListTermFoundInRawBytes guards the raw search for the lists' terms:
// a term in a private OW value, which is never decoded as text, is found
// in the file's raw bytes, in any letter case and composed or decomposed; one
// inside a longer word there is not.
func TestListTermFoundInRawBytes(t *testing.T) {
	n := listNeedle(t)
	c := New(Options{})
	for _, v := range []string{"..." + listTerm + "...", ".." + strings.ToLower(listTerm) + ".."} {
		b := []byte(strings.Repeat(".", 64) + v)
		if len(b)%2 == 1 {
			b = append(b, '.')
		}
		path := writeFile(t, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OW", b})
		rep, err := c.checkPath(t, context.Background(), path, engine.Needles{n}, nil)
		var res *Residual
		if !errors.As(err, &res) || res.First.Kind != KindListTerm || !res.First.Raw {
			t.Errorf("%q in raw bytes: %v %+v", v, err, rep)
		}
	}
	b := []byte(strings.Repeat(".", 64) + "Q" + listTerm + "Q..")
	path := writeFile(t, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OW", b})
	if _, err := c.checkPath(t, context.Background(), path, engine.Needles{n}, nil); err != nil {
		t.Errorf("a term inside a word of raw bytes: %v", err)
	}
}

// TestListTermInALongValueIsFound guards a term at the end of a value of
// unknown kind longer than is decoded whole, read as a stream.
func TestListTermInALongValueIsFound(t *testing.T) {
	b := []byte(strings.Repeat("ab ", 100_000) + listTerm + " ")
	if len(b)%2 == 1 {
		b = append(b, ' ')
	}
	path := writeFile(t, el{0x00090010, "LO", "ACME 1.0"}, el{privateOW, "OB", b})
	_, err := run(t, path, listNeedle(t))
	var res *Residual
	if !errors.As(err, &res) || res.First.Kind != KindListTerm {
		t.Errorf("a term in a long value: %v", err)
	}
}

// TestCleanedFilePassesAndKeptTermHolds guards the engine and the check
// together: a file whose descriptions a list cleans passes the check after
// with the record's needles; the same file with a term left in a value the
// profile keeps is held, the finding at that value's path.
func TestCleanedFilePassesAndKeptTermHolds(t *testing.T) {
	doc := &v1.ProfileDocument{
		TextLists: []*v1.TextList{{Name: "staff", Version: 1, Terms: []string{listTerm, "Ann Lee", "田中"}}},
		Rules: []*v1.ProfileRule{
			{Name: "desc", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyDescription"}},
				Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{Lists: []string{"staff"}}}}},
		}}
	e, problems := profile.Expand(doc, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	keys := engine.Keys{UID: bytes.Repeat([]byte{'u'}, 32), Date: bytes.Repeat([]byte{'d'}, 32), Hash: bytes.Repeat([]byte{'h'}, 32)}
	en, err := engine.New(e, keys, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		kept string
		held bool
	}{
		{"cleaned", "nothing here", false},
		{"kept", "kept " + listTerm, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := writeFile(t, el{0x00081030, "LO", "CT by Ann Lee for " + listTerm}, el{0x00204000, "LT", c.kept})
			raw, err := os.ReadFile(src) //nolint:gosec // G304: the test's own temporary file.
			if err != nil {
				t.Fatal(err)
			}
			df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(raw)), filereader.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ds := df.GetDataset()
			rec, err := en.Apply(context.Background(), ds, engine.Reception{})
			if err != nil {
				t.Fatal(err)
			}
			out := writeDS(t, ds, filewriter.FileMetaInfo{})
			data, err := os.ReadFile(out) //nolint:gosec // G304: the test's own temporary file.
			if err != nil {
				t.Fatal(err)
			}
			rep, err := New(Options{}).CheckWritten(context.Background(), bytes.NewReader(data), int64(len(data)), rec.Needles, rec.Written, nil)
			var res *Residual
			switch {
			case c.held && (!errors.As(err, &res) || res.First.Kind != KindListTerm || res.First.Path != "00204000"):
				t.Errorf("a kept term: %v %+v", err, rep)
			case !c.held && err != nil:
				t.Errorf("a cleaned file: %v %+v", err, rep)
			}
		})
	}
}
