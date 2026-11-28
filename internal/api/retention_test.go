package api_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// years is a period of whole years.
func years(n int32) *v1.RetentionPeriod { return &v1.RetentionPeriod{Years: n} }

// days is a period of whole days.
func days(n int32) *v1.RetentionPeriod { return &v1.RetentionPeriod{Days: n} }

// addRule is a call of AddRetentionRule on lab1.
func addRule(r *v1.RetentionRule) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.retention.AddRetentionRule, &v1.AddRetentionRuleRequest{Tenant: "lab1", Rule: r})
	}
}

// mustAddRule adds a rule to lab1 as the local operator.
func mustAddRule(t *testing.T, e *env, r *v1.RetentionRule) {
	t.Helper()
	if o := addRule(r)(e.as(local)); o.err != nil {
		t.Fatalf("adding rule %s: %v", r.GetName(), o.err)
	}
}

// reasonOf returns the ErrorInfo of a call's error.
func reasonOf(t *testing.T, o outcome) codes.Reason {
	t.Helper()
	var ce *connect.Error
	if !errors.As(o.err, &ce) {
		t.Fatalf("not an API error: %v", o.err)
	}
	r, ok := codes.ReasonOfError(ce)
	if !ok {
		t.Fatalf("no error info on %v", o.err)
	}
	return r
}

// testAddRetentionRule guards AddRetentionRule: admin on the tenant; a
// rule with a name and a period, each period within its bounds, dispose no
// shorter than keep, a selector of a receiver or a storage; a storage the
// tenant does not have is a precondition failure; a name taken is a
// conflict. With a keep rule standing, a person's delete is refused with
// RETENTION_MINIMUM and keep_until, a tenant's admin may not override it,
// and the local operator's override deletes.
func testAddRetentionRule(t *testing.T) {
	c := newCorrectionArchive(t)
	keep := &v1.RetentionRule{Name: "trial7", KeepAtLeast: years(10)}
	var confirm string
	c.run(t, []tcase{
		{name: "operator", as: c.lab1Operator, call: addRule(keep), code: connect.CodePermissionDenied},
		{name: "no rule", as: c.lab1Admin, call: addRule(nil), code: connect.CodeInvalidArgument, msg: "rule: is required"},
		{name: "a name that is not one", as: c.lab1Admin, call: addRule(&v1.RetentionRule{Name: "Trial 7", KeepAtLeast: years(1)}), code: connect.CodeInvalidArgument, msg: "rule.name"},
		{name: "no period", as: c.lab1Admin, call: addRule(&v1.RetentionRule{Name: "empty"}), code: connect.CodeInvalidArgument, msg: "rule:"},
		{name: "a period past 100 years", as: c.lab1Admin, call: addRule(&v1.RetentionRule{Name: "long", KeepAtLeast: years(101)}), code: connect.CodeInvalidArgument, msg: "rule.keep_at_least"},
		{name: "dispose shorter than keep", as: c.lab1Admin, call: addRule(&v1.RetentionRule{Name: "odd", KeepAtLeast: years(2), DisposeAfter: years(1)}),
			code: connect.CodeInvalidArgument, msg: "not shorter"},
		{name: "a receiver and a storage", as: c.lab1Admin, call: addRule(&v1.RetentionRule{Name: "both", KeepAtLeast: years(1),
			Selector: &v1.RetentionSelector{Receiver: "scanner1", Storage: "main"}}), code: connect.CodeInvalidArgument, msg: "rule.selector"},
		{name: "a storage the tenant does not have", as: c.lab1Admin, call: addRule(&v1.RetentionRule{Name: "where", KeepAtLeast: years(1),
			Selector: &v1.RetentionSelector{Storage: "nowhere"}}), code: connect.CodeFailedPrecondition, audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "added", as: c.lab1Admin, call: addRule(keep),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "trial7", details: map[string]string{"keep_at_least": "10y0d", "dispose_after": "none",
				"selector": "tenant", "action": "review"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.AddRetentionRuleResponse).GetRule()
				if r.GetName() != "trial7" || r.GetKeepAtLeast().GetYears() != 10 || r.GetDisposeAfter() != nil ||
					r.GetAction() != v1.RetentionAction_RETENTION_ACTION_REVIEW || r.GetCreateTime() == nil {
					t.Errorf("rule %v", r)
				}
			}},
		{name: "the name again", as: c.lab1Admin, call: addRule(keep), code: connect.CodeAlreadyExists},
		{name: "a delete's preview counts what the rule keeps", as: c.lab1Admin, call: deleteCall(subjectTarget(c.subject), "consent withdrawn", "", true),
			check: func(t *testing.T, o outcome) {
				p := o.res.(*v1.DeleteResponse).GetPreview()
				confirm = p.GetConfirm()
				if p.GetSessionsUnderMinimum() != 1 || p.GetKeepUntil() == nil {
					t.Errorf("preview %v", p)
				}
			}},
		{name: "a delete the rule keeps", as: c.lab1Admin, call: func(cl clients) outcome {
			return deleteCall(subjectTarget(c.subject), "consent withdrawn", confirm, false)(cl)
		}, code: connect.CodeFailedPrecondition, msg: "RETENTION_MINIMUM", check: func(t *testing.T, o outcome) {
			r := reasonOf(t, o)
			if r.Reason != "RETENTION_MINIMUM" || r.Domain != codes.Domain {
				t.Errorf("error info %+v", r)
			}
			if _, err := time.Parse(time.RFC3339, r.Metadata["keep_until"]); err != nil {
				t.Errorf("keep_until %q: %v", r.Metadata["keep_until"], err)
			}
		}},
		{name: "a tenant's admin may not override", as: c.lab1Admin, call: func(cl clients) outcome {
			return call(cl.corrections.Delete, &v1.DeleteRequest{Tenant: "lab1", Target: subjectTarget(c.subject), Reason: "consent withdrawn",
				Confirm: confirm, OverrideRetention: true, OverrideReason: "the participant withdrew"})
		}, code: connect.CodePermissionDenied},
		{name: "the local operator overrides", as: local, call: func(cl clients) outcome {
			return call(cl.corrections.Delete, &v1.DeleteRequest{Tenant: "lab1", Target: subjectTarget(c.subject), Reason: "consent withdrawn",
				Confirm: confirm, OverrideRetention: true, OverrideReason: "the participant withdrew"})
		}, check: func(t *testing.T, o outcome) {
			cr := o.res.(*v1.DeleteResponse).GetCorrection()
			if !cr.GetOverrideRetention() || cr.GetOverrideReason() != "the participant withdrew" {
				t.Errorf("correction %v", cr)
			}
		}},
	})
}

// overrideCall is a delete of target on lab1 that lifts retention.
func overrideCall(target *v1.CorrectionTarget, confirm string, validate bool) func(clients) outcome {
	return func(cl clients) outcome {
		return call(cl.corrections.Delete, &v1.DeleteRequest{Tenant: "lab1", Target: target, Reason: "consent withdrawn",
			Confirm: confirm, ValidateOnly: validate, OverrideRetention: true, OverrideReason: "the participant withdrew"})
	}
}

// TestRetentionOverrideRoles guards who may lift a keep-at-least: a
// tenant's admin is refused before its request is validated; a key that
// loses its installation grant after the request was let in is refused at
// the delete's commit, and the subject stays; a key that is admin on the
// installation and on the tenant lifts it and deletes.
func TestRetentionOverrideRoles(t *testing.T) {
	c := newCorrectionArchive(t)
	mustAddRule(t, c.env, &v1.RetentionRule{Name: "trial7", KeepAtLeast: years(10)})
	losing, losingID := c.key(onInstallation(master.RoleAdmin), c.onTenant("lab1", master.RoleAdmin))
	keeping, _ := c.key(onInstallation(master.RoleAdmin), c.onTenant("lab1", master.RoleAdmin))
	var confirm string
	preview := func(as caller) tcase {
		return tcase{name: "a preview", as: as, call: deleteCall(subjectTarget(c.subject), "consent withdrawn", "", true),
			check: func(t *testing.T, o outcome) { confirm = o.res.(*v1.DeleteResponse).GetPreview().GetConfirm() }}
	}
	c.run(t, []tcase{
		{name: "a tenant's admin, a target that is not one", as: c.lab1Admin, call: overrideCall(nil, "", true), code: connect.CodePermissionDenied},
		preview(losing),
	})
	removed := false
	api.SetGuarding(c.handlers, func() {
		if removed {
			return
		}
		removed = true
		if _, err := c.master.RemoveGrant(ctx, losingID, master.Installation, c.setupEntry(), master.LocalOperator()); err != nil {
			t.Errorf("removing the grant: %v", err)
		}
	})
	c.run(t, []tcase{
		{name: "the installation grant removed before the commit", as: losing, call: func(cl clients) outcome {
			return overrideCall(subjectTarget(c.subject), confirm, false)(cl)
		}, code: connect.CodePermissionDenied},
		{name: "the subject stays", as: c.lab1Admin, call: deleteCall(subjectTarget(c.subject), "consent withdrawn", "", true),
			check: func(t *testing.T, o outcome) {
				if p := o.res.(*v1.DeleteResponse).GetPreview(); p.GetSessionsUnderMinimum() != 1 {
					t.Errorf("preview %v", p)
				}
			}},
	})
	if !removed {
		t.Fatal("the override's guard never ran")
	}
	api.SetGuarding(c.handlers, nil)
	c.run(t, []tcase{
		preview(keeping),
		{name: "an installation admin overrides", as: keeping, call: func(cl clients) outcome {
			return overrideCall(subjectTarget(c.subject), confirm, false)(cl)
		}, check: func(t *testing.T, o outcome) {
			cr := o.res.(*v1.DeleteResponse).GetCorrection()
			if !cr.GetOverrideRetention() || cr.GetOverrideReason() != "the participant withdrew" {
				t.Errorf("correction %v", cr)
			}
		}},
	})
}

// TestConfirmClockNamesTheTenantsRefused guards a confirmation on the
// installation whose key loses its installation grant after the request
// was let in: the paused tenant is answered refused, not resumed, it stays
// paused, and the installation's entry records the request refused with
// the tenant named, never a plain success; with the grant kept, the same
// confirmation resumes it.
func TestConfirmClockNamesTheTenantsRefused(t *testing.T) {
	f := newFixture(t)
	st, err := f.registry.Store("lab1")
	if err != nil {
		t.Fatal(err)
	}
	rs := st.(tenant.Retention)
	for _, at := range []time.Time{f.now().Add(-400 * 24 * time.Hour), f.now()} {
		if _, err := rs.CheckRetentionClock(ctx, tenant.RetentionClockCheck{Now: at, MaxStep: tenant.DefaultMaxClockStep,
			Notice: 30 * 24 * time.Hour, Pass: true}); err != nil {
			t.Fatal(err)
		}
	}
	paused := func() bool {
		t.Helper()
		clock, err := rs.RetentionClock(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return clock.Paused
	}
	if !paused() {
		t.Fatal("a 400-day step did not pause retention")
	}
	losing, losingID := f.key(onInstallation(master.RoleAdmin))
	removed := false
	api.SetGuarding(f.handlers, func() {
		if removed {
			return
		}
		removed = true
		if _, err := f.master.RemoveGrant(ctx, losingID, master.Installation, f.setupEntry(), master.LocalOperator()); err != nil {
			t.Errorf("removing the grant: %v", err)
		}
	})
	f.run(t, []tcase{
		{name: "the grant removed before the tenant's change", as: losing, call: func(c clients) outcome {
			return confirmCall(installation(), timestamppb.New(f.now()), "checked")(c)
		}, audit: &wantAudit{outcome: master.OutcomeRefused, details: map[string]string{"tenants_refused": "lab1", "tenants": ""}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ConfirmClockResponse)
				if len(r.GetTenants()) != 0 || !slices.Equal(r.GetTenantsRefused(), []string{"lab1"}) {
					t.Errorf("answer %v", r)
				}
			}},
	})
	if !paused() {
		t.Error("a refused confirmation resumed the tenant")
	}
	api.SetGuarding(f.handlers, nil)
	f.run(t, []tcase{
		{name: "the grant kept", as: f.instAdmin, call: func(c clients) outcome {
			return confirmCall(installation(), timestamppb.New(f.now()), "checked")(c)
		}, audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"tenants": "lab1"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ConfirmClockResponse)
				if !slices.Equal(r.GetTenants(), []string{"lab1"}) || len(r.GetTenantsRefused()) != 0 {
					t.Errorf("answer %v", r)
				}
			}},
	})
	if paused() {
		t.Error("a confirmation did not resume the tenant")
	}
}

// testGetRetentionRule guards GetRetentionRule: reader on the tenant; a
// name required; a rule the tenant does not have is not found.
func testGetRetentionRule(t *testing.T) {
	f := newFixture(t)
	mustAddRule(t, f.env, &v1.RetentionRule{Name: "trial7", DisposeAfter: years(15), Action: v1.RetentionAction_RETENTION_ACTION_DELETE})
	get := func(name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.retention.GetRetentionRule, &v1.GetRetentionRuleRequest{Tenant: "lab1", Name: name})
		}
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: get("trial7"), code: connect.CodePermissionDenied},
		{name: "no name", as: f.lab1Reader, call: get(""), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "no such rule", as: f.lab1Reader, call: get("none"), code: connect.CodeNotFound, msg: "no retention rule"},
		{name: "read", as: f.lab1Reader, call: get("trial7"), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "trial7"},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.GetRetentionRuleResponse).GetRule()
				if r.GetDisposeAfter().GetYears() != 15 || r.GetAction() != v1.RetentionAction_RETENTION_ACTION_DELETE || r.GetSessionsDue() != 0 {
					t.Errorf("rule %v", r)
				}
			}},
	})
}

// testListRetentionRules guards ListRetentionRules: reader on the tenant;
// the rules by name; a page size past 1000 invalid.
func testListRetentionRules(t *testing.T) {
	f := newFixture(t)
	mustAddRule(t, f.env, &v1.RetentionRule{Name: "zeta", KeepAtLeast: years(1)})
	mustAddRule(t, f.env, &v1.RetentionRule{Name: "alpha", KeepAtLeast: years(2)})
	list := func(size int32) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.retention.ListRetentionRules, &v1.ListRetentionRulesRequest{Tenant: "lab1", PageSize: size})
		}
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: list(0), code: connect.CodePermissionDenied},
		{name: "a page too large", as: f.lab1Reader, call: list(1001), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "by name", as: f.lab1Reader, call: list(0), audit: inTenant("lab1", master.OutcomeDone), check: func(t *testing.T, o outcome) {
			rs := o.res.(*v1.ListRetentionRulesResponse).GetRules()
			if len(rs) != 2 || rs[0].GetName() != "alpha" || rs[1].GetName() != "zeta" {
				t.Errorf("rules %v", rs)
			}
		}},
	})
}

// setRule is a call of SetRetentionRule on lab1.
func setRule(r *v1.RetentionRule, paths ...string) func(clients) outcome {
	return func(c clients) outcome {
		var mask *fieldmaskpb.FieldMask
		if paths != nil {
			mask = &fieldmaskpb.FieldMask{Paths: paths}
		}
		return call(c.retention.SetRetentionRule, &v1.SetRetentionRuleRequest{Tenant: "lab1", Rule: r, UpdateMask: mask})
	}
}

// testSetRetentionRule guards SetRetentionRule: admin on the tenant; a
// mask required, never "*", the name or a server field; a rule the tenant
// does not have is not found; the rule as it would stand is checked; the
// fields named change and no other.
func testSetRetentionRule(t *testing.T) {
	f := newFixture(t)
	mustAddRule(t, f.env, &v1.RetentionRule{Name: "trial7", KeepAtLeast: years(10), DisposeAfter: years(15)})
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: setRule(&v1.RetentionRule{Name: "trial7"}, "action"), code: connect.CodePermissionDenied},
		{name: "no mask", as: f.lab1Admin, call: setRule(&v1.RetentionRule{Name: "trial7"}), code: connect.CodeInvalidArgument, msg: "update_mask"},
		{name: "*", as: f.lab1Admin, call: setRule(&v1.RetentionRule{Name: "trial7"}, "*"), code: connect.CodeInvalidArgument, msg: `"*"`},
		{name: "the name", as: f.lab1Admin, call: setRule(&v1.RetentionRule{Name: "trial7"}, "name"), code: connect.CodeInvalidArgument, msg: "does not change"},
		{name: "a server field", as: f.lab1Admin, call: setRule(&v1.RetentionRule{Name: "trial7"}, "sessions_due"), code: connect.CodeInvalidArgument, msg: "answered by the server"},
		{name: "no such rule", as: f.lab1Admin, call: setRule(&v1.RetentionRule{Name: "none"}, "action"), code: connect.CodeNotFound},
		{name: "dispose shorter than keep as it would stand", as: f.lab1Admin, call: setRule(&v1.RetentionRule{Name: "trial7", DisposeAfter: years(5)}, "dispose_after"),
			code: connect.CodeInvalidArgument, msg: "not shorter"},
		{name: "the action", as: f.lab1Admin, call: setRule(&v1.RetentionRule{Name: "trial7", Action: v1.RetentionAction_RETENTION_ACTION_DELETE, KeepAtLeast: years(1)}, "action"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "trial7", details: map[string]string{"update_mask": "action", "action": "delete"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.SetRetentionRuleResponse).GetRule()
				if r.GetAction() != v1.RetentionAction_RETENTION_ACTION_DELETE || r.GetKeepAtLeast().GetYears() != 10 || r.GetDisposeAfter().GetYears() != 15 {
					t.Errorf("rule %v: only the action may change", r)
				}
			}},
	})
}

// testRemoveRetentionRule guards RemoveRetentionRule: admin on the tenant;
// the rule as it was answered; a second call is not found.
func testRemoveRetentionRule(t *testing.T) {
	f := newFixture(t)
	mustAddRule(t, f.env, &v1.RetentionRule{Name: "trial7", KeepAtLeast: years(10)})
	remove := func(c clients) outcome {
		return call(c.retention.RemoveRetentionRule, &v1.RemoveRetentionRuleRequest{Tenant: "lab1", Name: "trial7"})
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: remove, code: connect.CodePermissionDenied},
		{name: "removed", as: f.lab1Admin, call: remove, audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "trial7"},
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.RemoveRetentionRuleResponse).GetRule(); r.GetName() != "trial7" || r.GetKeepAtLeast().GetYears() != 10 {
					t.Errorf("rule %v", r)
				}
			}},
		{name: "again", as: f.lab1Admin, call: remove, code: connect.CodeNotFound},
	})
}

// testListDueSessions guards ListDueSessions: reader on the tenant; a rule
// the tenant does not have is not found; nothing archived is nothing due.
func testListDueSessions(t *testing.T) {
	f := newFixture(t)
	mustAddRule(t, f.env, &v1.RetentionRule{Name: "trial7", DisposeAfter: days(1)})
	list := func(rule string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.retention.ListDueSessions, &v1.ListDueSessionsRequest{Tenant: "lab1", Rule: rule, IncludeHeld: true})
		}
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: list(""), code: connect.CodePermissionDenied},
		{name: "no such rule", as: f.lab1Reader, call: list("none"), code: connect.CodeNotFound},
		{name: "a rule that is not a name", as: f.lab1Reader, call: list("Trial"), code: connect.CodeInvalidArgument, msg: "rule"},
		{name: "nothing due", as: f.lab1Reader, call: list("trial7"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"rule": "trial7", "include_held": "true"}},
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.ListDueSessionsResponse).GetSessions(); len(s) != 0 {
					t.Errorf("due %v", s)
				}
			}},
	})
}

// disposeCall is a call of Dispose on lab1.
func disposeCall(rule, reason, confirm string, validate bool, most int32) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.retention.Dispose, &v1.DisposeRequest{Tenant: "lab1", Rule: rule, Reason: reason, Confirm: confirm, ValidateOnly: validate, MaxSessions: most})
	}
}

// testDispose guards Dispose: admin on the tenant; a reason, the token
// with validate_only only, max_sessions 0 to 1000; a token this server did
// not make, or made over other sessions, is refused; a preview lists what
// is due with a keyed token, the disposal queues a dispose job over
// exactly that, and the token sent again answers the same job.
func testDispose(t *testing.T) {
	c := newCorrectionArchive(t)
	mustAddRule(t, c.env, &v1.RetentionRule{Name: "trial7", DisposeAfter: days(1)})
	mustAddRule(t, c.env, &v1.RetentionRule{Name: "trial8", DisposeAfter: days(1)})
	// A rule added over data stores its due no sooner than the notice.
	c.advance(31 * 24 * time.Hour)
	var token, job string
	c.run(t, []tcase{
		{name: "operator", as: c.lab1Operator, call: disposeCall("trial7", "end of study", "", true, 0), code: connect.CodePermissionDenied},
		{name: "no reason", as: c.lab1Admin, call: disposeCall("trial7", "", "", true, 0), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "a token with validate_only", as: c.lab1Admin, call: disposeCall("trial7", "end", "k1.x", true, 0), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "no token", as: c.lab1Admin, call: disposeCall("trial7", "end", "", false, 0), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "max_sessions past 1000", as: c.lab1Admin, call: disposeCall("trial7", "end", "", true, 1001), code: connect.CodeInvalidArgument, msg: "max_sessions"},
		{name: "no such rule", as: c.lab1Admin, call: disposeCall("none", "end", "", true, 0), code: connect.CodeNotFound},
		{name: "a preview", as: c.lab1Admin, call: disposeCall("trial7", "end of study", "", true, 0),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "trial7", details: map[string]string{"validate_only": "true", "sessions": "2"}},
			check: func(t *testing.T, o outcome) {
				p := o.res.(*v1.DisposeResponse).GetPreview()
				token = p.GetConfirm()
				if len(p.GetSessions()) != 2 || !strings.HasPrefix(token, "k1.d1.") || len(token) > 128 || o.res.(*v1.DisposeResponse).GetJob() != nil {
					t.Errorf("preview %v, token %q (%d bytes)", p, token, len(token))
				}
			}},
		{name: "the store's own token, unkeyed", as: c.lab1Admin, call: func(cl clients) outcome {
			inner := strings.TrimPrefix(token[:strings.LastIndexByte(token, '.')], "k1.")
			return disposeCall("trial7", "end of study", inner, false, 0)(cl)
		}, code: connect.CodeFailedPrecondition, msg: "not one this server's preview"},
		{name: "a token whose MAC is changed", as: c.lab1Admin, call: func(cl clients) outcome {
			bad := []byte(token)
			if bad[len(bad)-1] == 'A' {
				bad[len(bad)-1] = 'B'
			} else {
				bad[len(bad)-1] = 'A'
			}
			return disposeCall("trial7", "end of study", string(bad), false, 0)(cl)
		}, code: connect.CodeFailedPrecondition, msg: "not one this server's preview"},
		{name: "disposed", as: c.lab1Admin, call: func(cl clients) outcome { return disposeCall("trial7", "end of study", token, false, 0)(cl) },
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "trial7", details: map[string]string{"validate_only": "false", "reason": "end of study"}},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.DisposeResponse).GetJob()
				job = j.GetId()
				if d := j.GetDispose(); job == "" || d.GetRule() != "trial7" || d.GetReason() != "end of study" || d.GetSessions() != 2 || d.GetScheduled() {
					t.Errorf("job %v", j)
				}
			}},
		{name: "the token again: the same job", as: c.lab1Admin, call: func(cl clients) outcome { return disposeCall("trial7", "end of study", token, false, 0)(cl) },
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.DisposeResponse).GetJob().GetId(); got != job {
					t.Errorf("a repeat answered %s, the first %s", got, job)
				}
				// The repeat's entry names the job it answered, not an id
				// of a job that was never made.
				c.checkAudit(t, "the token again", o.requestID, wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"job": job}})
			}},
		{name: "the token again under another rule", as: c.lab1Admin,
			call: func(cl clients) outcome { return disposeCall("trial8", "end of study", token, false, 0)(cl) },
			code: connect.CodeFailedPrecondition, msg: "another rule"},
		{name: "the retry of a dispose job", as: c.lab1Operator, call: func(cl clients) outcome { return retryJob(job)(cl) },
			code: connect.CodeFailedPrecondition, msg: "a dispose job is not retried"},
	})
}

// confirmCall is a call of ConfirmClock.
func confirmCall(scope *v1.Scope, observed *timestamppb.Timestamp, reason string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.retention.ConfirmClock, &v1.ConfirmClockRequest{Scope: scope, ObservedTime: observed, Reason: reason})
	}
}

// testConfirmClock guards ConfirmClock: admin on the scope; the time read
// and a reason required; a time more than 5 minutes off the server's is a
// precondition failure; a tenant not paused is answered as it stands; on
// the installation, every tenant paused is resumed, none here.
func testConfirmClock(t *testing.T) {
	f := newFixture(t)
	now := func() *timestamppb.Timestamp { return timestamppb.New(f.now()) }
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: confirmCall(tenantScope("lab1"), now(), "checked"), code: connect.CodePermissionDenied},
		{name: "a tenant's admin on the installation", as: f.lab1Admin, call: confirmCall(installation(), now(), "checked"), code: connect.CodePermissionDenied},
		{name: "no scope", as: f.instAdmin, call: confirmCall(nil, now(), "checked"), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "no time", as: f.lab1Admin, call: confirmCall(tenantScope("lab1"), nil, "checked"), code: connect.CodeInvalidArgument, msg: "observed_time"},
		{name: "no reason", as: f.lab1Admin, call: confirmCall(tenantScope("lab1"), now(), ""), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "a time an hour off", as: f.lab1Admin, call: func(c clients) outcome {
			return confirmCall(tenantScope("lab1"), timestamppb.New(f.now().Add(time.Hour)), "checked")(c)
		}, code: connect.CodeFailedPrecondition, msg: "5 minutes"},
		{name: "a tenant not paused", as: f.lab1Admin, call: func(c clients) outcome {
			return confirmCall(tenantScope("lab1"), now(), "checked against the site's time server")(c)
		},
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"reason": "checked against the site's time server"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ConfirmClockResponse)
				if len(r.GetTenants()) != 0 || r.GetStatus() == nil || r.GetStatus().GetPaused() {
					t.Errorf("answer %v", r)
				}
			}},
		{name: "the installation", as: f.instAdmin, call: func(c clients) outcome { return confirmCall(installation(), now(), "checked")(c) },
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"scope": "installation"}},
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.ConfirmClockResponse); len(r.GetTenants()) != 0 || r.GetStatus() != nil {
					t.Errorf("answer %v", r)
				}
			}},
	})
}

// testGetHold guards GetHold: operator on the tenant; an id required; one
// of no hold's form, or one the tenant does not have, is not found.
func testGetHold(t *testing.T) {
	f := newFixture(t)
	get := func(id string) func(clients) outcome {
		return func(c clients) outcome { return call(c.retention.GetHold, &v1.GetHoldRequest{Tenant: "lab1", Id: id}) }
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: get("01jh0000000000000000000000"), code: connect.CodePermissionDenied},
		{name: "no id", as: f.lab1Operator, call: get(""), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "an id of no hold's form", as: f.lab1Operator, call: get("not-a-hold"), code: connect.CodeNotFound, msg: "no hold"},
		{name: "no such hold", as: f.lab1Operator, call: get("01jh0000000000000000000000"), code: connect.CodeNotFound, msg: "no hold",
			audit: inTenant("lab1", master.OutcomeFailed)},
	})
}

// testListHolds guards ListHolds: operator on the tenant; states named
// only as active or released; a subject label that is not one invalid;
// none placed is none listed.
func testListHolds(t *testing.T) {
	f := newFixture(t)
	list := func(states []v1.HoldState, subject string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.retention.ListHolds, &v1.ListHoldsRequest{Tenant: "lab1", States: states, SubjectLabel: subject})
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: list(nil, ""), code: connect.CodePermissionDenied},
		{name: "unspecified state", as: f.lab1Operator, call: list([]v1.HoldState{v1.HoldState_HOLD_STATE_UNSPECIFIED}, ""), code: connect.CodeInvalidArgument, msg: "states"},
		{name: "a subject that is not a label", as: f.lab1Operator, call: list(nil, "a/b"), code: connect.CodeInvalidArgument, msg: "subject_label"},
		{name: "none", as: f.lab1Operator, call: list([]v1.HoldState{v1.HoldState_HOLD_STATE_ACTIVE}, "S1"), audit: inTenant("lab1", master.OutcomeDone),
			check: func(t *testing.T, o outcome) {
				if h := o.res.(*v1.ListHoldsResponse).GetHolds(); len(h) != 0 {
					t.Errorf("holds %v", h)
				}
			}},
	})
}
