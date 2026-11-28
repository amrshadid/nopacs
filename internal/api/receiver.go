package api

import (
	"context"
	"slices"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageNoReceiver answers a receiver name the tenant does not have.
const messageNoReceiver = "the tenant has no receiver with this name"

// receiverService is ReceiverService: a tenant's receivers, kept in its
// store, on the ports and addresses the installation allows. Whether a
// receiver listens is the part of the server that runs receivers; a handler
// only stores what it is to do, and answers the health it is told
// (Receivers).
type receiverService struct{ c *core }

var _ nopacsv1connect.ReceiverServiceHandler = receiverService{}

// lookupReceiver reads a receiver, answering one the tenant does not have as
// NotFound with the contract's message.
func lookupReceiver(ctx context.Context, p tenant.Pipeline, name string) (tenant.Receiver, error) {
	r, err := p.Receiver(ctx, name)
	if fault.KindOf(err) == fault.NotFound {
		return r, fault.Wrap(fault.NotFound, err, messageNoReceiver)
	}
	return r, err
}

// message returns r as the contract writes it, with its health now, for
// the caller of d: an import receiver, which holds no listener, is idle
// while enabled, and its map file, a path, is answered to an operator or
// more only (mayReadPaths).
func (s receiverService) message(d access.Decision, tenantName string, r tenant.Receiver) *v1.Receiver {
	if r.Type != tenant.ReceiverImport {
		return receiverMessage(r, s.c.receivers.Health(tenantName, r))
	}
	m := receiverMessage(r, importHealth(r))
	if mayReadPaths(d) {
		m.GetImporter().MapFile = r.Import.MapFile
	}
	return m
}

// AddReceiver adds a receiver to a tenant, with the contract's defaults
// filled in and stored, on a port the installation allotted to the tenant
// and an address it lists.
func (s receiverService) AddReceiver(ctx context.Context, req *v1.AddReceiverRequest) (*v1.AddReceiverResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReceiverServiceAddReceiverProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), requestReceiverDetails(req.GetReceiver()))
	if err != nil {
		return nil, err
	}
	c.places.RLock()
	defer c.places.RUnlock()
	m := req.GetReceiver()
	if dm := m.GetDimse(); dm != nil && dm.GetPort() == 0 {
		// No port given: the server's lowest allotted port that no receiver
		// of the tenant holds and that it can bind now, chosen under the
		// lock a change of the allotment takes. The request is checked
		// first, with a stand-in port, so the probe binds only an address
		// that is valid for a receiver (an IP literal, never a name to
		// resolve).
		probe := proto.CloneOf(m)
		probe.GetDimse().Port = 1
		if _, err := receiverFromRequest("receiver", probe); err != nil {
			return nil, c.failIn(d, st, err)
		}
		port, err := c.choosePort(ctx, p, st, dm.GetBindAddress())
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		m = proto.CloneOf(m)
		m.GetDimse().Port = int32(port) //nolint:gosec // G115: a port of the allotment, 1 to 65535.
	}
	in, err := receiverFromRequest("receiver", m)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := c.checkReceiverPlace(ctx, st, in); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := c.checkImportReceiver(ctx, st, in); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := c.checkDeidentification(ctx, p, st, in); err != nil {
		return nil, c.layerFail(d, st, access.Facts{}, err)
	}
	out, err := p.AddReceiver(ctx, in, c.now(), changeEntry(d, access.Facts{Details: receiverDetails(in, true)}), c.adminGuard(d))
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("receiver", err))
	}
	c.receiversChanged(req.GetTenant())
	return &v1.AddReceiverResponse{Receiver: s.message(d, req.GetTenant(), out)}, nil
}

// GetReceiver answers one receiver of a tenant, with its health now.
func (s receiverService) GetReceiver(ctx context.Context, req *v1.GetReceiverRequest) (*v1.GetReceiverResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ReceiverServiceGetReceiverProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := lookupReceiver(ctx, p, req.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	return &v1.GetReceiverResponse{Receiver: s.message(d, req.GetTenant(), out)}, nil
}

// ListReceivers answers a page of a tenant's receivers, ordered by name,
// each with its health now.
func (s receiverService) ListReceivers(ctx context.Context, req *v1.ListReceiversRequest) (*v1.ListReceiversResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ReceiverServiceListReceiversProcedure)
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
	items, next, err := p.ListReceivers(ctx, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListReceiversResponse{Receivers: make([]*v1.Receiver, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Receivers = append(out.Receivers, s.message(d, req.GetTenant(), it))
	}
	return out, nil
}

// SetReceiver changes the fields of a receiver its update_mask names, under
// every rule AddReceiver applies.
func (s receiverService) SetReceiver(ctx context.Context, req *v1.SetReceiverRequest) (*v1.SetReceiverResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReceiverServiceSetReceiverProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), maskDetails(req.GetUpdateMask().GetPaths()))
	if err != nil {
		return nil, err
	}
	in := req.GetReceiver()
	if in == nil {
		return nil, c.failIn(d, st, invalid("receiver", "is required: its name names the receiver to change"))
	}
	if err := itemName("receiver.name", in.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	paths, err := receiverMask.paths(req.GetUpdateMask())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	paths, importPaths := splitImportPaths(paths)
	fields := make([]tenant.ReceiverField, 0, len(paths)+1)
	for _, p := range paths {
		fields = append(fields, tenant.ReceiverField(p))
	}
	if len(importPaths) > 0 {
		// The store replaces an import receiver's configuration whole; the
		// fields named are set on the stored one below.
		fields = append(fields, tenant.FieldImporter)
	}
	change, err := receiverChange(in, fields)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if t := req.GetExpectedUpdateTime(); t != nil {
		if t.CheckValid() != nil {
			return nil, c.failIn(d, st, invalid("expected_update_time", "is a valid time"))
		}
		// A change with Updated set is made only while the stored
		// receiver has it (tenant.ApplyReceiver), in the store's
		// transaction.
		change.Updated = t.AsTime()
	}
	// A change of the naming rule or the de-identification is checked
	// against the other, and against the rule set the receiver runs, as
	// stored: taken alone, so two changes at once cannot each pass and
	// together give a receiver that de-identifies a rule that puts values
	// into labels, or archive rules that name a profile on a receiver that
	// archives as received (the store refuses both too).
	// A change of an import receiver's fields is composed from the stored
	// ones and checked against its jobs in progress, which an import job's
	// creation checks under the same lock, so neither passes the other.
	if slices.Contains(fields, tenant.FieldNaming) || slices.Contains(fields, tenant.FieldDeidentification) || len(importPaths) > 0 {
		c.places.Lock()
		defer c.places.Unlock()
	} else {
		c.places.RLock()
		defer c.places.RUnlock()
	}
	stored, err := lookupReceiver(ctx, p, in.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if len(importPaths) > 0 {
		if stored.Type == tenant.ReceiverImport {
			running, err := importJobInProgress(ctx, p, stored.Name)
			if err != nil {
				return nil, c.failIn(d, st, err)
			}
			if running {
				return nil, c.failIn(d, st, errImportJobRunning("change its import fields"))
			}
		}
		change.Import = mergeImport(stored.Import, change.Import, importPaths)
	}
	result, err := tenant.ApplyReceiver(stored, change, fields)
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("receiver", err))
	}
	if err := c.checkReceiverPlace(ctx, st, result); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if len(importPaths) > 0 {
		if err := c.checkImportReceiver(ctx, st, result); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	// Only a change of the choice, or of the naming rule of a receiver that
	// de-identifies, is checked: a receiver that archives as received keeps
	// doing so, and can be changed otherwise, after the setting is turned
	// off.
	if slices.Contains(fields, tenant.FieldDeidentification) || (result.Deidentifies() && slices.Contains(fields, tenant.FieldNaming)) {
		if err := c.checkDeidentification(ctx, p, st, result); err != nil {
			return nil, c.layerFail(d, st, access.Facts{}, err)
		}
	}
	// A new profile is a new layer under the rule set the receiver runs, whose
	// archive and destination profiles may only tighten it (layering is
	// judged at create and activate, which this change would otherwise
	// bypass). A receiver that runs no rule set has nothing to loosen.
	if slices.Contains(fields, tenant.FieldDeidentification) && result.Deidentifies() && result.Profile != stored.Profile {
		rs, err := p.RuleSet(ctx, result.Name, 0)
		if err != nil && fault.KindOf(err) != fault.NotFound {
			return nil, c.failIn(d, st, err)
		}
		if err == nil {
			if err := c.checkRuleSetProfiles(ctx, p, result, rs); err != nil {
				return nil, c.layerFail(d, st, access.Facts{}, err)
			}
		}
	}
	facts, err := c.checkChangeControl(ctx, p, stored, result, slices.Contains(fields, tenant.FieldDeidentification), req)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := p.SetReceiver(ctx, change, fields, c.now(), changeEntry(d, facts), c.adminGuard(d))
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("receiver", err))
	}
	c.receiversChanged(req.GetTenant())
	return &v1.SetReceiverResponse{Receiver: s.message(d, req.GetTenant(), out)}, nil
}

// RemoveReceiver removes a receiver's configuration. Its receptions and
// quarantined files stay, naming it.
func (s receiverService) RemoveReceiver(ctx context.Context, req *v1.RemoveReceiverRequest) (*v1.RemoveReceiverResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReceiverServiceRemoveReceiverProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	// Read first for the type, port and AE title the entry records.
	stored, err := lookupReceiver(ctx, p, req.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	facts := access.Facts{Details: receiverDetails(stored, false)}
	if stored.Type == tenant.ReceiverImport {
		// Under the lock an import job's creation takes, so no job is made
		// for the receiver between this check and its removal.
		c.places.Lock()
		defer c.places.Unlock()
		running, err := importJobInProgress(ctx, p, stored.Name)
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		if running {
			return nil, c.failWith(d, st, facts, errImportJobRunning("remove it"))
		}
	}
	out, err := p.RemoveReceiver(ctx, req.GetName(), changeEntry(d, facts), c.adminGuard(d))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoReceiver)
	}
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	c.receiversChanged(req.GetTenant())
	// The receiver as it was, stopped: its configuration as stored, and a
	// health that says it no longer runs, whatever ran it.
	gone := Health{State: HealthDown, Reason: DownOther, Detail: detailRemoved, Since: c.now()}
	return &v1.RemoveReceiverResponse{Receiver: receiverMessage(out, gone)}, nil
}

// ReceiverChanges is told, after a change of a tenant's receivers is
// committed (added, changed or removed), so what runs them acts on it at
// once instead of at its next periodic read. Config.Receivers may implement
// it; one that does not is left to its own reading.
type ReceiverChanges interface {
	// ReceiversChanged says the receivers of the tenant named tenantName
	// changed. It must not block: it is called on the request's path.
	ReceiversChanged(tenantName string)
}

// receiversChanged tells the part of the server that runs receivers, when
// it listens, that a tenant's receivers changed.
func (c *core) receiversChanged(tenantName string) {
	if rc, ok := c.receivers.(ReceiverChanges); ok {
		rc.ReceiversChanged(tenantName)
	}
}

// detailRemoved is the phrase of a removed receiver's health.
const detailRemoved = "removed"

// choosePort returns the port a receiver added without one is given
// (place.FreePort): the lowest allotted to the tenant that none of its
// receivers holds, allotments never overlapping between tenants, and that
// the server can bind on host now.
func (c *core) choosePort(ctx context.Context, p tenant.Pipeline, st tenant.Store, host string) (int, error) {
	allotted, err := c.values().OnTenant(st).Ports(ctx)
	if err != nil {
		return 0, err
	}
	receivers, err := allReceivers(ctx, p)
	if err != nil {
		return 0, err
	}
	used := make(map[int]bool, len(receivers))
	for _, r := range receivers {
		if r.Type == tenant.ReceiverDIMSE {
			used[r.DIMSE.Port] = true
		}
	}
	probe := place.Probe(place.SystemProbe)
	if c.seams.probe != nil {
		probe = c.seams.probe
	}
	return place.FreePort(allotted, used, host, probe)
}
