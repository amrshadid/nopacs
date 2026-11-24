package engine

import (
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// datasetAttributes is naming.ItemAttributes over a data set as received,
// so the engine reads a subject identity by the same rule as the naming
// rule reads it from the file's header (naming.ReadSubjectIdentity): one
// file, one identity, one label, one date shift.
type datasetAttributes struct{ ds *dataset.Dataset }

var _ naming.ItemAttributes = datasetAttributes{}

// Lookup implements naming.Attributes: a top-level value without its
// padding (topText).
func (a datasetAttributes) Lookup(t naming.Tag) (string, bool) {
	return topText(a.ds, uint32(t))
}

// Item implements naming.ItemAttributes: through the first item of each
// sequence of path to the value its last tag names. The DICOM library has
// decoded each item's text under its own Specific Character Set. A value
// there that is not text in memory (a sequence, a value left unread,
// longer than MaxValueLength) is an error, never an empty value.
func (a datasetAttributes) Item(path []naming.Tag) (string, bool, error) {
	ds := a.ds
	for i, t := range path {
		elem, ok := ds.Get(tag.Tag(t))
		if !ok {
			return "", false, nil
		}
		seq, isSeq := elem.GetValue().(*sequence.Sequence)
		if i == len(path)-1 {
			if isSeq {
				return "", true, errNotText
			}
			v, ok := elementText(ds, tag.Tag(t))
			if !ok {
				return "", true, errNotText
			}
			return naming.TrimValue(effectiveVR(tag.Tag(t), elem), v), true, nil
		}
		if !isSeq {
			return "", true, errNotText
		}
		if seq.Length() == 0 {
			return "", false, nil
		}
		item, ok := seq.Items()[0].(*dataset.Dataset)
		if !ok {
			return "", true, errNotText
		}
		ds = item
	}
	return "", false, nil
}

// readIdentity sets the file's subject identity once, before any element
// is changed: the key the caller read from the file's header
// (Reception.IdentityGiven), so the label the caller made and what the
// engine shifts and hashes are of one identity; or, when the caller gives
// none, the identity read here from the data set as received, under the
// reception's namespace. A file without one keeps hasIdentity false, and
// every action that needs it is refused (CauseNoSubject); none falls back
// to the Patient ID alone.
func (r *run) readIdentity(rec Reception) {
	if rec.IdentityGiven {
		r.identity, r.hasIdentity = rec.Identity, rec.Identity != ""
		return
	}
	id, err := naming.ReadSubjectIdentity(datasetAttributes{r.top}, rec.Namespace)
	if err != nil {
		return
	}
	r.identity, r.hasIdentity = string(id.Key()), true
}

// hashInput returns what the keyed hash of value at ds and t is taken
// over: the subject identity's key for the top-level Patient ID, so a
// profile's hash, lookup or template of the Patient ID gives what the
// subject label's code is the keyed hash of (two issuers' patients of one
// Patient ID get two codes, and a file with no issuer under the default
// namespace the code of its Patient ID alone); value itself for every
// other element. It reports false for the top-level Patient ID of a file
// whose identity could not be read (a NUL in the Patient ID, an issuer too
// long or left unread): hashing the Patient ID alone there would give the
// code of another population's patient, so the action fails instead.
func (r *run) hashInput(ds *dataset.Dataset, t tag.Tag, value string) (string, bool) {
	if ds == r.top && uint32(t) == tagPatientID {
		return r.identity, r.hasIdentity
	}
	return value, true
}
