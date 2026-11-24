package deidjob

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// deidSession archives public files de-identified by the version ref of
// basicProfile, under the tenant's keys, into the session, each scan of
// one name; it returns their archived bytes by name.
func (f *fixture) deidSession(session string, ref tenant.ProfileRef, scans map[string][]string, change map[string]func(*dataset.Dataset)) map[string][]byte {
	f.t.Helper()
	out := map[string][]byte{}
	var all []scanFiles
	for label, names := range scans {
		s := scanFiles{label: label, profile: ref}
		for _, n := range names {
			b := deidentify(f.t, corpus(f.t, n), basicProfile(), ref)
			if c := change[n]; c != nil {
				b = rewrite(f.t, b, c)
			}
			out[n] = b
			s.files = append(s.files, b)
		}
		all = append(all, s)
	}
	f.archive(session, all...)
	return out
}

// TestCheckPassesACleanArchive guards the check after archiving on files a
// receiver archived de-identified: each passes (no element the version
// removes, Patient Identity Removed YES, the method naming the version, no
// UID a received file carried), an instance archived as received passes
// with a note, and the check of the whole tenant ends done, its tallies
// holding no failing finding.
func TestCheckPassesACleanArchive(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.deidSession("ses-1", ref, map[string][]string{"mr": {"MR_small.dcm"}, "ct": {"ct5n-2062.dcm", "ct5n-2392.dcm"},
		"seg": {"liver_1frame.dcm"}}, nil)
	f.archive("ses-raw", scanFiles{label: "raw", files: [][]byte{corpus(t, "ct2n-6293.dcm")}})
	before := f.snapshot()
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidCheck, tenant.ProfileRef{}, "")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 4 || j.Counts.InstancesWarned != 1 || j.Counts.InstancesRefused != 0 {
		t.Fatalf("the check of a clean archive ended %s with %+v, failures %+v, tallies %v", j.State, j.Counts, f.failures(j.ID), f.tallies(j.ID))
	}
	if n := f.tallies(j.ID)["check|as_received"]; n != 1 {
		t.Errorf("the instance archived as received is tallied %d times", n)
	}
	if f.keyCalls.Load() == 0 {
		t.Error("the check never read the tenant's UID key to look up received UIDs")
	}
	if after := f.snapshot(); after != before {
		t.Errorf("the check changed the tenant's database or storage")
	}
}

// TestCheckFindsPlantedViolations guards the check after archiving on
// archived files planted with what the version forbids, each written
// straight to the storage with its catalog row, as a receiver with a fault
// would have archived it: an element the version removes kept
// (Manufacturer), Patient Identity Removed missing, a private element the
// version does not keep, and an original UID of another archived file
// kept (Frame of Reference UID). Each is refused as check_failed with its
// finding, and the tallies name the finding and the element's tag; a clean
// file beside them passes.
func TestCheckFindsPlantedViolations(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	mr := corpus(t, "MR_small.dcm")
	_, _, mrOriginal := uidsOf(t, mr)
	setText := func(tg tag.Tag, vr dataelem.VR, v string) func(*dataset.Dataset) {
		return func(ds *dataset.Dataset) {
			if err := ds.SetTextValue(tg, vr, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	files := f.deidSession("ses-1", ref, map[string][]string{"mr": {"MR_small.dcm"}, "ct": {"ct5n-2062.dcm", "ct5n-2392.dcm"},
		"ct2": {"ct2n-6293.dcm"}, "seg": {"liver_1frame.dcm"}}, map[string]func(*dataset.Dataset){
		"ct5n-2062.dcm": setText(tag.New(0x0008, 0x0070), dataelem.VR("LO"), "ACME MEDICAL"),
		"ct5n-2392.dcm": func(ds *dataset.Dataset) { ds.Remove(tag.New(0x0012, 0x0062)) },
		"ct2n-6293.dcm": func(ds *dataset.Dataset) {
			setText(tag.New(0x0009, 0x0010), dataelem.VR("LO"), "ACME 1.0")(ds)
			setText(tag.New(0x0009, 0x1001), dataelem.VR("LO"), "a vendor's note")(ds)
		},
		"liver_1frame.dcm": setText(tag.New(0x0020, 0x0052), dataelem.VR("UI"), mrOriginal),
	})
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidCheck, tenant.ProfileRef{}, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 4 {
		t.Fatalf("the check ended %s with %+v, failures %+v", j.State, j.Counts, f.failures(j.ID))
	}
	got := f.failures(j.ID)
	for n, detail := range map[string]string{"ct5n-2062.dcm": "removed_element", "ct5n-2392.dcm": "identity_not_removed",
		"ct2n-6293.dcm": "private_element", "liver_1frame.dcm": "received_uid"} {
		_, _, sop := uidsOf(t, files[n])
		if x := got[sop]; x.Code != tenant.FailureCheckFailed || x.Detail != detail {
			t.Errorf("%s: failure %+v, want check_failed %s", n, x, detail)
		}
	}
	tallies := f.tallies(j.ID)
	for _, key := range []string{"check|removed_element|00080070", "check|identity_not_removed|00120062",
		"check|private_element|00091001", "check|received_uid|00200052"} {
		if tallies[key] != 1 {
			t.Errorf("tally %s = %d (all: %v)", key, tallies[key], tallies)
		}
	}
	for key := range tallies {
		if bytes.Contains([]byte(key), []byte("ACME")) || bytes.Contains([]byte(key), []byte(mrOriginal)) {
			t.Errorf("a tally holds a value of a file: %q", key)
		}
	}
}

// TestDamageIsReportedNotJudged guards a deid job's reading: an archived
// file whose bytes on the storage were changed after archiving is refused
// as damaged_in_storage, never judged, and one missing from the storage
// likewise; the others are judged.
func TestDamageIsReportedNotJudged(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	files := f.deidSession("ses-1", ref, map[string][]string{"ct": {"ct5n-2062.dcm", "ct5n-2392.dcm"}, "mr": {"MR_small.dcm"}}, nil)
	_, _, changed := uidsOf(t, files["ct5n-2062.dcm"])
	_, _, gone := uidsOf(t, files["MR_small.dcm"])
	other := append(bytes.Clone(f.files[changed][:len(f.files[changed])-1]), 'x')
	if _, err := f.store.Put(f.ctx, f.keys[changed], bytes.NewReader(other)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(f.ctx, f.keys[gone]); err != nil {
		t.Fatal(err)
	}
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidCheck, tenant.ProfileRef{}, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	got := f.failures(j.ID)
	if j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 2 || got[changed].Code != tenant.FailureDamagedInStorage ||
		got[gone].Code != tenant.FailureDamagedInStorage || got[gone].Detail != detailMissing {
		t.Fatalf("a check over damaged files ended %+v, failures %+v", j.Counts, got)
	}
}

// TestTalliesOfAFileFitOneWrite guards the bound on one file's tallies:
// past the keys one progress write takes, the smallest counts are folded
// into "other", their total kept, so a profile of many rules never makes a
// write the store refuses.
func TestTalliesOfAFileFitOneWrite(t *testing.T) {
	m := map[string]int64{}
	var total int64
	for i := range tenant.MaxJobTallyBatch + 50 {
		m[tallyKey("rule", "r"+strconv.Itoa(i), "remove")] = int64(i + 1)
		total += int64(i + 1)
	}
	capTallies(m, tenant.MaxJobTallyBatch)
	var got int64
	for _, n := range m {
		got += n
	}
	if len(m) != tenant.MaxJobTallyBatch || got != total || m[tenant.TallyOther] == 0 || m["rule|r1|remove"] != 0 {
		t.Errorf("capped to %d keys, total %d (want %d), other %d", len(m), got, total, m[tenant.TallyOther])
	}
}
