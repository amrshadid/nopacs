package api

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageNoSender answers a sender name the tenant does not have.
const messageNoSender = "the tenant has no sender with this name"

// messageMarkNotAdmin answers a request that sets or clears a sender's
// allow_identified from a caller who is not the installation's admin.
const messageMarkNotAdmin = "sender.allow_identified: only an admin on the installation, or the local operator, sets or clears it; " +
	"what may leave the archive identified is the installation's to decide, not a tenant's"

// messageRedirectNotAdmin answers a change of where a sender marked
// allow_identified sends from a caller who is not the installation's
// admin.
const messageRedirectNotAdmin = "the sender is marked allow_identified: only an admin on the installation, or the local operator, " +
	"changes where it sends (its host, port, AE titles, URL, TLS or account), as only they set the mark"

// mayMark refuses, as permission denied with message, a change of a
// sender's allow_identified, or of where a marked sender sends, by a caller
// who is not the installation's admin, or by a masked key whatever its
// grants: the mark lets data leave the archive identified, so neither a
// tenant's admin nor an agent's key may choose it, nor point a marked
// sender elsewhere. The check refuses a masked key every change of a sender
// first; this holds even if that rule moves.
func mayMark(d access.Decision, message string) error {
	if d.Masked() || !d.Principal().Role(master.Installation).Includes(master.RoleAdmin) {
		return fault.New(fault.Denied, message)
	}
	return nil
}

// markRefused records a change of a sender's mark that mayMark refused, as
// refused in the installation's log, as the check's own refusals are (a
// refused caller causes no write in a tenant's database), with the check's
// details (the type added, or the mask's paths), and returns its API error.
func (c *core) markRefused(d access.Decision, tenantName string, err error) error {
	return c.failWith(d, nil, access.Facts{TenantName: tenantName}, err)
}

// markGuard is the guard of a change that sets or clears a sender's mark:
// the tenant's admin, as every change of a sender, and the installation's
// admin, both checked again in the change's transaction, so a grant revoked
// meanwhile refuses it.
func (c *core) markGuard(d access.Decision) tenant.Guard {
	onTenant := c.adminGuard(d)
	onInstallation := d.Actor(master.Installation, master.RoleAdmin)
	return func(ctx context.Context) error {
		if err := c.master.CheckActor(ctx, onInstallation); err != nil {
			return err
		}
		return onTenant(ctx)
	}
}

// senderService is SenderService: a tenant's senders, kept in its store,
// toward the destinations the installation allows. Whether a sender reaches
// its peer is the part of the server that sends; a handler only stores what
// it is to do, and answers the health it is told (SenderHealths).
type senderService struct{ c *core }

var _ nopacsv1connect.SenderServiceHandler = senderService{}

// lookupSender reads a sender, answering one the tenant does not have as
// NotFound with the contract's message.
func lookupSender(ctx context.Context, p tenant.Pipeline, name string) (tenant.Sender, error) {
	s, err := p.Sender(ctx, name)
	if fault.KindOf(err) == fault.NotFound {
		return s, fault.Wrap(fault.NotFound, err, messageNoSender)
	}
	return s, err
}

// senderName returns an Invalid error naming field unless name follows the
// rule of a sender's name.
func senderName(field, name string) error {
	if name == "" {
		return invalid(field, "is required")
	}
	return fieldOf(field, tenant.ValidateSenderName(name))
}

// message returns s as the contract writes it, with its health now.
func (s senderService) message(tenantName string, in tenant.Sender) *v1.Sender {
	return senderMessage(in, s.c.senders.SenderHealth(tenantName, in))
}

// sendersChanged tells the part of the server that sends that a tenant's
// senders or jobs changed.
func (c *core) sendersChanged(tenantName string) {
	if sc, ok := c.senders.(SenderChanges); ok {
		sc.SendersChanged(tenantName)
	}
}

// senderEndpoint returns where s connects: the host and port of a DIMSE
// peer, the host and port of a stow URL (443 or 80 when it names none), and
// whether the URL is plain http.
func senderEndpoint(s tenant.Sender) (host string, port int, plain bool) {
	if s.Type == tenant.SenderDIMSE {
		return s.DIMSE.Host, s.DIMSE.Port, false
	}
	u, err := url.Parse(s.STOW.URL)
	if err != nil {
		return "", 0, false
	}
	port = 443
	if u.Scheme == "http" {
		port, plain = 80, true
	}
	if p, err := strconv.Atoi(u.Port()); err == nil {
		port = p
	}
	return u.Hostname(), port, plain
}

// senderFields are the fields of a sender's configuration a refusal of its
// destination names: the one that holds the host, and the one that holds
// the port (a stow URL holds both).
func senderFields(s tenant.Sender) (host, port string) {
	if s.Type == tenant.SenderSTOW {
		return "sender.stow.url", "sender.stow.url"
	}
	return "sender.dimse.host", "sender.dimse.port"
}

// allowSender refuses, as a precondition, a sender whose host and port the
// installation's sender.destinations does not list, and a stow sender whose
// URL is plain http while sender.allow-plain-http is false. The host is not
// resolved: adding a sender connects to nothing, and the worker checks the
// address again at each connection.
func (c *core) allowSender(ctx context.Context, s tenant.Sender) error {
	host, port, plain := senderEndpoint(s)
	hostField, portField := senderFields(s)
	if plain {
		allowed, err := c.values().AllowPlainHTTP(ctx)
		if err != nil {
			return err
		}
		if !allowed {
			return precondition(hostField + ": the installation does not allow plain http (sender.allow-plain-http is false)")
		}
	}
	dests, err := c.values().SenderDestinations(ctx)
	if err != nil {
		return err
	}
	return destinationError(hostField, portField, dests.Allowed(host, netip.Addr{}, port))
}

// destinationError returns the precondition a refusal of
// settings.Destinations.Allowed is, naming hostField, or portField for a
// port; nil for nil.
func destinationError(hostField, portField string, err error) error {
	field := hostField
	switch {
	case err == nil:
		return nil
	case errors.Is(err, settings.ErrDestinationPort):
		return precondition(portField + ": the port is not among those the installation's sender.destinations allows for this host")
	}
	return precondition(field + ": the installation's sender.destinations does not list this host")
}

// AddSender adds a sender to a tenant toward a destination the installation
// lists, with the contract's defaults filled in and stored and its
// credential sealed for the sender's row before it reaches the store.
func (s senderService) AddSender(ctx context.Context, req *v1.AddSenderRequest) (*v1.AddSenderResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.SenderServiceAddSenderProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), map[string]string{detailType: senderType(req.GetSender())})
	if err != nil {
		return nil, err
	}
	guard := c.adminGuard(d)
	// The role is judged before the request is validated, so a caller
	// without it learns nothing from the shape of its request.
	if req.GetSender().GetAllowIdentified() {
		if err := mayMark(d, messageMarkNotAdmin); err != nil {
			return nil, c.markRefused(d, req.GetTenant(), err)
		}
		guard = c.markGuard(d)
	}
	in, credential, err := senderFromRequest("sender", req.GetSender())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	ref, _ := d.Tenant()
	// The write lock, so the installation's destinations are read, the
	// sender checked and stored in one step: a SetSender or an AddSender
	// that ran meanwhile would otherwise be checked against a copy that its
	// own change made stale.
	c.places.Lock()
	defer c.places.Unlock()
	if err := c.allowSender(ctx, in); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if credential != "" {
		if in.STOW.SealedCredential, err = place.SealCredential(c.keys, ref.ID, in.Name, credential); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	out, err := p.AddSender(ctx, in, c.now(), changeEntry(d, access.Facts{}), guard)
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("sender", err))
	}
	c.sendersChanged(req.GetTenant())
	return &v1.AddSenderResponse{Sender: s.message(req.GetTenant(), out)}, nil
}

// GetSender answers one sender of a tenant, with its health now and never
// its credential.
func (s senderService) GetSender(ctx context.Context, req *v1.GetSenderRequest) (*v1.GetSenderResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.SenderServiceGetSenderProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := senderName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := lookupSender(ctx, p, req.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	return &v1.GetSenderResponse{Sender: s.message(req.GetTenant(), out)}, nil
}

// ListSenders answers a page of a tenant's senders, ordered by name, each
// with its health now.
func (s senderService) ListSenders(ctx context.Context, req *v1.ListSendersRequest) (*v1.ListSendersResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.SenderServiceListSendersProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListSenders(ctx, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListSendersResponse{Senders: make([]*v1.Sender, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Senders = append(out.Senders, s.message(req.GetTenant(), it))
	}
	return out, nil
}

// SetSender changes the fields of a sender its update_mask names, under
// every rule AddSender applies to them; a credential the mask does not
// name is kept as it is.
func (s senderService) SetSender(ctx context.Context, req *v1.SetSenderRequest) (*v1.SetSenderResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.SenderServiceSetSenderProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), maskDetails(req.GetUpdateMask().GetPaths()))
	if err != nil {
		return nil, err
	}
	in := req.GetSender()
	if in == nil {
		return nil, c.failIn(d, st, invalid("sender", "is required: its name names the sender to change"))
	}
	if err := senderName("sender.name", in.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	paths, err := senderMask.paths(req.GetUpdateMask())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	fields := make([]tenant.SenderField, len(paths))
	guard := c.adminGuard(d)
	for i, p := range paths {
		fields[i] = tenant.SenderField(p)
		// Setting and clearing alike: either changes what may leave.
		if fields[i] == tenant.FieldSenderAllowIdentified {
			if err := mayMark(d, messageMarkNotAdmin); err != nil {
				return nil, c.markRefused(d, req.GetTenant(), err)
			}
			guard = c.markGuard(d)
		}
	}
	change, credential, err := senderChange(in, fields)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	newCredential := false
	for _, f := range fields {
		newCredential = newCredential || f == tenant.FieldSTOWCredential
	}
	if newCredential && credential != "" {
		if err := senderCredential("sender.stow.credential", credential); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	ref, _ := d.Tenant()
	// The write lock, and the stored sender read under it: see AddSender.
	c.places.Lock()
	defer c.places.Unlock()
	stored, err := lookupSender(ctx, p, in.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	// A marked sender pointed elsewhere would send identified data where
	// the installation's admin did not choose: such a change is theirs, as
	// the mark is. Read under the write lock, so the mark cannot be set
	// between this read and the change (AddSender, SetSender).
	if stored.AllowIdentified && slices.ContainsFunc(fields, tenant.RedirectsSender) {
		if err := mayMark(d, messageRedirectNotAdmin); err != nil {
			return nil, c.markRefused(d, req.GetTenant(), err)
		}
		guard = c.markGuard(d)
	}
	if newCredential && credential != "" {
		if change.STOW.SealedCredential, err = place.SealCredential(c.keys, ref.ID, in.GetName(), credential); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	result, destination, _, err := tenant.ApplySender(stored, change, fields)
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("sender", err))
	}
	if destination {
		if err := checkURLPort("sender.stow.url", result); err != nil {
			return nil, c.failIn(d, st, err)
		}
		if err := c.allowSender(ctx, result); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	out, err := p.SetSender(ctx, change, fields, c.now(), changeEntry(d, access.Facts{}), guard)
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("sender", err))
	}
	c.sendersChanged(req.GetTenant())
	return &v1.SetSenderResponse{Sender: s.message(req.GetTenant(), out)}, nil
}

// RemoveSender removes a sender's configuration and its sealed credential,
// and with cancel_jobs cancels its jobs that are queued or running in the
// same transaction. Its jobs stay, ended, naming it.
func (s senderService) RemoveSender(ctx context.Context, req *v1.RemoveSenderRequest) (*v1.RemoveSenderResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.SenderServiceRemoveSenderProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), removeSenderDetails(req))
	if err != nil {
		return nil, err
	}
	if err := senderName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, canceled, err := p.RemoveSender(ctx, req.GetName(), req.GetCancelJobs(), c.now(), changeEntry(d, access.Facts{}), c.adminGuard(d))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoSender)
	}
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	// The sender as it was, without a health: it no longer exists to reach
	// any peer.
	m := senderMessage(out, SenderHealth{})
	m.Health = nil
	return &v1.RemoveSenderResponse{Sender: m, JobsCanceled: canceled}, nil
}

// TestSender tries the sender's peer through the part of the server that
// sends, and reports each step; it sends no instance and changes nothing.
func (s senderService) TestSender(ctx context.Context, req *v1.TestSenderRequest) (*v1.TestSenderResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.SenderServiceTestSenderProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := senderName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	stored, err := lookupSender(ctx, p, req.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	results, err := c.senderTests.TestSender(ctx, req.GetTenant(), stored)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	passed := true
	out := &v1.TestSenderResponse{Results: make([]*v1.SenderTestResult, 0, len(results))}
	for _, r := range results {
		passed = passed && r.Outcome != TestFailed
		out.Results = append(out.Results, testResultMessage(r))
	}
	out.Passed = passed
	c.record(d, st, master.OutcomeDone, "", access.Facts{Details: map[string]string{detailPassed: strconv.FormatBool(passed)}})
	return out, nil
}
