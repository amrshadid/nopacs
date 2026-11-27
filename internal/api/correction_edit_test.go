package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
)

// editCall is a call of Edit.
func editCall(req *v1.EditRequest) func(clients) outcome {
	return func(c clients) outcome {
		req.Tenant = "lab1"
		return call(c.corrections.Edit, req)
	}
}

// nameChange sets Patient's Name, which every corpus file holds.
func nameChange(v string) []*v1.EditChange {
	return []*v1.EditChange{{Attribute: "PatientName", Value: v}}
}

// testEdit guards Edit: admin on the tenant, and never a masked key, which
// may not write values into archived files (refused with its message and
// the mark, in the installation's log); every invalid line, an attribute
// no edit changes among them, recorded in the tenant's log with the
// target's path and the attributes' tags, never a value; the preview lists
// every instance and attribute, its value now only with show_values (which
// is recorded); a token of other changes is refused; the edit with the
// preview's token is committed, kind edit, its entry naming it; the same
// token again answers the same correction.
func testEdit(t *testing.T) {
	c := newCorrectionArchive(t)
	masked, _ := c.maskedKey(c.onTenant("lab1", master.RoleAdmin))
	target := subjectTarget(c.subject)
	preview := func(changes []*v1.EditChange, show bool) *v1.EditRequest {
		return &v1.EditRequest{Target: target, Changes: changes, Reason: "name typed wrongly", ValidateOnly: true, ShowValues: show}
	}
	var confirm, id string
	c.run(t, []tcase{
		{name: "no grant", as: c.noGrant, call: editCall(preview(nameChange("Anon^One"), false)), code: connect.CodePermissionDenied},
		{name: "operator", as: c.lab1Operator, call: editCall(preview(nameChange("Anon^One"), false)), code: connect.CodePermissionDenied},
		{name: "a masked key", as: masked, call: editCall(preview(nameChange("Anon^One"), false)), code: connect.CodePermissionDenied,
			msg: access.MessageMaskedWrite, audit: &wantAudit{outcome: master.OutcomeRefused, reason: "masked key",
				details: map[string]string{access.DetailMasked: "true"}}},
		{name: "no reason", as: c.lab1Admin, call: editCall(&v1.EditRequest{Target: target, Changes: nameChange("x"), ValidateOnly: true}),
			code: connect.CodeInvalidArgument, msg: "reason", audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "-",
				details: map[string]string{"target": c.subject, "validate_only": "true", "attributes": "0010,0010", "changes": "1"}}},
		{name: "no change", as: c.lab1Admin, call: editCall(preview(nil, false)), code: connect.CodeInvalidArgument, msg: "changes"},
		{name: "an attribute no dictionary knows", as: c.lab1Admin, call: editCall(preview([]*v1.EditChange{{Attribute: "NoSuchThing", Value: "x"}}, false)),
			code: connect.CodeInvalidArgument, msg: "changes[0].attribute", audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed,
				details: map[string]string{"attributes": "?"}}},
		{name: "a UID the catalog is built on", as: c.lab1Admin, call: editCall(preview([]*v1.EditChange{{Attribute: "0020,000D", Value: "1.2.3"}}, false)),
			code: connect.CodeInvalidArgument, msg: "Study Instance UID"},
		{name: "show_values without a preview", as: c.lab1Admin, call: editCall(&v1.EditRequest{Target: target, Changes: nameChange("x"),
			Reason: "r", Confirm: "c1.1.x", ShowValues: true}), code: connect.CodeInvalidArgument, msg: "show_values"},
		{name: "no token", as: c.lab1Admin, call: editCall(&v1.EditRequest{Target: target, Changes: nameChange("x"), Reason: "r"}),
			code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "a token with a preview", as: c.lab1Admin, call: editCall(&v1.EditRequest{Target: target, Changes: nameChange("x"), Reason: "r",
			ValidateOnly: true, Confirm: "c1.1.x"}), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "a subject the tenant does not hold", as: c.lab1Admin, call: editCall(&v1.EditRequest{Target: subjectTarget("nobody"),
			Changes: nameChange("x"), Reason: "r", ValidateOnly: true}), code: connect.CodeNotFound},
		{name: "preview", as: c.lab1Admin, call: editCall(preview(nameChange("Anon^One"), false)),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "-", details: map[string]string{"validate_only": "true",
				"instances": "1", "attributes": "0010,0010", "reason": "name typed wrongly"}},
			check: func(t *testing.T, o outcome) {
				p := o.res.(*v1.EditResponse).GetPreview()
				confirm = p.GetConfirm()
				ins := p.GetInstances()
				if confirm == "" || len(confirm) > 128 || len(ins) != 1 || ins[0].GetSubjectLabel() != c.subject || ins[0].GetRevision() != 1 ||
					len(ins[0].GetAttributes()) != 1 || p.GetIssueTime() == nil {
					t.Fatalf("the preview is %v", p)
				}
				a := ins[0].GetAttributes()[0]
				if a.GetAttribute() != "0010,0010" || !a.GetPresent() || !a.GetKnown() || a.GetOldValue() != "" || a.GetNewValue() != "Anon^One" {
					t.Errorf("a preview without values answers %v", a)
				}
			}},
		{name: "preview with its values", as: c.lab1Admin, call: editCall(preview(nameChange("Anon^One"), true)),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"show_values": "true"}},
			check: func(t *testing.T, o outcome) {
				if a := o.res.(*v1.EditResponse).GetPreview().GetInstances()[0].GetAttributes()[0]; a.GetOldValue() == "" {
					t.Errorf("a preview with values answers no value now: %v", a)
				}
			}},
		{name: "a token of other changes", as: c.lab1Admin, call: func(cl clients) outcome {
			return editCall(&v1.EditRequest{Target: target, Changes: nameChange("Anon^Two"), Reason: "r", Confirm: confirm})(cl)
		}, code: connect.CodeFailedPrecondition},
		{name: "edit", as: c.lab1Admin, call: func(cl clients) outcome {
			return editCall(&v1.EditRequest{Target: target, Changes: nameChange("Anon^One"), Reason: "name typed wrongly", Confirm: confirm})(cl)
		},
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"validate_only": "false", "reason": "name typed wrongly",
				"attributes": "0010,0010"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.EditResponse)
				cor := r.GetCorrection()
				id = cor.GetId()
				if r.GetPreview() != nil || cor.GetState() != v1.CorrectionState_CORRECTION_STATE_COMMITTED || cor.GetKind() != v1.CorrectionKind_CORRECTION_KIND_EDIT ||
					cor.GetCounts().GetInstances() != 1 || cor.GetPurgeTime() == nil {
					t.Errorf("the edit answered %v", r)
				}
				c.flush()
				for _, e := range c.tenantLog("lab1") {
					if e.RequestID == o.requestID && e.Object != id {
						t.Errorf("the edit's entry names %q, want %s", e.Object, id)
					}
				}
			}},
		{name: "the same token again", as: c.lab1Admin, call: func(cl clients) outcome {
			return editCall(&v1.EditRequest{Target: target, Changes: nameChange("Anon^One"), Reason: "name typed wrongly", Confirm: confirm})(cl)
		}, check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.EditResponse).GetCorrection().GetId(); got != id {
				t.Errorf("a repeat answered %s, the first %s", got, id)
			}
		}},
	})
}
