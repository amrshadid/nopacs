package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// routeSetup is tenant t000 with a receiver and two versions of its rule sets,
// the first running, and what the fake answers to an explanation, a dry run and
// the hit counts.
func routeSetup(f *fakeserver.Server) {
	addTenants(f, 1)
	f.AddStorage("t000", &v1.Storage{Name: "main"})
	f.AddReceiver("t000", &v1.Receiver{Name: "scanner1", Storage: "main", Type: &v1.Receiver_Dimse{Dimse: &v1.DimseReceiver{AeTitle: "ARCHIVE", Port: 11112}}})
	f.SetRouteAnswers(
		&v1.ExplainRouteResponse{Version: 2,
			Decision: &v1.RouteDecision{ArchiveRule: "lab-c", Storage: "main", ForwardRules: []string{"to-pacs"}},
			Rules: []*v1.RuleResult{
				{Phase: v1.RulePhase_RULE_PHASE_ARCHIVE, Rule: "lab-c", Truth: v1.RouteTruth_ROUTE_TRUTH_TRUE,
					DecidedBy: &v1.DecidingComparison{Comparison: "PatientName = ?", Tag: "0010,0010", Vr: "PN", Present: true, Truth: v1.RouteTruth_ROUTE_TRUTH_TRUE, Values: []string{"Doe^Jane"}}},
				{Phase: v1.RulePhase_RULE_PHASE_FORWARD, Rule: "to-pacs", Truth: v1.RouteTruth_ROUTE_TRUTH_TRUE},
			}},
		&v1.DryRunRoutesResponse{Version: 2, Same: 1, Changed: 1, Sessions: []*v1.DryRunSession{
			{SubjectLabel: "sub-a", SessionLabel: "ses-1", RecordedVersion: 1, RecordedArchiveRule: "lab-a", ArchiveRule: "lab-a", Outcome: v1.DryRunOutcome_DRY_RUN_OUTCOME_SAME},
			{SubjectLabel: "sub-b", SessionLabel: "ses-1", RecordedVersion: 1, RecordedArchiveRule: "lab-a", ArchiveRule: "lab-c", Outcome: v1.DryRunOutcome_DRY_RUN_OUTCOME_CHANGED},
		}},
		[]*v1.RouteHit{{Version: 1, Phase: v1.RulePhase_RULE_PHASE_ARCHIVE, Rule: "lab-a", Receptions: 12, LastTime: timestamp(-3)},
			{Version: 1, Phase: v1.RulePhase_RULE_PHASE_ARCHIVE, Receptions: 4, LastTime: timestamp(-1)}})
}

// makeRuleSets writes two versions through the API as the command line does:
// the program does not create them.
func makeRuleSets(t *testing.T, r *rig) {
	t.Helper()
	for i, rule := range []string{"lab-a", "lab-c"} {
		if _, err := r.m.c.client.Routes.CreateRuleSet(context.Background(), &v1.CreateRuleSetRequest{Tenant: "t000", Receiver: "scanner1", Activate: i == 0,
			RuleSet: &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: rule, Storage: "main", When: "Modality = \"MR\""}}, Comment: "version " + rule}}); err != nil {
			t.Fatalf("making a rule set: %v", err)
		}
	}
}

// openRuleSets opens the rule set versions of the receiver of tenant t000.
func openRuleSets(r *rig) {
	r.key("enter", "enter", "r", "enter", "v")
	r.expect("Rule sets of scanner1")
}

// TestRuleSetVersionsAreListedViewedAndActivated guards a receiver's rule sets:
// the versions are listed newest first with the running one marked; a version
// opens on its rules; activating asks for the version typed and moves the mark;
// and running none is a choice of its own.
func TestRuleSetVersionsAreListedViewedAndActivated(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, routeSetup)
		makeRuleSets(t, r)
		openRuleSets(r)
		r.expect("VERSION", "version lab-c", "version lab-a", "yes", "nopacs route create")
		r.key("enter")
		r.expect("Rule set scanner1@2", "Running:", "no", "Archive rule 1:", "lab-c", "Modality", "to main")
		r.key("esc")

		// Version 2 is under the cursor: activating asks for its number typed.
		activations := r.calls("ActivateRuleSet")
		r.key("a")
		r.expect("Run rule set 2 on scanner1", "Type the version")
		r.typeText("1")
		r.key("enter")
		r.expect("not done: type 2 exactly")
		if r.calls("ActivateRuleSet") != activations {
			t.Fatalf("an activation was sent with the version typed wrong")
		}
		r.key("ctrl+u")
		r.typeText("2")
		r.key("enter")
		if r.calls("ActivateRuleSet") != activations+1 {
			t.Fatalf("the activation was not sent")
		}
		r.expect("Rule sets of scanner1", "receiver scanner1 runs rule set version 2")

		// None: version 0.
		r.key("z")
		r.expect("Run no rule set on scanner1")
		r.typeText("0")
		r.key("enter")
		r.expect("receiver scanner1 runs no rule set")
	})
}

// TestExplainHidesValuesUnlessAskedAndSaysWhichSourceIsGiven guards the
// explanation of a rule set: the values each comparison saw are drawn only when
// they are asked for and then say they are audited; one source is needed, an
// instance or a file; a file is read and sent.
func TestExplainHidesValuesUnlessAskedAndSaysWhichSourceIsGiven(t *testing.T) {
	r := newRig(t, 140, 40, routeSetup)
	makeRuleSets(t, r)
	openRuleSets(r)
	r.key("x")
	r.expect("Explain rule set 2 of scanner1")
	explains := r.calls("ExplainRoute")
	// Neither source, then both.
	r.key("enter", "enter", "enter", "enter", "enter")
	r.expect("give an instance or a file, one of the two")
	r.key("tab")
	r.fill("sub-a/ses-1/scan-1", "/tmp/not-needed")
	r.key("enter", "enter", "enter", "enter")
	r.expect("give an instance or a file, one of the two")
	if r.calls("ExplainRoute") != explains {
		t.Fatalf("an explanation was asked for with no source or two")
	}
	// Values hidden by default.
	r.key("tab", "tab", "ctrl+u")
	r.key("enter", "enter", "enter", "enter")
	r.expect("Explanation by scanner1@2", "Decision:", "archive rule lab-c to main", "Forward rules:", "to-pacs",
		"archive lab-c:", "true, decided by PatientName = ? (tag 0010,0010, VR PN, present)", "Values:", "hidden")
	r.refuse("Doe^Jane")
	if got := r.fake.LastExplain(); got.GetShowValues() || got.GetInstance().GetScanLabel() != "scan-1" || got.GetVersion() != 2 {
		t.Errorf("the explanation asked: %v", got)
	}

	// Values asked for: drawn, and the screen says they were recorded.
	r.key("esc", "x")
	r.fill("sub-a/ses-1/scan-1", "", "", "", "")
	r.key("right", "enter")
	r.expect("saw Doe^Jane", "shown, and recorded in the audit log")
	if !r.fake.LastExplain().GetShowValues() {
		t.Errorf("the values were not asked for")
	}

	// A file is read by the program and sent as the receiver would receive it.
	file := filepath.Join(t.TempDir(), "x.dcm")
	if err := os.WriteFile(file, []byte("DICM"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.key("esc", "x")
	r.fill("", file, "SCANNER1")
	r.key("enter", "enter", "enter")
	r.expect("Explanation by scanner1@2")
	if got := r.fake.LastExplain(); string(got.GetFile()) != "DICM" || got.GetCallingAeTitle() != "SCANNER1" {
		t.Errorf("the explanation asked: %v", got)
	}
	// A file that is not there says so.
	r.key("esc", "x")
	r.fill("", filepath.Join(t.TempDir(), "absent.dcm"))
	r.key("enter", "enter", "enter", "enter")
	r.expect("cannot read")
}

// TestExplainShowingValuesIsRefusedToAMaskedKey guards the audited choice: a
// masked key is offered no yes, and a server that refuses a masked caller's
// request for values says why on the form.
func TestExplainShowingValuesIsRefusedToAMaskedKey(t *testing.T) {
	setup := func(f *fakeserver.Server) { routeSetup(f) }
	r := newKeyRig(t, 140, 40, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_OPERATOR}}, maskedKeySetup(setup))
	makeRuleSets(t, r)
	r.key("enter", "enter", "r", "enter", "v", "x")
	r.expect("Explain rule set 2 of scanner1", "[no]")
	r.key("tab", "tab", "tab", "tab", "right")
	r.expect("[no]")
	r.refuse("[yes]")

	// A caller the server knows to be masked, though the screen did not: refused.
	r = newRig(t, 140, 40, setup)
	makeRuleSets(t, r)
	openRuleSets(r)
	r.fake.SetCallerMasked(true)
	r.key("x")
	r.fill("sub-a/ses-1/scan-1", "", "", "", "")
	r.key("right", "enter")
	r.expect("a masked key is never answered an original value", "Explain rule set 2 of scanner1")
	r.refuse("saw Doe^Jane")
}

// TestDryRunAndHitsShowWhatTheyCount guards the two reads of a rule set: a dry
// run shows each session beside the rules that routed it and its totals, a bound
// outside 1 to 1000 refused on the form, and the hits list the receptions each rule
// routed, "(default)" for none.
func TestDryRunAndHitsShowWhatTheyCount(t *testing.T) {
	r := newRig(t, 140, 40, routeSetup)
	makeRuleSets(t, r)
	openRuleSets(r)
	r.key("d")
	r.expect("Try rule set 2 of scanner1", "Sessions")
	dry := r.calls("DryRunRoutes")
	r.key("ctrl+u")
	r.typeText("5000")
	r.key("enter")
	r.expect("sessions is a whole number from 1 to 1000")
	if r.calls("DryRunRoutes") != dry {
		t.Fatalf("a dry run was asked for with a bound out of range")
	}
	r.key("ctrl+u")
	r.typeText("100")
	r.key("enter")
	r.expect("Dry run of scanner1@2", "Result:", "1 the same, 1 changed, 0 unknown", "sub-a/ses-1:", "same; went by lab-a @1", "sub-b/ses-1:", "changed", "would go by lab-c")
	r.key("esc", "h")
	r.expect("Route hits of scanner1@2", "lab-a", "12", "(default)", "4")
}

// TestRoutingKeysFollowTheRole guards the keys of the rule sets: a reader reads the
// versions and the hits only, an operator also explains and tries them, an admin
// also activates; and the server's own refusal of an operator's activation is
// shown on the form.
func TestRoutingKeysFollowTheRole(t *testing.T) {
	open := func(role v1.Role) *rig {
		r := newKeyRig(t, 140, 40, []keyGrant{{tenant: "t000", role: role}}, routeSetup)
		makeRuleSets(t, r)
		r.key("enter", "enter", "r", "enter", "v")
		r.expect("Rule sets of scanner1")
		helpOf(r, "Rule sets of scanner1")
		return r
	}
	r := open(v1.Role_ROLE_READER)
	r.expect("hits")
	r.refuse("explain", "dry run", "activate", "run none")
	r = open(v1.Role_ROLE_OPERATOR)
	r.expect("hits", "explain", "dry run")
	r.refuse("activate", "run none")
	r = open(v1.Role_ROLE_ADMIN)
	r.expect("hits", "explain", "dry run", "activate", "run none")
}
