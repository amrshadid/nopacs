package api_test

import (
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestReleaseThroughAProfileVersion guards a release that names a profile
// version: it needs admin on the tenant (operator is refused); a version
// the tenant does not have, or one that cannot de-identify a receiver's
// files (Retain UIDs), is a precondition failure saying why; a version
// named through a receiver that archives as received, and any release of a
// record made de-identified through such a receiver, are precondition
// failures, the record still held; through a receiver that de-identifies
// the release is answered with the version, recorded in its audit entry,
// and handed to the pipeline.
func TestReleaseThroughAProfileVersion(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	storeVersion(t, f.env, "research", basicDocument())
	keeps, err := profile.New("keeps-uids", 1, &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic"), profile.AnnexE("retain-uids")}}, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.pipelineOf("lab1").(tenant.Profiles).CreateProfileVersion(ctx, tenant.NewProfileVersion{Name: "keeps-uids",
		Source: keeps.Source(), Expanded: keeps.ExpandedText(), Checksum: keeps.Checksum()}, f.now(), f.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
	r := dimseReceiver("deid", "main", 11120)
	r.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add deid", addReceiver("lab1", r)(f.as(local)))

	held := f.heldFile(t, "deid", false)
	plain := f.heldFile(t, "scanner1", false)
	deidHeld := f.heldFile(t, "deid", false)
	q, err := f.pipelineOf("lab1").QuarantinedFile(ctx, deidHeld)
	if err != nil {
		t.Fatal(err)
	}
	// A record made de-identified, held again under a new id.
	q.ID, q.Deidentified, q.Incoming = f.newID(), true, "q/"+q.ID+"-d"
	if _, err := f.pipelineOf("lab1").HoldFiles(ctx, []tenant.QuarantinedFile{q}); err != nil {
		t.Fatal(err)
	}
	release := func(id, receiver string, ref *v1.ProfileVersionRef) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.quarantine.ReleaseQuarantinedFile, &v1.ReleaseQuarantinedFileRequest{Tenant: "lab1", Id: id,
				Mode: v1.ReleaseMode_RELEASE_MODE_AS_IS, Receiver: receiver, Profile: ref})
		}
	}
	research := &v1.ProfileVersionRef{Name: "research", Version: 1}
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: release(held, "", research), code: connect.CodePermissionDenied},
		{name: "a version that is not well formed", as: f.lab1Admin, call: release(held, "", &v1.ProfileVersionRef{Name: "research"}),
			code: connect.CodeInvalidArgument, msg: "profile"},
		{name: "a version the tenant does not have", as: f.lab1Admin, call: release(held, "", &v1.ProfileVersionRef{Name: "research", Version: 2}),
			code: connect.CodeFailedPrecondition, msg: "no profile version research@2"},
		{name: "a version that keeps the placing UIDs", as: f.lab1Admin, call: release(held, "", &v1.ProfileVersionRef{Name: "keeps-uids", Version: 1}),
			code: connect.CodeFailedPrecondition, msg: "Retain UIDs"},
		{name: "a version through a receiver that archives as received", as: f.lab1Admin, call: release(plain, "", research),
			code: connect.CodeFailedPrecondition, msg: "only through a receiver that de-identifies"},
		{name: "a record made de-identified through a receiver that archives as received", as: f.lab1Operator, call: release(q.ID, "scanner1", nil),
			code: connect.CodeFailedPrecondition, msg: "released only through one that does"},
		{name: "released through the version", as: f.lab1Admin, call: release(held, "", research),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: held, details: map[string]string{"mode": "RELEASE_MODE_AS_IS",
				"receiver": "deid", "profile": "research@1"}},
			check: func(t *testing.T, o outcome) {
				got := o.res.(*v1.ReleaseQuarantinedFileResponse).GetFile()
				if got.GetState() != v1.QuarantineState_QUARANTINE_STATE_RELEASED || got.GetReleaseProfile().GetName() != "research" ||
					got.GetReleaseProfile().GetVersion() != 1 {
					t.Errorf("%v", got)
				}
				if released, _ := f.parts.decisions(); !slices.Contains(released, "lab1/"+held) {
					t.Errorf("the release was not handed to the pipeline: %v", released)
				}
			}},
	})
	for _, id := range []string{plain, q.ID} {
		if still, err := f.pipelineOf("lab1").QuarantinedFile(ctx, id); err != nil || still.State != tenant.QuarantineHeld {
			t.Errorf("record %s after a refused release: %s (%v)", strings.TrimSpace(id), still.State, err)
		}
	}
}
