package dicomweb

import (
	"encoding/json"
	"slices"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"
)

// The attributes the catalog answers, and those a search names by tag.
var (
	TagStudyDate                      = tag.New(0x0008, 0x0020)
	TagInstanceAvailability           = tag.New(0x0008, 0x0056)
	TagModalitiesInStudy              = tag.New(0x0008, 0x0061)
	TagModality                       = tag.New(0x0008, 0x0060)
	TagSOPClassUID                    = tag.New(0x0008, 0x0016)
	TagSOPInstanceUID                 = tag.New(0x0008, 0x0018)
	TagRetrieveURL                    = tag.New(0x0008, 0x1190)
	TagSeriesDescription              = tag.New(0x0008, 0x103E)
	TagPatientID                      = tag.New(0x0010, 0x0020)
	TagStudyInstanceUID               = tag.New(0x0020, 0x000D)
	TagSeriesInstanceUID              = tag.New(0x0020, 0x000E)
	TagSeriesNumber                   = tag.New(0x0020, 0x0011)
	TagNumberOfStudyRelatedSeries     = tag.New(0x0020, 0x1206)
	TagNumberOfStudyRelatedInstances  = tag.New(0x0020, 0x1208)
	TagNumberOfSeriesRelatedInstances = tag.New(0x0020, 0x1209)
	TagAvailableTransferSyntaxUID     = tag.New(0x0008, 0x3002)
)

// The attributes a search returns whether or not it is asked for them
// (PS3.18 10.6.3.3, Tables 10.6.3-3 to 10.6.3-5).
var (
	studyDefaults = []tag.Tag{
		TagStudyDate,
		tag.New(0x0008, 0x0030), // StudyTime
		tag.New(0x0008, 0x0050), // AccessionNumber
		TagInstanceAvailability,
		TagModalitiesInStudy,
		tag.New(0x0008, 0x0090), // ReferringPhysicianName
		tag.New(0x0008, 0x0201), // TimezoneOffsetFromUTC
		TagRetrieveURL,
		tag.New(0x0010, 0x0010), // PatientName
		TagPatientID,
		tag.New(0x0010, 0x0030), // PatientBirthDate
		tag.New(0x0010, 0x0040), // PatientSex
		TagStudyInstanceUID,
		tag.New(0x0020, 0x0010), // StudyID
		TagNumberOfStudyRelatedSeries,
		TagNumberOfStudyRelatedInstances,
	}
	seriesDefaults = []tag.Tag{
		TagModality,
		tag.New(0x0008, 0x0201), // TimezoneOffsetFromUTC
		TagSeriesDescription,
		TagRetrieveURL,
		TagSeriesInstanceUID,
		TagSeriesNumber,
		TagNumberOfSeriesRelatedInstances,
		tag.New(0x0040, 0x0244), // PerformedProcedureStepStartDate
		tag.New(0x0040, 0x0245), // PerformedProcedureStepStartTime
		tag.New(0x0040, 0x0275), // RequestAttributesSequence
	}
	instanceDefaults = []tag.Tag{
		TagSOPClassUID,
		TagSOPInstanceUID,
		TagInstanceAvailability,
		tag.New(0x0008, 0x0201), // TimezoneOffsetFromUTC
		TagRetrieveURL,
		tag.New(0x0020, 0x0013), // InstanceNumber
		tag.New(0x0028, 0x0010), // Rows
		tag.New(0x0028, 0x0011), // Columns
		tag.New(0x0028, 0x0100), // BitsAllocated
		tag.New(0x0028, 0x0008), // NumberOfFrames
	}
	// studyAll and seriesAll are what includefield=all adds at the study
	// and the series level: optional keys of each level (PS3.4 C.6.1.1)
	// the metadata files hold.
	studyAll = []tag.Tag{
		tag.New(0x0008, 0x1030), // StudyDescription
		tag.New(0x0010, 0x1010), // PatientAge
		tag.New(0x0010, 0x1020), // PatientSize
		tag.New(0x0010, 0x1030), // PatientWeight
		tag.New(0x0008, 0x0080), // InstitutionName
	}
	seriesAll = []tag.Tag{
		tag.New(0x0008, 0x0021), // SeriesDate
		tag.New(0x0008, 0x0031), // SeriesTime
		tag.New(0x0018, 0x0015), // BodyPartExamined
		tag.New(0x0018, 0x1030), // ProtocolName
		tag.New(0x0020, 0x0060), // Laterality
		tag.New(0x0008, 0x0070), // Manufacturer
	}
)

// Returned returns the attributes a result of the search holds when the
// archive has them: those of its level by default, and those of each level
// above that its path does not name (PS3.18 10.6.3.3), the attributes it
// matches and asks for with includefield, and, for includefield=all, the
// optional keys of those levels; at the instance level, includefield=all
// is every attribute (AllAttributes). Attributes of a level below the one
// searched are not returned (PS3.18 10.6.3.3.1).
func (q Query) Returned() map[tag.Tag]bool {
	out := map[tag.Tag]bool{}
	add := func(ts []tag.Tag) {
		for _, t := range ts {
			out[t] = true
		}
	}
	study := q.Level == Study || q.Study == ""
	series := q.Level == Series || (q.Level == Instance && q.Series == "")
	if study {
		add(studyDefaults)
		if q.IncludeAll {
			add(studyAll)
		}
	}
	if series {
		add(seriesDefaults)
		if q.IncludeAll {
			add(seriesAll)
		}
	}
	if q.Level == Instance {
		add(instanceDefaults)
	}
	for _, k := range q.Keys {
		out[k.Tag] = true
	}
	add(q.Include)
	// Study Instance UID and Series Instance UID name every result above
	// them, whatever the path names, so a client can place it.
	out[TagStudyInstanceUID] = true
	if q.Level != Study {
		out[TagSeriesInstanceUID] = true
	}
	return out
}

// AllAttributes reports whether a result holds every attribute the
// metadata file has for it: includefield=all at the instance level.
func (q Query) AllAttributes() bool { return q.IncludeAll && q.Level == Instance }

// Result is one result of a search, built as a data set by go-dicom and
// written as DICOM JSON (PS3.18 F.2), with elements taken as they are from
// a metadata file's DICOM JSON where the catalog does not hold the value.
type Result struct {
	ds  *dataset.Dataset
	raw map[string]json.RawMessage
}

// NewResult returns an empty result.
func NewResult() *Result {
	return &Result{ds: dataset.NewDataset(), raw: map[string]json.RawMessage{}}
}

// Set puts an attribute with its values: text for a string VR, one text
// per value; text of a number for IS and DS; numbers for US, UL and the
// other binary numbers. An attribute set twice keeps the first.
func (r *Result) Set(t tag.Tag, vr dataelem.VR, value any) {
	if r.ds.Contains(t) {
		return
	}
	_ = r.ds.Add(dataelem.NewDataElement(t, vr, value))
}

// Has reports whether the result holds the attribute.
func (r *Result) Has(t tag.Tag) bool {
	_, raw := r.raw[t.Hex()]
	return raw || r.ds.Contains(t)
}

// SetRaw puts an element of DICOM JSON as it is, under its key of eight
// hexadecimal digits, unless the result holds the attribute already. An
// element that carries binary data (InlineBinary) or points at it
// (BulkDataURI) is left out: a search answers attributes, not bulk data.
func (r *Result) SetRaw(key string, el json.RawMessage) {
	if _, ok := r.raw[key]; ok {
		return
	}
	var probe struct {
		InlineBinary *string `json:"InlineBinary"`
		BulkDataURI  *string `json:"BulkDataURI"`
	}
	if json.Unmarshal(el, &probe) != nil || probe.InlineBinary != nil || probe.BulkDataURI != nil {
		return
	}
	r.raw[key] = el
}

// MarshalJSON writes the result as one DICOM JSON object, its members in
// order of tag.
func (r *Result) MarshalJSON() ([]byte, error) {
	m, err := r.ds.ToDICOMJSON()
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(m)+len(r.raw))
	for k, v := range r.raw {
		out[k] = v
	}
	for k, el := range m {
		b, err := json.Marshal(el)
		if err != nil {
			return nil, err
		}
		out[k] = b
	}
	// encoding/json writes a map's members in order of key, which is the
	// order of tag for keys of eight upper-case hexadecimal digits.
	return json.Marshal(out)
}

// FromDataset puts the elements of a metadata file's data set, DICOM JSON
// as an object keyed by tag, that keep says to keep and the result does
// not hold yet.
func (r *Result) FromDataset(set map[string]json.RawMessage, keep func(key string) bool) {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !keep(k) || r.dsHasKey(k) {
			continue
		}
		r.SetRaw(k, set[k])
	}
}

// dsHasKey reports whether the data set holds the attribute of a key of
// eight hexadecimal digits.
func (r *Result) dsHasKey(key string) bool {
	t, ok := attributeTagHex(key)
	return ok && r.ds.Contains(t)
}

// attributeTagHex reads a key of eight hexadecimal digits as a tag.
func attributeTagHex(key string) (tag.Tag, bool) {
	if len(key) != 8 {
		return 0, false
	}
	var v uint32
	for i := 0; i < 8; i++ {
		c := key[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		default:
			return 0, false
		}
		v = v<<4 | uint32(d)
	}
	return tag.Tag(v), true
}
