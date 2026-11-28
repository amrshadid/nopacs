package tenant

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// MaxDryRunSessions bounds the sessions a dry run of a rule set reads: each
// costs one metadata file read per scan.
const MaxDryRunSessions = 1000

// RouteHit is how many receptions one rule of a rule set version routed,
// counted from the receptions' records.
type RouteHit struct {
	// Version, Phase and Rule name the rule; an empty Rule in the archive
	// phase is the rule set's default (no archive rule held).
	Version int32
	Phase   RulePhase
	Rule    string
	// Receptions is how many receptions it routed, Series how many series
	// its forwards sent (0 for an archive rule), and Last when the latest
	// was routed.
	Receptions int64
	Series     int64
	Last       time.Time
}

// RoutedSession is a session a receiver made, as a dry run of a rule set
// reads it: its labels and storage, what the routing chose for it, and the
// rules its receptions were routed by.
type RoutedSession struct {
	// SubjectLabel and SessionLabel name it; StudyUID is its Study
	// Instance UID as the catalog holds it.
	SubjectLabel, SessionLabel, StudyUID string
	// Storage is where it is archived.
	Storage string
	// Route is what the routing chose for it (ReceptionRoute.Archived),
	// with the AE titles of its first file.
	Route ReceptionRoute
	// Matches are the records of the rules its receptions were routed by,
	// each phase and rule once, at the latest version that routed it.
	Matches []RouteMatch
	// Created is when it was made.
	Created time.Time
}

// Routing is a tenant's routing rule sets, versioned per receiver, and the
// records of what they routed. A version is never changed once stored: the
// store refuses an update of one, so a record's version always names the
// rules it was routed by.
type Routing interface {
	// CreateRuleSet stores rs (rs.Validate) as its receiver's next version,
	// Created now and CreatedBy the audit entry's key, with the entry; and,
	// when activate, points the receiver at it in the same transaction
	// under every rule AddReceiver applies to what a receiver names (its
	// storages are the tenant's and archive targets, its senders the
	// tenant's, a profile only on a receiver that de-identifies). It
	// answers the version stored. A receiver the tenant does not have is
	// NotFound.
	CreateRuleSet(ctx context.Context, rs RuleSet, activate bool, now time.Time, audit master.AuditEntry, guard Guard) (RuleSet, error)
	// RuleSet answers one version of a receiver's rule set; version 0 is
	// the one it runs, NotFound when it runs none. A version the receiver
	// does not have is NotFound.
	RuleSet(ctx context.Context, receiver string, version int32) (RuleSet, error)
	// ListRuleSets answers a page of a receiver's rule set versions, the
	// newest first.
	ListRuleSets(ctx context.Context, receiver string, page master.Page) ([]RuleSet, string, error)
	// ActivateRuleSet points the receiver at one of its versions, with the
	// entry, under the rules of CreateRuleSet; version 0 runs none. It
	// answers the version it runs after, the zero rule set for none.
	ActivateRuleSet(ctx context.Context, receiver string, version int32, now time.Time, audit master.AuditEntry, guard Guard) (RuleSet, error)
	// RouteHits answers, for each rule of the receiver's records, how many
	// receptions it routed: of one version, or of every version for 0, in
	// order of version, phase and rule.
	RouteHits(ctx context.Context, receiver string, version int32) ([]RouteHit, error)
	// RecentSessions answers the latest sessions the receiver made, the
	// newest first, at most limit (1 to MaxDryRunSessions).
	RecentSessions(ctx context.Context, receiver string, limit int) ([]RoutedSession, error)
}
