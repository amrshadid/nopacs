package tui

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// correctionSetup is a tenant with a storage, a committed delete and a
// relabel still copying, and a finding.
func correctionSetup(f *fakeserver.Server) {
	addTenants(f, 1)
	f.AddStorage("t000", &v1.Storage{Name: "main"})
	f.AddCorrection("t000", &v1.Correction{Id: "cor_1", Kind: v1.CorrectionKind_CORRECTION_KIND_DELETE, State: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
		Target: &v1.CorrectionTarget{SubjectLabel: "sub-1"}, Reason: "consent withdrawn", Counts: &v1.CorrectionCounts{Instances: 4, Bytes: 4096},
		CreateTime: timestamp(-24), PurgeTime: timestamp(24 * 6),
		Dispositions:    []*v1.StorageDisposition{{Storage: "main", ObjectsRemoved: 4}, {Storage: "copy", ReplicaOf: "main", ObjectsUnknown: 4}},
		CopiesAbandoned: []string{"copy"}, OverrideRetention: true, OverrideReason: "consent withdrawn in writing"})
	f.AddCorrection("t000", &v1.Correction{Id: "cor_2", Kind: v1.CorrectionKind_CORRECTION_KIND_RELABEL, State: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
		Target: &v1.CorrectionTarget{SubjectLabel: "sub-2"}, ToLabel: "sub-3", Counts: &v1.CorrectionCounts{Instances: 2}, CreateTime: timestamp(-2)})
}

// TestCorrectionsListAndDetail guards the list and the detail of a
// correction: each storage's disposition, with the primary a replica is of,
// the copies a detach abandoned worded as unknown, and an override of
// retention with its reason.
func TestCorrectionsListAndDetail(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, correctionSetup)
		r.key("enter", "enter", "x")
		r.expect("Corrections of t000", "cor_1", "delete", "committed", "sub-1", "cor_2", "relabel")
		r.key("down", "enter")
		r.expect("Correction cor_1", "On main:", "4 removed: the bytes are gone", "On copy (replica of main):", "may still be there",
			"Copies abandoned:", "detached before the purge reached them", "Retention overridden:", "consent withdrawn in writing")
	})
}

// TestDeleteShowsItsPreviewAndNeedsTheTargetTyped guards a delete: the form
// asks what and why and answers with the preview, which says what would go and
// carries the token; then the target must be typed again; only then is the
// data deleted. An override of retention needs its reason.
func TestDeleteShowsItsPreviewAndNeedsTheTargetTyped(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, correctionSetup)
		r.key("enter", "enter", "x", "n")
		r.expect("Delete data in t000", "Nothing is deleted until you")
		deletes := r.calls("Delete")
		// A target and no reason: refused before anything is sent.
		r.typeText("sub-9/ses-1")
		r.key("enter", "enter", "enter", "enter", "enter")
		r.expect("say why the data is deleted")
		// An override without its reason.
		r.key("up", "up", "up", "up")
		r.key("tab", "tab")
		r.typeText("wrong patient")
		r.key("tab", "right", "enter", "enter")
		r.expect("say why the retention rule is lifted")
		if r.calls("Delete") != deletes {
			t.Fatalf("a preview was asked for with a form that was refused")
		}
		r.typeText("consent withdrawn in writing")
		r.key("enter")
		r.expect("Delete sub-9/ses-1", "Removes:", "1 subjects, 1 sessions", "On main:", "removed (directory)", "Retention:", "overridden: consent withdrawn in writing",
			"Confirm token:", "fake.sub-9/ses-1")
		if got := r.calls("Delete"); got != deletes+1 {
			t.Fatalf("Delete was called %d times for the preview", got-deletes)
		}

		r.key("c")
		r.expect("Delete sub-9/ses-1", "This hides 1 instances from every read")
		r.key("enter")
		r.expect("not done: type sub-9/ses-1 exactly")
		r.typeText("sub-9")
		r.key("enter")
		if r.calls("Delete") != deletes+1 {
			t.Fatalf("a delete was sent with the target half typed")
		}
		r.key("ctrl+u")
		r.typeText("sub-9/ses-1")
		r.key("enter")
		if r.calls("Delete") != deletes+2 {
			t.Fatalf("the delete was not sent")
		}
		r.expect("Corrections of t000", "deleted sub-9/ses-1: correction")
		if cs := r.fake.Corrections("t000"); len(cs) != 3 || !cs[2].GetOverrideRetention() || cs[2].GetOverrideReason() != "consent withdrawn in writing" {
			t.Errorf("the correction: %v", cs)
		}
	})
}

// TestRelabelReplaceUndoPurgeNeedTheirTypedConfirmation guards the other
// changes of data: relabel and replace ask for what they act on to be typed
// again, undo and purge for the correction's id, and none sends anything with
// the word wrong.
func TestRelabelReplaceUndoPurgeNeedTheirTypedConfirmation(t *testing.T) {
	r := newRig(t, 120, 30, correctionSetup)
	r.key("enter", "enter", "x")

	// Relabel: the target typed again.
	r.key("l")
	r.expect("Relabel data in t000", "undo stops a relabel still copying")
	relabels := r.calls("Relabel")
	r.typeText("sub-5")
	r.key("tab")
	r.typeText("sub-6")
	r.key("tab")
	r.typeText("wrong label")
	r.key("tab")
	r.typeText("sub-")
	r.key("enter")
	r.expect("not done: type sub-5 again, exactly, to go ahead")
	if r.calls("Relabel") != relabels {
		t.Fatalf("a relabel was sent with the confirmation wrong")
	}
	r.typeText("5")
	r.key("enter")
	if r.calls("Relabel") != relabels+1 {
		t.Fatalf("the relabel was not sent")
	}
	r.expect("Corrections of t000", "relabel 01jd")

	// Replace: the file id typed again.
	r.key("m")
	r.expect("Replace an instance in t000")
	replaces := r.calls("Replace")
	r.typeText("q_41Ab")
	r.key("tab")
	r.typeText("corrected file sent again")
	r.key("tab")
	r.typeText("q_41")
	r.key("enter")
	r.expect("not done: type q_41Ab again, exactly, to go ahead")
	if r.calls("Replace") != replaces {
		t.Fatalf("a replace was sent with the confirmation wrong")
	}
	r.typeText("Ab")
	r.key("enter")
	if r.calls("Replace") != replaces+1 {
		t.Fatalf("the replace was not sent")
	}
	r.key("esc")

	// Undo and purge of the first correction: the id typed.
	r.key("down", "down")
	r.key("u")
	r.expect("Undo correction")
	undos := r.calls("UndoCorrection")
	r.key("enter", "enter")
	r.expect("not done: type")
	r.key("up")
	r.typeText("nope")
	r.key("enter", "enter")
	if r.calls("UndoCorrection") != undos {
		t.Fatalf("an undo was sent with the id wrong")
	}
	r.key("esc", "p")
	r.expect("Purge correction", "Once a purge has started nothing is undone")
	purges := r.calls("PurgeCorrection")
	r.key("enter")
	r.expect("not done: type")
	if r.calls("PurgeCorrection") != purges {
		t.Fatalf("a purge was sent with no id typed")
	}
}

// TestFindingsAreListedAndScrubAndRebuildAskFirst guards the maintenance
// screen: the findings that hold, a scrub that only reads, and a rebuild that
// replaces the catalog only after the tenant's name is typed.
func TestFindingsAreListedAndScrubAndRebuildAskFirst(t *testing.T) {
	r := newRig(t, 120, 30, func(f *fakeserver.Server) {
		correctionSetup(f)
		f.AddScrubFinding("t000", &v1.ScrubFinding{Id: "fnd_9", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_ORPHAN, Storage: "main", Key: "k", FindTime: timestamp(-1)})
		f.AddScrubFinding("t000", &v1.ScrubFinding{Id: "fnd_8", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_MANIFEST_DAMAGED, Storage: "main", SubjectLabel: "sub-1",
			SessionLabel: "ses-1", ScanLabel: "scan-1", ExpectedSize: proto.Int64(5), FindTime: timestamp(-2)})
	})
	r.key("enter", "enter", "f")
	r.expect("Scrub findings of t000", "fnd_9", "orphan", "fnd_8", "manifest-damaged")
	r.key("enter")
	r.expect("Finding fnd_9", "Kind:", "orphan")
	r.key("esc")

	r.key("s")
	r.expect("Scrub t000", "A scrub only reads")
	r.key("enter")
	r.expect("scrub job job_1 queued")
	if sj := r.fake.Job("t000", "job_1").GetScrub(); sj == nil {
		t.Errorf("the scrub job: %v", r.fake.Job("t000", "job_1"))
	}

	// A replace of the catalog needs the tenant's name; a compare does not.
	r.key("b")
	r.expect("Rebuild the catalog of t000", "replace swaps the catalog in use")
	rebuilds := r.calls("CreateJob")
	r.key("right", "enter", "enter", "enter")
	r.expect("not done: type t000 exactly")
	r.typeText("t000")
	r.key("up")
	r.typeText("main")
	r.key("enter", "enter")
	if r.calls("CreateJob") != rebuilds {
		t.Fatalf("a replace was sent with a stray storage and the name typed")
	}
	r.expect("a replace builds from every storage")
	r.key("up", "ctrl+u", "down", "enter")
	if r.calls("CreateJob") != rebuilds+1 {
		t.Fatalf("the replace was not sent")
	}
	r.expect("rebuild job job_2 queued")
}

// TestProfilesRoutesAndTheSealVerify guards the read screens: a tenant's
// profiles, a receiver's routes in order, and the verification of an audit
// log, which says whether it is intact and, when it is not, what was found.
func TestProfilesRoutesAndTheSealVerify(t *testing.T) {
	r := newRig(t, 120, 30, func(f *fakeserver.Server) {
		addTenants(f, 1)
		f.AddStorage("t000", &v1.Storage{Name: "main"})
		f.AddReceiver("t000", &v1.Receiver{Name: "scanner1", Storage: "main", Type: &v1.Receiver_Dimse{Dimse: &v1.DimseReceiver{AeTitle: "ARCHIVE", Port: 11112}}})
	})
	if _, err := r.m.c.client.Routes.CreateRuleSet(context.Background(), &v1.CreateRuleSetRequest{Tenant: "t000", Receiver: "scanner1", Activate: true,
		RuleSet: &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: "lab-a", Storage: "main"}, {Name: "lab-b", Storage: "main"}}}}); err != nil {
		t.Fatalf("making a rule set: %v", err)
	}
	if _, err := r.m.c.client.Profiles.CreateProfileVersion(context.Background(), &v1.CreateProfileVersionRequest{Tenant: "t000", Name: "research",
		Document: &v1.ProfileDocument{Bases: []string{"annex-e/basic:2026d"}}}); err != nil {
		t.Fatalf("making a profile: %v", err)
	}
	r.key("enter", "enter", "p")
	r.expect("Profiles of t000", "research", "1")
	r.key("enter")
	r.expect("Profile research", "Latest version:", "1")
	r.key("esc", "esc", "r", "enter", "u")
	r.expect("Routes of scanner1", "lab-a", "lab-b", "every study")
	r.key("esc", "esc", "esc", "v")
	r.expect("Audit seal of tenant t000", "Intact:", "yes", "Last checkpoint:")
	r.key("esc")

	r.fake.SetVerifyAnswer(&v1.VerifyAuditResponse{Intact: false, EntriesChecked: 10, Problems: []*v1.AuditVerifyProblem{{Kind: v1.AuditVerifyProblemKind_AUDIT_VERIFY_PROBLEM_KIND_CHAIN_BROKEN, Seq: 7, Detail: "the entry's chain does not match"}}}, 0)
	r.key("v")
	r.expect("Audit seal of tenant t000", "Intact:", "NO", "Problem:", "at seq 7", "the entry's chain does not match")
}
