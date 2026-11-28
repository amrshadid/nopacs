package tui

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// fill types each text into the field the focus is on and moves to the next,
// leaving the focus on the field of the last text: an empty text leaves its
// field as it is.
func (r *rig) fill(texts ...string) {
	r.t.Helper()
	for i, s := range texts {
		if s != "" {
			r.typeText(s)
		}
		if i < len(texts)-1 {
			r.key("tab")
		}
	}
}

// maskedKeySetup makes the key the key rigs connect with a masked key, as one
// made for an outside model, and then runs setup.
func maskedKeySetup(setup func(*fakeserver.Server)) func(*fakeserver.Server) {
	return func(f *fakeserver.Server) {
		f.AddKey(&v1.Key{Id: "k00000000001", Holder: "an outside model", Masked: true})
		if setup != nil {
			setup(f)
		}
	}
}

// helpOf opens the help screen of the screen on top, which lists every key it
// has, and says what it shows; the hint line is one line wide and cuts a long
// list.
func helpOf(r *rig, heading string) {
	r.t.Helper()
	r.key("?")
	r.expect("Keys of: " + heading)
}

// catalogSetup is tenant t000 with one subject holding one session of two
// scans, of modalities MR and SR.
func catalogSetup(f *fakeserver.Server) {
	seedReading(f)
	f.AddScan("t000", &v1.Scan{SubjectLabel: "sub-a", SessionLabel: "ses-1", Label: "scan-2", Modality: "SR", SeriesDescription: "report", InstanceCount: 1, SizeBytes: 100, ArchiveTime: timestamp(-2)})
}

// openKeyTenant opens the first tenant on a rig whose caller holds a key.
func openKeyTenant(t *testing.T, grants []keyGrant, setup func(*fakeserver.Server)) *rig {
	t.Helper()
	r := newKeyRig(t, 120, 40, grants, setup)
	r.key("enter", "enter")
	r.expect("Tenant t000")
	return r
}
