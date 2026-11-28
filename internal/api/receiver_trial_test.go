package api_test

import (
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestChangeControlGatesAChangeOfAReceiversProfile guards change control:
// a receiver already running one profile version is pointed at another only
// when that version was tried over its last receptions and none of the
// files tried would be held; a version never tried, or tried with held
// files, is refused as a precondition failure saying so; a force with no
// reason, or on a change that is no change of the profile, is invalid; a
// force with a reason makes the change and the audit entry says it was
// forced, with the reason and why; a version tried clean is pointed at
// without one.
func TestChangeControlGatesAChangeOfAReceiversProfile(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	pipe := f.pipelineOf("lab1")
	for _, name := range []string{"alpha", "beta"} {
		v, err := profile.New(name, 1, &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}, profile.Shipped())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := pipe.(tenant.Profiles).CreateProfileVersion(ctx, tenant.NewProfileVersion{Name: name, Source: v.Source(),
			Expanded: v.ExpandedText(), Checksum: v.Checksum()}, f.now(), f.setupEntry(), nil); err != nil {
			t.Fatal(err)
		}
	}
	rcv := dimseReceiver("deid", "main", 11120)
	rcv.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "alpha", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add deid", addReceiver("lab1", rcv)(f.as(f.lab1Admin)))
	point := func(name string, force bool, reason string, paths ...string) outcome {
		r := &v1.Receiver{Name: "deid", Deidentification: &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: name, Version: 1}}}
		if len(paths) == 0 {
			paths = []string{"deidentification"}
		}
		return call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: r, UpdateMask: mask(paths...),
			Force: force, ForceReason: reason})
	}
	if o := point("beta", false, ""); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "was not tried") {
		t.Fatalf("pointing at a version never tried: %v", o.err)
	}
	trials := pipe.(tenant.ProfileTrials)
	beta := tenant.ProfileRef{Name: "beta", Version: 1}
	record := func(ref tenant.ProfileRef, held int64) {
		t.Helper()
		if err := trials.RecordProfileTrial(ctx, tenant.ProfileTrial{Receiver: "deid", Profile: ref, Tried: f.now(), Sessions: 2, Files: 6, Changed: 3, Held: held},
			f.setupEntry(), nil); err != nil {
			t.Fatal(err)
		}
	}
	record(beta, 2)
	if o := point("beta", false, ""); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "found 2 files it would hold") {
		t.Fatalf("pointing at a version whose trial held files: %v", o.err)
	}
	if o := point("beta", true, ""); o.code() != connect.CodeInvalidArgument {
		t.Errorf("a force with no reason: %v", o.err)
	}
	if o := point("beta", true, "x", "quiet_period"); o.code() != connect.CodeInvalidArgument {
		t.Errorf("a force on a change that is not the profile's: %v", o.err)
	}
	if o := point("beta", true, "the site's rule, tried on another receiver"); o.err != nil {
		t.Fatalf("a forced change: %v", o.err)
	}
	var forced master.AuditEntry
	for _, e := range f.tenantLog("lab1") {
		if e.Details["forced"] == "true" {
			forced = e
		}
	}
	if forced.Details["force_reason"] != "the site's rule, tried on another receiver" || forced.Details["trial"] != "beta@1:held=2" ||
		forced.Details["profile"] != "beta@1" || !strings.Contains(forced.Method, "SetReceiver") {
		t.Errorf("the audit entry of the forced change is %+v", forced)
	}
	record(tenant.ProfileRef{Name: "alpha", Version: 1}, 0)
	if o := point("alpha", false, ""); o.err != nil {
		t.Errorf("pointing at a version tried clean: %v", o.err)
	}
}

// TestTryReceiverProfileIsAnAdminsAndNeverAMaskedKeys guards the trial's
// role: a reader is refused, and so is a masked key whatever its grant.
func TestTryReceiverProfileIsAnAdminsAndNeverAMaskedKeys(t *testing.T) {
	f := newPlaceFixture(t)
	reader, _ := f.key(f.onTenant("lab1", master.RoleReader))
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	req := &v1.TryReceiverProfileRequest{Tenant: "lab1", Receiver: "deid", Profile: "alpha", ProfileVersion: 1}
	for who, c := range map[string]caller{"reader": reader, "masked admin": masked} {
		if o := call(f.as(c).receivers.TryReceiverProfile, req); o.code() != connect.CodePermissionDenied {
			t.Errorf("%s: %v, want permission denied", who, o.err)
		}
	}
}

// TestARerunIsAnAdminsAndNeverAMaskedKeys guards the re-run's role: a job
// that re-runs a profile over the archive, or previews it, is refused to an
// operator and to a masked key, and a re-run needs its reason.
func TestARerunIsAnAdminsAndNeverAMaskedKeys(t *testing.T) {
	f := newPlaceFixture(t)
	operator, _ := f.key(f.onTenant("lab1", master.RoleOperator))
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	for _, mode := range []v1.DeidJobMode{v1.DeidJobMode_DEID_JOB_MODE_RERUN, v1.DeidJobMode_DEID_JOB_MODE_RERUN_PREVIEW} {
		req := &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Deid{Deid: &v1.DeidJob{Mode: mode, Profile: "alpha", Reason: "a newer rule"}}}
		for who, c := range map[string]caller{"operator": operator, "masked admin": masked} {
			if o := call(f.as(c).jobs.CreateJob, req); o.code() != connect.CodePermissionDenied {
				t.Errorf("%s, %s: %v, want permission denied", who, mode, o.err)
			}
		}
	}
	noReason := &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Deid{Deid: &v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_RERUN, Profile: "alpha"}}}
	if o := call(f.as(f.lab1Admin).jobs.CreateJob, noReason); o.code() != connect.CodeInvalidArgument || !strings.Contains(o.err.Error(), "reason") {
		t.Errorf("a re-run with no reason: %v", o.err)
	}
}

// testTryReceiverProfile guards TryReceiverProfile's role: a key with no
// grant, a reader and an operator are refused, and an admin of a server
// that tries nothing over receptions is answered unavailable.
func testTryReceiverProfile(t *testing.T) {
	f := newPlaceFixture(t)
	try := func(c clients) outcome {
		return call(c.receivers.TryReceiverProfile, &v1.TryReceiverProfileRequest{Tenant: "lab1", Receiver: "scanner1", Profile: "research", ProfileVersion: 1})
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: try, code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: try, code: connect.CodePermissionDenied},
		{name: "admin without a server that tries", as: f.lab1Admin, call: try, code: connect.CodeUnavailable},
	})
}
