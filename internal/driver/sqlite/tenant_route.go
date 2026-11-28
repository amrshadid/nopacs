package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's routing rule sets and the records of what they routed
// (tenant.Routing), on the tables of migrations/tenant/0001_baseline.sql:
// package sqlkit's one implementation, run here in SQLite's transactions.

// CreateRuleSet implements tenant.Routing.
func (t *Tenant) CreateRuleSet(ctx context.Context, rs tenant.RuleSet, activate bool, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.RuleSet, error) {
	return t.sending().CreateRuleSet(ctx, rs, activate, now, audit, guard)
}

// RuleSet implements tenant.Routing.
func (t *Tenant) RuleSet(ctx context.Context, receiver string, version int32) (tenant.RuleSet, error) {
	return t.sending().RuleSet(ctx, receiver, version)
}

// ListRuleSets implements tenant.Routing.
func (t *Tenant) ListRuleSets(ctx context.Context, receiver string, page master.Page) ([]tenant.RuleSet, string, error) {
	return t.sending().ListRuleSets(ctx, receiver, page)
}

// ActivateRuleSet implements tenant.Routing.
func (t *Tenant) ActivateRuleSet(ctx context.Context, receiver string, version int32, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.RuleSet, error) {
	return t.sending().ActivateRuleSet(ctx, receiver, version, now, audit, guard)
}

// RouteHits implements tenant.Routing.
func (t *Tenant) RouteHits(ctx context.Context, receiver string, version int32) ([]tenant.RouteHit, error) {
	return t.sending().RouteHits(ctx, receiver, version)
}

// RecentSessions implements tenant.Routing.
func (t *Tenant) RecentSessions(ctx context.Context, receiver string, limit int) ([]tenant.RoutedSession, error) {
	return t.sending().RecentSessions(ctx, receiver, limit)
}
