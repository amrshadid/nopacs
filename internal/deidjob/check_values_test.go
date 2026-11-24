package deidjob

import (
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCheckFindsValuesTheVersionEmpties guards what the check sees of an
// element present: one whose value the version empties holding a value
// (Study Date, PS3.15 Z) is a finding, kept_value; one the version
// removes unless its IOD requires it (Device Serial Number, X/Z/D) and
// that resolves to removal, likewise a finding; the same files as
// archived pass.
func TestCheckFindsValuesTheVersionEmpties(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	set := func(tg tag.Tag, vr, v string) func(*dataset.Dataset) {
		return func(ds *dataset.Dataset) {
			if err := ds.SetTextValue(tg, dataelem.VR(vr), v); err != nil {
				t.Fatal(err)
			}
		}
	}
	files := f.deidSession("ses-1", ref, map[string][]string{"ct": {"ct5n-2062.dcm"}, "mr": {"MR_small.dcm"}, "ct2": {"ct2n-6293.dcm"}},
		map[string]func(*dataset.Dataset){
			"ct5n-2062.dcm": set(tag.New(0x0008, 0x0020), "DA", "20200101"),
			"MR_small.dcm":  set(tag.New(0x0018, 0x1000), "LO", "SN-0042"),
		})
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidCheck, tenant.ProfileRef{}, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	got := f.failures(j.ID)
	_, _, ct := uidsOf(t, files["ct5n-2062.dcm"])
	_, _, mr := uidsOf(t, files["MR_small.dcm"])
	if j.Counts.InstancesSent != 1 || got[ct].Detail != "kept_value" || got[mr].Code != tenant.FailureCheckFailed {
		t.Fatalf("the check ended %+v, failures %+v, tallies %v", j.Counts, got, f.tallies(j.ID))
	}
	tallies := f.tallies(j.ID)
	if tallies["check|kept_value|00080020"] != 1 || tallies["check|removed_element|00181000"]+tallies["check|kept_value|00181000"] != 1 {
		t.Errorf("the tallies do not name the elements: %v", tallies)
	}
}

// TestReceivedDeidentifiedDataIsNoReceivedUID guards the lookup of
// received UIDs on de-identified data sent back in: a file archived
// de-identified, then received again through the de-identifying receiver
// (its UIDs remapped once more), leaves the first copy's UIDs in the
// catalog as the remapping of the second's received UIDs; they are UIDs
// the remapping made, not originals, so neither copy is a finding.
func TestReceivedDeidentifiedDataIsNoReceivedUID(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	first := deidentify(t, corpus(t, "ct5n-2062.dcm"), basicProfile(), ref)
	again := deidentify(t, first, basicProfile(), ref)
	f.archive("ses-1", scanFiles{label: "ct", files: [][]byte{first}, profile: ref})
	f.archive("ses-2", scanFiles{label: "ct", files: [][]byte{again}, profile: ref})
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidCheck, tenant.ProfileRef{}, "")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 2 {
		t.Fatalf("de-identified data received again: %s %+v, failures %+v, tallies %v", j.State, j.Counts, f.failures(j.ID), f.tallies(j.ID))
	}
}
