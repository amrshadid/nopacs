package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/stow"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// STOWPattern is the route of a tenant's STOW-RS service (PS3.18 10.5): a
// POST to .../dicom-web/studies, or to .../studies/<Study Instance UID> to
// store into one study. It is one route for the subtree, as DICOMweb's GET
// route is: the router takes two routes of one shape only for different
// methods, and the handler reads which of the two resources the rest of
// the path names, answering any other 404.
const STOWPattern = "POST /tenants/{tenant}/dicom-web/{rest...}"

// The details of a STOW-RS request's audit entry.
const (
	// detailStored and detailRefused count the parts stored and refused.
	detailStored  = "stored"
	detailRefused = "refused"
)

// messageNoSTOWReceiver refuses a key with the role on the tenant that no
// running stow receiver of the tenant lists, the local operator included:
// a stow receiver is closed until a key is listed (receiver.proto,
// StowReceiver).
const messageNoSTOWReceiver = "no stow receiver of the tenant takes requests from this key"

// STOWNeed is the requirement and the audit facts of a request to
// STOWPattern, read from the path values the server sets before the check:
// writer on the tenant the path names, the role that sends data in
// (access.proto, Role), with the resource's shape in the details. Whether
// a receiver lists the key is asked after the check, so a caller without
// the role learns nothing of the tenant's receivers.
func STOWNeed(r *http.Request) (access.Requirement, access.Request) {
	return onTenantNamed(r.PathValue("tenant"), master.RoleWriter),
		access.Request{Details: map[string]string{detailResource: dicomweb.ShapeOf(r.PathValue("rest"))}}
}

// STOW serves STOWPattern once the server's check has allowed the request:
// the stow receiver that lists the caller's key reads the request part by
// part into the tenant's reception pipeline and writes the answer (package
// stow). A request no receiver takes is refused as PERMISSION_DENIED.
func (h Handlers) STOW(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	return h.c.stow(ctx, w, r)
}

// stow is STOW.
func (c *core) stow(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	d, err := c.changing(ctx, STOWPattern)
	if err != nil {
		return err
	}
	_, facts := STOWNeed(r)
	name := r.PathValue("tenant")
	_, st, err := c.inTenant(ctx, d, name, facts.Details)
	if err != nil {
		return err
	}
	study, ok := stowStudy(r.PathValue("rest"))
	if !ok {
		return c.failWith(d, st, access.Facts{Details: facts.Details}, fault.New(fault.NotFound, "there is no such STOW-RS resource"))
	}
	ep, ok, err := c.stowReceiverOf(ctx, name, d.Principal().KeyID())
	if err != nil {
		return c.failWith(d, st, access.Facts{Details: facts.Details}, err)
	}
	if !ok || ep == nil {
		return c.failWith(d, st, access.Facts{Details: facts.Details}, fault.New(fault.Denied, messageNoSTOWReceiver))
	}
	// A request may take as long as its client sends: the caller's key and
	// grant are checked again every RecheckInterval, as for a long read,
	// and a request whose caller may no longer store ends at its next read
	// of the body. The parts stored before are kept, as for any request
	// refused part way.
	ctx, stop := c.watchKey(ctx, d)
	defer stop()
	res := ep.Serve(ctx, w, r, stow.Request{
		Study:       study,
		Base:        webBase(r, name),
		Caller:      tenant.Caller{Address: callerAddress(r.RemoteAddr)},
		Association: w.Header().Get(RequestIDHeader),
		Refused: func() bool {
			_, refused := refusedWhileOpen(ctx)
			return refused
		},
	})
	if refusal, refused := refusedWhileOpen(ctx); refused && res.Unanswered {
		// Confirm recorded the refusal.
		return c.toAPI(d, refusal)
	}
	details := withDetails(facts.Details, map[string]string{
		detailReceiver: ep.Receiver().Name, detailStored: strconv.Itoa(res.Stored), detailRefused: strconv.Itoa(res.Refused),
	})
	outcome := master.OutcomeDone
	if res.Reason != "" || res.Status >= http.StatusInternalServerError {
		outcome = master.OutcomeFailed
	}
	c.record(d, st, outcome, res.Reason, access.Facts{Details: details})
	return nil
}

// stowReceiverOf returns the running stow receiver of the tenant that
// takes requests made with the key keyID: the one that lists the key, or
// else the one that lists a key it was rotated from, followed back in the
// master (master.FindRotatedFrom), so a system that rotates its key keeps
// storing without its receiver being changed. The master is read only for
// a key no receiver lists. A failure to read it is returned.
func (c *core) stowReceiverOf(ctx context.Context, tenantName, keyID string) (*stow.Endpoint, bool, error) {
	if c.stowReceivers == nil || keyID == "" {
		return nil, false, nil
	}
	if ep, ok := c.stowReceivers.Find(tenantName, keyID); ok {
		return ep, true, nil
	}
	var ep *stow.Endpoint
	_, err := master.FindRotatedFrom(ctx, c.master, keyID, func(id string) bool {
		var ok bool
		ep, ok = c.stowReceivers.Find(tenantName, id)
		return ok
	})
	if err != nil {
		return nil, false, err
	}
	return ep, ep != nil, nil
}

// stowStudy reads the resource of a STOW-RS path after dicom-web/:
// "studies", the service's root, or "studies/<UID>", one study, whose UID
// must be well formed (PS3.5 9.1). It reports false for any other path.
func stowStudy(rest string) (string, bool) {
	if rest == "studies" {
		return "", true
	}
	study, ok := strings.CutPrefix(rest, "studies/")
	if !ok || !uid.Valid(study) {
		return "", false
	}
	return study, true
}

// callerAddress returns a request's remote address in the form a received
// file records (tenant.Caller.Address), empty when it is not one.
func callerAddress(remote string) string {
	if (tenant.Caller{Address: remote}).Validate() != nil {
		return ""
	}
	return remote
}
