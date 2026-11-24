package deidjob

import (
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The conditions the tests' versions read.
const (
	onReceiver     = v1.ConditionSubject_CONDITION_SUBJECT_RECEIVER
	onManufacturer = v1.ConditionSubject_CONDITION_SUBJECT_MANUFACTURER
	ctMaker        = "GE MEDICAL SYSTEMS"
	tagSeriesDesc  = "0008103E"
)

// checkOne archives the CT image de-identified by doc as received at the
// receiver "recv" (changed by change, when given), runs a check over it,
// and returns the job as it ended.
func checkOne(t *testing.T, doc *v1.ProfileDocument, change func(*dataset.Dataset)) (*fixture, tenant.Job) {
	t.Helper()
	f := newFixture(t)
	ref := f.storeVersion("research", doc)
	b := deidentifyAt(t, corpus(t, "ct5n-2062.dcm"), doc, ref, engine.Reception{Receiver: "recv", CallingAETitle: "SCANNER1"})
	if change != nil {
		b = rewrite(t, b, change)
	}
	f.archive("ses-1", scanFiles{label: "ct", files: [][]byte{b}, profile: ref})
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidCheck, tenant.ProfileRef{}, "ses-1")
	s.Poke("lab")
	return f, f.waitJob(s, j.ID, 30*time.Second, ended)
}

// TestCheckReadsTheReceptionsFacts guards a version whose rule keeps an
// element for files that came to one receiver: the check runs the rule
// with the receiver and calling AE title of the instance's reception, so
// the kept element is no finding.
func TestCheckReadsTheReceptionsFacts(t *testing.T) {
	f, j := checkOne(t, withRules(keepWhen("keep-desc-recv", "SeriesDescription", onReceiver, "recv", false)), nil)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 1 {
		t.Fatalf("a version keeping an element at its receiver: %s %+v, failures %+v, tallies %v", j.State, j.Counts, f.failures(j.ID), f.tallies(j.ID))
	}
}

// TestCheckOfAConditionOnARemovedValueIsUndecidable guards a version whose
// rule keeps an element when the file's Manufacturer has a value the
// version removes: the archive no longer says what the Manufacturer was,
// so the element kept is a note (check|undecidable), never a finding.
func TestCheckOfAConditionOnARemovedValueIsUndecidable(t *testing.T) {
	f, j := checkOne(t, withRules(keepWhen("keep-desc-ge", "SeriesDescription", onManufacturer, ctMaker, false)), nil)
	if j.State != tenant.JobDone || j.Counts.InstancesWarned != 1 || j.Counts.InstancesRefused != 0 {
		t.Fatalf("a condition on a removed value: %s %+v, failures %+v", j.State, j.Counts, f.failures(j.ID))
	}
	if n := f.tallies(j.ID)["check|undecidable|"+tagSeriesDesc]; n != 1 {
		t.Errorf("check|undecidable|%s = %d (all: %v)", tagSeriesDesc, n, f.tallies(j.ID))
	}
	for _, x := range f.failures(j.ID) {
		if x.Code != tenant.FailureUndecided {
			t.Errorf("the note is %+v", x)
		}
	}
}

// TestCheckNeverPassesAConditionItCannotJudge guards the other side: a
// version that keeps an element unless the Manufacturer has a value it
// removes, over a file that had that value (so the element was removed),
// with the element planted back. The rule's condition now holds on the
// archive alone; the check must not pass it silently.
func TestCheckNeverPassesAConditionItCannotJudge(t *testing.T) {
	plant := func(ds *dataset.Dataset) {
		if err := ds.SetTextValue(tag.New(0x0008, 0x103E), dataelem.VR("LO"), "SmartScore - Gated 0.5 sec"); err != nil {
			t.Fatal(err)
		}
	}
	f, j := checkOne(t, withRules(keepWhen("keep-desc-not-ge", "SeriesDescription", onManufacturer, ctMaker, true)), plant)
	if j.Counts.InstancesSent != 0 {
		t.Fatalf("a planted element under a condition the archive cannot judge passed: %s %+v, tallies %v", j.State, j.Counts, f.tallies(j.ID))
	}
}

// TestDryRunReadsTheReceptionsFacts guards the dry run's conditions: a
// rule that keeps an element at a receiver decides it for a file that
// came to that receiver.
func TestDryRunReadsTheReceptionsFacts(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", withRules(keepWhen("keep-desc-recv", "SeriesDescription", onReceiver, "recv", false)))
	f.archive("ses-raw", scanFiles{label: "ct", files: [][]byte{corpus(t, "ct5n-2062.dcm")}})
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidDryRun, ref, "ses-raw")
	s.Poke("lab")
	f.waitJob(s, j.ID, 30*time.Second, ended)
	if n := f.tallies(j.ID)["rule|keep-desc-recv|keep"]; n != 1 {
		t.Errorf("the rule at its receiver decided %d elements (tallies %v)", n, f.tallies(j.ID))
	}
}
