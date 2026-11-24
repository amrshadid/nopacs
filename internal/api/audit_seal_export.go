package api

import (
	"context"
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/auditseal"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// ExportAudit streams a range of a log's sealed entries, each checkpoint
// after the last entry it covers, behind a header with what checks them
// outside the server. It holds one page of checkpoints and one batch of
// entries at a time (auditseal.Export), and each message is one entry, one
// checkpoint or the header, so no message nears the contract's 4 MB: an
// entry is bounded by the audit log's own bounds, a few KiB with its
// encoded form. The caller's key is checked again while the stream is open,
// as every stream's is; one entry is recorded when it opens and one when it
// ends, with how many entries it carried.
func (s auditSealService) ExportAudit(ctx context.Context, req *v1.ExportAuditRequest, stream nopacsv1connect.AuditSealServiceExportAuditServerStream) error {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AuditSealServiceExportAuditProcedure)
	if err != nil {
		return err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return c.fail(d, err)
	}
	details := sealRangeDetails(req.GetScope(), req.GetFromSeq(), req.ToSeq)
	l, err := c.sealLogOf(ctx, d, scope, details)
	if err != nil {
		return err
	}
	fail := func(err error) error { return c.failWith(d, l.st, access.Facts{Details: details}, err) }
	o := auditseal.ExportOptions{Installation: c.installation, Log: l.name}
	if o.From, o.To, err = sealRange(req.GetFromSeq(), req.ToSeq); err != nil {
		return fail(err)
	}
	if o.Keys, err = c.auditKeys(); err != nil {
		return fail(err)
	}
	keys, err := publicKeyMessages(o.Keys)
	if err != nil {
		return fail(err)
	}
	if o.OtherAnchors, err = c.otherAnchors(ctx, l); err != nil {
		return fail(err)
	}

	ctx, stop := c.watchKey(ctx, d)
	defer stop()
	c.record(d, l.st, master.OutcomeDone, "", access.Facts{Details: withDetails(details, map[string]string{detailStream: "opened"})})
	var checkpoints int64
	n, err := auditseal.Export(ctx, l.reads, o, func(it auditseal.ExportItem) error {
		m := exportMessage(it, l.tenant, keys)
		if it.Checkpoint != nil {
			checkpoints++
		}
		return stream.Send(m)
	})
	end := func(how string) access.Facts {
		return access.Facts{Details: withDetails(details, map[string]string{detailStream: "ended", detailEnded: how,
			detailEntries: strconv.FormatInt(n, 10), detailCheckpointsSent: strconv.FormatInt(checkpoints, 10)})}
	}
	if refusal, ok := refusedWhileOpen(ctx); ok {
		// Confirm recorded the refusal.
		return c.toAPI(d, refusal)
	}
	switch {
	case err == nil:
		c.record(d, l.st, master.OutcomeDone, "", end("complete"))
		return nil
	case ctx.Err() != nil:
		// The client went away, or the server is stopping; an export is
		// made again from its start, or from the seq after the last
		// checkpoint received.
		c.record(d, l.st, master.OutcomeFailed, "the stream ended before its last entry: the client went away or the server stopped", end("canceled"))
		return ctx.Err()
	}
	return c.failWith(d, l.st, end(strings.ReplaceAll(fault.KindOf(err).String(), " ", "_")), err)
}

// exportMessage returns one item of an export as the contract writes it.
// keys are the header's public keys, already written.
func exportMessage(it auditseal.ExportItem, tenantName string, keys []*v1.AuditPublicKey) *v1.ExportAuditResponse {
	switch {
	case it.Header != nil:
		h := it.Header
		m := &v1.AuditExportHeader{InstallationId: h.Installation, Tenant: tenantName, PublicKeys: keys,
			ChainBefore: h.ChainBefore[:], EncodingVersion: auditseal.EncodingVersion, Log: h.Log, FromSeq: h.From}
		for _, a := range h.Anchors {
			m.Anchors = append(m.Anchors, &v1.AuditAnchor{Log: a.Log, Digest: a.Digest[:], ToSeq: a.To})
		}
		return &v1.ExportAuditResponse{Kind: &v1.ExportAuditResponse_Header{Header: m}}
	case it.Entry != nil:
		e := it.Entry
		return &v1.ExportAuditResponse{Kind: &v1.ExportAuditResponse_Entry{Entry: &v1.SealedAuditEntry{
			Seq: e.Seq, Entry: entryMessage(e.Entry), Encoded: e.Encoded, Chain: e.Chain[:]}}}
	}
	return &v1.ExportAuditResponse{Kind: &v1.ExportAuditResponse_Checkpoint{Checkpoint: checkpointMessage(*it.Checkpoint, tenantName)}}
}
