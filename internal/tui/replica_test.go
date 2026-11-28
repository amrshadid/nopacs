package tui

import (
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// replicaSetup is a tenant t000 with a primary "main", its replica "copy" and
// two scrub findings on main.
func replicaSetup(f *fakeserver.Server) {
	addTenants(f, 1)
	f.AddStorage("t000", &v1.Storage{Name: "main"})
	f.AddStorage("t000", &v1.Storage{Name: "copy", ReplicaOf: "main"})
	f.AddStorage("t000", &v1.Storage{Name: "spare"})
	f.SetReplicaLag("t000", &v1.ReplicaStatus{Replica: "copy", ScansInStep: 12, ScansPending: 3, ScansDamaged: 1, Lag: durationpb.New(2 * time.Hour)})
	f.AddScrubFinding("t000", &v1.ScrubFinding{Id: "fnd_1", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_DAMAGED, Storage: "main", SubjectLabel: "sub-1",
		SessionLabel: "ses-1", ScanLabel: "scan-1", InstanceId: "i-1", ExpectedSize: proto.Int64(10), FindTime: timestamp(-3)})
	f.AddScrubFinding("t000", &v1.ScrubFinding{Id: "fnd_2", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_MISSING, Storage: "main", SubjectLabel: "sub-2",
		SessionLabel: "ses-1", ScanLabel: "scan-1", InstanceId: "i-2", ExpectedSize: proto.Int64(10), FindTime: timestamp(-2)})
}

// openReplicas opens the replicas of the first tenant.
func openReplicas(r *rig) {
	r.key("enter", "enter", "b")
	r.expect("Replicas of t000")
}

// TestReplicaListAttachDetach guards the replica screens: the list shows how
// far each replica is, attach asks for the storage and its primary, and detach
// asks for the replica's name to be typed, and for a reason when it abandons
// the purges that count the replica (which cannot be undone).
func TestReplicaListAttachDetach(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, replicaSetup)
		openReplicas(r)
		r.expect("copy", "main", "online", "12", "2h")

		// Attach: both names are needed before anything is sent.
		r.key("a")
		r.expect("Attach a replica to t000", "no archived file")
		attaches := r.calls("AttachReplica")
		r.key("enter", "enter")
		r.expect("name the storage and its primary")
		r.key("shift+tab")
		r.typeText("spare")
		r.key("tab")
		r.typeText("main")
		r.key("enter")
		if r.calls("AttachReplica") != attaches+1 {
			t.Fatalf("the attach was not sent")
		}
		r.expect("Replicas of t000", "spare", "storage spare is a replica of main; copy job job_1 queued")

		// Detach spare: nothing without the name; abandoning needs a reason.
		r.key("down", "d")
		r.expect("Detach replica spare", "No file is deleted")
		detaches := r.calls("DetachReplica")
		r.key("enter", "enter", "enter")
		r.expect("not done: type spare exactly")
		r.key("up", "up")
		r.typeText("spar")
		r.key("enter", "enter", "enter")
		if r.calls("DetachReplica") != detaches {
			t.Fatalf("a detach was sent with the name half typed")
		}
		r.key("up", "up")
		r.typeText("e")
		r.key("tab", "right", "tab", "enter")
		r.expect("say why the purges are abandoned")
		if r.calls("DetachReplica") != detaches {
			t.Fatalf("an abandon with no reason was sent")
		}
		r.typeText("the site burned")
		r.key("enter")
		if r.calls("DetachReplica") != detaches+1 {
			t.Fatalf("the detach was not sent")
		}
		r.expect("storage spare is an ordinary storage again; no file was deleted")
	})
}

// TestReplicaBackfillNotServed guards what a server that does not serve the
// backfill (an older one) shows: one plain line, not an internal error, and
// nothing sent before the person said yes.
func TestReplicaBackfillNotServed(t *testing.T) {
	r := newRig(t, 100, 24, replicaSetup)
	r.fake.RefuseBackfill()
	openReplicas(r)
	r.key("b")
	r.expect("Backfill replica copy")
	before := r.calls("BackfillReplica")
	r.key("enter")
	r.expect("choose yes to start the copy")
	if r.calls("BackfillReplica") != before {
		t.Fatalf("a backfill was sent before yes was chosen")
	}
	r.key("right", "enter")
	r.expect("backfill: not served: this server does not serve it yet; nothing was changed")
	r.refuse("internal error")
	r.fake.ServeBackfill()
	r.key("enter")
	r.expect("copy job job_1 queued")
}

// TestRepairNeedsTheStorageNameTyped guards the repair screens: the findings
// that hold on the primary are listed, one is repaired or all, and each asks
// for the primary's name to be typed first because the repair writes to it.
func TestRepairNeedsTheStorageNameTyped(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, replicaSetup)
		openReplicas(r)
		r.key("r")
		r.expect("Findings on main", "fnd_1", "damaged", "fnd_2", "missing")
		repairs := r.calls("Repair")
		r.key("r")
		r.expect("Repair main", "checked against the catalog")
		r.key("enter", "enter")
		r.expect("not done: type main exactly")
		r.key("up")
		r.typeText("mai")
		r.key("enter", "enter")
		if r.calls("Repair") != repairs {
			t.Fatalf("a repair was sent with the name half typed")
		}
		r.key("up")
		r.typeText("n")
		r.key("tab")
		r.typeText("copy")
		r.key("enter")
		if r.calls("Repair") != repairs+1 {
			t.Fatalf("the repair was not sent")
		}
		r.expect("Replicas of t000", "repair job job_1 queued")
		if rj := r.fake.Job("t000", "job_1").GetRepair(); rj.GetStorage() != "main" || rj.GetFrom() != "copy" || len(rj.GetFindings()) != 1 || rj.GetFindings()[0] != "fnd_1" {
			t.Errorf("the repair job: %v", rj)
		}

		// All of them, from the findings list.
		r.key("r", "a")
		r.expect("Repair main", "every finding that holds")
		r.typeText("main")
		r.key("enter", "enter")
		if rj := r.fake.Job("t000", "job_2").GetRepair(); !rj.GetAll() {
			t.Errorf("the repair of all: %v", rj)
		}
	})
}

// TestSwitchFlow guards a switch: the preview shows what stands in its way
// and the token; confirmation is typed (the first characters of the token or
// the primary's name) and refused otherwise; a blocked switch needs its checks
// passed over with a reason; once made the screen shows the pause and the
// associations refused; a refusal from the server shows its reason and the
// delay to wait.
func TestSwitchFlow(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) {
			replicaSetup(f)
			f.SetSwitchBlockers("t000", "main", "replica not in step: 12 scans")
		})
		openReplicas(r)
		r.key("s")
		r.expect("Switch main to copy", "Ready:", "no: something stands in its way", "Blocked by:", "replica not in step: 12 scans",
			"Confirm token:", "sw.main.copy.0", "Writes pause for:", "scanner1")

		r.key("c")
		r.expect("Type to confirm", "sw.main.", "or: main")
		switches := r.calls("SwitchStorage")
		// Nothing, a wrong word and a short prefix are all refused.
		for _, typed := range []string{"", "yes", "sw.m"} {
			r.key("ctrl+u")
			r.typeText(typed)
			r.key("enter", "enter", "enter")
			r.expect("not done: type the first 8 characters of the token, or main, exactly")
			r.key("up", "up")
		}
		// Right word, but the blocker is still there and no reason is given.
		r.key("ctrl+u")
		r.typeText("main")
		r.key("enter", "enter", "enter")
		r.expect("the preview shows blockers")
		if r.calls("SwitchStorage") != switches {
			t.Fatalf("a switch was sent with blockers and no reason")
		}
		// Pass the checks over: a reason is needed.
		r.key("up", "right", "enter", "enter")
		r.expect("say why the checks are passed over")
		r.typeText("compared on another host")
		r.key("enter")
		if r.calls("SwitchStorage") != switches+1 {
			t.Fatalf("the switch was not sent")
		}
		r.expect("Switching main to copy", "copy becomes the primary main", "Result:", "made", "Writes paused for:", "1.5s",
			"Associations refused:", "4", "Generation:", "1")
		r.key("esc")
		r.refuse("Switching main")
	})
}

// TestSwitchConfirmsByTheTokensFirstCharacters guards the other way to
// confirm: eight characters of the token, typed, switch a tenant that nothing
// blocks, with no reason.
func TestSwitchConfirmsByTheTokensFirstCharacters(t *testing.T) {
	r := newRig(t, 120, 30, replicaSetup)
	openReplicas(r)
	r.key("s")
	r.expect("Ready:", "yes")
	r.key("c")
	switches := r.calls("SwitchStorage")
	r.typeText("sw.main.")
	r.key("enter", "enter", "enter")
	if r.calls("SwitchStorage") != switches+1 {
		t.Fatalf("the switch was not sent")
	}
	r.expect("Result:", "made")
}

// TestSwitchRefusalShowsItsReason guards a refusal: the line at the bottom
// names the server's reason and how long to wait, and the screen says the
// switch was refused and every write resumes on the old place.
func TestSwitchRefusalShowsItsReason(t *testing.T) {
	r := newRig(t, 140, 30, replicaSetup)
	openReplicas(r)
	r.key("s")
	r.key("c")
	// The server answers a running switch as unavailable; the reason and the
	// delay travel in the details only for a reason of the answer's own kind.
	r.fake.FailWithReason(nopacsv1connect.ReplicaServiceSwitchStorageProcedure, fault.Unavailable, "a switch of this storage is already running", "STORAGE_SWITCHING", 20*time.Second)
	r.typeText("main")
	r.key("enter", "enter", "enter")
	r.expect("switch: STORAGE_SWITCHING: a switch of this storage is already running (try again in 20s)", "refused: every write resumes on the old place")
}

// TestSwitchRefusalWithoutDetailsShowsItsMessage guards an answer that carries
// no error details: the screen shows the server's message, with no reason and no
// delay, and nothing fails on the missing details.
func TestSwitchRefusalWithoutDetailsShowsItsMessage(t *testing.T) {
	r := newRig(t, 140, 30, replicaSetup)
	openReplicas(r)
	r.key("s")
	r.key("c")
	r.fake.FailWithoutDetails(nopacsv1connect.ReplicaServiceSwitchStorageProcedure, connect.CodeFailedPrecondition, "a switch of this storage is already running")
	r.typeText("main")
	r.key("enter", "enter", "enter")
	r.expect("switch: a switch of this storage is already running", "refused: every write resumes on the old place")
	r.refuse("try again in")
	r.refuse("STORAGE_SWITCHING")
}

// TestSwitchUnavailableAnswerIsARefusal guards the state of the switch screen:
// the server uses "unavailable" for refusals (the drain ran out, another switch
// runs), and an answer it sent is a refusal with its reason, never "goes on in
// the server"; only a call the client's own deadline ended is that.
func TestSwitchUnavailableAnswerIsARefusal(t *testing.T) {
	r := newRig(t, 140, 30, replicaSetup)
	openReplicas(r)
	r.key("s")
	r.key("c")
	r.fake.FailWithReason(nopacsv1connect.ReplicaServiceSwitchStorageProcedure, fault.Unavailable, "the writes in flight did not end within the drain timeout", "STORAGE_SWITCHING", 20*time.Second)
	r.typeText("main")
	r.key("enter", "enter", "enter")
	r.expect("refused: every write resumes on the old place", "the writes in flight did not end within the drain timeout")
	r.refuse("goes on in the server")
}

// TestStatusShowsTierReplicaSwitchEncryptionAndCoverage guards the status of a
// tenant: each storage says its tier, its primary, whether a switch is running,
// encryption at rest no more strongly than the storage or its admin said, and
// how much scrubs have read; each replica says how many scans wait; and the
// retention state says when it is paused.
func TestStatusShowsTierReplicaSwitchEncryptionAndCoverage(t *testing.T) {
	r := newRig(t, 200, 40, func(f *fakeserver.Server) {
		replicaSetup(f)
		f.SetStorageStatus("t000", "main", func(s *v1.StorageStatus) {
			s.Switching = true
			s.EncryptionAtRest = v1.EncryptionAtRest_ENCRYPTION_AT_REST_DECLARED
			s.Coverage = &v1.ScrubCoverage{Objects: 100, Verified: 90, Share: 0.9}
		})
		f.SetStorageStatus("t000", "copy", func(s *v1.StorageStatus) {
			s.EncryptionAtRest = v1.EncryptionAtRest_ENCRYPTION_AT_REST_ENCRYPTED
			s.EncryptionAlgorithm = "aws:kms"
		})
		f.SetRetentionStatus("t000", &v1.RetentionStatus{Rules: 1, Paused: true, PauseReason: "clock jumped"})
	})
	r.key("enter", "enter", "t")
	r.expect("Storage main:", "online tier", "SWITCHING: its receivers refuse new data", "declared by the admin (not checked by the server)", "90.0% of 100 objects",
		"Storage copy:", "replica of main", "yes, aws:kms (the storage says so)", "Replica copy:", "of main, 12 scans in step, 3 waiting",
		"Retention:", "PAUSED: clock jumped")
}

// TestServerTextIsCleanedOnTheNewScreens guards the rule that text from the
// network is cleaned before it is drawn: a hold's reason and a finding's
// labels that hold an escape sequence reach the frame with no control
// character in it.
func TestServerTextIsCleanedOnTheNewScreens(t *testing.T) {
	r := newRig(t, 120, 30, func(f *fakeserver.Server) {
		replicaSetup(f)
		f.AddHold("t000", &v1.Hold{Id: "hold_x", Target: &v1.HoldTarget{SubjectLabel: "sub\x1b[2J-1"}, Reason: "case\x1b[31m 114\a", State: v1.HoldState_HOLD_STATE_ACTIVE})
		f.AddScrubFinding("t000", &v1.ScrubFinding{Id: "fnd_\x1b[1m", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_MISSING, Storage: "main", SubjectLabel: "s\x1b]0;x\a"})
	})
	r.key("enter", "enter", "l")
	r.expect("Holds of t000", "hold_x")
	r.key("enter")
	r.expect("Hold hold_x")
	for _, frame := range r.frames {
		for _, bad := range []string{"\x1b[2J", "\x1b[31m", "\a", "\x1b]0"} {
			if contains(frame, bad) {
				t.Fatalf("a control sequence reached the screen: %q", bad)
			}
		}
	}
	r.key("esc", "esc", "f")
	r.expect("Scrub findings of t000")
	for _, frame := range r.frames {
		if contains(frame, "\x1b]0") || contains(frame, "\x1b[1m") {
			t.Fatalf("a control sequence reached the screen")
		}
	}
}

// contains is strings.Contains, named for the frames it is asked of.
func contains(frame, text string) bool { return strings.Contains(frame, text) }
