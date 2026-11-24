package master

import (
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// Actor is who makes a change, as the change carries it to the store: an API
// key, with the role it must still hold on a scope when the change is
// written, or a caller the store does not check (the local operator, the
// server acting for itself). Every changing operation of Store takes one and
// checks it inside the change's transaction, so a key revoked, expired or
// stripped of its grant after the request was checked makes no change: the
// check and the change are one step, with no moment between them for a
// revocation to fall into.
//
// The zero Actor is no one, and every change refuses it as Invalid: a caller
// that forgot to say who acts gets an error, never an unchecked change.
type Actor struct {
	kind  actorKind
	keyID string
	scope Scope
	role  Role
	clock func() time.Time
}

// actorKind says which caller an Actor is.
type actorKind uint8

// The kinds of Actor. The zero kind is no one.
const (
	actorNone actorKind = iota
	actorKey
	actorLocal
	actorServer
	actorGiveBack
)

// ActingKey is the key keyID, which must still hold role or more on scope
// when the change is written. A grant on the installation is not a role on a
// tenant, so scope is the exact scope the change needs. The empty role asks
// only that the key be valid: it exists, is not revoked, and has not expired.
//
// clock is the server's clock, the one its audit entries' times come from.
// The store reads it once, inside the change's transaction, after it holds
// what it locks and after it read the key, and judges the key's expiry at
// that time (Judge): a change that waited for a lock past the key's expiry is
// refused. It is not the database's clock, so the expiry and the audit log
// are judged by one clock, and a test that sets the clock decides the answer.
func ActingKey(keyID string, scope Scope, role Role, clock func() time.Time) Actor {
	return Actor{kind: actorKey, keyID: keyID, scope: scope, role: role, clock: clock}
}

// LocalOperator is the caller on the machine's local socket: it holds no key,
// is admin on every scope, and is never refused.
func LocalOperator() Actor { return Actor{kind: actorLocal} }

// Server is the server acting for itself, on no request: a tenant's state it
// watches, a creation it settles or finishes after a restart, the
// installation it sets up. It is never refused.
func Server() Actor { return Actor{kind: actorServer} }

// ExportGiveBack is the server giving back a tenant whose export failed
// before its summary was written, so no transfer key reached anyone: it
// ends a person's act, not one of the server's own checks, and must not
// depend on the key that asked for the export, which may have been revoked
// or have expired meanwhile (often the very reason the export ended). It is
// never refused as a key is; CheckStateChange lets it take a moved tenant
// to ready and nothing else.
func ExportGiveBack() Actor { return Actor{kind: actorGiveBack} }

// IsServer reports whether a is the server acting for itself (Server).
func (a Actor) IsServer() bool { return a.kind == actorServer }

// IsExportGiveBack reports whether a is the server giving back a tenant
// whose export failed (ExportGiveBack).
func (a Actor) IsExportGiveBack() bool { return a.kind == actorGiveBack }

// IsLocalOperator reports whether a is the caller on the machine's local
// socket (LocalOperator).
func (a Actor) IsLocalOperator() bool { return a.kind == actorLocal }

// IsKey reports whether a is a key, the one kind of Actor a store checks.
func (a Actor) IsKey() bool { return a.kind == actorKey }

// KeyID returns the acting key's id, or "" when a is not a key.
func (a Actor) KeyID() string { return a.keyID }

// Scope returns the scope a key's role is needed on.
func (a Actor) Scope() Scope { return a.scope }

// Role returns the role a key needs on its scope; empty when only the key's
// validity is asked.
func (a Actor) Role() Role { return a.role }

// String names the actor for a log line: "local operator", "server", or
// "key <id>". It never holds a secret.
func (a Actor) String() string {
	switch a.kind {
	case actorKey:
		return "key " + a.keyID
	case actorLocal:
		return "local operator"
	case actorServer:
		return "server"
	case actorGiveBack:
		return "server giving back a failed export"
	}
	return "no one"
}

// Validate returns an Invalid error when a cannot act: the zero Actor, or a
// key with an id, scope or role that is not valid, or no clock.
func (a Actor) Validate() error {
	switch a.kind {
	case actorLocal, actorServer, actorGiveBack:
		return nil
	case actorKey:
	default:
		return invalid("a change names who makes it")
	}
	if err := ValidateID("acting key id", a.keyID); err != nil {
		return err
	}
	if err := a.scope.Validate(); err != nil {
		return err
	}
	if a.role != "" && !a.role.Valid() {
		return invalid("a role is reader, writer, operator or admin")
	}
	if a.clock == nil {
		return invalid("an acting key has a clock to judge its expiry by")
	}
	return nil
}

// The reasons a change is refused for its actor, as Refusal.Reason gives
// them. They are for the audit log and never reach the refused caller, who is
// told only that its key is no good, or that it may not do this.
const (
	// RefusedUnknownKey is a key that does not exist (any more).
	RefusedUnknownKey = "unknown key"
	// RefusedRevoked is a key that was revoked.
	RefusedRevoked = "key revoked"
	// RefusedExpired is a key whose expiry is at or before the change.
	RefusedExpired = "key expired"
	// RefusedNoRole is a key without the role the change needs on its scope.
	RefusedNoRole = "no sufficient grant"
)

// Refusal is in the chain of the error a change returns when its actor may
// no longer act; nothing was written. The error's kind is Unauthenticated for
// a key that is no good (unknown, revoked, expired) and Denied for a key
// without the role, the kinds the access check answers for the same causes.
// Reason is one of the Refused constants. Find it with RefusalOf.
type Refusal struct {
	// Reason says why, as one of the Refused constants.
	Reason string
}

// Error returns the reason, for the log.
func (r *Refusal) Error() string { return "the acting key may not make this change: " + r.Reason }

// RefusalOf returns the Refusal in err's chain, and whether there is one.
func RefusalOf(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) && r != nil {
		return r, true
	}
	return nil, false
}

// The messages of a refusal's error. A caller that answers a request maps a
// refusal to the access check's own fixed messages; these say the same thing
// and nothing more, for a caller that does not.
const (
	messageKeyNoGood = "the acting key is no longer valid"
	messageNoRole    = "the acting key may not make this change"
)

// Judge decides whether a may make its change, from what a store read inside
// the change's transaction: whether the key was found, its revoked time and
// its expiry (the zero time for none), and the role it holds on a.Scope()
// (the empty Role for none). It returns nil for an actor that is not a key,
// and otherwise nil or an error with a Refusal in its chain. It is the one
// statement of the rule every driver applies, so the drivers differ only in
// how they read. It reads a key's clock once, when the driver calls it,
// which is after the driver holds its locks and has read the key, so the
// expiry is judged at the moment the change is decided.
func (a Actor) Judge(found bool, revoked, expires time.Time, role Role) error {
	if a.kind != actorKey {
		return nil
	}
	reason := ""
	switch {
	case !found:
		reason = RefusedUnknownKey
	case !revoked.IsZero():
		reason = RefusedRevoked
	case !expires.IsZero() && !a.clock().Before(expires):
		reason = RefusedExpired
	case a.role != "" && !role.Includes(a.role):
		return fault.Wrap(fault.Denied, &Refusal{Reason: RefusedNoRole}, messageNoRole)
	default:
		return nil
	}
	return fault.Wrap(fault.Unauthenticated, &Refusal{Reason: reason}, messageKeyNoGood)
}

// rank orders the roles: each includes the ones before it.
func (r Role) rank() int {
	switch r {
	case RoleReader:
		return 1
	case RoleWriter:
		return 2
	case RoleOperator:
		return 3
	case RoleAdmin:
		return 4
	}
	return 0
}

// Includes reports whether r allows what need allows: each role includes the
// ones before it (reader, writer, operator, admin). No role includes an
// unknown one, and the empty role includes nothing.
func (r Role) Includes(need Role) bool {
	n := need.rank()
	return n > 0 && r.rank() >= n
}
