package api

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The keys of the details the four services of a tenant's reception put in
// audit entries, beside those requirement.go names.
const (
	detailType          = "type"
	detailMask          = "update_mask"
	detailPort          = "port"
	detailAETitle       = "ae_title"
	detailStorage       = "storage"
	detailPassed        = "passed"
	detailReceiver      = "receiver"
	detailReception     = "reception"
	detailSteps         = "steps"
	detailInProgress    = "in_progress"
	detailStudyUID      = "study_instance_uid"
	detailSOPUID        = "sop_instance_uid"
	detailState         = "state"
	detailReason        = "reason"
	detailMode          = "mode"
	detailIncludeHeader = "include_header"
)

// presentValue is the value of a detail that records only that a filter was
// given: a UID a caller searched by is a search for a person's data, and its
// value is never written to a log (reception.proto, quarantine.proto).
const presentValue = "present"

// MaxServerIDLength bounds an id the server makes (a reception's, a
// quarantined file's), the contract's 64 bytes.
const MaxServerIDLength = master.MaxIDLength

// precondition returns a refusal of a state that forbids a change, with a
// message safe to show.
func precondition(message string) error { return fault.New(fault.Precondition, message) }

// onTenantNamed returns the requirement of role on the tenant a request
// names by name: on that tenant when the name is valid, and otherwise on the
// installation, which is also the requirement of a malformed name (doc.go).
func onTenantNamed(name string, role master.Role) access.Requirement {
	if master.ValidateTenantName(name) == nil {
		return access.OnTenant(name, role)
	}
	return access.OnInstallation(role)
}

// inTenant returns the pipeline of the tenant a request into one tenant
// names, and its store, which every later record of the request goes to. A
// name that is not valid is recorded in the installation's log and answered
// as invalid; a tenant that is gone or not ready is recorded and answered as
// sinkFor says. details are the request's audit details, which an entry
// moved to the installation's log keeps.
func (c *core) inTenant(ctx context.Context, d access.Decision, name string, details map[string]string) (tenant.Pipeline, tenant.Store, error) {
	if err := tenantName("tenant", name); err != nil {
		return nil, nil, c.fail(d, err)
	}
	st, err := c.sinkFor(ctx, d, access.TenantNamed(name), details)
	if err != nil {
		return nil, nil, err
	}
	p, err := c.pipeline(st)
	if err != nil {
		return nil, nil, c.failIn(d, st, err)
	}
	return p, st, nil
}

// pipeline returns st as the pipeline's port. Every driver implements it, so
// a store that does not is the server's fault.
func (c *core) pipeline(st tenant.Store) (tenant.Pipeline, error) {
	p, ok := st.(tenant.Pipeline)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store does not keep the reception pipeline's data")
	}
	return p, nil
}

// roleGuard returns the check a change in the tenant d names makes of its
// caller, as adminGuard does, for role: the master's check that d's key
// still holds role on that tenant.
func (c *core) roleGuard(d access.Decision, role master.Role) tenant.Guard {
	ref, _ := d.Tenant()
	by := d.Actor(master.TenantScope(ref.ID), role)
	return func(ctx context.Context) error {
		if err := c.master.CheckActor(ctx, by); err != nil {
			return err
		}
		return c.writable(ref.Name)
	}
}

// itemName returns an Invalid error naming field unless name follows the
// rule of a tenant's name, which storages, receivers and roots share.
func itemName(field, name string) error {
	if name == "" {
		return invalid(field, "is required")
	}
	if master.ValidateTenantName(name) != nil {
		return invalid(field, "is 1 to 63 of a-z, 0-9 and -, starting with a letter, and not a device name Windows reserves (con, prn, aux, nul, com0-9, lpt0-9)")
	}
	return nil
}

// serverID returns an Invalid error naming field unless id could be an id
// the server made: required, at most 64 bytes, of the characters it uses.
func serverID(field, id string) error {
	if id == "" {
		return invalid(field, "is required")
	}
	if len(id) > MaxServerIDLength || master.ValidateID(field, id) != nil {
		return invalid(field, "is not an id this server makes: at most 64 of A-Z, a-z, 0-9, _ and -")
	}
	return nil
}

// fieldOf prefixes a port's Invalid message with the request's field, so
// the caller is told where the rule failed; another error is returned as it
// is.
func fieldOf(field string, err error) error {
	if fault.KindOf(err) != fault.Invalid {
		return err
	}
	return fault.Wrap(fault.Invalid, err, field+": "+fault.Message(err))
}

// maskRules are what a partial change's update_mask may name: the paths of
// the fields it changes, and the paths it refuses each with its reason
// (access.proto, "Partial changes").
type maskRules struct {
	// fields are the paths a change may name.
	fields []string
	// refused are paths that name something real a change may not name: a
	// type's whole message, a field the server answers.
	refused map[string]string
}

// paths returns the paths of a request's update_mask, checked by the
// contract's rules: the mask is required and not empty; "*" is refused, so
// a call changes only what it names; each path is a field the change may
// name, named once; a path in refused is refused with its reason; any other
// path names no field.
func (r maskRules) paths(m *fieldmaskpb.FieldMask) ([]string, error) {
	if m == nil || len(m.GetPaths()) == 0 {
		return nil, invalid("update_mask", "is required and names at least one field")
	}
	seen := make([]string, 0, len(m.GetPaths()))
	for _, p := range m.GetPaths() {
		switch {
		case p == "*":
			return nil, invalid("update_mask", `may not be "*": name each field the call changes`)
		case slices.Contains(seen, p):
			return nil, invalid("update_mask", "names "+strconv.Quote(p)+" twice")
		}
		if why, ok := r.refused[p]; ok {
			return nil, invalid("update_mask", strconv.Quote(p)+" "+why)
		}
		if !slices.Contains(r.fields, p) {
			return nil, invalid("update_mask", strconv.Quote(clip(p, 64))+" names no field that can be changed")
		}
		seen = append(seen, p)
	}
	return seen, nil
}

// clip cuts s to at most n bytes for a message, so a caller's long text is
// not echoed whole.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// wholeSeconds returns a duration a request gives, or an Invalid error
// naming field unless it is valid and a whole number of seconds. A nil
// duration is zero and absent.
func wholeSeconds(field string, d *durationpb.Duration) (time.Duration, bool, error) {
	if d == nil {
		return 0, false, nil
	}
	if d.CheckValid() != nil || d.GetNanos() != 0 || d.GetSeconds() < 0 {
		return 0, true, invalid(field, "is a whole, positive number of seconds")
	}
	if d.GetSeconds() > int64(48*time.Hour/time.Second) {
		// Past every bound of a receiver; refused here so the conversion to
		// a duration cannot overflow.
		return 0, true, invalid(field, "is longer than any bound allows")
	}
	return d.AsDuration(), true, nil
}

// joinDetail writes a list as one detail's value.
func joinDetail(items []string) string { return strings.Join(items, ",") }

// changeEntry returns the audit entry of a change a store writes with it,
// with the facts the handler learned.
func changeEntry(d access.Decision, facts access.Facts) master.AuditEntry {
	return d.Entry(master.OutcomeDone, "", facts)
}

// timeDetails adds a listing's time filters to its audit details, as
// ListAuditEntries records them.
func timeDetails(d map[string]string, start time.Time, hasStart bool, end time.Time, hasEnd bool) {
	if hasStart {
		d[detailStart] = start.UTC().Format(time.RFC3339Nano)
	}
	if hasEnd {
		d[detailEnd] = end.UTC().Format(time.RFC3339Nano)
	}
}
