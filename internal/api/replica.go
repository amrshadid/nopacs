package api

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/storageswitch"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The keys of the details of ReplicaService's entries, beside those
// requirement.go names.
const (
	// detailPrimary is the primary a replica is attached to, or was.
	detailPrimary = "primary"
	// detailReplica is the replica a switch or a repair names.
	detailReplica = "replica"
	// detailAbandonReason and detailSkipReason are a person's reasons for
	// abandoning purges and for passing over a switch's checks.
	detailAbandonReason = "abandon_reason"
	detailSkipReason    = "skip_reason"
	// detailJob is the id of the job a request queued or was answered.
	detailJob = "job"
)

// Switcher is what SwitchStorage asks of a tenant's switch runner
// (storageswitch.Switcher): a preview, an accept, and the switch's end.
type Switcher interface {
	// Validate answers whether a switch could start now and its token.
	Validate(ctx context.Context, primary, replica string, skip bool) (storageswitch.Preview, error)
	// Begin accepts a switch under its token and starts it.
	Begin(ctx context.Context, r storageswitch.Request) (tenant.StorageSwitch, error)
	// Wait answers the switch of id once it has ended, or as it stands
	// with ctx's error when ctx ends first.
	Wait(ctx context.Context, id string) (tenant.StorageSwitch, error)
}

// Switches finds the switch runner of a tenant by name. A tenant the server
// runs none for (not ready, or the server stopping) is an Unavailable error.
type Switches interface {
	Switcher(ctx context.Context, tenantName string) (Switcher, error)
}

// SwitchesFunc is a func that is Switches.
type SwitchesFunc func(ctx context.Context, tenantName string) (Switcher, error)

// Switcher calls f.
func (f SwitchesFunc) Switcher(ctx context.Context, tenantName string) (Switcher, error) {
	return f(ctx, tenantName)
}

// replicaService is ReplicaService: a tenant's replicas, kept in its store,
// copied by the server's replicator, mended by repair jobs and swapped in
// by the tenant's switch runner (Config.Switches).
//
// BackfillReplica, Recall and VerifySample are in the contract and not
// served yet (api/not-served.txt): the server keeps no replicate job a
// backfill could answer, and no read of a target's objects per storage
// that a recall's preview and a sample need. The server does not mount
// them, so their bodies here are never reached.
type replicaService struct {
	nopacsv1connect.UnimplementedReplicaServiceHandler
	c *core
}

var _ nopacsv1connect.ReplicaServiceHandler = replicaService{}

// replication returns st as the replicas' port.
func replication(st tenant.Store) (tenant.Replication, error) {
	r, ok := st.(tenant.Replication)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no replicas")
	}
	return r, nil
}

// storageNotFound gives a store's NotFound for a storage the contract's
// message.
func storageNotFound(err error) error {
	if fault.KindOf(err) == fault.NotFound {
		return fault.Wrap(fault.NotFound, err, messageNoStorage)
	}
	return err
}

// replicaStatusMessage returns how far a replica is from its primary, as
// the contract writes it, at now.
func replicaStatusMessage(r tenant.ReplicaStatus, now time.Time) *v1.ReplicaStatus {
	m := &v1.ReplicaStatus{
		Replica: r.Replica, Primary: r.Primary, Tier: tierMessage(r.Tier), ScansInStep: r.InStep, ScansPending: r.Pending,
		ScansCopying: r.Copying, ScansDamaged: r.Damaged, ObjectsDamaged: r.ObjectsDamaged, SharesBucket: r.SharesBucket,
		RecallsInProgress: r.RecallsInProgress, Lag: durationpb.New(0),
	}
	if !r.OldestQueued.IsZero() {
		m.OldestPendingTime = timestamp(r.OldestQueued)
		m.Lag = durationpb.New(max(now.Sub(r.OldestQueued), 0))
	}
	if r.LastFault != "" {
		m.LastFault = enumOf[v1.StorageFault](v1.StorageFault_value, "STORAGE_FAULT_", r.LastFault)
		m.LastFaultTime = timestamp(r.LastFaultTime)
	}
	return m
}

// tierMessage returns a storage's tier as the contract writes it: the
// empty tier is online.
func tierMessage(t tenant.StorageTier) v1.StorageTier {
	if t == tenant.TierArchive {
		return v1.StorageTier_STORAGE_TIER_ARCHIVE
	}
	return v1.StorageTier_STORAGE_TIER_ONLINE
}

// ListReplicas answers a page of a tenant's replicas, each with how far it
// is from its primary.
func (s replicaService) ListReplicas(ctx context.Context, req *v1.ListReplicasRequest) (*v1.ListReplicasResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ReplicaServiceListReplicasProcedure)
	if err != nil {
		return nil, err
	}
	details := map[string]string{detailPrimary: req.GetPrimary()}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), details)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: details}
	if req.GetPrimary() != "" {
		if err := itemName("primary", req.GetPrimary()); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rs, err := replication(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	items, next, err := rs.ListReplicas(ctx, req.GetPrimary(), page)
	if err != nil {
		return nil, c.failWith(d, st, facts, listError(err, page.Cursor))
	}
	c.record(d, st, master.OutcomeDone, "", facts)
	now := c.now()
	out := &v1.ListReplicasResponse{Replicas: make([]*v1.ReplicaStatus, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Replicas = append(out.Replicas, replicaStatusMessage(it, now))
	}
	return out, nil
}

// AttachReplica makes a storage of the tenant a replica of a primary; the
// store queues every scan of the primary for it, as its backfill.
func (s replicaService) AttachReplica(ctx context.Context, req *v1.AttachReplicaRequest) (*v1.AttachReplicaResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReplicaServiceAttachReplicaProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailPrimary: req.GetPrimary()}}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	if err := itemName("replica", req.GetReplica()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if err := itemName("primary", req.GetPrimary()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rs, err := replication(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	out, _, _, err := rs.AttachReplica(ctx, req.GetReplica(), req.GetPrimary(), c.now(), changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, storageNotFound(err))
	}
	// The store keeps the backfill as the replica's scans owed, not yet as
	// a job of its own, so no backfill job is answered.
	return &v1.AttachReplicaResponse{Storage: storageMessage(out)}, nil
}

// DetachReplica makes a replica an ordinary storage again, deleting
// nothing on it.
func (s replicaService) DetachReplica(ctx context.Context, req *v1.DetachReplicaRequest) (*v1.DetachReplicaResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReplicaServiceDetachReplicaProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailAbandonPurges: strconv.FormatBool(req.GetAbandonPurges())}}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	if err := itemName("replica", req.GetReplica()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	switch {
	case req.GetAbandonPurges() && req.GetAbandonReason() == "":
		return nil, c.failWith(d, st, facts, invalid("abandon_reason", "is required with abandon_purges"))
	case !req.GetAbandonPurges() && req.GetAbandonReason() != "":
		return nil, c.failWith(d, st, facts, invalid("abandon_reason", "is empty without abandon_purges"))
	}
	if req.GetAbandonPurges() {
		if err := tenant.ValidateUndoReason(req.GetAbandonReason()); err != nil {
			return nil, c.failWith(d, st, facts, fieldOf("abandon_reason", err))
		}
		facts.Details[detailAbandonReason] = req.GetAbandonReason()
	}
	// Read first for its former primary, which the entry records.
	stored, err := lookupStorage(ctx, p, req.GetReplica())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailPrimary] = stored.ReplicaOf
	rs, err := replication(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	out, _, err := rs.DetachReplica(ctx, tenant.ReplicaDetach{Replica: req.GetReplica(), Abandon: req.GetAbandonPurges(),
		Reason: req.GetAbandonReason(), Now: c.now()}, changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, storageNotFound(err))
	}
	return &v1.DetachReplicaResponse{Storage: storageMessage(out)}, nil
}

// sameRepair reports whether a repair job mends what r asks: the same
// storage, replica and findings (in any order), or all.
func sameRepair(j tenant.RepairJob, r *v1.RepairRequest) bool {
	if j.Storage != r.GetStorage() || j.From != r.GetFrom() || j.All != r.GetAll() || j.Automatic || len(j.Findings) != len(r.GetFindings()) {
		return false
	}
	for _, id := range r.GetFindings() {
		if !slices.Contains(j.Findings, id) {
			return false
		}
	}
	return true
}

// runningRepair returns the repair job of storage queued or running, and
// whether there is one.
func runningRepair(ctx context.Context, p tenant.Pipeline, storage string) (tenant.Job, bool, error) {
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		items, next, err := p.ListJobs(ctx, tenant.JobFilter{InProgress: true}, page)
		if err != nil {
			return tenant.Job{}, false, err
		}
		for _, j := range items {
			if j.Kind == tenant.JobRepair && j.Repair != nil && j.Repair.Storage == storage {
				return j, true, nil
			}
		}
		if next == "" {
			return tenant.Job{}, false, nil
		}
		page.Cursor = next
	}
}

// Repair queues a repair job that puts back on a primary the objects its
// findings name, read from a replica; a matching job queued or running is
// answered instead.
func (s replicaService) Repair(ctx context.Context, req *v1.RepairRequest) (*v1.RepairResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReplicaServiceRepairProcedure)
	if err != nil {
		return nil, err
	}
	findings := strconv.Itoa(len(req.GetFindings()))
	if req.GetAll() {
		findings = "all"
	}
	facts := access.Facts{Details: map[string]string{detailReplica: req.GetFrom(), detailFindings: findings}}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	if err := itemName("storage", req.GetStorage()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if req.GetFrom() != "" {
		if err := itemName("from", req.GetFrom()); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
	}
	switch n := len(req.GetFindings()); {
	case n == 0 && !req.GetAll():
		return nil, c.failWith(d, st, facts, invalid("findings", "or all is required"))
	case n > 0 && req.GetAll():
		return nil, c.failWith(d, st, facts, invalid("findings", "is empty with all"))
	case n > tenant.MaxRepairFindings:
		return nil, c.failWith(d, st, facts, invalid("findings", "names at most "+strconv.Itoa(tenant.MaxRepairFindings)+" findings"))
	}
	for i, id := range req.GetFindings() {
		if err := serverID("findings", id); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		if slices.Contains(req.GetFindings()[:i], id) {
			return nil, c.failWith(d, st, facts, invalid("findings", "names each finding once"))
		}
	}
	// The store checks the rest in the transaction that makes the job: the
	// storage a primary, the replica named one of it, each finding on it,
	// holding and of a kind a repair mends, and no switch of it running.
	running, found, err := runningRepair(ctx, p, req.GetStorage())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if found {
		if !sameRepair(*running.Repair, req) {
			return nil, c.failWith(d, st, facts, precondition("a repair of the storage "+req.GetStorage()+" with other findings is queued or running ("+running.ID+")"))
		}
		facts.Details[detailJob] = running.ID
		c.record(d, st, master.OutcomeDone, "", facts)
		return &v1.RepairResponse{Job: jobMessage(running, "")}, nil
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failWith(d, st, facts, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	nj := tenant.NewJob{ID: id, Kind: tenant.JobRepair, Created: c.now(), Repair: &tenant.RepairJob{Storage: req.GetStorage(), From: req.GetFrom(),
		Findings: slices.Clone(req.GetFindings()), All: req.GetAll()}}
	if err := nj.Validate(); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailJob] = id
	out, err := p.CreateJob(ctx, nj, changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.RepairResponse{Job: jobMessage(out, "")}, nil
}

// The bounds of a switch's drain (storageswitch), the contract's.
const (
	minDrain = storageswitch.MinDrainTimeout
	maxDrain = storageswitch.MaxDrainTimeout
)

// checkSwitch returns the Invalid error of a switch request, and its drain.
func checkSwitch(req *v1.SwitchStorageRequest) (time.Duration, error) {
	if err := itemName("primary", req.GetPrimary()); err != nil {
		return 0, err
	}
	if err := itemName("replica", req.GetReplica()); err != nil {
		return 0, err
	}
	var drain time.Duration
	if dt := req.GetDrainTimeout(); dt != nil {
		if dt.CheckValid() != nil || dt.AsDuration() < minDrain || dt.AsDuration() > maxDrain {
			return 0, invalid("drain_timeout", "is 1 second to 1 hour")
		}
		drain = dt.AsDuration()
	}
	switch {
	case req.GetValidateOnly() && req.GetConfirm() != "":
		return 0, invalid("confirm", "is empty with validate_only")
	case !req.GetValidateOnly() && req.GetConfirm() == "":
		return 0, invalid("confirm", "is the confirm token of a preview (validate_only) of the same primary and replica")
	case len(req.GetConfirm()) > tenant.MaxConfirmLength:
		return 0, invalid("confirm", "is at most 128 bytes")
	case req.GetSkipVerification() && req.GetSkipReason() == "":
		return 0, invalid("skip_reason", "is required with skip_verification")
	case !req.GetSkipVerification() && req.GetSkipReason() != "":
		return 0, invalid("skip_reason", "is empty without skip_verification")
	}
	if req.GetSkipVerification() {
		if err := tenant.ValidateUndoReason(req.GetSkipReason()); err != nil {
			return 0, fieldOf("skip_reason", err)
		}
	}
	return drain, nil
}

// switcher returns the tenant's switch runner.
func (c *core) switcher(ctx context.Context, name string) (Switcher, error) {
	if c.switches == nil {
		return nil, fault.New(fault.Unavailable, "the server runs no switch of the tenant's storages now; try again shortly")
	}
	return c.switches.Switcher(ctx, name)
}

// SwitchStorage previews a switch of a primary to its replica, or starts
// the one its token was given for and answers its end.
func (s replicaService) SwitchStorage(ctx context.Context, req *v1.SwitchStorageRequest) (*v1.SwitchStorageResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReplicaServiceSwitchStorageProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{detailReplica: req.GetReplica(), detailValidateOnly: strconv.FormatBool(req.GetValidateOnly()),
		detailSkipVerification: strconv.FormatBool(req.GetSkipVerification())}}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	drain, err := checkSwitch(req)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if req.GetSkipVerification() {
		facts.Details[detailSkipReason] = req.GetSkipReason()
	}
	for _, name := range []string{req.GetPrimary(), req.GetReplica()} {
		if _, err := lookupStorage(ctx, p, name); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
	}
	sw, err := c.switcher(ctx, req.GetTenant())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if req.GetValidateOnly() {
		pv, err := sw.Validate(ctx, req.GetPrimary(), req.GetReplica(), req.GetSkipVerification())
		if err != nil {
			return nil, c.failWith(d, st, facts, storageNotFound(err))
		}
		facts.Details["scans_pending"] = strconv.FormatInt(pv.ScansPending, 10)
		c.record(d, st, master.OutcomeDone, "", facts)
		return &v1.SwitchStorageResponse{Ready: pv.Ready, Blockers: pv.Blockers, ScansPending: pv.ScansPending, Receivers: pv.Receivers,
			Confirm: pv.Confirm, SwitchGeneration: pv.Generation}, nil
	}
	begun, err := sw.Begin(ctx, storageswitch.Request{Primary: req.GetPrimary(), Replica: req.GetReplica(), Confirm: req.GetConfirm(),
		SkipVerification: req.GetSkipVerification(), SkipReason: req.GetSkipReason(), DrainTimeout: drain,
		Audit: changeEntry(d, facts), Guard: c.roleGuard(d, master.RoleAdmin)})
	switch {
	case err != nil && begun.ID != "":
		// Accepted and recorded with its entry, then not run.
		return nil, c.toAPI(d, err)
	case err != nil:
		return nil, c.failWith(d, st, facts, storageNotFound(err))
	}
	ended, err := sw.Wait(ctx, begun.ID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The switch runs on whatever becomes of the call: its outcome
			// is in the storage's status and its event.
			return nil, connect.NewError(connect.CodeDeadlineExceeded,
				"the switch was accepted and runs on; its outcome is in the storage's status (switching, last_switch_time) and the storage_switched event")
		}
		return nil, c.toAPI(d, err)
	}
	if ended.State == tenant.SwitchRefused {
		return nil, c.toAPI(d, precondition("the switch was refused: "+ended.Refusal+"; every write resumed on the old place"))
	}
	out, err := c.switchMessage(ctx, p, ended)
	if err != nil {
		// The switch and its entry are written; only the answer failed.
		return nil, c.toAPI(d, err)
	}
	return out, nil
}

// switchMessage returns a switch that ended done as the contract answers
// it, with both storages as they stand after it.
func (c *core) switchMessage(ctx context.Context, p tenant.Pipeline, sw tenant.StorageSwitch) (*v1.SwitchStorageResponse, error) {
	primary, err := lookupStorage(ctx, p, sw.Primary)
	if err != nil {
		return nil, err
	}
	replica, err := lookupStorage(ctx, p, sw.Replica)
	if err != nil {
		return nil, err
	}
	return &v1.SwitchStorageResponse{Ready: true, ScansPending: sw.ScansDrained, Primary: storageMessage(primary), Replica: storageMessage(replica),
		Pause: durationpb.New(sw.Pause), AssociationsRefused: sw.AssociationsRefused, SwitchTime: timestamp(sw.Swapped),
		SwitchGeneration: primary.SwitchGeneration, Blockers: []string{}}, nil
}
