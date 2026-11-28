package leaktest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/tag"
)

// corpusDir holds the public DICOM test files the seeded files are made
// from (pydicom's test files, copied unchanged).
const corpusDir = "../receive/testdata"

// seedFile is one synthetic file: a public file whose identifying values are
// replaced by unmistakable ones. Every value in values must reach no surface.
type seedFile struct {
	// name says what the file is for, and names it on disk.
	name string
	// base is the public file the seed is made from.
	base string
	// n makes every value of the file its own.
	n int
	// seriesDescription is written to Series Description, which the
	// profile keeps: a value there that holds an original is what the check
	// after finds again.
	seriesDescription func(s seedFile) string
	// values are the original values the file carries, each searched for on
	// every surface (the parts of a name are searched as well).
	values []string
	// path is where the file was written.
	path string
}

// seedValues returns the original values of the file n: a person's name, a
// Patient ID, two other Patient IDs, an Accession Number, the name of a
// staff member in a free-text element the profile cleans, a second name in a
// private element, a birth date and a study date, and the three UIDs the
// file is placed by.
func seedValues(n int) (name, id string, other [2]string, accession, staff, private, birth, study string, uids [3]string) {
	name = fmt.Sprintf("Zxqvnt%d^Kwalbrix%d", n, n)
	id = fmt.Sprintf("SEEDID-7K93X%d", n)
	other = [2]string{fmt.Sprintf("OPID-W3V9Q%d", n), fmt.Sprintf("OPID-R8M2T%d", n)}
	accession = fmt.Sprintf("ACC-QJ48Z%d", n)
	staff = fmt.Sprintf("Ozmanthe%d Quillfeather%d", n, n)
	private = fmt.Sprintf("Qorvanth%d^Belisar%d", n, n)
	birth = fmt.Sprintf("1937052%d", n)
	study = fmt.Sprintf("2016022%d", n)
	root := fmt.Sprintf("1.2.826.0.1.3680043.8.498.77%d", n)
	uids = [3]string{root + ".1", root + ".2", root + ".3"}
	return
}

// privateCreator and the element of the private value: a private group's
// creator is (gggg,0010), and its first element is (gggg,1001).
const privateCreator = "SEEDCREATOR"

// staffList is the text list of staff names the profile cleans out of free
// text, and holds a file back for when one is left in a kept element.
func staffList() []string {
	var out []string
	for n := 1; n <= 9; n++ {
		_, _, _, _, staff, _, _, _, _ := seedValues(n)
		out = append(out, staff)
	}
	return out
}

// seeds returns the five files of the test, not yet written:
//   - deid-dimse: for the receiver that de-identifies, over DIMSE
//   - deid-stow: for the receiver that de-identifies, over STOW-RS
//   - raw-dimse: for the receiver that archives as received, over DIMSE
//   - held-residual: its Patient ID is also typed into Series Description,
//     which the profile keeps, so the check after finds a removed value
//   - held-list-term: a staff name is typed into Series Description, which
//     the profile keeps, so the check after finds a term of the text list
func seeds() []seedFile {
	benign := func(seedFile) string { return "Head routine" }
	mk := func(name, base string, n int, sd func(seedFile) string) seedFile {
		nm, id, other, acc, staff, priv, birth, study, uids := seedValues(n)
		vals := []string{nm, id, other[0], other[1], acc, staff, priv, birth, study, uids[0], uids[1], uids[2]}
		return seedFile{name: name, base: base, n: n, seriesDescription: sd, values: vals}
	}
	return []seedFile{
		mk("deid-dimse", "ct5n-2062.dcm", 1, benign),
		mk("deid-stow", "MR_small.dcm", 2, benign),
		mk("raw-dimse", "ct2n-6293.dcm", 3, benign),
		mk("held-residual", "ct5n-2392.dcm", 4, func(s seedFile) string {
			_, id, _, _, _, _, _, _, _ := seedValues(s.n)
			return "Follow-up for " + id
		}),
		mk("held-list-term", "ct5n-2693.dcm", 5, func(s seedFile) string {
			_, _, _, _, staff, _, _, _, _ := seedValues(s.n)
			return "Reviewed by " + staff
		}),
	}
}

// even pads a value to an even length: a UID with a zero, the rest with a
// space (PS3.5 6.2).
func even(vr, v string) []byte {
	b := []byte(v)
	if len(b)%2 == 1 {
		if vr == "UI" {
			b = append(b, 0)
		} else {
			b = append(b, ' ')
		}
	}
	return b
}

// writeSeed makes the file in dir with the DICOM library: the public file
// read whole, its identifying elements replaced, written again in its own
// transfer syntax. It sets s.path.
func writeSeed(t *testing.T, dir string, s *seedFile) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusDir, s.base)) // #nosec G304 -- a test's own file names.
	if err != nil {
		t.Fatal(err)
	}
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(raw)), filereader.ReadOptions{})
	if err != nil {
		t.Fatalf("%s: %v", s.base, err)
	}
	ds := df.GetDataset()
	name, id, other, accession, staff, private, birth, study, uids := seedValues(s.n)
	set := func(g, e uint16, vr, v string) {
		ds.Remove(tag.New(g, e))
		if err := ds.Add(dataelem.NewDataElement(tag.New(g, e), dataelem.VR(vr), even(vr, v))); err != nil {
			t.Fatalf("%s: element (%04x,%04x): %v", s.name, g, e, err)
		}
	}
	set(0x0010, 0x0010, "PN", name)
	set(0x0010, 0x0020, "LO", id)
	set(0x0010, 0x1000, "LO", other[0]+`\`+other[1])
	set(0x0008, 0x0050, "SH", accession)
	set(0x0008, 0x103E, "LO", s.seriesDescription(*s))
	set(0x0020, 0x4000, "LT", "Scanned by "+staff+" on the night shift")
	set(0x0009, 0x0010, "LO", privateCreator)
	set(0x0009, 0x1001, "LO", private)
	set(0x0010, 0x0030, "DA", birth)
	set(0x0008, 0x0020, "DA", study)
	set(0x0020, 0x000D, "UI", uids[0])
	set(0x0020, 0x000E, "UI", uids[1])
	set(0x0008, 0x0018, "UI", uids[2])

	ts := df.FileMetaInfo.TransferSyntaxUID
	s.path = filepath.Join(dir, s.name+".dcm")
	f, err := os.Create(s.path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	fw := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(f))
	fw.SetFileMetaInfo(&filewriter.FileMetaInfo{
		MediaStorageSOPClassUID:    uidOf(ds, 0x0008, 0x0016),
		MediaStorageSOPInstanceUID: uids[2],
		TransferSyntaxUID:          ts,
	})
	fw.SetValueSource(bytes.NewReader(raw), ts != "1.2.840.10008.1.2.2")
	if err := fw.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		t.Fatal(err)
	}
	if err := fw.Write(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// What was written holds every value: a seed the library did not write
	// would make the search prove nothing.
	back, err := os.ReadFile(s.path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range s.values {
		if !bytes.Contains(back, []byte(v)) {
			t.Fatalf("%s: the written file does not hold %q", s.name, v)
		}
	}
}

// uidOf returns a top-level UID of a data set, its padding removed.
func uidOf(ds *dataset.Dataset, g, e uint16) string {
	el, ok := ds.Get(tag.New(g, e))
	if !ok {
		return ""
	}
	b, ok := el.Bytes()
	if !ok {
		return ""
	}
	return strings.TrimRight(string(b), "\x00 ")
}

// variants returns the forms of an original value a surface might show it
// in, all lower case (the search lowers the text): the value, a person's
// name with the carets turned to spaces, to commas and to nothing, each part
// of a name or of a list apart, and a date as YYYYMMDD and YYYY-MM-DD.
func variants(v string) []string {
	set := map[string]bool{}
	var spelled []string
	add := func(s string) {
		if len(s) >= 6 {
			set[strings.ToLower(s)] = true
			spelled = append(spelled, s)
		}
	}
	add(v)
	add(strings.ReplaceAll(v, "^", " "))
	add(strings.ReplaceAll(v, "^", ", "))
	add(strings.ReplaceAll(v, "^", ""))
	add(strings.ReplaceAll(v, "^", "_"))
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == '^' || r == ' ' || r == '\\' }) {
		add(part)
	}
	if len(v) == 8 && strings.Trim(v, "0123456789") == "" {
		set[v[:4]+"-"+v[4:6]+"-"+v[6:]] = true
		set[v[:4]+"/"+v[4:6]+"/"+v[6:]] = true
		set[v[6:]+"."+v[4:6]+"."+v[:4]] = true
		set[v] = true
	}
	// A value in a bytes field of a JSON answer is base64 of its bytes,
	// which depend on where the value starts in them: three spellings, the
	// characters its neighbors share left out.
	for _, form := range spelled {
		if len(form) < 8 {
			continue
		}
		for off := range 3 {
			enc := base64.RawStdEncoding.EncodeToString(append(make([]byte, off), form...))
			from, to := []int{0, 2, 3}[off], 8*(off+len(form))/6
			if to > from+4 {
				set[strings.ToLower(enc[from:to])] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// needle is one variant of one seeded value.
type needle struct {
	// value is the original value the variant is of.
	value string
	// form is the text searched for, lower case.
	form string
}

// needlesOf returns every variant of every value of every seed.
func needlesOf(all []seedFile) []needle {
	seen := map[string]bool{}
	var out []needle
	for _, s := range all {
		for _, v := range s.values {
			for _, f := range variants(v) {
				if seen[f] {
					continue
				}
				seen[f] = true
				out = append(out, needle{value: v, form: f})
			}
		}
	}
	return out
}
