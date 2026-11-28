package api_test

import (
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// forcedEntries returns the audit entries of lab1 that record a forced
// change past change control.
func forcedEntries(f *placeFixture) []master.AuditEntry {
	var out []master.AuditEntry
	for _, e := range f.tenantLog("lab1") {
		if e.Details["forced"] == "true" {
			out = append(out, e)
		}
	}
	return out
}

// doorsFixture has the profiles alpha (basic), beta and gamma (each stricter),
// a receiver "deid" running alpha and a receiver "plain" archiving as received.
func doorsFixture(t *testing.T) *placeFixture {
	t.Helper()
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "main"))(f.as(local)))
	storeVersion(t, f.env, "alpha", basicDocument())
	for _, name := range []string{"beta", "gamma"} {
		d := basicDocument()
		d.Rules = []*v1.ProfileRule{removeRule("kvp", "KVP"), removeRule("inst", "InstitutionName")}
		if name == "gamma" {
			d.Rules = append(d.Rules, removeRule("protocol", "ProtocolName"))
		}
		storeVersion(t, f.env, name, d)
	}
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "alpha", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add deid", addReceiver("lab1", r)(f.as(local)))
	mustCall[*v1.AddReceiverResponse](t, "add plain", addReceiver("lab1", dimseReceiver("plain", "main", 11121))(f.as(local)))
	return f
}

// recordTrial stores a trial of the profile for the receiver.
func recordTrial(t *testing.T, f *placeFixture, receiver, name string, files, held int64) {
	t.Helper()
	err := f.pipelineOf("lab1").(tenant.ProfileTrials).RecordProfileTrial(ctx, tenant.ProfileTrial{Receiver: receiver,
		Profile: tenant.ProfileRef{Name: name, Version: 1}, Tried: f.now(), Sessions: 1, Files: files, Changed: files - held - 0, Held: held},
		f.setupEntry(), nil)
	if err != nil {
		t.Fatal(err)
	}
}

// TestChangeControlGivesAReceiverItsFirstProfileOnlyAfterATrial guards that a
// receiver archiving as received is gated as one changing versions is: given
// a profile never tried it is refused, and a force with a reason makes the
// change and is audited.
func TestChangeControlGivesAReceiverItsFirstProfileOnlyAfterATrial(t *testing.T) {
	f := doorsFixture(t)
	point := func(force bool, reason string) outcome {
		return call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", UpdateMask: mask("deidentification"),
			Receiver: &v1.Receiver{Name: "plain", Deidentification: &v1.Receiver_Profile{Profile: profileRef("beta")}}, Force: force, ForceReason: reason})
	}
	if o := point(false, ""); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "was not tried") {
		t.Fatalf("a first profile never tried: %v", o.err)
	}
	if o := point(true, "tried elsewhere"); o.err != nil {
		t.Fatalf("a forced first profile: %v", o.err)
	}
	if got := forcedEntries(f); len(got) != 1 || got[0].Details["force_reason"] != "tried elsewhere" {
		t.Errorf("the forced change's audit entries: %+v", got)
	}
}

// TestChangeControlAsksForFilesRead guards that a trial that read no file
// opens nothing: a receiver that received nothing cannot show a version
// safe, so the change needs a force.
func TestChangeControlAsksForFilesRead(t *testing.T) {
	f := doorsFixture(t)
	recordTrial(t, f, "deid", "beta", 0, 0)
	o := call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", UpdateMask: mask("deidentification"),
		Receiver: &v1.Receiver{Name: "deid", Deidentification: &v1.Receiver_Profile{Profile: profileRef("beta")}}})
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "read no file") {
		t.Fatalf("a trial that read no file: %v", o.err)
	}
	recordTrial(t, f, "deid", "beta", 4, 0)
	if o := call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", UpdateMask: mask("deidentification"),
		Receiver: &v1.Receiver{Name: "deid", Deidentification: &v1.Receiver_Profile{Profile: profileRef("beta")}}}); o.err != nil {
		t.Errorf("a trial that read files and held none: %v", o.err)
	}
}

// TestChangeControlCoversTheRuleSetsArchiveRules guards the side door: a
// rule set whose archive rule names a profile version not tried over the
// receiver's last receptions is refused when it is made active and when an
// existing version is activated, unless forced with a reason, which is
// audited like a receiver's; a version tried clean is taken.
func TestChangeControlCoversTheRuleSetsArchiveRules(t *testing.T) {
	f := doorsFixture(t)
	rule := func(name string) *v1.RuleSet {
		return &v1.RuleSet{ArchiveRules: []*v1.ArchiveRule{{Name: "rest", Storage: "main", Profile: profileRef(name)}}}
	}
	create := func(name string, activate, force bool, reason string) outcome {
		return call(f.as(f.lab1Admin).routes.CreateRuleSet, &v1.CreateRuleSetRequest{Tenant: "lab1", Receiver: "deid", RuleSet: rule(name),
			Activate: activate, Force: force, ForceReason: reason})
	}
	if o := create("beta", true, false, ""); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "was not tried") {
		t.Fatalf("an active rule set naming a version never tried: %v", o.err)
	}
	if o := create("beta", true, true, ""); o.code() != connect.CodeInvalidArgument {
		t.Errorf("a force with no reason: %v", o.err)
	}
	if o := create("beta", true, true, "tried on another receiver"); o.err != nil {
		t.Fatalf("a forced rule set: %v", o.err)
	}
	if got := forcedEntries(f); len(got) != 1 || got[0].Details["force_reason"] != "tried on another receiver" {
		t.Errorf("the forced rule set's audit entries: %+v", got)
	}
	// A stored version, then its activation.
	mustCall[*v1.CreateRuleSetResponse](t, "store", create("gamma", false, false, ""))
	activate := func(force bool, reason string) outcome {
		return call(f.as(f.lab1Admin).routes.ActivateRuleSet, &v1.ActivateRuleSetRequest{Tenant: "lab1", Receiver: "deid", Version: 2,
			Force: force, ForceReason: reason})
	}
	if o := activate(false, ""); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "was not tried") {
		t.Fatalf("activating a version that names a version never tried: %v", o.err)
	}
	recordTrial(t, f, "deid", "gamma", 3, 0)
	if o := activate(false, ""); o.err != nil {
		t.Errorf("activating a version tried clean: %v", o.err)
	}
}

// TestARerunRefusesAVersionLooserThanTheBase guards the base: a re-run of a
// version that does not stack over the tenant's base is refused when the job
// is made, with the loosening listed; a version that does is accepted.
func TestARerunRefusesAVersionLooserThanTheBase(t *testing.T) {
	f := baseFixture(t)
	mustCall[*v1.SetSettingResponse](t, "set the base", setBase("floor@1")(f.as(f.lab1Admin)))
	rerun := func(name string) outcome {
		return call(f.as(f.lab1Admin).jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Deid{Deid: &v1.DeidJob{
			Mode: v1.DeidJobMode_DEID_JOB_MODE_RERUN, Profile: name, Reason: "a newer rule"}}})
	}
	wantLoosens(t, "a re-run of a version looser than the base", rerun("loose"), "loosens_lower_layer")
	if o := rerun("tight"); o.err != nil {
		t.Errorf("a re-run of a version that stacks over the base: %v", o.err)
	}
}
