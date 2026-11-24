package api

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// relabeled returns err, a NotFound a lookup by the labels p answered, with
// the reason RELABELED and the labels the data has now when a committed
// relabel of this tenant moved p (access.proto, "not found"), so a client
// that holds old labels finds the data. Any other error, and a NotFound no
// relabel explains, is returned as it is. The pointer is read from the
// tenant's own relabels and catalog alone: it names only labels this
// tenant gave, and only while the catalog holds the data under them, never
// an original identifier.
//
// Only a caller who may read the tenant's corrections (operator or more on
// the tenant, as ListCorrections asks) is given the pointer: it tells the
// old label's new one, and a relabel may have been made to take an
// identifying label out of use, which a reader probing old labels would
// otherwise undo. A reader is answered as for a label never relabeled. The
// caller is the request's Decision, read from ctx; a request without one is
// given no pointer.
func relabeled(ctx context.Context, st tenant.Store, p label.Path, err error) error {
	if fault.KindOf(err) != fault.NotFound || tenant.ValidateRelabelLookup(p) != nil || !readsCorrections(ctx) {
		return err
	}
	cs, cerr := corrections(st)
	if cerr != nil {
		return err
	}
	moved, ok, lerr := cs.RelabeledTo(ctx, p)
	switch {
	case lerr != nil:
		// The lookup failed after the read it explains: the database's
		// failure is the answer, not a bare not found that may be wrong.
		return lerr
	case !ok:
		return err
	}
	return fault.Wrap(fault.NotFound, err, tenant.ReasonRelabeled+": "+fault.Message(err)+"; the relabel "+moved.Correction+
		" gave it the labels "+labelsText(moved.To))
}

// readsCorrections reports whether the request in ctx was decided for a
// caller whose role on its tenant includes operator, the role that may read
// the tenant's corrections. A role on the installation is not a role on a
// tenant (access.Principal.Role).
func readsCorrections(ctx context.Context) bool {
	d, ok := DecisionFrom(ctx)
	if !ok {
		return false
	}
	ref, ok := d.Tenant()
	if !ok {
		return false
	}
	return d.Principal().Role(master.TenantScope(ref.ID)).Includes(master.RoleOperator)
}

// labelsText writes labels at the depth they name, for a message.
func labelsText(p label.Path) string {
	s := "subject " + p.Subject
	if p.Session != "" {
		s += ", session " + p.Session
	}
	if p.Scan != "" {
		s += ", scan " + p.Scan
	}
	return s
}

// filterPath returns the labels a list's filter names, from the subject
// down, stopping at the first empty one.
func filterPath(subject, session string) label.Path {
	if subject == "" {
		return label.Path{}
	}
	return label.Path{Subject: subject, Session: session}
}
