package api

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// detailTargets is the audit detail of a batch delete's count of targets.
const detailTargets = "targets"

// batchTokenPrefix starts every token a batch delete's preview answers, so
// a token of another kind (a delete's, a disposal's) is told apart at once.
const batchTokenPrefix = "k2."

// deleteBatchDetails returns a batch delete's audit details: how many
// targets it names and whether it is a preview, never the targets
// themselves.
func deleteBatchDetails(r *v1.DeleteBatchRequest) map[string]string {
	return map[string]string{detailTargets: strconv.Itoa(len(r.GetTargets())), detailValidateOnly: strconv.FormatBool(r.GetValidateOnly())}
}

// keyedBatchConfirm wraps the store's batch token in a MAC under the
// installation's confirm key, so only this server makes a token a batch
// takes; the store's token stays readable inside it, so a batch sent again
// after its job deleted the targets still finds the job its token made
// (keyedDisposeConfirm does the same for a disposal).
func keyedBatchConfirm(key *secret.DerivedKey, token string) (string, error) {
	mac, err := key.MAC([]byte(token))
	if err != nil {
		return "", err
	}
	return batchTokenPrefix + token + "." + base64.RawURLEncoding.EncodeToString(mac[:disposeMACLength]), nil
}

// unkeyBatchConfirm returns the store's token inside a token
// keyedBatchConfirm made, or a precondition failure for any other.
func unkeyBatchConfirm(key *secret.DerivedKey, keyed string) (string, error) {
	bad := precondition("the confirm token is not one this server's preview of a batch delete gave")
	rest, ok := strings.CutPrefix(keyed, batchTokenPrefix)
	i := strings.LastIndexByte(rest, '.')
	if !ok || i < 0 {
		return "", bad
	}
	token, got := rest[:i], rest[i+1:]
	if _, ok := tenant.BatchConfirmIssued(token); !ok {
		return "", bad
	}
	mac, err := key.MAC([]byte(token))
	if err != nil {
		return "", err
	}
	if !hmac.Equal([]byte(got), []byte(base64.RawURLEncoding.EncodeToString(mac[:disposeMACLength]))) {
		return "", bad
	}
	return token, nil
}

// batchMax returns correction.batch-max on the tenant of st, the
// contract's bound when the server's list does not hold it.
func (c *core) batchMax(ctx context.Context, st tenant.Store) (int, error) {
	v, known, err := c.tenantSetting(ctx, st, settings.CorrectionBatchMax)
	if err != nil || !known {
		return tenant.MaxDeleteBatchTargets, err
	}
	n, err := settings.ParseBatchMax(v)
	if err != nil {
		return 0, fault.New(fault.Internal, "api: the tenant's "+settings.CorrectionBatchMax+" "+err.Error())
	}
	return n, nil
}

// deleteGrace returns delete.grace on the tenant of st, the built-in
// default when the server's list does not hold it.
func (c *core) deleteGrace(ctx context.Context, st tenant.Store) (time.Duration, error) {
	v, known, err := c.tenantSetting(ctx, st, settings.DeleteGrace)
	if err != nil || !known {
		return tenant.DefaultGrace, err
	}
	g, err := settings.ParseDeleteGrace(v)
	if err != nil {
		return 0, fault.New(fault.Internal, "api: the tenant's "+settings.DeleteGrace+" "+err.Error())
	}
	return g, nil
}

// checkDeleteBatch returns the Invalid error of a batch delete request, or
// its targets; most is the tenant's correction.batch-max.
func checkDeleteBatch(req *v1.DeleteBatchRequest, most int) ([]tenant.CorrectionTarget, error) {
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return nil, fieldOf("reason", err)
	}
	switch {
	case len(req.GetTargets()) == 0:
		return nil, invalid("targets", "name at least one subject, session or scan")
	case len(req.GetTargets()) > most:
		return nil, invalid("targets", "are at most "+strconv.Itoa(most)+", the tenant's "+settings.CorrectionBatchMax)
	case req.GetValidateOnly() && req.GetConfirm() != "":
		return nil, invalid("confirm", "is empty with validate_only")
	case !req.GetValidateOnly() && req.GetConfirm() == "":
		return nil, invalid("confirm", "is the confirm token of a preview (validate_only) of the same targets and reason")
	case len(req.GetConfirm()) > tenant.MaxConfirmLength:
		return nil, invalid("confirm", "is at most 128 bytes")
	}
	targets := make([]tenant.CorrectionTarget, len(req.GetTargets()))
	for i, t := range req.GetTargets() {
		targets[i] = targetOf(t)
	}
	if err := tenant.ValidateBatchTargets(targets, most); err != nil {
		return nil, fieldOf("targets", err)
	}
	return targets, nil
}

// deleteBatches returns the tenant's batch deletes of st.
func deleteBatches(st tenant.Store) (tenant.DeleteBatches, error) {
	db, ok := st.(tenant.DeleteBatches)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no batch deletes")
	}
	return db, nil
}

// DeleteBatch answers a batch delete's preview, or makes the job its
// confirm token was given for. The job's runner is the server's: it
// deletes each target through the tenant's corrections engine.
func (s correctionService) DeleteBatch(ctx context.Context, req *v1.DeleteBatchRequest) (*v1.DeleteBatchResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.CorrectionServiceDeleteBatchProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: deleteBatchDetails(req)}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	most, err := c.batchMax(ctx, st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	targets, err := checkDeleteBatch(req, most)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailReason] = req.GetReason()
	if c.corrections == nil {
		return nil, c.failWith(d, st, facts, fault.New(fault.Unavailable, "the server runs no corrections"))
	}
	db, err := deleteBatches(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	key, err := c.confirmKey()
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if req.GetValidateOnly() {
		pv, err := db.PreviewDeleteBatch(ctx, targets, req.GetReason(), c.now())
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		keyed, err := keyedBatchConfirm(key, pv.Confirm)
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		grace, err := c.deleteGrace(ctx, st)
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		c.record(d, st, master.OutcomeDone, "", facts)
		return &v1.DeleteBatchResponse{Preview: batchPreviewMessage(pv, keyed, c.now().Add(grace))}, nil
	}
	token, err := unkeyBatchConfirm(key, req.GetConfirm())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failWith(d, st, facts, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	facts.Details[detailJob] = id
	job, _, err := db.QueueDeleteBatch(ctx, tenant.DeleteBatchQueue{ID: id, Targets: targets, Reason: req.GetReason(), Confirm: token,
		KeyID: callerKeyID(d), Max: most, Now: c.now()}, changeEntry(d, facts), c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.DeleteBatchResponse{Preview: &v1.DeleteBatchPreview{Counts: &v1.CorrectionCounts{Instances: job.Counts.Instances,
		Bytes: job.Counts.Bytes}}, Job: c.newJobViews(p, req.GetTenant()).message(ctx, job)}, nil
}

// batchPreviewMessage returns a batch delete's preview as the contract
// writes it.
func batchPreviewMessage(p tenant.DeleteBatchPreview, confirm string, purge time.Time) *v1.DeleteBatchPreview {
	m := &v1.DeleteBatchPreview{Counts: correctionCounts(p.Counts), TargetsHeld: p.Held, TargetsKept: p.Kept, PurgeTime: timestamp(purge),
		Confirm: confirm, Items: make([]*v1.DeleteBatchItem, 0, len(p.Items))}
	for _, it := range p.Items {
		item := &v1.DeleteBatchItem{Target: targetMessage(it.Target), Counts: correctionCounts(it.Counts), Storages: it.Storages,
			Holds: it.Holds, SessionsUnderMinimum: it.SessionsUnderMinimum, KeepUntil: timestamp(it.KeepUntil)}
		for _, r := range it.Replicas {
			item.Replicas = append(item.Replicas, r.Replica)
		}
		for _, sp := range it.SentTo {
			item.SentTo = append(item.SentTo, sp.Sender)
		}
		m.Items = append(m.Items, item)
	}
	return m
}

// batchOutcomes maps a target's outcome to the contract's.
var batchOutcomes = map[tenant.BatchOutcome]v1.DeleteBatchOutcome{
	tenant.BatchPending:        v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_PENDING,
	tenant.BatchDeleted:        v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_DELETED,
	tenant.BatchSkippedHeld:    v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_HELD,
	tenant.BatchSkippedKept:    v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_KEPT,
	tenant.BatchSkippedChanged: v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_CHANGED,
	tenant.BatchSkippedGone:    v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_GONE,
	tenant.BatchSkippedOverlap: v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_OVERLAP,
	tenant.BatchSkippedRefused: v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_REFUSED,
}

// batchPageToken is the token of the page of a batch job's targets after
// position: bound to the job, so one job's token is refused for another.
func batchPageToken(job string, position int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("dbt1:" + job + ":" + strconv.Itoa(position)))
}

// batchPageAfter returns the position a page token names for the job, 0
// for none, or the Invalid error of a token this list did not give.
func batchPageAfter(job, token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	bad := invalid("page_token", "is not one this list returned for this job")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, bad
	}
	rest, ok := strings.CutPrefix(string(raw), "dbt1:"+job+":")
	n, err := strconv.Atoi(rest)
	if !ok || err != nil || n < 1 || n > tenant.MaxDeleteBatchTargets || strconv.Itoa(n) != rest {
		return 0, bad
	}
	return n, nil
}

// ListDeleteBatchTargets answers a page of a batch delete job's targets, in
// order, each with what became of it.
func (s correctionService) ListDeleteBatchTargets(ctx context.Context, req *v1.ListDeleteBatchTargetsRequest) (*v1.ListDeleteBatchTargetsResponse,
	error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.CorrectionServiceListDeleteBatchTargetsProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("job_id", req.GetJobId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	after, err := batchPageAfter(req.GetJobId(), page.Cursor)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	db, err := deleteBatches(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	ts, err := db.DeleteBatchTargets(ctx, req.GetJobId(), after, page.Limit+1)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	out := &v1.ListDeleteBatchTargetsResponse{Targets: []*v1.DeleteBatchTarget{}}
	for i, t := range ts {
		if i == page.Limit {
			out.NextPageToken = batchPageToken(req.GetJobId(), ts[i-1].Ord)
			break
		}
		out.Targets = append(out.Targets, &v1.DeleteBatchTarget{Position: int64(t.Ord), Target: targetMessage(t.Target), Outcome: batchOutcomes[t.Outcome],
			CorrectionId: t.Correction, Instances: t.Instances, Bytes: t.Bytes})
	}
	c.done(d, st)
	return out, nil
}
