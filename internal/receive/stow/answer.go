package stow

import (
	"encoding/json"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"
)

// The attributes of the answer (PS3.18 10.5.3.2, the Store Instances
// Response Module of PS3.18 Annex I).
var (
	tagRetrieveURL    = tag.New(0x0008, 0x1190)
	tagFailureReason  = tag.New(0x0008, 0x1197)
	tagFailedSOPs     = tag.New(0x0008, 0x1198)
	tagReferencedSOPs = tag.New(0x0008, 0x1199)
	tagRefSOPClass    = tag.New(0x0008, 0x1150)
	tagRefSOPInstance = tag.New(0x0008, 0x1155)
)

// answer returns the DICOM JSON (PS3.18 F.2) of a request's answer, written
// by go-dicom: the Referenced SOP Sequence (0008,1199), each stored
// instance with its Retrieve URL, the WADO-RS URL of the instance (PS3.18
// 10.4.1); the Failed SOP Sequence (0008,1198), each refused part that has
// UIDs to list with its Failure Reason; and the study's Retrieve URL when
// what was stored is of one study, or of the study the request named.
// Without retrieve, no Retrieve URL is given, neither an instance's nor the
// study's: a Retrieve URL says where an instance is retrieved by the UIDs
// the answer lists (PS3.18 10.5.3), and an instance archived under remapped
// UIDs is not retrieved by the ones the sender sent; a URL by the remapped
// ones would hand the sender the mapping. The answer then lists each
// stored instance by its class and the SOP Instance UID the sender sent.
func answer(base, study string, outcomes []outcome, retrieve bool) ([]byte, error) {
	stored, failed := sequence.New(), sequence.New()
	studies := map[string]bool{}
	for _, o := range outcomes {
		if !o.listed {
			continue
		}
		item := dataset.NewDataset()
		if err := add(item, tagRefSOPClass, dataelem.UI, o.class); err != nil {
			return nil, err
		}
		if err := add(item, tagRefSOPInstance, dataelem.UI, o.instance); err != nil {
			return nil, err
		}
		if o.reason != 0 {
			if err := add(item, tagFailureReason, dataelem.US, o.reason); err != nil {
				return nil, err
			}
			if err := failed.Append(item); err != nil {
				return nil, err
			}
			continue
		}
		studies[o.study] = true
		if !retrieve {
			if err := stored.Append(item); err != nil {
				return nil, err
			}
			continue
		}
		if err := add(item, tagRetrieveURL, dataelem.UR, base+"/studies/"+o.study+"/series/"+o.series+"/instances/"+o.instance); err != nil {
			return nil, err
		}
		if err := stored.Append(item); err != nil {
			return nil, err
		}
	}
	ds := dataset.NewDataset()
	if study == "" && len(studies) == 1 {
		for only := range studies {
			study = only
		}
	}
	if retrieve && study != "" && stored.Length() > 0 {
		if err := add(ds, tagRetrieveURL, dataelem.UR, base+"/studies/"+study); err != nil {
			return nil, err
		}
	}
	if stored.Length() > 0 {
		if err := add(ds, tagReferencedSOPs, dataelem.SQ, stored); err != nil {
			return nil, err
		}
	}
	if failed.Length() > 0 {
		if err := add(ds, tagFailedSOPs, dataelem.SQ, failed); err != nil {
			return nil, err
		}
	}
	m, err := ds.ToDICOMJSON()
	if err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// add adds one element to a data set.
func add(ds *dataset.Dataset, t tag.Tag, vr dataelem.VR, value any) error {
	return ds.Add(dataelem.NewDataElement(t, vr, value))
}
