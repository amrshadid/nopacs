package access

import (
	"context"
	"crypto/sha256"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// The messages a refused caller is shown. Each is one fixed text for its
// kind, whatever the real cause, so the answer cannot be used to tell an
// unknown key from a revoked one, or a tenant that does not exist from one
// the key has no grant on (access.proto, "What a caller may do"). The real
// cause is in the audit entry's reason.
const (
	// MessageUnauthenticated answers every credential that is no good.
	MessageUnauthenticated = "the request carries no valid key"
	// MessageDenied answers a good key with no sufficient grant, and a good
	// key naming a tenant that does not exist when it may not list every
	// tenant.
	MessageDenied = "the key may not do this"
	// MessageNoTenant answers a caller who may list every tenant and names
	// one that does not exist: such a caller learns nothing new from it, and
	// an operator who mistypes a name is told so.
	MessageNoTenant = "no tenant has this name"
	// MessageMasked answers a masked key that asks for a value not in its
	// archived, de-identified form. It says only what the key is, never
	// whether what it named exists or what it holds.
	MessageMasked = "a masked key is never answered an original value"
	// MessageMaskedDestination answers a masked key that asks to choose
	// where archived data goes. It says only what the key is.
	MessageMaskedDestination = "a masked key may not choose where archived data goes; a person does"
	// MessageMaskedWrite answers a masked key that asks to write values
	// into archived files. It says only what the key is.
	MessageMaskedWrite = "a masked key may not write values into archived files; a person does"
)

// The reasons a refusal's audit entry records. They are for the admin who
// reads the log and never reach the refused caller.
const (
	reasonNoKey        = "no key"
	reasonMalformed    = "malformed key"
	reasonUnknownKey   = "unknown key"
	reasonWrongSecret  = "wrong secret"
	reasonRevoked      = "key revoked"
	reasonExpired      = "key expired"
	reasonSuspended    = "key suspended"
	reasonNoLocalUser  = "local caller without a usable user id"
	reasonNoGrant      = "no sufficient grant"
	reasonNoTenant     = "no such tenant"
	reasonBadCheck     = "server error: the check was called without a valid requirement or method"
	reasonStoreFailure = "the master database failed during the check"
	reasonTenantRule   = "the tenant's networks could not be read from its database"
	reasonGone         = "the request ended before the check finished: canceled by the caller or past its deadline"
	reasonMasked       = "a masked key asked for original values"
	reasonMaskedPlace  = "a masked key asked to choose where archived data goes"
	reasonMaskedWrite  = "a masked key asked to write values into archived files"
)

// maxGrantPages bounds how many pages of a key's grants WhoAmI reads: a
// thousand grants a page, so a hundred thousand grants, beyond any
// installation's tenants. A store that keeps returning a cursor is a broken
// store, and the call fails rather than loop.
const maxGrantPages = 100

// unknownKeyHash is what the secret of a key with an unknown id is compared
// with, so that answering an unknown id costs the same hash and comparison
// as answering a wrong secret, and the time of the answer does not tell the
// two apart.
var unknownKeyHash [sha256.Size]byte

// Check decides whether the caller cred may make a request that needs req,
// and is the only way to get a Decision. The server calls it once for every
// request. Its steps, in the order of the API contract:
//
//  1. The credential: the key is parsed, read by its id, its secret compared
//     in constant time, and refused when revoked or expired; or it is the
//     local operator. Any failure is Unauthenticated with
//     MessageUnauthenticated. A credential Authenticate returned has had
//     this step and is taken as it was accepted.
//  2. For a key, one RolesFor reads its role on the installation and, when
//     req names a tenant, whether that tenant exists, its id, and the key's
//     role there, by the name as the request gave it, every time: there is
//     no cache, so a grant removed is gone on the next request. The local
//     operator, who has no grants, reads the tenant by name instead.
//  3. A missing tenant is NotFound with MessageNoTenant for a caller who may
//     list every tenant, and otherwise Denied with MessageDenied, exactly as
//     for an existing tenant without a grant: both make the same two calls,
//     Key then RolesFor, and queue one audit entry.
//  4. The role is compared with req; too little is Denied.
//
// A refusal's audit entry is handed to the audit writer for the master's log,
// so a refused caller causes no write in a tenant's database, with its real
// cause and the tenant's name as given; it is not written on the request's
// path. When the request is allowed, the Decision makes its entries. A key
// that passes step 1 and was not marked used within the minute has its last
// use marked, off the request's path. A store that fails is Unavailable or
// Internal, never a refusal.
func (a *Access) Check(ctx context.Context, cred Credential, req Requirement, facts Request) (Decision, error) {
	entry := a.newEntry(facts)
	entry.ID = a.newID()
	if master.ValidateID("audit entry id", entry.ID) != nil {
		// The id source is broken: no entry could be recorded, so the
		// request fails rather than go unrecorded.
		return Decision{}, fault.New(fault.Internal, "internal error")
	}
	if entry.Method == "" || !req.valid() || len(facts.Details) > MaxRequestDetails {
		// A server bug, not the caller's doing: it fails, and is recorded.
		if entry.Method == "" {
			entry.Method = "unknown"
		}
		entry.Details = nil
		a.record(entry, master.OutcomeFailed, reasonBadCheck)
		return Decision{}, fault.New(fault.Internal, "internal error")
	}
	if req.namesTenant() {
		entry.TenantName = clipTenantName(req.tenant)
	}

	p, reason, err := a.authenticate(ctx, cred, facts, &entry)
	if err != nil {
		return Decision{}, a.fail(ctx, entry, err)
	}
	if reason != "" {
		a.record(entry, master.OutcomeRefused, reason)
		return Decision{}, fault.New(fault.Unauthenticated, MessageUnauthenticated)
	}

	d := Decision{newID: a.newID, clock: a.clock, entered: new(atomic.Bool)}
	var tenantMissing bool
	switch {
	case p.isKey:
		name := ""
		if req.namesTenant() {
			// The name as the request gave it, whether or not a tenant has
			// it: a missing tenant and a missing grant make the same call
			// with the same kind of argument, and the store runs the same
			// statement for both, a name that is not valid included.
			name = req.tenant
		}
		r, err := a.store.RolesFor(ctx, p.key.ID, name)
		if err != nil {
			return Decision{}, a.fail(ctx, entry, err)
		}
		p.instRole = r.Installation
		if req.namesTenant() {
			if r.TenantFound {
				p.tenantID, p.tenantRole = r.TenantID, r.Tenant
				d.tenant, d.hasTenant = TenantRef{ID: r.TenantID, Name: req.tenant}, true
			} else {
				tenantMissing = true
			}
		}
	case req.namesTenant():
		// The local operator holds no grant; only the tenant is read.
		t, err := a.store.TenantByName(ctx, req.tenant)
		switch kind := fault.KindOf(err); {
		case err == nil:
			d.tenant, d.hasTenant = TenantRef{ID: t.ID, Name: req.tenant}, true
		case kind == fault.NotFound || kind == fault.Invalid:
			tenantMissing = true
		default:
			return Decision{}, a.fail(ctx, entry, err)
		}
	}
	d.principal = p

	switch {
	case tenantMissing && req.kind == needTenantGrants && atLeast(p.Role(master.Installation), master.RoleAdmin):
		// The installation's admin changes grants in the master, which
		// answers a tenant it does not have after the key is checked
		// again, as for any change of grants: the check lets it through.
	case tenantMissing && p.CanListTenants():
		a.record(entry, master.OutcomeFailed, reasonNoTenant)
		return Decision{}, fault.New(fault.NotFound, MessageNoTenant)
	case tenantMissing:
		a.record(entry, master.OutcomeRefused, reasonNoTenant)
		return Decision{}, fault.New(fault.Denied, MessageDenied)
	case !allows(p, req, d.tenant.ID):
		a.record(entry, master.OutcomeRefused, reasonNoGrant)
		return Decision{}, fault.New(fault.Denied, MessageDenied)
	case req.originals && p.Masked():
		// After the grant, so a masked key with no grant is told what any
		// key with no grant is told, and learns nothing of the tenant.
		a.record(entry, master.OutcomeRefused, reasonMasked)
		return Decision{}, fault.New(fault.Denied, MessageMasked)
	case req.destination && p.Masked():
		// After the grant, as for originals.
		a.record(entry, master.OutcomeRefused, reasonMaskedPlace)
		return Decision{}, fault.New(fault.Denied, MessageMaskedDestination)
	case req.writes && p.Masked():
		// After the grant, as for originals.
		a.record(entry, master.OutcomeRefused, reasonMaskedWrite)
		return Decision{}, fault.New(fault.Denied, MessageMaskedWrite)
	}
	// A change of grants an installation's admin makes is a change of the
	// master, judged by the installation's networks alone, as a tenant's
	// registration read by the installation is.
	registrationByInstallation := (req.kind == needTenantRegistration && p.CanListTenants()) ||
		(req.kind == needTenantGrants && atLeast(p.Role(master.Installation), master.RoleAdmin))
	if d.hasTenant && p.isKey && !registrationByInstallation {
		// The tenant's networks are judged only once the caller may act on
		// the tenant, so a caller with no grant there is told Denied, as for
		// a tenant that does not exist, and learns nothing of its rules.
		// A tenant's registration read by a caller the installation's role
		// allows is a read of the master, judged by the installation's
		// networks alone: the tenant's own rule lives in the tenant's
		// database, and an installation's admin must see a tenant whose
		// database stopped as unreachable, not be refused for it.
		refused, err := a.tenantRuleRefusal(ctx, d.tenant, facts)
		if err != nil {
			return Decision{}, a.failTenantRule(ctx, entry, err)
		}
		if refused {
			return Decision{}, a.refuseByRule(entry, RuleTenantNetworks, req.tenant)
		}
	}
	if p.isKey {
		// The key is marked used only by a request the check allows, so a
		// request refused for its grants or its tenant's networks leaves the
		// key's last use, and the address beside it, as they were.
		a.queueMark(p.key, entry.Time, entry.RemoteAddress)
	}
	entry.ID = ""
	d.entry = entry
	d.req = req
	return d, nil
}

// Authenticate is the first step of Check alone: it decides whether cred is
// a good credential, reading the key once, and nothing about what the caller
// may do. A server calls it before it routes a request, so that a caller who
// is not authenticated is answered the same, whatever it asked for.
//
// A credential that is no good is refused exactly as Check refuses it: its
// audit entry, with facts and the real reason, is handed to the audit writer
// as refused, and the error is Unauthenticated with MessageUnauthenticated. A
// store that fails is Unavailable or Internal, and recorded as failed. A good
// credential is returned as one that carries the caller: Check given it does
// not read the key again, so a request reads its key once whether or not it
// went through Authenticate first. A key's last use is marked by Check, once
// it allows the request, never here.
func (a *Access) Authenticate(ctx context.Context, cred Credential, facts Request) (Credential, error) {
	if cred.accepted != nil {
		return cred, nil
	}
	entry := a.newEntry(facts)
	entry.ID = a.newID()
	if master.ValidateID("audit entry id", entry.ID) != nil {
		return Credential{}, fault.New(fault.Internal, "internal error")
	}
	if entry.Method == "" {
		entry.Method = "unknown"
		entry.Details = nil
		a.record(entry, master.OutcomeFailed, reasonBadCheck)
		return Credential{}, fault.New(fault.Internal, "internal error")
	}
	p, reason, err := a.authenticate(ctx, cred, facts, &entry)
	if err != nil {
		return Credential{}, a.fail(ctx, entry, err)
	}
	if reason != "" {
		a.record(entry, master.OutcomeRefused, reason)
		return Credential{}, fault.New(fault.Unauthenticated, MessageUnauthenticated)
	}
	return Credential{accepted: &p}, nil
}

// RecordUnchecked hands the audit entry of a request that never reached
// Check to the audit writer for the master's log, as failed with reason: a
// request whose caller Authenticate accepted and that the server answered
// before its check (a path nothing serves, a call the RPC library refused as
// malformed). The contract has every request write one entry; such a request
// has no Decision to write it with. The entry names the caller cred carries
// and the facts given. It goes through the queue of allowed requests'
// entries, so refusals of callers with no key never push it out, and never
// blocks or fails. A credential Authenticate did not accept records nothing:
// its refusal is already recorded; it is counted as failed and logged.
func (a *Access) RecordUnchecked(cred Credential, facts Request, reason string) {
	if cred.accepted == nil {
		a.auditAcct.add(auditFailed, 1)
		a.log.Error("access: an unchecked request's entry was asked for a credential that was not accepted; nothing was written")
		return
	}
	e := a.newEntry(facts)
	e.ID = a.newID()
	if p := cred.accepted; p.isLoc {
		e.LocalUser = auditText(p.local, master.MaxAuditFieldLength)
	} else {
		e.KeyID = p.key.ID
		e = markMaskedKey(e, p.Masked())
	}
	e.Outcome = master.OutcomeFailed
	e.Reason = auditText(reason, master.MaxAuditReasonLength)
	a.enqueueRecord(a.store, e)
}

// allows reports whether p meets req, given the id of the tenant req named
// (found by Check).
func allows(p Principal, req Requirement, tenantID string) bool {
	switch req.kind {
	case needAuthenticated:
		return p.isKey || p.isLoc
	case needInstallation:
		return atLeast(p.Role(master.Installation), req.role)
	case needTenant:
		return atLeast(p.Role(master.TenantScope(tenantID)), req.role)
	case needTenantRegistration:
		return atLeast(p.Role(master.TenantScope(tenantID)), master.RoleReader) || p.CanListTenants()
	case needTenantGrants:
		return atLeast(p.Role(master.TenantScope(tenantID)), master.RoleAdmin) || atLeast(p.Role(master.Installation), master.RoleAdmin)
	}
	return false
}

// authenticate turns cred into a principal. It returns a non-empty reason
// when the credential is refused, and an error only when the store failed.
// It sets the entry's key id or local user as soon as either is known, so a
// refusal of a key with a known shape records which key it was
// (audit.proto, AuditEntry.key_id).
//
// A key whose own rules (its networks, its client certificate) refuse the
// request is refused here like any key that is no good, with the rule's
// name as the reason and its event raised.
func (a *Access) authenticate(ctx context.Context, cred Credential, facts Request, entry *master.AuditEntry) (Principal, string, error) {
	if p := cred.accepted; p != nil {
		// Authenticate read and judged this key earlier in this same request.
		// Its expiry is judged again now, from the key as read, at no cost;
		// a revocation or a removal since is seen only by reading the key
		// again, which a credential marked by Reread asks for.
		if p.isLoc {
			entry.LocalUser = auditText(p.local, master.MaxAuditFieldLength)
			return *p, "", nil
		}
		entry.KeyID = p.key.ID
		*entry = markMaskedKey(*entry, p.key.Masked)
		if reason := keyState(p.key, entry.Time); reason != "" {
			return Principal{}, reason, nil
		}
		if reason := a.ruleReason(p.key, facts, *entry); reason != "" {
			return Principal{}, reason, nil
		}
		if cred.reread {
			k, err := a.store.Key(ctx, p.key.ID)
			if fault.KindOf(err) == fault.NotFound {
				return Principal{}, reasonUnknownKey, nil
			}
			if err != nil {
				return Principal{}, "", err
			}
			if reason := keyState(k, entry.Time); reason != "" {
				return Principal{}, reason, nil
			}
			if reason := a.ruleReason(k, facts, *entry); reason != "" {
				return Principal{}, reason, nil
			}
		}
		return *p, "", nil
	}
	if cred.local {
		entry.LocalUser = auditText(cred.localUser, master.MaxAuditFieldLength)
		// A user id the entry could not hold as it is (too long, or with a
		// character that had to be escaped) is not one the socket reported.
		if cred.localUser == "" || entry.LocalUser != cred.localUser {
			return Principal{}, reasonNoLocalUser, nil
		}
		return Principal{local: cred.localUser, isLoc: true}, "", nil
	}
	if cred.key == "" {
		return Principal{}, reasonNoKey, nil
	}
	id, sec, err := secret.ParseAPIKey(cred.key)
	if err != nil {
		return Principal{}, reasonMalformed, nil
	}
	entry.KeyID = id
	k, err := a.store.Key(ctx, id)
	if fault.KindOf(err) == fault.NotFound {
		a.verify(sec, unknownKeyHash[:])
		return Principal{}, reasonUnknownKey, nil
	}
	if err != nil {
		return Principal{}, "", err
	}
	if reason := a.keyRefusal(k, sec, entry.Time); reason != "" {
		if reason != reasonWrongSecret {
			// The caller holds the key: its entry says what key it is.
			*entry = markMaskedKey(*entry, k.Masked)
		}
		return Principal{}, reason, nil
	}
	*entry = markMaskedKey(*entry, k.Masked)
	if reason := a.ruleReason(k, facts, *entry); reason != "" {
		return Principal{}, reason, nil
	}
	k.Hash = nil
	return Principal{key: k, isKey: true}, "", nil
}

// keyRefusal returns why a key read from the master is refused at now, or
// "". The secret is compared first: a caller who does not hold it learns
// nothing more, and the log does not record "revoked" for a guess.
func (a *Access) keyRefusal(k master.Key, sec string, now time.Time) string {
	if !a.verify(sec, k.Hash) {
		return reasonWrongSecret
	}
	return keyState(k, now)
}

// ruleReason returns the audit reason when k's own rules refuse the request
// entry stands for, having raised the refusal's event, or "". It is judged
// after the secret and the key's state, so a caller who does not hold the
// key, or holds one that ended, never meets it.
func (a *Access) ruleReason(k master.Key, facts Request, entry master.AuditEntry) string {
	rule := keyRuleRefusal(k, facts)
	if rule == "" {
		return ""
	}
	a.raise(Event{Kind: EventRefused, Time: entry.Time, Rule: rule, KeyID: k.ID,
		Address: entry.RemoteAddress, Tenant: entry.TenantName, Method: entry.Method})
	return RuleReason(rule)
}

// allGrants reads every grant of a key, page by page.
func (a *Access) allGrants(ctx context.Context, keyID string) ([]master.Grant, error) {
	var out []master.Grant
	cursor := ""
	for range maxGrantPages {
		page, next, err := a.store.GrantsByKey(ctx, keyID, master.Page{Cursor: cursor, Limit: master.MaxPageLimit})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, fault.New(fault.Internal, "access: a key's grants did not end after "+strconv.Itoa(maxGrantPages)+" pages")
}

// newEntry starts a request's audit entry from its facts, at the time of the
// check, without an id. Every text is escaped and cut to the master's
// bounds: the facts come from the request, and an entry the master refuses
// would be a refusal without a record, while a control character in the log
// could forge a line of it.
func (a *Access) newEntry(facts Request) master.AuditEntry {
	e := master.AuditEntry{
		Time:          a.clock(),
		RemoteAddress: auditText(facts.RemoteAddress, master.MaxAuditFieldLength),
		Method:        auditText(facts.Method, master.MaxAuditFieldLength),
		Object:        auditText(facts.Object, master.MaxAuditFieldLength),
		RequestID:     auditText(facts.RequestID, master.MaxAuditFieldLength),
	}
	if len(facts.Details) > 0 {
		e.Details = make(map[string]string, len(facts.Details))
		for k, v := range facts.Details {
			k = auditText(k, master.MaxAuditDetailKeyLength)
			if k == "" {
				continue
			}
			e.Details[k] = auditText(v, master.MaxAuditDetailValueLength)
		}
	}
	return e
}

// record hands a refusal's, or a failed check's, audit entry to the audit
// writer, for the master's log: a refused caller must cause no write in a
// tenant's database. The request already has its answer, and the answer to a
// refused caller must not depend on whether the log took it, nor wait for a
// synced commit.
func (a *Access) record(e master.AuditEntry, outcome master.Outcome, reason string) {
	e.Outcome = outcome
	e.Reason = auditText(reason, master.MaxAuditReasonLength)
	a.enqueueRefusal(e)
}

// fail turns a store's failure during a check into the caller's error, and
// records it. A master that does not answer is Unavailable, as the contract
// says ("a database that does not answer is a failure, not a refusal"); any
// other failure is Internal, whose message says nothing of the cause.
//
// A store read that failed because the request's own context ended (the
// caller canceled, or its deadline passed) is not a failure of the store:
// it is recorded as failed with reasonGone, never as the master failing, and
// the caller's error is the context's own, which the server answers as
// CANCELED or DEADLINE_EXCEEDED and logs as such.
func (a *Access) fail(ctx context.Context, e master.AuditEntry, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		a.record(e, master.OutcomeFailed, reasonGone)
		return cerr
	}
	a.record(e, master.OutcomeFailed, reasonStoreFailure+": "+fault.KindOf(err).String())
	return storeError(err)
}

// failTenantRule is fail for a tenant's networks that could not be read:
// the rule lives in the tenant's database, so a database that does not
// answer is the tenant's, and the caller is told so, not that the master
// failed. The request is refused either way: a rule that cannot be read
// lets nobody through.
func (a *Access) failTenantRule(ctx context.Context, e master.AuditEntry, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		a.record(e, master.OutcomeFailed, reasonGone)
		return cerr
	}
	a.record(e, master.OutcomeFailed, reasonTenantRule+": "+fault.KindOf(err).String())
	if fault.KindOf(err) == fault.Unavailable {
		return fault.Wrap(fault.Unavailable, err, "the tenant's database does not answer")
	}
	return fault.Wrap(fault.Internal, err, "internal error")
}

// storeError returns the caller's error for a store's failure.
func storeError(err error) error {
	if fault.KindOf(err) == fault.Unavailable {
		return fault.Wrap(fault.Unavailable, err, "the master database does not answer")
	}
	return fault.Wrap(fault.Internal, err, "internal error")
}

// clipTenantName returns a tenant's name as the request gave it, for the
// audit entry (audit.proto: kept as given, also when no tenant has it). A
// name that could not be a tenant's is quoted in ASCII (strconv.QuoteToASCII,
// which escapes every control and non-ASCII character as auditText would),
// so it cannot pass for one in the log, and cut to an audit field. It is not
// passed through auditText as well, which would escape its escapes again.
func clipTenantName(name string) string {
	if master.ValidateTenantName(name) == nil {
		return name
	}
	q := strconv.QuoteToASCII(name)
	if len(q) > master.MaxAuditFieldLength {
		q = q[:master.MaxAuditFieldLength]
	}
	return q
}
