package engine

import (
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"
	"github.com/amrshadid/go-dicom/valuerep"

	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/version"
)

// planMethod plans the record of the de-identification in the data set
// (PS3.15 E.1.1, E.3.6), through the DICOM library's one writer of it
// (anonymize.RecordDeidentificationMethod): Patient Identity Removed
// (0012,0062) YES; De-identification Method Code Sequence (0012,0064)
// with the PS3.16 CID 7050 code of each Annex E base the profile version
// was built on (profile.MethodCodes); and De-identification Method
// (0012,0063), LO of any number of values (PS3.6), holding those codes'
// meanings, then the method the caller names, then this build's version
// and the DICOM library's: what a stored profile version does depends on
// the library's data dictionary, IOD tables and UID registry as well,
// which the engine reads as it runs, so each instance says which it was
// made with. Longitudinal Temporal Information Modified (0028,0303) is
// what the profile's dates option says (PS3.15 E.3.6, Engine.temporal),
// and MODIFIED whenever a date was shifted. A profile built on no Annex E
// base, or whose own rules keep what a base changes or removes
// (profile.KeepsBaseChange), records no code: the codes would claim what
// the file is not. It then writes no code sequence, and (0012,0063) only
// when the caller names a method.
//
// Of the pixels (PS3.15 E.3.1): a file a redact rule filled records the
// Clean Pixel Data Option's code beside the others, alone when there are
// none, and Burned In Annotation (0028,0301) NO, since a person said where
// the text is; a file the detector filled records
// anonymize.AutomaticRedactionNote in (0012,0063), one the detector found
// nothing in detectorFoundNone, and each leaves (0028,0301) as received,
// since a detector can miss. No other file records the code, whatever
// bases the profile names, nor one that holds Float or Double Float Pixel
// Data beside the image filled.
//
// The library writes into a data set of its own here, and each element it
// wrote is planned like every other change, so a file refused later is
// left as it was.
func (r *run) planMethod() error {
	temporal := r.en.temporal
	if r.record.DatesShifted {
		temporal = "MODIFIED"
	}
	var meanings []string
	codes := r.en.codes
	px := r.record.Pixels
	// The code and Burned In Annotation NO claim the file's images are
	// clean: never while an image of it, Float or Double Float Pixel Data
	// beside the one filled, stays as received.
	redacted := px != nil && px.Action == pixelRedact && len(px.Regions) > 0 && !otherImage(r.top)
	detected := px != nil && px.Action == pixelDetect && len(px.Regions) > 0
	foundNone := px != nil && px.Action == pixelDetect && len(px.Regions) == 0
	if redacted {
		codes = withCleanPixelData(codes)
	}
	if len(codes) > 0 {
		scratch := dataset.NewDataset()
		if err := anonymize.RecordDeidentificationMethod(scratch, codes, anonymize.MethodRecord{IdentityRemoved: true}); err != nil {
			return fmt.Errorf("engine: recording the de-identification method: %w", err)
		}
		seq, ok := scratch.Get(tag.Tag(tagDeidMethodCodes))
		if !ok {
			return errors.New("engine: the DICOM library wrote no de-identification method code sequence")
		}
		r.ops = append(r.ops, op{kind: opElement, ds: r.top, tag: tag.Tag(tagDeidMethodCodes), elem: seq})
		r.record.Written.MethodCodes = writtenCodes(seq)
		r.record.Entries = append(r.record.Entries, Entry{Path: formatTag(tagDeidMethodCodes), Tag: tagDeidMethodCodes, VR: "SQ", Action: "set", Rule: -1,
			Reason: "the PS3.16 CID 7050 codes of the PS3.15 Annex E profile and options the profile version is built on (PS3.15 E.1.1)", Changed: true})
		if v, ok := elementText(scratch, tag.Tag(tagDeidMethod)); ok && v != "" {
			meanings = strings.Split(v, `\`)
		}
	}
	r.put(tagIdentityRemoved, "CS", "YES", "the file was de-identified (PS3.15 E.1.1)")
	values := meanings
	switch {
	case detected:
		values = append(values, anonymize.AutomaticRedactionNote)
	case foundNone:
		values = append(values, detectorFoundNone)
	}
	if r.en.method != "" {
		values = append(append(values, r.en.method), buildVersions()...)
	}
	if redacted {
		r.put(tagBurnedInAnnotation, "CS", "NO", "the pixel rule's rectangles, which a person placed over the text, were filled (PS3.15 E.3.1)")
	}
	if len(values) > 0 {
		r.record.Written.Method = strings.Join(values, `\`)
		r.put(tagDeidMethod, "LO", r.record.Written.Method,
			"the profile and options the file was de-identified by, the profile version, and the versions of the software that applied it (PS3.15 E.1.1)")
	}
	if temporal != "" {
		r.put(tagTemporalModified, "CS", temporal, "what the dates option says, or that dates were shifted (PS3.15 E.3.6)")
	}
	return nil
}

// writtenCodes reads back the codes of the De-identification Method Code
// Sequence the DICOM library wrote, as its items hold them, so the record
// says exactly what the file carries (a Coding Scheme Designator the
// library filled in included), not what was asked of the library. An item
// that is not a data set, or a value that is not text, reads as empty: the
// check after then finds the sequence unlike what it holds and searches it.
func writtenCodes(seq *dataelem.DataElement) []anonymize.MethodCode {
	items, _ := seq.GetValue().(*sequence.Sequence)
	if items == nil {
		return nil
	}
	out := make([]anonymize.MethodCode, 0, items.Length())
	for i := range items.Length() {
		var c anonymize.MethodCode
		if it, err := items.Get(i); err == nil {
			if ds, ok := it.(*dataset.Dataset); ok {
				c.Value, _ = elementText(ds, tag.Tag(tagCodeValue))
				c.Scheme, _ = elementText(ds, tag.Tag(tagCodingScheme))
				c.Meaning, _ = elementText(ds, tag.Tag(tagCodeMeaning))
			}
		}
		out = append(out, c)
	}
	return out
}

// tagCodeMeaning is Code Meaning, of an item of a code sequence (PS3.3
// 8.8).
const tagCodeMeaning = 0x00080104

// dicomLibrary is the module path of the DICOM library.
const dicomLibrary = "github.com/amrshadid/go-dicom"

// buildVersions returns this build's version and the DICOM library's, as
// two values of De-identification Method: "NoPACs <version>" and "go-dicom
// <version>", each "unknown" where the build did not record it (a test
// binary records no dependencies).
var buildVersions = sync.OnceValue(func() []string {
	bi, _ := debug.ReadBuildInfo()
	return []string{methodValue("NoPACs", version.Get().Version), methodValue("go-dicom", libraryVersion(bi))}
})

// libraryVersion returns the DICOM library's version in build information,
// the version it is replaced by when it is, and "unknown" when bi is nil
// or does not list it.
func libraryVersion(bi *debug.BuildInfo) string {
	if bi == nil {
		return "unknown"
	}
	for _, d := range bi.Deps {
		if d.Path != dicomLibrary {
			continue
		}
		if d.Replace != nil && d.Replace.Version != "" {
			return d.Replace.Version
		}
		if d.Version != "" {
			return d.Version
		}
	}
	return "unknown"
}

// methodValue writes a name and a version as one LO value (PS3.5 6.2: at
// most 64 characters of the default repertoire, no backslash), the
// version "unknown" when it is not one.
func methodValue(name, v string) string {
	s := name + " " + v
	if len(s) > maxMethodLength || !isASCII(s) || strings.ContainsAny(s, `\`) || valuerep.ValidateValue("LO", s) != nil {
		return name + " unknown"
	}
	return s
}

// detectorFoundNone is what De-identification Method (0012,0063) says of
// an image a detect rule looked at and found no text in: a detector can
// miss, so it is said, and Burned In Annotation is left as received.
const detectorFoundNone = "Burned-in text detector found none, unverified"

// otherImage reports whether ds holds an image no pixel rule fills: Float
// or Double Float Pixel Data at its top level (PS3.3 C.7.6.24).
func otherImage(ds *dataset.Dataset) bool {
	_, f := ds.Get(tag.Tag(tagFloatPixelData))
	_, d := ds.Get(tag.Tag(tagDoubleFloatPixelData))
	return f || d
}

// withCleanPixelData returns codes with the Clean Pixel Data Option's code,
// in the order profile.MethodCodes lists them: after the basic profile's,
// or first when there is none.
func withCleanPixelData(codes []anonymize.MethodCode) []anonymize.MethodCode {
	clean := profile.CleanPixelDataCode()
	out := make([]anonymize.MethodCode, 0, len(codes)+1)
	at := 0
	if len(codes) > 0 && codes[0].Value == anonymize.MethodBasicProfile.Code().Value {
		at = 1
	}
	out = append(append(append(out, codes[:at]...), clean), codes[at:]...)
	return out
}

// MethodRecord is the record of the de-identification every file an
// Engine makes carries when no pixel rule changed it: what a DICOM
// Conformance Statement says of the method (PS3.15 E.1.1, E.3.6).
type MethodRecord struct {
	// Codes are the PS3.16 CID 7050 codes written in De-identification
	// Method Code Sequence (0012,0064); none when the profile is built on
	// no Annex E base or its own rules keep what a base changes.
	Codes []anonymize.MethodCode
	// Values are the values of De-identification Method (0012,0063): the
	// codes' meanings as the DICOM library writes them, then the method
	// the Engine was given and the versions of this build and its DICOM
	// library.
	Values []string
	// Temporal is Longitudinal Temporal Information Modified (0028,0303)
	// as the profile's dates option writes it, empty for none.
	Temporal string
}

// Method returns the record planMethod writes in a file no pixel rule
// changed, worked out as it works it out, so a description of a profile
// says exactly what its files carry.
func (en *Engine) Method() (MethodRecord, error) {
	out := MethodRecord{Codes: slices.Clone(en.codes), Temporal: en.temporal}
	if len(en.codes) > 0 {
		scratch := dataset.NewDataset()
		if err := anonymize.RecordDeidentificationMethod(scratch, en.codes, anonymize.MethodRecord{IdentityRemoved: true}); err != nil {
			return MethodRecord{}, fmt.Errorf("engine: recording the de-identification method: %w", err)
		}
		if v, ok := elementText(scratch, tag.Tag(tagDeidMethod)); ok && v != "" {
			out.Values = strings.Split(v, `\`)
		}
	}
	if en.method != "" {
		out.Values = append(append(out.Values, en.method), buildVersions()...)
	}
	return out, nil
}
