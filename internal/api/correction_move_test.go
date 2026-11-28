package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
)

// moveCall is a call of Move in lab1.
func moveCall(req *v1.MoveRequest) func(clients) outcome {
	return func(c clients) outcome {
		req.Tenant = "lab1"
		return call(c.corrections.Move, req)
	}
}

// mergeCall is a call of Merge in lab1.
func mergeCall(req *v1.MergeRequest) func(clients) outcome {
	return func(c clients) outcome {
		req.Tenant = "lab1"
		return call(c.corrections.Merge, req)
	}
}

// testMove guards Move: admin on the tenant and never a masked key, the
// refusal recorded; the reason, the token and a destination other than
// the session's subject required; a subject or destination the tenant
// lacks not found; a subject's last session refused (a merge is the way).
func testMove(t *testing.T) {
	c := newCorrectionArchive(t)
	masked, _ := c.maskedKey(c.onTenant("lab1", master.RoleAdmin))
	sessions := c.a.scans(t)
	var session string
	for _, p := range sessions {
		if p.Subject == c.subject {
			session = p.Session
		}
	}
	preview := &v1.MoveRequest{SubjectLabel: c.subject, SessionLabel: session, ToSubjectLabel: c.other, Reason: "filed under the wrong patient",
		ValidateOnly: true}
	c.run(t, []tcase{
		{name: "operator", as: c.lab1Operator, call: moveCall(preview), code: connect.CodePermissionDenied},
		{name: "a masked key", as: masked, call: moveCall(preview), code: connect.CodePermissionDenied, msg: access.MessageMaskedDestination,
			audit: &wantAudit{outcome: master.OutcomeRefused, reason: "masked key", details: map[string]string{access.DetailMasked: "true"}}},
		{name: "no reason", as: c.lab1Admin, call: moveCall(&v1.MoveRequest{SubjectLabel: c.subject, SessionLabel: session, ToSubjectLabel: c.other,
			ValidateOnly: true}), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "its own subject", as: c.lab1Admin, call: moveCall(&v1.MoveRequest{SubjectLabel: c.subject, SessionLabel: session,
			ToSubjectLabel: c.subject, Reason: "r", ValidateOnly: true}), code: connect.CodeInvalidArgument, msg: "to_subject_label"},
		{name: "no token", as: c.lab1Admin, call: moveCall(&v1.MoveRequest{SubjectLabel: c.subject, SessionLabel: session, ToSubjectLabel: c.other,
			Reason: "r"}), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "a destination the tenant lacks", as: c.lab1Admin, call: moveCall(&v1.MoveRequest{SubjectLabel: c.subject, SessionLabel: session,
			ToSubjectLabel: "nobody", Reason: "r", ValidateOnly: true}), code: connect.CodeNotFound},
		{name: "a subject's last session", as: c.lab1Admin, call: moveCall(preview), code: connect.CodeFailedPrecondition, msg: "merge",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "-", details: map[string]string{"target": c.subject + "/" + session,
				"to_label": c.other, "validate_only": "true", "rewrite_identity": "false"}}},
	})
}

// testMerge guards Merge: never a masked key; the preview counts what
// moves and gives a token; the merge with it is answered prepared, of kind
// move, and publishes in the background, the merged subject then not
// found; its token used again answers it; UndoCorrection makes the subject
// again.
func testMerge(t *testing.T) {
	c := newCorrectionArchive(t)
	masked, _ := c.maskedKey(c.onTenant("lab1", master.RoleAdmin))
	preview := &v1.MergeRequest{SubjectLabel: c.subject, IntoSubjectLabel: c.other, Reason: "one patient under two ids", ValidateOnly: true}
	var confirm, id string
	c.run(t, []tcase{
		{name: "a masked key", as: masked, call: mergeCall(preview), code: connect.CodePermissionDenied, msg: access.MessageMaskedDestination},
		{name: "preview", as: c.lab1Admin, call: mergeCall(preview), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "-",
			details: map[string]string{"target": c.subject, "to_label": c.other, "validate_only": "true"}},
			check: func(t *testing.T, o outcome) {
				p := o.res.(*v1.MergeResponse).GetPreview()
				confirm = p.GetConfirm()
				if confirm == "" || len(confirm) > 128 || p.GetCounts().GetSessions() != 1 || p.GetCounts().GetInstances() != int64(c.instances) ||
					len(p.GetSessions()) != 1 || len(p.GetHolds()) != 0 || len(p.GetClashes()) != 0 || p.GetPurgeTime() == nil {
					t.Fatalf("the preview is %v", p)
				}
			}},
		{name: "merge", as: c.lab1Admin, call: func(cl clients) outcome {
			return mergeCall(&v1.MergeRequest{SubjectLabel: c.subject, IntoSubjectLabel: c.other, Reason: "one patient under two ids",
				Confirm: confirm})(cl)
		}, audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"target": c.subject, "to_label": c.other,
			"validate_only": "false", "reason": "one patient under two ids"}},
			check: func(t *testing.T, o outcome) {
				cor := o.res.(*v1.MergeResponse).GetCorrection()
				id = cor.GetId()
				if cor.GetKind() != v1.CorrectionKind_CORRECTION_KIND_MOVE || cor.GetToLabel() != c.other {
					t.Fatalf("the merge answered %v", cor)
				}
				c.waitCorrection(t, id, func(cor *v1.Correction) bool { return cor.GetState() == v1.CorrectionState_CORRECTION_STATE_COMMITTED })
				if c.subjectAnswers(t, c.subject) {
					t.Error("the merged subject still answers")
				}
			}},
		{name: "the same token again", as: c.lab1Admin, call: func(cl clients) outcome {
			return mergeCall(&v1.MergeRequest{SubjectLabel: c.subject, IntoSubjectLabel: c.other, Reason: "one patient under two ids",
				Confirm: confirm})(cl)
		}, check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.MergeResponse).GetCorrection().GetId(); got != id {
				t.Errorf("a repeat answered %s, the first %s", got, id)
			}
		}},
		{name: "undo", as: c.lab1Admin, call: func(cl clients) outcome { return undoCall(id, "two patients after all")(cl) },
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.UndoCorrectionResponse).GetCorrection(); got.GetState() != v1.CorrectionState_CORRECTION_STATE_UNDONE {
					t.Fatalf("the undo answered %v", got)
				}
				if !c.subjectAnswers(t, c.subject) {
					t.Error("after the undo the merged subject does not answer")
				}
			}},
	})
}
