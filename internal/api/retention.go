package api

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The settings retention reads. A server's list that does not hold one (a
// test's own list) is read as its built-in default.
const (
	settingShortenNotice = settings.RetentionShortenNotice
	settingMaxPerPass    = settings.RetentionMaxPerPass
)

// The messages of what a tenant does not have.
const (
	messageNoRule = "the tenant has no retention rule with this name"
	messageNoHold = "the tenant has no hold with this id"
)

// The keys of the details of RetentionService's entries.
const (
	detailRule        = "rule"
	detailIncludeHeld = "include_held"
	detailObserved    = "observed_time"
	detailServerTime  = "server_time"
	detailSessions    = "sessions"
	detailSubject     = "subject_label"
)

// retentionService is RetentionService: a tenant's retention rules, the
// sessions due under them, their disposal, the clock they are read
// against, and holds, kept in the tenant's store. A hold is placed and
// released through the tenant's hold writer (Config.Holds), which puts its
// record on every storage its target touches before its row commits, so it
// outlives a lost database.
type retentionService struct {
	nopacsv1connect.UnimplementedRetentionServiceHandler
	c *core
}

var _ nopacsv1connect.RetentionServiceHandler = retentionService{}

// retention returns st as retention's port.
func retention(st tenant.Store) (tenant.Retention, error) {
	r, ok := st.(tenant.Retention)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no retention rules")
	}
	return r, nil
}

// holdsOf returns st as the holds' port.
func holdsOf(st tenant.Store) (tenant.Holds, error) {
	h, ok := st.(tenant.Holds)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no holds")
	}
	return h, nil
}

// disposal returns st as disposal's port.
func disposal(st tenant.Store) (tenant.Disposal, error) {
	dp, ok := st.(tenant.Disposal)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no disposals")
	}
	return dp, nil
}

// ruleError gives a store's NotFound for a rule the contract's message.
func ruleError(err error) error {
	if fault.KindOf(err) == fault.NotFound {
		return fault.Wrap(fault.NotFound, err, messageNoRule)
	}
	return err
}

// tenantSetting returns the value of the setting named name on the tenant
// of st, and false when the server's list does not hold it.
func (c *core) tenantSetting(ctx context.Context, st tenant.Store, name string) (string, bool, error) {
	if _, known := c.settings.Lookup(name); !known {
		return "", false, nil
	}
	v, err := c.values().Tenant(ctx, st, name)
	return v, err == nil, err
}

// retentionNotice returns retention.shorten-notice on the tenant of st,
// tenant.DefaultShortenNotice when the server's list does not hold it.
func (c *core) retentionNotice(ctx context.Context, st tenant.Store) (time.Duration, error) {
	v, known, err := c.tenantSetting(ctx, st, settingShortenNotice)
	if err != nil || !known {
		return tenant.DefaultShortenNotice, err
	}
	d, err := settings.ParseDuration(v)
	if err != nil || d < tenant.MinShortenNotice || d > tenant.MaxShortenNotice {
		return 0, fault.New(fault.Internal, "api: the tenant's "+settingShortenNotice+" is not a notice of 1 to 365 days")
	}
	return d, nil
}

// maxPerPass returns retention.max-per-pass on the tenant of st, the most
// sessions one dispose job deletes; tenant.MaxDisposeSessions when the
// server's list does not hold it.
func (c *core) maxPerPass(ctx context.Context, st tenant.Store) (int, error) {
	v, known, err := c.tenantSetting(ctx, st, settingMaxPerPass)
	if err != nil || !known {
		return tenant.MaxDisposeSessions, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > tenant.MaxPerPass {
		return 0, fault.New(fault.Internal, "api: the tenant's "+settingMaxPerPass+" is not a whole number of 1 to 100000")
	}
	return n, nil
}

// periodOf returns a period a request gives, zero for none, checked.
func periodOf(field string, p *v1.RetentionPeriod) (tenant.RetentionPeriod, error) {
	if p == nil {
		return tenant.RetentionPeriod{}, nil
	}
	out := tenant.RetentionPeriod{Years: int(p.GetYears()), Days: int(p.GetDays())}
	if err := out.Validate(); err != nil {
		return out, fieldOf(field, err)
	}
	return out, nil
}

// periodMessage returns a period as the contract writes it, nil for none.
func periodMessage(p tenant.RetentionPeriod) *v1.RetentionPeriod {
	if p.IsZero() {
		return nil
	}
	return &v1.RetentionPeriod{Years: int32(p.Years), Days: int32(p.Days)} //nolint:gosec // G115: checked at most 100 and 365.
}

// actionOf returns the action a request names: unspecified is review.
func actionOf(field string, a v1.RetentionAction) (tenant.RetentionAction, error) {
	switch a {
	case v1.RetentionAction_RETENTION_ACTION_UNSPECIFIED, v1.RetentionAction_RETENTION_ACTION_REVIEW:
		return tenant.RetentionReview, nil
	case v1.RetentionAction_RETENTION_ACTION_DELETE:
		return tenant.RetentionDelete, nil
	}
	return "", invalid(field, "is review or delete")
}

// actionMessage returns an action as the contract writes it.
func actionMessage(a tenant.RetentionAction) v1.RetentionAction {
	if a == tenant.RetentionDelete {
		return v1.RetentionAction_RETENTION_ACTION_DELETE
	}
	return v1.RetentionAction_RETENTION_ACTION_REVIEW
}

// selectorOf returns the selector a request gives, checked by its form.
func selectorOf(field string, s *v1.RetentionSelector) (tenant.RetentionSelector, error) {
	out := tenant.RetentionSelector{Receiver: s.GetReceiver(), Route: s.GetRoute(), Storage: s.GetStorage()}
	for _, x := range []struct{ name, value string }{{"receiver", out.Receiver}, {"route", out.Route}, {"storage", out.Storage}} {
		if x.value != "" {
			if err := itemName(field+"."+x.name, x.value); err != nil {
				return out, err
			}
		}
	}
	if err := out.Validate(); err != nil {
		return out, fieldOf(field, err)
	}
	return out, nil
}

// ruleOf returns the rule a request gives, every field checked by its form.
func ruleOf(m *v1.RetentionRule) (tenant.RetentionRule, error) {
	if m == nil {
		return tenant.RetentionRule{}, invalid("rule", "is required")
	}
	if err := itemName("rule.name", m.GetName()); err != nil {
		return tenant.RetentionRule{}, err
	}
	r := tenant.RetentionRule{Name: m.GetName()}
	var err error
	if r.Selector, err = selectorOf("rule.selector", m.GetSelector()); err != nil {
		return r, err
	}
	if r.KeepAtLeast, err = periodOf("rule.keep_at_least", m.GetKeepAtLeast()); err != nil {
		return r, err
	}
	if r.DisposeAfter, err = periodOf("rule.dispose_after", m.GetDisposeAfter()); err != nil {
		return r, err
	}
	if r.Action, err = actionOf("rule.action", m.GetAction()); err != nil {
		return r, err
	}
	if err := r.Validate(); err != nil {
		return r, fieldOf("rule", err)
	}
	return r, nil
}

// ruleMessage returns a rule as the contract writes it.
func ruleMessage(r tenant.RetentionRule) *v1.RetentionRule {
	m := &v1.RetentionRule{Name: r.Name, KeepAtLeast: periodMessage(r.KeepAtLeast), DisposeAfter: periodMessage(r.DisposeAfter),
		Action: actionMessage(r.Action), CreateTime: timestamp(r.Created), UpdateTime: timestamp(r.Updated),
		NoticeUntil: timestamp(r.NoticeUntil), SessionsDue: r.SessionsDue}
	if !r.Selector.IsZero() {
		m.Selector = &v1.RetentionSelector{Receiver: r.Selector.Receiver, Route: r.Selector.Route, Storage: r.Selector.Storage}
	}
	return m
}

// ruleDetails returns a rule's selector, periods and action as an entry's
// details.
func ruleDetails(r tenant.RetentionRule) map[string]string {
	period := func(p tenant.RetentionPeriod) string {
		if p.IsZero() {
			return "none"
		}
		return strconv.Itoa(p.Years) + "y" + strconv.Itoa(p.Days) + "d"
	}
	sel := "tenant"
	switch {
	case r.Selector.Storage != "":
		sel = "storage/" + r.Selector.Storage
	case r.Selector.Route != "":
		sel = "receiver/" + r.Selector.Receiver + "/route/" + r.Selector.Route
	case r.Selector.Receiver != "":
		sel = "receiver/" + r.Selector.Receiver
	}
	return map[string]string{"selector": sel, "keep_at_least": period(r.KeepAtLeast), "dispose_after": period(r.DisposeAfter),
		"action": string(r.Action)}
}

// AddRetentionRule adds a rule to a tenant.
func (s retentionService) AddRetentionRule(ctx context.Context, req *v1.AddRetentionRuleRequest) (*v1.AddRetentionRuleResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RetentionServiceAddRetentionRuleProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	r, err := ruleOf(req.GetRule())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	facts := access.Facts{Object: r.Name, Details: ruleDetails(r)}
	notice, err := c.retentionNotice(ctx, st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rs, err := retention(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	out, err := rs.AddRetentionRule(ctx, r, tenant.RetentionChange{Now: c.now(), Notice: notice}, changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.AddRetentionRuleResponse{Rule: ruleMessage(out)}, nil
}

// GetRetentionRule answers one rule of a tenant, with its sessions due now.
func (s retentionService) GetRetentionRule(ctx context.Context, req *v1.GetRetentionRuleRequest) (*v1.GetRetentionRuleResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RetentionServiceGetRetentionRuleProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	rs, err := retention(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := rs.RetentionRule(ctx, req.GetName(), c.now())
	if err != nil {
		return nil, c.failIn(d, st, ruleError(err))
	}
	c.done(d, st)
	return &v1.GetRetentionRuleResponse{Rule: ruleMessage(out)}, nil
}

// ListRetentionRules answers a page of a tenant's rules, by name.
func (s retentionService) ListRetentionRules(ctx context.Context, req *v1.ListRetentionRulesRequest) (*v1.ListRetentionRulesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RetentionServiceListRetentionRulesProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	rs, err := retention(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := rs.ListRetentionRules(ctx, c.now(), page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListRetentionRulesResponse{Rules: make([]*v1.RetentionRule, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Rules = append(out.Rules, ruleMessage(it))
	}
	return out, nil
}

// ruleMask is what SetRetentionRule's update_mask may name.
var ruleMask = maskRules{
	fields: []string{"selector", "keep_at_least", "dispose_after", "action"},
	refused: map[string]string{
		"name":              "is the rule's name, which does not change",
		"selector.receiver": "names a field of the selector: name the whole selector",
		"selector.route":    "names a field of the selector: name the whole selector",
		"selector.storage":  "names a field of the selector: name the whole selector",
		"create_time":       "is answered by the server and cannot be set",
		"update_time":       "is answered by the server and cannot be set",
		"notice_until":      "is answered by the server and cannot be set",
		"sessions_due":      "is answered by the server and cannot be set",
	},
}

// SetRetentionRule changes the fields of a rule its update_mask names.
func (s retentionService) SetRetentionRule(ctx context.Context, req *v1.SetRetentionRuleRequest) (*v1.SetRetentionRuleResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RetentionServiceSetRetentionRuleProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: maskDetails(req.GetUpdateMask().GetPaths())}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	in := req.GetRule()
	if in == nil {
		return nil, c.failWith(d, st, facts, invalid("rule", "is required: its name names the rule to change"))
	}
	if err := itemName("rule.name", in.GetName()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	paths, err := ruleMask.paths(req.GetUpdateMask())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rs, err := retention(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	now := c.now()
	r, err := rs.RetentionRule(ctx, in.GetName(), now)
	if err != nil {
		return nil, c.failWith(d, st, facts, ruleError(err))
	}
	for _, p := range paths {
		switch p {
		case "selector":
			r.Selector, err = selectorOf("rule.selector", in.GetSelector())
		case "keep_at_least":
			r.KeepAtLeast, err = periodOf("rule.keep_at_least", in.GetKeepAtLeast())
		case "dispose_after":
			r.DisposeAfter, err = periodOf("rule.dispose_after", in.GetDisposeAfter())
		case "action":
			r.Action, err = actionOf("rule.action", in.GetAction())
		}
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
	}
	if err := r.Validate(); err != nil {
		return nil, c.failWith(d, st, facts, fieldOf("rule", err))
	}
	for k, v := range ruleDetails(r) {
		facts.Details[k] = v
	}
	notice, err := c.retentionNotice(ctx, st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	out, err := rs.SetRetentionRule(ctx, r, tenant.RetentionChange{Now: now, Notice: notice}, changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, ruleError(err))
	}
	return &v1.SetRetentionRuleResponse{Rule: ruleMessage(out)}, nil
}

// RemoveRetentionRule removes a rule at once; what it kept is kept until
// the notice ends.
func (s retentionService) RemoveRetentionRule(ctx context.Context, req *v1.RemoveRetentionRuleRequest) (*v1.RemoveRetentionRuleResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RetentionServiceRemoveRetentionRuleProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	notice, err := c.retentionNotice(ctx, st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	rs, err := retention(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := rs.RemoveRetentionRule(ctx, req.GetName(), tenant.RetentionChange{Now: c.now(), Notice: notice}, changeEntry(d, access.Facts{}),
		c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failIn(d, st, ruleError(err))
	}
	return &v1.RemoveRetentionRuleResponse{Rule: ruleMessage(out)}, nil
}

// dueMessage returns a due session as the contract writes it.
func dueMessage(s tenant.DueSession) *v1.DueSession {
	return &v1.DueSession{SubjectLabel: s.SubjectLabel, SessionLabel: s.SessionLabel, Rule: s.Rule, Action: actionMessage(s.Action),
		StartTime: timestamp(s.Start), DueTime: timestamp(s.Due), Held: s.Held, Instances: s.Instances, Bytes: s.Bytes, Undone: s.Undone}
}

// ListDueSessions answers a page of the sessions due now, oldest due
// first.
func (s retentionService) ListDueSessions(ctx context.Context, req *v1.ListDueSessionsRequest) (*v1.ListDueSessionsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RetentionServiceListDueSessionsProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailRule: req.GetRule(), detailIncludeHeld: strconv.FormatBool(req.GetIncludeHeld())}}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	if req.GetRule() != "" {
		if err := itemName("rule", req.GetRule()); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rs, err := retention(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	now := c.now()
	if req.GetRule() != "" {
		if _, err := rs.RetentionRule(ctx, req.GetRule(), now); err != nil {
			return nil, c.failWith(d, st, facts, ruleError(err))
		}
	}
	items, next, err := rs.DueSessions(ctx, tenant.DueFilter{Rule: req.GetRule(), IncludeHeld: req.GetIncludeHeld(), Now: now}, page)
	if err != nil {
		return nil, c.failWith(d, st, facts, listError(err, page.Cursor))
	}
	c.record(d, st, master.OutcomeDone, "", facts)
	out := &v1.ListDueSessionsResponse{Sessions: make([]*v1.DueSession, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Sessions = append(out.Sessions, dueMessage(it))
	}
	return out, nil
}

// disposeTokenPrefix starts every token a disposal's preview answers, so
// a token of another kind (a delete's, a switch's) is told apart at once.
const disposeTokenPrefix = "k1."

// disposeMACLength is how many bytes of the HMAC-SHA-256 a disposal's
// token keeps: 128 bits, as RFC 2104 5 allows a truncated MAC, so the token
// stays within the contract's 128 bytes.
const disposeMACLength = 16

// keyedDisposeConfirm wraps the store's disposal token in a MAC under the
// installation's confirm key, so only this server makes a token a disposal
// takes: the store's token is a digest a client could compute from what
// the preview shows. The store's token stays readable inside it, so a
// disposal sent again after its job deleted the sessions (when no preview
// would list them any more) still finds the job its token made.
func keyedDisposeConfirm(key *secret.DerivedKey, token string) (string, error) {
	mac, err := key.MAC([]byte(token))
	if err != nil {
		return "", err
	}
	return disposeTokenPrefix + token + "." + base64.RawURLEncoding.EncodeToString(mac[:disposeMACLength]), nil
}

// unkeyDisposeConfirm returns the store's token inside a token
// keyedDisposeConfirm made, or a precondition failure for any other.
func unkeyDisposeConfirm(key *secret.DerivedKey, keyed string) (string, error) {
	bad := precondition("the confirm token is not one this server's preview of a disposal gave")
	rest, ok := strings.CutPrefix(keyed, disposeTokenPrefix)
	i := strings.LastIndexByte(rest, '.')
	if !ok || i < 0 {
		return "", bad
	}
	token, got := rest[:i], rest[i+1:]
	if _, _, ok := tenant.DisposeConfirmParts(token); !ok {
		return "", bad
	}
	mac, err := key.MAC([]byte(token))
	if err != nil {
		return "", err
	}
	want := base64.RawURLEncoding.EncodeToString(mac[:disposeMACLength])
	if !hmac.Equal([]byte(got), []byte(want)) {
		return "", bad
	}
	return token, nil
}

// confirmKey derives the installation's key for confirm tokens.
func (c *core) confirmKey() (*secret.DerivedKey, error) {
	return c.keys.Derive(secret.ConfirmToken)
}

// disposalMessage returns a disposal's preview as the contract writes it.
func disposalMessage(p tenant.DisposalPreview, confirm string) *v1.DisposalPreview {
	m := &v1.DisposalPreview{Rule: p.Rule, SessionsNotListed: p.NotListed, SessionsHeld: p.Held, Counts: correctionCounts(p.Counts),
		Confirm: confirm, Sessions: make([]*v1.DueSession, 0, len(p.Sessions))}
	for _, s := range p.Sessions {
		m.Sessions = append(m.Sessions, dueMessage(s))
	}
	return m
}

// checkDispose returns the Invalid error of a disposal request, and the
// most sessions its preview lists.
func checkDispose(req *v1.DisposeRequest) (int, error) {
	if err := itemName("rule", req.GetRule()); err != nil {
		return 0, err
	}
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return 0, fieldOf("reason", err)
	}
	switch {
	case req.GetValidateOnly() && req.GetConfirm() != "":
		return 0, invalid("confirm", "is empty with validate_only")
	case !req.GetValidateOnly() && req.GetConfirm() == "":
		return 0, invalid("confirm", "is the confirm token of a preview (validate_only) of the same rule")
	case len(req.GetConfirm()) > tenant.MaxConfirmLength:
		return 0, invalid("confirm", "is at most 128 bytes")
	case req.GetMaxSessions() < 0 || req.GetMaxSessions() > tenant.MaxDisposeSessions:
		return 0, invalid("max_sessions", "is 0 to 1000; 0 asks for 100")
	}
	limit := int(req.GetMaxSessions())
	if limit == 0 {
		limit = tenant.DefaultDisposeSessions
	}
	return limit, nil
}

// Dispose answers a disposal's preview and its token, or queues the
// dispose job a token was given for; a token used already answers its job.
func (s retentionService) Dispose(ctx context.Context, req *v1.DisposeRequest) (*v1.DisposeResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RetentionServiceDisposeProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailValidateOnly: strconv.FormatBool(req.GetValidateOnly())}}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	limit, err := checkDispose(req)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailReason] = req.GetReason()
	dp, err := disposal(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	key, err := c.confirmKey()
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if req.GetValidateOnly() {
		most, err := c.maxPerPass(ctx, st)
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		if limit > most {
			return nil, c.failWith(d, st, facts, invalid("max_sessions", "is no more than the tenant's retention.max-per-pass, "+strconv.Itoa(most)))
		}
		pv, err := dp.PreviewDisposal(ctx, req.GetRule(), limit, c.now())
		if err != nil {
			return nil, c.failWith(d, st, facts, ruleError(err))
		}
		keyed, err := keyedDisposeConfirm(key, pv.Confirm)
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		facts.Details[detailSessions] = strconv.Itoa(len(pv.Sessions))
		c.record(d, st, master.OutcomeDone, "", facts)
		return &v1.DisposeResponse{Preview: disposalMessage(pv, keyed)}, nil
	}
	token, err := unkeyDisposeConfirm(key, req.GetConfirm())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failWith(d, st, facts, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	facts.Details[detailJob] = id
	job, pv, made, err := dp.QueueDispose(ctx, tenant.DisposeQueue{ID: id, Rule: req.GetRule(), Reason: req.GetReason(), Confirm: token,
		KeyID: callerKeyID(d), Now: c.now()}, changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, ruleError(err))
	}
	_ = made
	return &v1.DisposeResponse{Preview: disposalMessage(pv, ""), Job: c.newJobViews(p, req.GetTenant()).message(ctx, job)}, nil
}

// retentionStatusMessage returns where a tenant's retention stands, as the
// contract writes it, from its clock and, when given, its rules' and
// holds' counts.
func retentionStatusMessage(clock tenant.RetentionClock) *v1.RetentionStatus {
	m := &v1.RetentionStatus{LastPassTime: timestamp(clock.LastPass), Paused: clock.Paused}
	if clock.Paused {
		m.PauseReason = clock.Reason
		m.ClockStep = durationpb.New(clock.Step)
	}
	return m
}

// ConfirmClock ends a pause of retention for a clock jump, on one tenant
// or on every tenant paused.
func (s retentionService) ConfirmClock(ctx context.Context, req *v1.ConfirmClockRequest) (*v1.ConfirmClockResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RetentionServiceConfirmClockProcedure)
	if err != nil {
		return nil, err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	now := c.now()
	facts := access.Facts{Details: map[string]string{detailScope: scope.String(), detailServerTime: now.UTC().Format(time.RFC3339)}}
	if req.GetObservedTime() != nil {
		facts.Details[detailObserved] = req.GetObservedTime().AsTime().UTC().Format(time.RFC3339)
	}
	st, err := c.sinkFor(ctx, d, scope, facts.Details)
	if err != nil {
		return nil, err
	}
	if req.GetObservedTime() == nil || req.GetObservedTime().CheckValid() != nil {
		return nil, c.failWith(d, st, facts, invalid("observed_time", "is required: the time read from a clock the admin trusts"))
	}
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return nil, c.failWith(d, st, facts, fieldOf("reason", err))
	}
	facts.Details[detailReason] = req.GetReason()
	confirm := tenant.ClockConfirm{Observed: req.GetObservedTime().AsTime(), Now: now, Reason: req.GetReason()}
	if err := tenant.CheckClockAgreement(confirm.Observed, now); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if !scope.IsInstallation() {
		rs, err := retention(st)
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		clock, paused, err := rs.ConfirmClock(ctx, confirm, changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		out := &v1.ConfirmClockResponse{Tenants: []string{}, Status: retentionStatusMessage(clock)}
		if paused {
			out.Tenants = []string{scope.Tenant}
		}
		return out, nil
	}
	all, err := c.confirmEveryClock(ctx, d, confirm, facts)
	if err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	facts.Details[detailTenants] = clip(joinDetail(all.resumed), master.MaxAuditFieldLength-8)
	outcome, reason := master.OutcomeDone, ""
	if len(all.refused) > 0 {
		// A tenant left paused is never recorded as a plain success: the
		// entry says which were not confirmed, and refused when the key's
		// own role was the cause (it lost its grant after the request was
		// let in), as the check's refusals are.
		facts.Details[detailTenantsRefused] = clip(joinDetail(all.refused), master.MaxAuditFieldLength-8)
		outcome, reason = master.OutcomeFailed, "the confirmation of "+strconv.Itoa(len(all.refused))+" paused tenants was refused or failed"
		if all.denied {
			outcome = master.OutcomeRefused
		}
	}
	c.record(d, nil, outcome, reason, facts)
	return &v1.ConfirmClockResponse{Tenants: all.resumed, TenantsRefused: all.refused}, nil
}

// detailTenantsRefused is the names of the paused tenants a confirmation on
// the installation left paused, joined by commas.
const detailTenantsRefused = "tenants_refused"

// everyClock is what a confirmation on the installation did: the tenants
// resumed and those left paused, each ordered by name, and whether the
// key's role refused any.
type everyClock struct {
	resumed, refused []string
	denied           bool
}

// confirmEveryClock confirms the clock of every tenant whose retention is
// paused, as the installation's admin. Each tenant's confirmation is
// written in its own log; a tenant whose confirmation is refused (the key
// no longer admin on the installation, the tenant moved or fenced since)
// or fails is answered left paused. A tenant whose database does not
// answer when its clock is read, or that is moved or fenced before, is
// passed over: it is not known to be paused.
func (c *core) confirmEveryClock(ctx context.Context, d access.Decision, confirm tenant.ClockConfirm, facts access.Facts) (everyClock, error) {
	out := everyClock{resumed: []string{}, refused: []string{}}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		rows, next, err := c.master.ListTenants(ctx, page)
		if err != nil {
			return everyClock{}, err
		}
		for _, t := range rows {
			st, err := c.reg.Store(t.Name)
			if err != nil {
				continue
			}
			rs, ok := st.(tenant.Retention)
			if !ok {
				continue
			}
			clock, err := rs.RetentionClock(ctx)
			if err != nil || !clock.Paused {
				continue
			}
			entry := changeEntry(d, access.Facts{TenantName: t.Name, Details: facts.Details})
			_, paused, err := rs.ConfirmClock(ctx, confirm, entry, c.installationGuard(d, t.Name))
			switch {
			case err != nil:
				out.refused = append(out.refused, t.Name)
				if _, refusal := access.RefusedAtChange(err); refusal != nil {
					out.denied = true
				}
			case paused:
				out.resumed = append(out.resumed, t.Name)
			}
		}
		if next == "" {
			slices.Sort(out.resumed)
			slices.Sort(out.refused)
			return out, nil
		}
		page.Cursor = next
	}
}

// installationGuard returns the check a change the installation's admin
// makes in the tenant named name makes of its caller: the master's check
// that d's key still holds admin on the installation, and the tenant
// writable.
func (c *core) installationGuard(d access.Decision, name string) tenant.Guard {
	by := d.Actor(master.Installation, master.RoleAdmin)
	return func(ctx context.Context) error {
		if c.seams.guarding != nil {
			c.seams.guarding()
		}
		if err := c.master.CheckActor(ctx, by); err != nil {
			return err
		}
		return c.writable(name)
	}
}

// holdMessage returns a hold as the contract writes it.
func holdMessage(h tenant.Hold) *v1.Hold {
	state := v1.HoldState_HOLD_STATE_ACTIVE
	if h.State == tenant.HoldReleased {
		state = v1.HoldState_HOLD_STATE_RELEASED
	}
	return &v1.Hold{Id: h.ID, Target: &v1.HoldTarget{SubjectLabel: h.Target.Subject, SessionLabel: h.Target.Session, ScanLabel: h.Target.Scan,
		WholeTenant: h.Target.WholeTenant}, Reason: h.Reason, State: state, PlacedByKeyId: h.PlacedBy, PlaceTime: timestamp(h.Placed),
		ReleaseReason: h.ReleaseReason, ReleasedByKeyId: h.ReleasedBy, ReleaseTime: timestamp(h.Released), FoundOnStorage: h.FoundOnStorage,
		Storages: h.Storages, StoragesPending: h.StoragesPending}
}

// GetHold answers one hold, with its reason.
func (s retentionService) GetHold(ctx context.Context, req *v1.GetHoldRequest) (*v1.GetHoldResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RetentionServiceGetHoldProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := correctionID(req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if tenant.ValidateHoldID(req.GetId()) != nil {
		return nil, c.failIn(d, st, fault.New(fault.NotFound, messageNoHold))
	}
	hs, err := holdsOf(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	h, err := hs.Hold(ctx, req.GetId())
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoHold)
	}
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	return &v1.GetHoldResponse{Hold: holdMessage(h)}, nil
}

// ListHolds answers a page of a tenant's holds, newest first.
func (s retentionService) ListHolds(ctx context.Context, req *v1.ListHoldsRequest) (*v1.ListHoldsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RetentionServiceListHoldsProcedure)
	if err != nil {
		return nil, err
	}
	states := make([]string, 0, len(req.GetStates()))
	for _, s := range req.GetStates() {
		states = append(states, s.String())
	}
	facts := access.Facts{Details: map[string]string{detailStates: clip(joinDetail(states), 256), detailSubject: req.GetSubjectLabel()}}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	f := tenant.HoldFilter{Subject: req.GetSubjectLabel()}
	if len(req.GetStates()) > tenant.MaxHoldStates {
		return nil, c.failWith(d, st, facts, invalid("states", "names at most 16 states"))
	}
	for _, s := range req.GetStates() {
		switch s {
		case v1.HoldState_HOLD_STATE_ACTIVE:
			f.States = append(f.States, tenant.HoldActive)
		case v1.HoldState_HOLD_STATE_RELEASED:
			f.States = append(f.States, tenant.HoldReleased)
		default:
			return nil, c.failWith(d, st, facts, invalid("states", "names active or released, never HOLD_STATE_UNSPECIFIED"))
		}
	}
	if err := f.Validate(); err != nil {
		return nil, c.failWith(d, st, facts, fieldOf("subject_label", err))
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	hs, err := holdsOf(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	items, next, err := hs.ListHolds(ctx, f, page)
	if err != nil {
		return nil, c.failWith(d, st, facts, listError(err, page.Cursor))
	}
	c.record(d, st, master.OutcomeDone, "", facts)
	out := &v1.ListHoldsResponse{Holds: make([]*v1.Hold, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Holds = append(out.Holds, holdMessage(it))
	}
	return out, nil
}
