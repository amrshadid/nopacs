package tui

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// retentionSetup is a tenant with a storage, and two sessions due, one held.
func retentionSetup(f *fakeserver.Server) {
	addTenants(f, 1)
	f.AddStorage("t000", &v1.Storage{Name: "main"})
	f.AddDueSession("t000", &v1.DueSession{SubjectLabel: "sub-1", SessionLabel: "ses-1", Rule: "seven-years", Action: v1.RetentionAction_RETENTION_ACTION_DELETE,
		DueTime: timestamppb.New(fixedNow.AddDate(0, -1, 0)), Instances: 120, Bytes: 50 << 20})
	f.AddDueSession("t000", &v1.DueSession{SubjectLabel: "sub-2", SessionLabel: "ses-1", Rule: "seven-years", Action: v1.RetentionAction_RETENTION_ACTION_DELETE,
		DueTime: timestamppb.New(fixedNow.AddDate(0, -1, 0)), Held: true, Instances: 3, Bytes: 1024})
}

// addSevenYears adds the rule the retention tests work on, through the form.
func addSevenYears(r *rig) {
	r.key("a")
	r.expect("Add a retention rule")
	r.typeText("seven-years")
	r.key("tab", "tab", "tab", "tab")
	r.typeText("7y")
	r.key("tab")
	r.typeText("10y")
	r.key("tab", "right", "enter")
}

// TestRetentionRulesAddChangeRemove guards the rule screens: a rule is added
// from a form with its periods and action, the list shows it, a change names
// the whole rule as the form shows it, and removal asks for the rule's name to
// be typed first.
func TestRetentionRulesAddChangeRemove(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, retentionSetup)
		r.key("enter", "enter", "i")
		r.expect("Retention rules of t000", "no rule yet")
		addSevenYears(r)
		r.expect("Retention rules of t000", "retention rule seven-years added", "seven-years", "every session", "7y", "10y", "delete")

		// A change: the form holds what the rule has; the dispose period moves.
		r.key("e")
		r.expect("Change rule seven-years")
		r.key("tab", "tab", "tab", "tab", "tab", "ctrl+u")
		r.typeText("12y")
		r.key("tab", "enter")
		r.expect("retention rule seven-years changed", "12y")
		r.refuse("10y")
		// A period that is not one is refused before anything is sent.
		before := r.calls("SetRetentionRule")
		r.key("e", "tab", "tab", "tab", "tab", "ctrl+u")
		r.typeText("seven")
		r.key("tab", "enter", "enter")
		r.expect("Keep at least is not a period")
		if r.calls("SetRetentionRule") != before {
			t.Fatalf("a change with a bad period was sent")
		}
		r.key("esc")

		// Removal needs the name.
		r.key("d")
		r.expect("Remove rule seven-years", "stays kept until the tenant's notice")
		removes := r.calls("RemoveRetentionRule")
		r.key("enter")
		r.expect("not done: type seven-years exactly")
		r.typeText("seven-year")
		r.key("enter")
		if r.calls("RemoveRetentionRule") != removes {
			t.Fatalf("a removal was sent with the name half typed")
		}
		r.typeText("s")
		r.key("enter")
		if r.calls("RemoveRetentionRule") != removes+1 {
			t.Fatalf("the removal was not sent")
		}
		r.expect("retention rule seven-years removed")
		r.refuse("12y")
	})
}

// TestDisposeShowsItsPreviewAndNeedsTheRuleNameTyped guards a disposal: the
// reason comes first, then the preview lists what would be deleted with the
// token, then the rule's name must be typed, and only then is the job queued;
// data that fell due after the preview makes the server refuse the token.
func TestDisposeShowsItsPreviewAndNeedsTheRuleNameTyped(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, retentionSetup)
		r.key("enter", "enter", "i")
		addSevenYears(r)
		r.key("x")
		r.expect("Dispose of what is due under seven-years", "Nothing is deleted until you confirm it there")
		r.key("enter")
		r.expect("say why the sessions are disposed of")
		if r.calls("Dispose") != 0 {
			t.Fatalf("a preview was asked for with no reason")
		}
		r.typeText("retention period over")
		r.key("enter")
		r.expect("Dispose under seven-years", "Sessions:", "1 listed", "1 held and skipped", "Confirm token:", "dp.seven-years.1", "sub-1/ses-1")
		r.refuse("sub-2")
		if got := r.calls("Dispose"); got != 1 {
			t.Fatalf("Dispose was called %d times for the preview, want 1", got)
		}

		// A session falls due after the preview: the server refuses its token.
		r.key("c")
		r.expect("Delete 1 sessions under seven-years", "exactly the sessions the preview listed")
		r.fake.AddDueSession("t000", &v1.DueSession{SubjectLabel: "sub-3", SessionLabel: "ses-1", Rule: "seven-years", DueTime: timestamp(-1)})
		r.key("enter")
		r.expect("not done: type seven-years exactly")
		r.typeText("seven-years")
		r.key("enter")
		r.expect("retention dispose: the confirm token does not match")
		if got := r.calls("Dispose"); got != 2 {
			t.Fatalf("Dispose was called %d times, want 2 (the preview and the refused token)", got)
		}
		r.key("esc", "esc")
		r.key("x")
		r.typeText("retention period over")
		r.key("enter")
		r.expect("dp.seven-years.2", "2 listed")
		r.key("c")
		r.typeText("seven-years")
		r.key("enter")
		r.expect("Retention rules of t000", "disposal queued as job job_1")
		if dj := r.fake.Job("t000", "job_1").GetDispose(); dj.GetRule() != "seven-years" || dj.GetReason() != "retention period over" || dj.GetSessions() != 2 {
			t.Errorf("the dispose job: %v", dj)
		}
	})
}

// TestDueListAndRetentionClock guards the due sessions, held ones marked, and
// the clock: the retention state says it is paused, and the confirmation takes
// the time the person typed with its offset, refusing one that is not a time.
func TestDueListAndRetentionClock(t *testing.T) {
	r := newRig(t, 120, 30, func(f *fakeserver.Server) {
		retentionSetup(f)
		f.SetRetentionStatus("t000", &v1.RetentionStatus{Rules: 1, SessionsDue: 2, SessionsDueHeld: 1, Paused: true, PauseReason: "clock jumped",
			ClockStep: durationpb.New(72 * time.Hour), ActiveHolds: 1})
	})
	r.key("enter", "enter", "i")
	addSevenYears(r)
	r.key("u")
	r.expect("Sessions due in t000 under seven-years", "sub-1/ses-1", "sub-2/ses-1", "held", "50.0 MiB")
	r.key("esc", "t")
	r.expect("Retention of t000", "Paused:", "yes: clock jumped", "Clock step seen:", "3d", "Active holds:")
	r.key("c")
	r.expect("Confirm the time for t000", "type it with its offset")
	clocks := r.calls("ConfirmClock")
	r.typeText("noon")
	r.key("tab", "enter")
	r.expect("type a time with its offset")
	r.key("up", "ctrl+u")
	r.typeText("2026-11-17T12:01:00Z")
	r.key("enter", "enter")
	r.expect("say why the time is right")
	r.typeText("read from the site clock")
	r.key("enter")
	if r.calls("ConfirmClock") != clocks+1 {
		t.Fatalf("the confirmation was not sent")
	}
	r.expect("Retention of t000", "time confirmed; retention resumes", "Paused:", "no")
}

// TestHoldsPlaceReleaseAndNotServed guards the hold screens: the list and a
// hold's detail with its reason; placing and releasing on a server that does
// not serve them show one plain line; once served, a hold needs a reason and
// the release needs the hold's id typed.
func TestHoldsPlaceReleaseAndNotServed(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) {
			retentionSetup(f)
			f.AddHold("t000", &v1.Hold{Id: "hold_a", Target: &v1.HoldTarget{SubjectLabel: "sub-1"}, Reason: "litigation 114", State: v1.HoldState_HOLD_STATE_ACTIVE,
				PlacedByKeyId: "k1", PlaceTime: timestamp(-5), Storages: []string{"main"}})
		})
		r.key("enter", "enter", "l")
		r.expect("Holds of t000", "hold_a", "sub-1", "active", "litigation 114")
		r.key("enter")
		r.expect("Hold hold_a", "Covers:", "sub-1", "Reason:", "litigation 114", "Record on:", "main")
		r.key("esc")

		// Not served yet: one plain line.
		r.key("p")
		r.expect("Place a hold in t000")
		r.key("tab")
		r.typeText("sub-2")
		r.key("tab")
		r.typeText("audit")
		r.key("enter")
		r.expect("hold place: not served: this server does not serve it yet; nothing was changed")
		r.refuse("internal error")
		r.key("esc")

		r.fake.ServeHolds()
		// A hold with no reason is refused before anything is sent.
		r.key("p")
		places := r.calls("PlaceHold")
		r.key("enter", "enter", "enter")
		r.expect("say why the hold is placed")
		if r.calls("PlaceHold") != places {
			t.Fatalf("a hold was placed with no reason")
		}
		r.key("up", "up", "right", "tab", "tab")
		r.typeText("whole tenant freeze")
		r.key("enter")
		if r.calls("PlaceHold") != places+1 {
			t.Fatalf("the hold was not placed")
		}
		r.expect("Holds of t000", "placed on whole tenant")

		// Release: the id typed, then a reason.
		r.key("r")
		r.expect("Release hold hold_1")
		releases := r.calls("ReleaseHold")
		r.key("enter", "enter")
		r.expect("not done: type hold_1 exactly")
		r.key("up")
		r.typeText("hold_1")
		r.key("enter", "enter")
		r.expect("say why the hold is released")
		if r.calls("ReleaseHold") != releases {
			t.Fatalf("a hold was released with no reason")
		}
		r.typeText("closed")
		r.key("enter")
		if r.calls("ReleaseHold") != releases+1 {
			t.Fatalf("the release was not sent")
		}
		r.expect("hold hold_1 released")
	})
}
