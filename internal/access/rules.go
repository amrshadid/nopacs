package access

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The rules that refuse a request for where it comes from, by name, as an
// event and the audit entry's reason name them. A request off the local
// socket must pass all of them: the installation's denied networks, then its
// allowed networks (both judged by the server before any key is read), then,
// once the key and its grant are known, the tenant's networks, the key's
// networks and the key's client certificate. A refusal by any of them is
// answered as a key that is no good is answered (Unauthenticated, with
// MessageUnauthenticated), so the caller learns nothing of the rule.
const (
	// RuleDeniedNetworks is the installation's access.denied-networks.
	RuleDeniedNetworks = "installation-denied-networks"
	// RuleAllowedNetworks is the installation's access.allowed-networks.
	RuleAllowedNetworks = "installation-allowed-networks"
	// RuleTenantNetworks is the tenant's access.tenant-networks.
	RuleTenantNetworks = "tenant-networks"
	// RuleKeyNetworks is the key's own networks (master.KeyRules).
	RuleKeyNetworks = "key-networks"
	// RuleKeyClientCert is the client certificate the key must come with.
	RuleKeyClientCert = "key-client-certificate"
	// RuleAddressBlocked is a block of an address that made too many
	// requests with bad or unknown keys.
	RuleAddressBlocked = "address-blocked"
	// RuleKeyBlocked is a block of a key refused too often for its grants.
	RuleKeyBlocked = "key-blocked"
)

// RuleReason returns the audit reason of a refusal by rule: the rule's
// name, which an admin reading the log searches for. It never reaches the
// caller.
func RuleReason(rule string) string { return "refused by the rule " + rule }

// EventKind is what an access event reports.
type EventKind string

// The kinds of access event.
const (
	// EventRefused is a request refused by one of the rules above, named
	// in Event.Rule.
	EventRefused EventKind = "access.refused"
	// EventBlocked is an address or a key blocked for its failed calls;
	// Event.Rule is RuleAddressBlocked or RuleKeyBlocked, and Event.Until
	// when the block ends.
	EventBlocked EventKind = "access.blocked"
	// EventKeyRotated is a key rotated; Event.Detail is its successor's id
	// and Event.Until when the rotated key stops working.
	EventKeyRotated EventKind = "key.rotated"
	// EventKeyExpiring is a key that expires within the installation's
	// key.expiry-warning; Event.Until is its expiry. It is raised once a
	// day per key while that holds.
	EventKeyExpiring EventKind = "key.expiring"
)

// Event is one thing about access the installation's admin is told of as it
// happens, besides the audit entry that records it. It carries ids,
// addresses and rule names, never a secret.
type Event struct {
	// Kind is what happened.
	Kind EventKind
	// Time is when, by the server's clock.
	Time time.Time
	// Rule names the rule that refused or blocked, for EventRefused and
	// EventBlocked.
	Rule string
	// KeyID is the key concerned; empty for a request refused before its
	// key was read, or an address blocked.
	KeyID string
	// Address is the caller's address; empty for a rotation made on the
	// local socket and for a key expiring.
	Address string
	// Tenant is the tenant the request named, as it named it; empty for
	// none.
	Tenant string
	// Method is the API method of the request refused; empty for events
	// that are not about one request.
	Method string
	// Until is when a block ends, a rotated key stops working, or an
	// expiring key expires.
	Until time.Time
	// Detail is the successor's id of a rotation; empty otherwise.
	Detail string
}

// Events receives the access events. It is called on the request's path and
// must not block: a sink that writes anywhere slow hands the event to its
// own queue. Nil discards.
type Events func(Event)

// raise hands e to the configured sink, if any.
func (a *Access) raise(e Event) {
	if a.events != nil {
		a.events(e)
	}
}

// keyRuleRefusal returns the rule of k that refuses a request with facts, or
// "": none on the local socket; otherwise the key's networks, when it lists
// any, must hold the caller's address, and the key's client certificate,
// when it names one, must be the one the request presented. An address that
// does not parse holds no network, so a key with networks is refused rather
// than allowed when the server could not say where a request came from.
func keyRuleRefusal(k master.Key, facts Request) string {
	if facts.Local {
		return ""
	}
	if len(k.Rules.Networks) > 0 && !k.Rules.Networks.Contains(addressOf(facts.RemoteAddress)) {
		return RuleKeyNetworks
	}
	if k.Rules.ClientCert != "" && facts.ClientCert != k.Rules.ClientCert {
		return RuleKeyClientCert
	}
	return ""
}

// tenantRuleRefusal reads the networks of the tenant t and reports
// whether they refuse a request with facts: never on the local socket, never
// when the tenant lists none. It returns an error only when the networks
// could not be read, which the check answers as a failure, not a refusal:
// a tenant's rule that cannot be read is never taken as no rule.
func (a *Access) tenantRuleRefusal(ctx context.Context, t TenantRef, facts Request) (bool, error) {
	if facts.Local || a.tenantNetworks == nil {
		return false, nil
	}
	n, err := a.tenantNetworks(ctx, t)
	if err != nil {
		return false, err
	}
	return len(n) > 0 && !n.Contains(addressOf(facts.RemoteAddress)), nil
}

// refuseByRule records a refusal of the request entry stands for by rule, in
// the master's log, raises its event, and returns the answer a key that is
// no good gets.
func (a *Access) refuseByRule(entry master.AuditEntry, rule, tenant string) error {
	a.record(entry, master.OutcomeRefused, RuleReason(rule))
	a.raise(Event{Kind: EventRefused, Time: entry.Time, Rule: rule, KeyID: entry.KeyID,
		Address: entry.RemoteAddress, Tenant: tenant, Method: entry.Method})
	return fault.New(fault.Unauthenticated, MessageUnauthenticated)
}

// RecordRefusal records a request the server refused before or beside the
// check, for one of the rules it judges itself (RuleDeniedNetworks,
// RuleAllowedNetworks, RuleAddressBlocked, RuleKeyBlocked), in the master's
// log as refused with RuleReason(rule), with keyID when the key was known,
// and raises its event: EventRefused, or EventBlocked with until when
// blocked is set (the request that set off a block). It never blocks the
// request and fails nothing, as every refusal's entry: it goes through the
// refusals' queue. It returns the error the caller is answered with, the one
// a key that is no good gets.
func (a *Access) RecordRefusal(facts Request, keyID, rule string, blocked bool, until time.Time) error {
	entry := a.newEntry(facts)
	entry.ID = a.newID()
	if master.ValidateID("key id", keyID) == nil {
		entry.KeyID = keyID
	}
	if entry.Method == "" {
		entry.Method = "unknown"
	}
	a.record(entry, master.OutcomeRefused, RuleReason(rule))
	e := Event{Kind: EventRefused, Time: entry.Time, Rule: rule, KeyID: entry.KeyID, Address: entry.RemoteAddress, Method: entry.Method}
	if blocked {
		e.Kind, e.Until = EventBlocked, until
	}
	a.raise(e)
	return fault.New(fault.Unauthenticated, MessageUnauthenticated)
}

// TenantNetworks returns the networks every key used on the tenant t must be
// used from (the tenant's access.tenant-networks, else the installation's
// value of it, else none). The server gives it, since the value lives in the
// tenant's own database, which this package does not reach. An error is
// answered as a failure of the check.
type TenantNetworks func(ctx context.Context, t TenantRef) (settings.Networks, error)

// CachedTenantNetworks returns read with each tenant's answer kept for
// every, so a request on a tenant does not read the tenant's database for
// its rule: a change of a tenant's networks applies within every. An answer
// that failed is not kept. One entry is kept per tenant id, so the cache is
// as large as the tenants and no larger.
func CachedTenantNetworks(read TenantNetworks, every time.Duration, now func() time.Time) TenantNetworks {
	type entry struct {
		n  settings.Networks
		at time.Time
	}
	var mu sync.Mutex
	kept := map[string]entry{}
	return func(ctx context.Context, t TenantRef) (settings.Networks, error) {
		at := now()
		mu.Lock()
		e, ok := kept[t.ID]
		mu.Unlock()
		if ok && at.Sub(e.at) < every {
			return e.n, nil
		}
		n, err := read(ctx, t)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		kept[t.ID] = entry{n: n, at: at}
		mu.Unlock()
		return n, nil
	}
}

// LogEvents returns an Events that writes each event to log as one line at
// the warning level, "access event", with its fields: the installation's
// admin reads the access events in the server's log, and in the audit log,
// which holds every refusal and rotation whatever happens to a log line.
func LogEvents(log *slog.Logger) Events {
	return func(e Event) {
		attrs := []slog.Attr{slog.String("event", string(e.Kind))}
		for _, f := range []struct{ k, v string }{
			{"rule", e.Rule}, {"key_id", e.KeyID}, {"remote", e.Address}, {"tenant", e.Tenant},
			{"method", e.Method}, {"detail", e.Detail},
		} {
			if f.v != "" {
				attrs = append(attrs, slog.String(f.k, f.v))
			}
		}
		if !e.Until.IsZero() {
			attrs = append(attrs, slog.Time("until", e.Until))
		}
		log.LogAttrs(context.Background(), slog.LevelWarn, "access event", attrs...)
	}
}

// addressOf returns the address a server wrote as a request's remote
// address, "ip" or "ip:port", or the zero Addr when it is neither.
func addressOf(remote string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap.Addr()
	}
	if a, err := netip.ParseAddr(remote); err == nil {
		return a
	}
	return netip.Addr{}
}
