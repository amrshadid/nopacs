package api_test

import (
	"context"
	"slices"
	"strconv"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// correctionArchive is lab1 with three public corpus files archived (two
// subjects: one MR instance, and two CT instances of one study), and the
// labels of the subject whose instances are fewest.
type correctionArchive struct {
	*placeFixture
	a *archive
	// subject is the MR file's subject; others the CT subject's label.
	subject, other string
	// instances counts the subject's instances, keys their objects.
	instances int
	keys      []string
}

// newCorrectionArchive returns lab1 with the corpus files archived.
func newCorrectionArchive(t *testing.T) *correctionArchive {
	t.Helper()
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{corpusFile(t, "MR_small.dcm"), corpusFile(t, "ct2n-6293.dcm"), corpusFile(t, "ct5n-2062.dcm")})
	a.settle(t)
	bySubject := map[string][]string{}
	for _, p := range a.scans(t) {
		for _, h := range a.held(t, p) {
			bySubject[p.Subject] = append(bySubject[p.Subject], h.Key)
		}
	}
	if len(bySubject) != 2 {
		t.Fatalf("the archive holds %d subjects, want 2", len(bySubject))
	}
	c := &correctionArchive{placeFixture: f, a: a}
	for s, keys := range bySubject {
		if len(keys) == 1 {
			c.subject, c.keys, c.instances = s, keys, 1
		} else {
			c.other = s
		}
	}
	return c
}

// subjectTarget names a whole subject.
func subjectTarget(label string) *v1.CorrectionTarget {
	return &v1.CorrectionTarget{SubjectLabel: label}
}

// deleteCall is a call of Delete.
func deleteCall(target *v1.CorrectionTarget, reason, confirm string, validateOnly bool) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.corrections.Delete, &v1.DeleteRequest{Tenant: "lab1", Target: target, Reason: reason, Confirm: confirm, ValidateOnly: validateOnly})
	}
}

// deleteSubject previews and deletes a subject as lab1's admin and returns
// the delete.
func (c *correctionArchive) deleteSubject(t *testing.T, label string) *v1.Correction {
	t.Helper()
	prev := mustCall[*v1.DeleteResponse](t, "preview", deleteCall(subjectTarget(label), "consent withdrawn", "", true)(c.as(c.lab1Admin)))
	res := mustCall[*v1.DeleteResponse](t, "delete", deleteCall(subjectTarget(label), "consent withdrawn", prev.GetPreview().GetConfirm(), false)(c.as(c.lab1Admin)))
	return res.GetCorrection()
}

// subjectAnswers reports whether the catalog answers the subject.
func (c *correctionArchive) subjectAnswers(t *testing.T, label string) bool {
	t.Helper()
	_, err := c.as(local).catalog.GetSubject(context.Background(), &v1.GetSubjectRequest{Tenant: "lab1", SubjectLabel: label})
	switch {
	case err == nil:
		return true
	case connect.CodeOf(err) == connect.CodeNotFound:
		return false
	}
	t.Fatal(err)
	return false
}

// testDelete guards Delete: admin on the tenant; every invalid line,
// recorded in the tenant's log with the target's path (a label that is not
// one recorded as "invalid"); a subject the tenant does not hold is not
// found; the preview hides nothing and answers the counts, the storage and
// what it will do, the labels held after the purge and a token of at most
// 128 bytes; a token this server did not make, or made for another target,
// is refused; the delete with the token hides the subject alone, keeps its
// bytes until the purge, and writes one entry naming the correction; the
// same token again answers the same correction, and with another target is
// refused; override_retention is refused as not served while no retention
// rule exists, its entry recording what was asked.
func testDelete(t *testing.T) {
	c := newCorrectionArchive(t)
	var confirm, id string
	c.run(t, []tcase{
		{name: "no grant", as: c.noGrant, call: deleteCall(subjectTarget(c.subject), "x", "", true), code: connect.CodePermissionDenied},
		{name: "operator", as: c.lab1Operator, call: deleteCall(subjectTarget(c.subject), "x", "", true), code: connect.CodePermissionDenied},
		{name: "no reason", as: c.lab1Admin, call: deleteCall(subjectTarget(c.subject), "", "", true), code: connect.CodeInvalidArgument, msg: "reason",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "-", details: map[string]string{"target": c.subject, "validate_only": "true"}}},
		{name: "no token", as: c.lab1Admin, call: deleteCall(subjectTarget(c.subject), "x", "", false), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "a token with a preview", as: c.lab1Admin, call: deleteCall(subjectTarget(c.subject), "x", "c2.1.00", true), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "a scan without its session", as: c.lab1Admin, call: deleteCall(&v1.CorrectionTarget{SubjectLabel: c.subject, ScanLabel: "T1"}, "x", "", true),
			code: connect.CodeInvalidArgument, msg: "target"},
		{name: "a label that is not one", as: c.lab1Admin, call: deleteCall(subjectTarget("Smith^John /x"), "x", "", true), code: connect.CodeInvalidArgument,
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, details: map[string]string{"target": "invalid"}}},
		{name: "a subject the tenant does not hold", as: c.lab1Admin, call: deleteCall(subjectTarget("nobody"), "x", "", true), code: connect.CodeNotFound},
		{name: "preview", as: c.lab1Admin, call: deleteCall(subjectTarget(c.subject), "consent withdrawn", "", true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "-", details: map[string]string{"validate_only": "true", "instances": "1",
				"subjects": "1", "reason": "consent withdrawn", "target": c.subject}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.DeleteResponse)
				p := r.GetPreview()
				confirm = p.GetConfirm()
				if r.GetCorrection() != nil || confirm == "" || len(confirm) > 128 || p.GetCounts().GetInstances() != 1 || !slices.Equal(p.GetStorages(), []string{"main"}) ||
					p.GetPurgeTime() == nil || len(p.GetStoragePurges()) != 1 || p.GetStoragePurges()[0].GetExpected() != v1.ObjectDisposition_OBJECT_DISPOSITION_REMOVED ||
					!slices.Equal(p.GetHeldAfterPurge(), []string{c.subject}) {
					t.Errorf("the preview is %v", r)
				}
				if !c.subjectAnswers(t, c.subject) {
					t.Error("a preview hid the subject")
				}
			}},
		{name: "a token this server did not make", as: c.lab1Admin, call: deleteCall(subjectTarget(c.subject), "x", "c2.1700000000000000."+
			"00000000000000000000000000000000000000000000000000000000000000ff", false), code: connect.CodeFailedPrecondition},
		{name: "a token of another target", as: c.lab1Admin, call: func(cl clients) outcome { return deleteCall(subjectTarget(c.other), "x", confirm, false)(cl) },
			code: connect.CodeFailedPrecondition},
		{name: "delete", as: c.lab1Admin, call: func(cl clients) outcome {
			return deleteCall(subjectTarget(c.subject), "consent withdrawn", confirm, false)(cl)
		},
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"validate_only": "false", "instances": "1", "reason": "consent withdrawn"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.DeleteResponse)
				cor := r.GetCorrection()
				id = cor.GetId()
				if cor.GetState() != v1.CorrectionState_CORRECTION_STATE_COMMITTED || cor.GetKind() != v1.CorrectionKind_CORRECTION_KIND_DELETE ||
					cor.GetReason() != "consent withdrawn" || cor.GetCommitTime() == nil || cor.GetPurgeTime() == nil || r.GetPreview().GetConfirm() != "" ||
					cor.GetOrigin() != v1.CorrectionOrigin_CORRECTION_ORIGIN_PERSON || len(cor.GetHolds()) == 0 {
					t.Errorf("the delete answered %v", r)
				}
				if c.subjectAnswers(t, c.subject) {
					t.Error("the deleted subject still answers")
				}
				if !c.subjectAnswers(t, c.other) {
					t.Error("the other subject no longer answers")
				}
				for _, k := range c.keys {
					if len(c.a.object(t, k)) == 0 {
						t.Errorf("the deleted subject's object %s is gone before its purge", k)
					}
				}
				c.flush()
				var en []master.AuditEntry
				for _, e := range c.tenantLog("lab1") {
					if e.RequestID == o.requestID {
						en = append(en, e)
					}
				}
				if len(en) != 1 || en[0].Object != id {
					t.Errorf("the delete's entries are %+v, want one naming %s", en, id)
				}
			}},
		{name: "the same token again", as: c.lab1Admin, call: func(cl clients) outcome {
			return deleteCall(subjectTarget(c.subject), "consent withdrawn", confirm, false)(cl)
		},
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.DeleteResponse).GetCorrection().GetId(); got != id {
					t.Errorf("a repeat answered %s, the first %s", got, id)
				}
			}},
		{name: "the used token with another target", as: c.lab1Admin, call: func(cl clients) outcome {
			return deleteCall(subjectTarget(c.other), "consent withdrawn", confirm, false)(cl)
		}, code: connect.CodeFailedPrecondition},
		{name: "override_retention", as: c.lab1Admin, call: func(cl clients) outcome {
			return call(cl.corrections.Delete, &v1.DeleteRequest{Tenant: "lab1", Target: subjectTarget(c.other), Reason: "consent withdrawn",
				ValidateOnly: true, OverrideRetention: true, OverrideReason: "the participant asked"})
		}, code: connect.CodePermissionDenied, msg: "only an admin on the installation, or the local operator",
			audit: &wantAudit{outcome: master.OutcomeRefused, object: "-", tenant: "lab1", details: map[string]string{"override_retention": "true",
				"override_reason": "the participant asked", "reason": "consent withdrawn"}}},
		{name: "override_retention by the local operator", as: local, call: func(cl clients) outcome {
			return call(cl.corrections.Delete, &v1.DeleteRequest{Tenant: "lab1", Target: subjectTarget(c.other), Reason: "consent withdrawn",
				ValidateOnly: true, OverrideRetention: true, OverrideReason: "the participant asked"})
		}, audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"override_retention": "true",
			"override_reason": "the participant asked"}}},
	})
}

// undoCall is a call of UndoCorrection.
func undoCall(id, reason string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.corrections.UndoCorrection, &v1.UndoCorrectionRequest{Tenant: "lab1", Id: id, Reason: reason})
	}
}

// testUndoCorrection guards UndoCorrection: admin on the tenant; the id
// and the reason are required; an id of another form, or one no correction
// has, is not found; the undo shows the subject again, ends the delete
// undone with its reason and no purge time or holds, and a repeat answers
// it as it stands.
func testUndoCorrection(t *testing.T) {
	c := newCorrectionArchive(t)
	d := c.deleteSubject(t, c.subject)
	c.run(t, []tcase{
		{name: "operator", as: c.lab1Operator, call: undoCall(d.GetId(), "x"), code: connect.CodePermissionDenied},
		{name: "no id", as: c.lab1Admin, call: undoCall("", "x"), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "no reason", as: c.lab1Admin, call: undoCall(d.GetId(), ""), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "an id of another form", as: c.lab1Admin, call: undoCall("nope", "x"), code: connect.CodeNotFound, msg: "no correction"},
		{name: "an id no correction has", as: c.lab1Admin, call: undoCall(correctionULID, "x"), code: connect.CodeNotFound, msg: "no correction"},
		{name: "undo", as: c.lab1Admin, call: undoCall(d.GetId(), "wrong subject"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: d.GetId(), details: map[string]string{"reason": "wrong subject"}},
			check: func(t *testing.T, o outcome) {
				cor := o.res.(*v1.UndoCorrectionResponse).GetCorrection()
				if cor.GetState() != v1.CorrectionState_CORRECTION_STATE_UNDONE || cor.GetUndoReason() != "wrong subject" || cor.GetEndTime() == nil ||
					cor.GetPurgeTime() != nil || len(cor.GetHolds()) != 0 {
					t.Errorf("the undo answered %v", cor)
				}
				if !c.subjectAnswers(t, c.subject) {
					t.Error("the undone subject does not answer")
				}
			}},
		{name: "undo again", as: c.lab1Admin, call: undoCall(d.GetId(), "again"), check: func(t *testing.T, o outcome) {
			if o.res.(*v1.UndoCorrectionResponse).GetCorrection().GetUndoReason() != "wrong subject" {
				t.Error("a repeat changed the undo")
			}
		}},
	})
}

// purgeCall is a call of PurgeCorrection.
func purgeCall(id string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.corrections.PurgeCorrection, &v1.PurgeCorrectionRequest{Tenant: "lab1", Id: id})
	}
}

// purged reports whether a correction reached purged.
func purged(c *v1.Correction) bool { return c.GetState() == v1.CorrectionState_CORRECTION_STATE_PURGED }

// testPurgeCorrection guards PurgeCorrection: admin on the tenant; an
// undone delete is refused as a precondition; the purge starts at once,
// before the grace ends, runs in the background to purged, says per
// storage what it removed, counts every object it owns as removed or kept,
// and leaves no byte of the subject on the storage; a repeat answers it as
// it stands.
func testPurgeCorrection(t *testing.T) {
	c := newCorrectionArchive(t)
	d := c.deleteSubject(t, c.subject)
	undone := c.deleteSubject(t, c.other)
	mustCall[*v1.UndoCorrectionResponse](t, "undo", undoCall(undone.GetId(), "kept")(c.as(c.lab1Admin)))
	c.run(t, []tcase{
		{name: "reader", as: c.lab1Reader, call: purgeCall(d.GetId()), code: connect.CodePermissionDenied},
		{name: "operator", as: c.lab1Operator, call: purgeCall(d.GetId()), code: connect.CodePermissionDenied},
		{name: "no id", as: c.lab1Admin, call: purgeCall(""), code: connect.CodeInvalidArgument},
		{name: "unknown", as: c.lab1Admin, call: purgeCall(correctionULID), code: connect.CodeNotFound},
		{name: "undone", as: c.lab1Admin, call: purgeCall(undone.GetId()), code: connect.CodeFailedPrecondition},
		{name: "purge", as: c.lab1Admin, call: purgeCall(d.GetId()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: d.GetId()},
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.PurgeCorrectionResponse).GetCorrection().GetState(); s != v1.CorrectionState_CORRECTION_STATE_PURGING &&
					s != v1.CorrectionState_CORRECTION_STATE_PURGED {
					t.Errorf("the purge answered %v", s)
				}
				done := c.waitCorrection(t, d.GetId(), purged)
				if len(done.GetDispositions()) != 1 || done.GetDispositions()[0].GetObjectsRemoved() == 0 || done.GetDispositions()[0].GetObjectsHidden() != 0 ||
					done.GetCounts().GetObjectsOwned() != done.GetCounts().GetObjectsRemoved()+done.GetCounts().GetObjectsKept() {
					t.Errorf("the purge ended %v", done)
				}
				for _, k := range c.keys {
					if _, err := c.a.store.Read(context.Background(), k, 0, -1); fault.KindOf(err) != fault.NotFound {
						t.Errorf("the purged object %s reads %v", k, err)
					}
				}
			}},
		{name: "purge again", as: c.lab1Admin, call: purgeCall(d.GetId()), check: func(t *testing.T, o outcome) {
			if !purged(o.res.(*v1.PurgeCorrectionResponse).GetCorrection()) {
				t.Error("a repeat did not answer the purge as it stands")
			}
		}},
	})
}

// allowCall is a call of AllowArrivals.
func allowCall(id string, v *v1.HeldValue) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.corrections.AllowArrivals, &v1.AllowArrivalsRequest{Tenant: "lab1", Id: id, Reason: "consent given again", Value: v})
	}
}

// testAllowArrivals guards AllowArrivals: admin on the tenant; a value is
// required, of a kind the contract names; a delete not yet purged is
// refused (the entry names the value's kind, never the value); once purged
// the delete holds its subject's label first, then its UIDs; a value it
// does not hold is refused; each value allowed leaves the holds, and once
// none is left the delete's arrivals are allowed, and asked again answer
// it as it stands.
func testAllowArrivals(t *testing.T) {
	c := newCorrectionArchive(t)
	d := c.deleteSubject(t, c.subject)
	subject := &v1.HeldValue{Kind: v1.HeldValueKind_HELD_VALUE_KIND_SUBJECT, Value: c.subject}
	c.run(t, []tcase{
		{name: "operator", as: c.lab1Operator, call: allowCall(d.GetId(), subject), code: connect.CodePermissionDenied},
		{name: "no value", as: c.lab1Admin, call: allowCall(d.GetId(), nil), code: connect.CodeInvalidArgument, msg: "value"},
		{name: "an unspecified kind", as: c.lab1Admin, call: allowCall(d.GetId(), &v1.HeldValue{Value: c.subject}), code: connect.CodeInvalidArgument, msg: "value.kind"},
		{name: "not yet purged", as: c.lab1Admin, call: allowCall(d.GetId(), subject), code: connect.CodeFailedPrecondition,
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: d.GetId(), details: map[string]string{"value_kind": "HELD_VALUE_KIND_SUBJECT"}}},
	})
	mustCall[*v1.PurgeCorrectionResponse](t, "purge", purgeCall(d.GetId())(c.as(c.lab1Admin)))
	done := c.waitCorrection(t, d.GetId(), purged)
	holds := done.GetHolds()
	if len(holds) < 3 || holds[0].GetKind() != v1.HeldValueKind_HELD_VALUE_KIND_SUBJECT || holds[0].GetValue() != c.subject {
		t.Fatalf("the purged delete holds %v", holds)
	}
	c.run(t, []tcase{
		{name: "a value it does not hold", as: c.lab1Admin, call: allowCall(d.GetId(), &v1.HeldValue{Kind: v1.HeldValueKind_HELD_VALUE_KIND_SUBJECT, Value: "other"}),
			code: connect.CodeFailedPrecondition},
		{name: "allow the subject", as: c.lab1Admin, call: allowCall(d.GetId(), subject),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: d.GetId(), details: map[string]string{"reason": "consent given again"}},
			check: func(t *testing.T, o outcome) {
				cor := o.res.(*v1.AllowArrivalsResponse).GetCorrection()
				if len(cor.GetHolds()) != len(holds)-1 || cor.GetArrivalsAllowed() {
					t.Errorf("after one value, the delete holds %v (allowed %v)", cor.GetHolds(), cor.GetArrivalsAllowed())
				}
			}},
	})
	for _, h := range holds[1:] {
		mustCall[*v1.AllowArrivalsResponse](t, "allow "+h.GetKind().String(), allowCall(d.GetId(), h)(c.as(c.lab1Admin)))
	}
	c.run(t, []tcase{{name: "every value allowed, asked again", as: c.lab1Admin, call: allowCall(d.GetId(), subject), check: func(t *testing.T, o outcome) {
		if cor := o.res.(*v1.AllowArrivalsResponse).GetCorrection(); !cor.GetArrivalsAllowed() || len(cor.GetHolds()) != 0 {
			t.Errorf("the delete is %v", cor)
		}
	}}})
}

// relabelCall is a call of Relabel.
func relabelCall(target *v1.CorrectionTarget, to string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.corrections.Relabel, &v1.RelabelRequest{Tenant: "lab1", Target: target, ToLabel: to, Reason: "wrong subject id at the scanner"})
	}
}

// testRelabel guards Relabel: admin on the tenant; an instance or a new
// label that is not one is invalid; a label another subject holds is a
// conflict; the relabel is answered at once and commits in the background,
// the subject then under its new label alone; the same relabel asked again
// answers the same correction.
func testRelabel(t *testing.T) {
	c := newCorrectionArchive(t)
	var id string
	c.run(t, []tcase{
		{name: "operator", as: c.lab1Operator, call: relabelCall(subjectTarget(c.subject), "sub-renamed"), code: connect.CodePermissionDenied},
		{name: "an instance", as: c.lab1Admin, call: relabelCall(&v1.CorrectionTarget{SubjectLabel: c.subject, SessionLabel: "E", ScanLabel: "T", InstanceId: "2.25.1"}, "x"),
			code: connect.CodeInvalidArgument, msg: "instance"},
		{name: "a new label that is not one", as: c.lab1Admin, call: relabelCall(subjectTarget(c.subject), "a/b"), code: connect.CodeInvalidArgument, msg: "to_label"},
		{name: "a label held", as: c.lab1Admin, call: relabelCall(subjectTarget(c.subject), c.other), code: connect.CodeAlreadyExists},
		{name: "relabel", as: c.lab1Admin, call: relabelCall(subjectTarget(c.subject), "sub-renamed"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"target": c.subject, "to_label": "sub-renamed"}},
			check: func(t *testing.T, o outcome) {
				cor := o.res.(*v1.RelabelResponse).GetCorrection()
				id = cor.GetId()
				if cor.GetKind() != v1.CorrectionKind_CORRECTION_KIND_RELABEL || cor.GetToLabel() != "sub-renamed" {
					t.Errorf("the relabel answered %v", cor)
				}
				c.waitCorrection(t, id, func(cor *v1.Correction) bool { return cor.GetState() == v1.CorrectionState_CORRECTION_STATE_COMMITTED })
				if c.subjectAnswers(t, c.subject) || !c.subjectAnswers(t, "sub-renamed") {
					t.Error("the subject is not under its new label alone")
				}
			}},
		{name: "the same relabel again", as: c.lab1Admin, call: relabelCall(subjectTarget(c.subject), "sub-renamed"), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.RelabelResponse).GetCorrection().GetId(); got != id {
				t.Errorf("a repeat answered %s, the first %s", got, id)
			}
		}},
	})
}

// replaceCall is a call of Replace.
func replaceCall(id string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.corrections.Replace, &v1.ReplaceRequest{Tenant: "lab1", QuarantinedFile: id, Reason: "corrected file sent again"})
	}
}

// testReplace guards Replace: admin on the tenant; a file of another
// reason is refused; the corrected file a sender sent again, quarantined
// as a different file, replaces the archived instance as revision 2, the
// entry recording the old and new checksums; ResolveInstances then answers
// revision 1 superseded by it, its bytes kept, and revision 2 current, each
// with the checksum served now; the same file again answers the same
// correction.
func testReplace(t *testing.T) {
	c := newCorrectionArchive(t)
	b := corpusFile(t, "MR_small.dcm")
	b[len(b)-7] ^= 0x5a // a pixel changed: the corrected file, same UIDs
	held := c.a.sendAgain(t, b, v1.QuarantineReason_QUARANTINE_REASON_DIFFERENT_FILE)
	other := c.heldFile(t, "scanner1", false)
	var id string
	c.run(t, []tcase{
		{name: "operator", as: c.lab1Operator, call: replaceCall(held.GetId()), code: connect.CodePermissionDenied},
		{name: "no file", as: c.lab1Admin, call: replaceCall(""), code: connect.CodeInvalidArgument, msg: "quarantined_file"},
		{name: "unknown", as: c.lab1Admin, call: replaceCall("nope"), code: connect.CodeNotFound},
		{name: "another reason", as: c.lab1Admin, call: replaceCall(other), code: connect.CodeFailedPrecondition, msg: "different file"},
		{name: "replace", as: c.lab1Admin, call: replaceCall(held.GetId()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"quarantined_file": held.GetId(), "old_sha256": c.a.sha(t, c.keys[0]),
				"new_sha256": held.GetSha256(), "target": "*"}},
			check: func(t *testing.T, o outcome) {
				cor := o.res.(*v1.ReplaceResponse).GetCorrection()
				id = cor.GetId()
				if cor.GetState() != v1.CorrectionState_CORRECTION_STATE_COMMITTED || cor.GetNewRevision() != 2 || cor.GetOldRevision() != 1 ||
					cor.GetNewSha256() != held.GetSha256() || cor.GetQuarantinedFile() != held.GetId() {
					t.Errorf("the replace answered %v", cor)
				}
				res := mustCall[*v1.ResolveInstancesResponse](t, "resolve", call(c.as(c.lab1Reader).corrections.ResolveInstances, &v1.ResolveInstancesRequest{Tenant: "lab1",
					Members: []*v1.InstanceRef{{InstanceId: held.GetSopInstanceUid(), Revision: 1}, {InstanceId: held.GetSopInstanceUid(), Revision: 2}}}))
				r := res.GetInstances()
				if len(r) != 2 || r[0].GetDisposition() != v1.InstanceDisposition_INSTANCE_DISPOSITION_SUPERSEDED || r[0].GetCorrection() != id || !r[0].GetBytesKept() ||
					r[0].GetSha256() != held.GetSha256() || r[1].GetDisposition() != v1.InstanceDisposition_INSTANCE_DISPOSITION_CURRENT || r[1].GetSha256() != held.GetSha256() {
					t.Errorf("resolve answers %v", r)
				}
			}},
		{name: "the same file again", as: c.lab1Admin, call: replaceCall(held.GetId()), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.ReplaceResponse).GetCorrection().GetId(); got != id {
				t.Errorf("a repeat answered %s, the first %s", got, id)
			}
		}},
	})
}

// testListCorrections guards ListCorrections: operator on the tenant; an
// unspecified kind and too large a page are invalid; each filter keeps what
// it names, the filters recorded; no correction of this version is a
// retention rule's; a list never answers holds.
func testListCorrections(t *testing.T) {
	c := newCorrectionArchive(t)
	d := c.deleteSubject(t, c.subject)
	list := func(r *v1.ListCorrectionsRequest) func(clients) outcome {
		r.Tenant = "lab1"
		return func(cl clients) outcome { return call(cl.corrections.ListCorrections, r) }
	}
	ids := func(o outcome) []string {
		var out []string
		for _, x := range o.res.(*v1.ListCorrectionsResponse).GetCorrections() {
			out = append(out, x.GetId())
			if len(x.GetHolds()) != 0 {
				panic("a list answered holds")
			}
		}
		return out
	}
	c.run(t, []tcase{
		{name: "reader", as: c.lab1Reader, call: list(&v1.ListCorrectionsRequest{}), code: connect.CodePermissionDenied},
		{name: "an unspecified kind", as: c.lab1Operator, call: list(&v1.ListCorrectionsRequest{Kinds: []v1.CorrectionKind{0}}), code: connect.CodeInvalidArgument},
		{name: "too large a page", as: c.lab1Operator, call: list(&v1.ListCorrectionsRequest{PageSize: 1001}), code: connect.CodeInvalidArgument},
		{name: "every one", as: c.lab1Operator, call: list(&v1.ListCorrectionsRequest{}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone},
			check: func(t *testing.T, o outcome) {
				if got := ids(o); !slices.Equal(got, []string{d.GetId()}) {
					t.Errorf("listed %v", got)
				}
			}},
		{name: "relabels only", as: c.lab1Operator, call: list(&v1.ListCorrectionsRequest{Kinds: []v1.CorrectionKind{v1.CorrectionKind_CORRECTION_KIND_RELABEL}}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"kinds": "CORRECTION_KIND_RELABEL"}},
			check: func(t *testing.T, o outcome) {
				if got := ids(o); len(got) != 0 {
					t.Errorf("listed %v", got)
				}
			}},
		{name: "a retention rule's", as: c.lab1Operator, call: list(&v1.ListCorrectionsRequest{Origins: []v1.CorrectionOrigin{v1.CorrectionOrigin_CORRECTION_ORIGIN_RETENTION}}),
			check: func(t *testing.T, o outcome) {
				if got := ids(o); len(got) != 0 {
					t.Errorf("listed %v", got)
				}
			}},
		{name: "committed", as: c.lab1Operator, call: list(&v1.ListCorrectionsRequest{States: []v1.CorrectionState{v1.CorrectionState_CORRECTION_STATE_COMMITTED}}),
			check: func(t *testing.T, o outcome) {
				if got := ids(o); !slices.Equal(got, []string{d.GetId()}) {
					t.Errorf("listed %v", got)
				}
			}},
	})
}

// testGetCorrection guards GetCorrection: operator on the tenant; the id is
// required and at most 64 bytes; an unknown one is not found; the
// correction is answered with its counts and holds, its id the entry's
// object.
func testGetCorrection(t *testing.T) {
	c := newCorrectionArchive(t)
	d := c.deleteSubject(t, c.subject)
	get := func(id string) func(clients) outcome {
		return func(cl clients) outcome {
			return call(cl.corrections.GetCorrection, &v1.GetCorrectionRequest{Tenant: "lab1", Id: id})
		}
	}
	c.run(t, []tcase{
		{name: "reader", as: c.lab1Reader, call: get(d.GetId()), code: connect.CodePermissionDenied},
		{name: "no id", as: c.lab1Operator, call: get(""), code: connect.CodeInvalidArgument},
		{name: "too long", as: c.lab1Operator, call: get(string(make([]byte, 65))), code: connect.CodeInvalidArgument},
		{name: "unknown", as: c.lab1Operator, call: get(correctionULID), code: connect.CodeNotFound, msg: "no correction"},
		{name: "the delete", as: c.lab1Operator, call: get(d.GetId()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: d.GetId()},
			check: func(t *testing.T, o outcome) {
				cor := o.res.(*v1.GetCorrectionResponse).GetCorrection()
				if cor.GetId() != d.GetId() || cor.GetCounts().GetInstances() != 1 || len(cor.GetHolds()) == 0 || cor.GetTarget().GetSubjectLabel() != c.subject {
					t.Errorf("answered %v", cor)
				}
			}},
	})
}

// testListScrubFindings guards ListScrubFindings: reader on the tenant; an
// unspecified kind, a storage name that is not one and a token not made by
// the list are invalid; a tenant no scrub ran over answers none, its
// filters recorded.
func testListScrubFindings(t *testing.T) {
	f := newFixture(t)
	list := func(r *v1.ListScrubFindingsRequest) func(clients) outcome {
		r.Tenant = "lab1"
		return func(cl clients) outcome { return call(cl.corrections.ListScrubFindings, r) }
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: list(&v1.ListScrubFindingsRequest{}), code: connect.CodePermissionDenied},
		{name: "an unspecified kind", as: f.lab1Reader, call: list(&v1.ListScrubFindingsRequest{Kinds: []v1.ScrubFindingKind{0}}), code: connect.CodeInvalidArgument},
		{name: "a storage name that is not one", as: f.lab1Reader, call: list(&v1.ListScrubFindingsRequest{Storage: "A B"}), code: connect.CodeInvalidArgument},
		{name: "a token not made here", as: f.lab1Reader, call: list(&v1.ListScrubFindingsRequest{PageToken: "x"}), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "none", as: f.lab1Reader, call: list(&v1.ListScrubFindingsRequest{IncludeCleared: true, Kinds: []v1.ScrubFindingKind{v1.ScrubFindingKind_SCRUB_FINDING_KIND_DAMAGED}}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"include_cleared": "true", "kinds": "SCRUB_FINDING_KIND_DAMAGED"}},
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.ListScrubFindingsResponse); len(r.GetFindings()) != 0 || r.GetNextPageToken() != "" {
					t.Errorf("answered %v", r)
				}
			}},
	})
}

// testResolveInstances guards ResolveInstances: reader on the tenant; no
// pair, more than 1000, revision 0 and a UID that is not one are invalid;
// each pair is answered once, in order, a pair given twice twice; an
// instance never held and a revision above any are unknown, never an error;
// the entry counts the pairs and the dispositions and holds no instance id.
func testResolveInstances(t *testing.T) {
	c := newCorrectionArchive(t)
	held := c.a.held(t, c.a.scans(t)[0])[0]
	resolve := func(members ...*v1.InstanceRef) func(clients) outcome {
		return func(cl clients) outcome {
			return call(cl.corrections.ResolveInstances, &v1.ResolveInstancesRequest{Tenant: "lab1", Members: members})
		}
	}
	many := make([]*v1.InstanceRef, 1001)
	for i := range many {
		many[i] = &v1.InstanceRef{InstanceId: "2.25." + strconv.Itoa(i+1), Revision: 1}
	}
	c.run(t, []tcase{
		{name: "no grant", as: c.noGrant, call: resolve(&v1.InstanceRef{InstanceId: held.SOPInstanceUID, Revision: 1}), code: connect.CodePermissionDenied},
		{name: "no pair", as: c.lab1Reader, call: resolve(), code: connect.CodeInvalidArgument, msg: "members"},
		{name: "too many", as: c.lab1Reader, call: resolve(many...), code: connect.CodeInvalidArgument, msg: "members"},
		{name: "revision 0", as: c.lab1Reader, call: resolve(&v1.InstanceRef{InstanceId: held.SOPInstanceUID}), code: connect.CodeInvalidArgument, msg: "revision"},
		{name: "a bad uid", as: c.lab1Reader, call: resolve(&v1.InstanceRef{InstanceId: "x y", Revision: 1}), code: connect.CodeInvalidArgument, msg: "instance_id"},
		{name: "current, unknown, above any", as: c.lab1Reader,
			call: resolve(&v1.InstanceRef{InstanceId: held.SOPInstanceUID, Revision: 1}, &v1.InstanceRef{InstanceId: "2.25.7", Revision: 1},
				&v1.InstanceRef{InstanceId: held.SOPInstanceUID, Revision: 2147483647}, &v1.InstanceRef{InstanceId: held.SOPInstanceUID, Revision: 1}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"members": "4", "current": "2", "unknown": "2"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ResolveInstancesResponse).GetInstances()
				want := []v1.InstanceDisposition{v1.InstanceDisposition_INSTANCE_DISPOSITION_CURRENT, v1.InstanceDisposition_INSTANCE_DISPOSITION_UNKNOWN,
					v1.InstanceDisposition_INSTANCE_DISPOSITION_UNKNOWN, v1.InstanceDisposition_INSTANCE_DISPOSITION_CURRENT}
				for i, w := range want {
					if i >= len(r) || r[i].GetDisposition() != w {
						t.Fatalf("answered %v", r)
					}
				}
				if r[0].GetSha256() != held.ArchivedSHA256.String() && r[0].GetSha256() != held.SHA256.String() || r[0].GetCurrentRevision() != 1 ||
					r[2].GetMember().GetRevision() != 2147483647 {
					t.Errorf("answered %v", r)
				}
				c.flush()
				for _, e := range c.tenantLog("lab1") {
					if e.RequestID == o.requestID {
						for _, v := range e.Details {
							if v == held.SOPInstanceUID {
								t.Error("the entry holds an instance id")
							}
						}
					}
				}
			}},
	})
}

// sha returns the SHA-256 of an object of the archive, in hexadecimal.
func (a *archive) sha(t *testing.T, key string) string {
	t.Helper()
	for _, p := range a.scans(t) {
		for _, h := range a.held(t, p) {
			if h.Key == key {
				if !h.ArchivedSHA256.IsZero() {
					return h.ArchivedSHA256.String()
				}
				return h.SHA256.String()
			}
		}
	}
	t.Fatalf("no object %s", key)
	return ""
}
