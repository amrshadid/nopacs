package api_test

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestReceiverDeidentification guards a receiver's choice of
// de-identification over the API: a profile version the tenant does not
// have is a precondition failure; one it has is stored and answered as the
// oneof's profile, and a receiver added without a choice is answered as
// received; with receiver.allow-as-received off, a receiver added as
// received, or changed to it, is a precondition failure naming the
// setting, while a receiver that archives as received keeps doing so and
// can still be changed otherwise; a profile version that keeps the placing
// UIDs (Retain UIDs), and a naming rule that puts a value
// into a label unhashed, are precondition failures saying why; and a mask
// may name only the oneof.
func TestReceiverDeidentification(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	withProfile := func(name string, port int32) *v1.Receiver {
		r := dimseReceiver(name, "main", port)
		r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}
		return r
	}
	o := addReceiver("lab1", withProfile("deid", 11120))(f.as(f.lab1Admin))
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "no profile version research@1") {
		t.Fatalf("a profile version the tenant does not have: %v", o.err)
	}
	store := func(name string, doc *v1.ProfileDocument) {
		t.Helper()
		v, err := profile.New(name, 1, doc, profile.Shipped())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.pipelineOf("lab1").(tenant.Profiles).CreateProfileVersion(ctx, tenant.NewProfileVersion{
			Name: name, Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum()}, f.now(), f.setupEntry(), nil); err != nil {
			t.Fatal(err)
		}
	}
	basic := []string{profile.AnnexE("basic")}
	store("research", &v1.ProfileDocument{Bases: basic})
	store("keeps-uids", &v1.ProfileDocument{Bases: append(basic, profile.AnnexE("retain-uids"))})
	for _, c := range []struct {
		profile, want string
	}{
		{"keeps-uids", "it does not remap the UIDs a file is placed by (as the Retain UIDs option does)"},
	} {
		r := withProfile("refused", 11123)
		r.GetProfile().Name = c.profile
		if o := addReceiver("lab1", r)(f.as(f.lab1Admin)); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), c.want) {
			t.Errorf("the profile %s: %v", c.profile, o.err)
		}
	}
	plainRule := withProfile("plain-labels", 11123)
	plainRule.Naming = &v1.NamingRule{Subject: "sub-{PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"}
	if o := addReceiver("lab1", plainRule)(f.as(f.lab1Admin)); o.code() != connect.CodeFailedPrecondition ||
		!strings.Contains(o.err.Error(), "the naming rule puts PatientID into a label as received") {
		t.Errorf("a naming rule with a plain placeholder: %v", o.err)
	}
	got := mustCall[*v1.AddReceiverResponse](t, "add deid", addReceiver("lab1", withProfile("deid", 11120))(f.as(f.lab1Admin))).GetReceiver()
	if p := got.GetProfile(); p.GetName() != "research" || p.GetVersion() != 1 {
		t.Errorf("answered %v", got.GetDeidentification())
	}
	plain := mustCall[*v1.AddReceiverResponse](t, "add plain", addReceiver("lab1", dimseReceiver("plain", "main", 11121))(f.as(f.lab1Admin))).GetReceiver()
	if plain.GetAsReceived() == nil {
		t.Errorf("a receiver added without a choice is answered %v", plain.GetDeidentification())
	}

	f.setSetting(tenantScope("lab1"), settings.ReceiverAllowAsReceived, "false")
	o = addReceiver("lab1", dimseReceiver("plain2", "main", 11122))(f.as(f.lab1Admin))
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), settings.ReceiverAllowAsReceived) {
		t.Errorf("a receiver as received with the setting off: %v", o.err)
	}
	set := func(r *v1.Receiver, paths ...string) outcome {
		return call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: r, UpdateMask: mask(paths...)})
	}
	if o := set(&v1.Receiver{Name: "plain", Enabled: false}, "enabled"); o.err != nil {
		t.Errorf("a receiver as received changed otherwise with the setting off: %v", o.err)
	}
	if o := set(&v1.Receiver{Name: "deid"}, "deidentification"); o.code() != connect.CodeFailedPrecondition {
		t.Errorf("a receiver changed to as received with the setting off: %v", o.err)
	}
	if o := call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", UpdateMask: mask("deidentification"),
		Receiver: &v1.Receiver{Name: "plain", Deidentification: &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}},
		Force:    true, ForceReason: "a version this test does not try over receptions"}); o.err != nil || o.res.(*v1.SetReceiverResponse).GetReceiver().GetProfile().GetName() != "research" {
		t.Errorf("a receiver changed to a profile version: %v", o.err)
	}
	if o := set(&v1.Receiver{Name: "deid", Naming: &v1.NamingRule{Subject: "sub-{PatientID}", Session: "ses-{#StudyInstanceUID}",
		Scan: "scan-{#SeriesInstanceUID}"}}, "naming"); o.code() != connect.CodeFailedPrecondition {
		t.Errorf("a receiver that de-identifies changed to a plain naming rule: %v", o.err)
	}
	if o := set(&v1.Receiver{Name: "plain"}, "profile"); o.code() != connect.CodeInvalidArgument {
		t.Errorf("a mask naming a member of the oneof: %v", o.err)
	}
	if o := set(&v1.Receiver{Name: "plain", Deidentification: &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research"}}},
		"deidentification"); o.code() != connect.CodeInvalidArgument {
		t.Errorf("a profile with no version: %v", o.err)
	}
}

// TestTwoChangesCannotCombineAPlainRuleWithAProfile guards two SetReceiver
// calls at once on a receiver that archives as received, one changing its
// naming rule to one with an unhashed placeholder and one changing it to
// de-identify: each is checked against what the other committed, so the
// receiver never ends with both, however they interleave.
func TestTwoChangesCannotCombineAPlainRuleWithAProfile(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	v, err := profile.New("research", 1, &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.pipelineOf("lab1").(tenant.Profiles).CreateProfileVersion(ctx, tenant.NewProfileVersion{
		Name: "research", Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum()}, f.now(), f.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
	set := func(r *v1.Receiver, paths ...string) outcome {
		return call(f.as(f.lab1Admin).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: r, UpdateMask: mask(paths...)})
	}
	plain := &v1.NamingRule{Subject: "sub-{PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"}
	for i := range 20 {
		name := "race" + strconv.Itoa(i)
		mustCall[*v1.AddReceiverResponse](t, "add", addReceiver("lab1", dimseReceiver(name, "main", int32(11120+i%10)))(f.as(f.lab1Admin)))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); set(&v1.Receiver{Name: name, Naming: plain}, "naming") }()
		go func() {
			defer wg.Done()
			set(&v1.Receiver{Name: name, Deidentification: &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}}, "deidentification")
		}()
		wg.Wait()
		r, err := f.pipelineOf("lab1").Receiver(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if r.Deidentifies() && len(r.PlainPlaceholders()) > 0 {
			t.Fatalf("round %d: the receiver de-identifies with a naming rule that puts %v into labels", i, r.PlainPlaceholders())
		}
		if _, err := f.pipelineOf("lab1").RemoveReceiver(ctx, name, f.setupEntry(), nil); err != nil {
			t.Fatal(err)
		}
	}
}
