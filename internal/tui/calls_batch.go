package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// This file holds the calls of the batch delete screens.

// batchOutcomePrefix starts every name of a batch target's outcome.
const batchOutcomePrefix = "DELETE_BATCH_OUTCOME_"

// maxFilteredScans bounds the scans a catalog filter may select for a batch,
// so a filter that matches the whole archive does not fill memory before the
// server's own bound on a batch refuses it.
const maxFilteredScans = 20000

// batchPreviewMsg is the answer of a batch delete's preview: each target with
// what keeps it from being deleted, and the token the batch must carry, with
// the request it answers.
type batchPreviewMsg struct {
	prev *v1.DeleteBatchPreview
	req  *v1.DeleteBatchRequest
}

// batchStartedMsg is the answer of a batch delete that was confirmed: the job
// that deletes each target in turn.
type batchStartedMsg struct{ job *v1.Job }

// jobPausedMsg is the answer of a pause or a resume: the job as it stands, and
// the sentence to show.
type jobPausedMsg struct {
	job  *v1.Job
	note string
}

// scanFilter is the filters of a catalog listing of scans, as a batch takes
// its targets from a search.
type scanFilter struct {
	modality, description, receiver string
	since, until                    *timestamppb.Timestamp
}

// parseScanFilter reads what a person typed for a filter: space-separated
// key=value pairs, each key once, among modality, description-prefix,
// receiver, archived-since and archived-until (a date, or a date and time as
// 2006-01-02T15:04). An empty text is no filter.
func parseScanFilter(text string, loc *time.Location) (*scanFilter, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	f := &scanFilter{}
	seen := map[string]bool{}
	for _, part := range strings.Fields(text) {
		k, v, ok := strings.Cut(part, "=")
		if !ok || v == "" || seen[k] {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not key=value, or its key is given twice", clean(part)))
		}
		seen[k] = true
		switch k {
		case "modality":
			f.modality = v
		case "description-prefix":
			f.description = v
		case "receiver":
			f.receiver = v
		case "archived-since", "archived-until":
			t, err := parseFilterTime(v, loc)
			if err != nil {
				return nil, fault.New(fault.Invalid, k+": "+fault.Message(err))
			}
			if k == "archived-since" {
				f.since = timestamppb.New(t)
			} else {
				f.until = timestamppb.New(t)
			}
		default:
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not a filter: use modality, description-prefix, receiver, archived-since or archived-until", clean(k)))
		}
	}
	if f.since != nil && f.until != nil && !f.until.AsTime().After(f.since.AsTime()) {
		return nil, fault.New(fault.Invalid, "archived-until must be after archived-since")
	}
	return f, nil
}

// parseFilterTime reads a date, or a date and time, in the person's zone.
func parseFilterTime(s string, loc *time.Location) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fault.New(fault.Invalid, fmt.Sprintf("%q is not a date as 2006-01-02 or 2006-01-02T15:04", clean(s)))
}

// filteredTargets lists every scan the filter selects, page after page, as
// batch targets.
func filteredTargets(ctx context.Context, cl *client.Client, tenant string, f *scanFilter) ([]*v1.CorrectionTarget, error) {
	order := v1.CatalogOrder_CATALOG_ORDER_UNSPECIFIED
	if f.description != "" {
		order = v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION
	}
	var out []*v1.CorrectionTarget
	token := ""
	for {
		res, err := client.Call(ctx, cl, "catalog scans", func(ctx context.Context) (*v1.ListScansResponse, error) {
			return cl.Catalog.ListScans(ctx, &v1.ListScansRequest{Tenant: tenant, Modality: f.modality, SeriesDescriptionPrefix: f.description,
				Receiver: f.receiver, StartTime: f.since, EndTime: f.until, Order: order, PageSize: pageSize, PageToken: token})
		})
		if err != nil {
			return nil, err
		}
		for _, s := range res.GetScans() {
			out = append(out, &v1.CorrectionTarget{SubjectLabel: s.GetSubjectLabel(), SessionLabel: s.GetSessionLabel(), ScanLabel: s.GetLabel()})
		}
		if len(out) > maxFilteredScans {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("the filter selects more than %d scans: narrow it", maxFilteredScans))
		}
		if token = res.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(out) == 0 {
		return nil, fault.New(fault.NotFound, "no scan of the tenant matches the filter")
	}
	return out, nil
}

// previewBatch asks what a batch delete would remove and for the token it must
// carry; nothing is deleted. The targets are those given, or the scans a
// filter selects.
func previewBatch(c *ctx, owner int, tenant, reason string, targets []*v1.CorrectionTarget, filter *scanFilter) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		ts := targets
		if filter != nil {
			var err error
			if ts, err = filteredTargets(ctx, cl, tenant, filter); err != nil {
				return failMsg{err}
			}
		}
		req := &v1.DeleteBatchRequest{Tenant: tenant, Targets: ts, Reason: reason}
		preview := &v1.DeleteBatchRequest{Tenant: tenant, Targets: ts, Reason: reason, ValidateOnly: true}
		res, err := client.Call(ctx, cl, "correction delete-batch", func(ctx context.Context) (*v1.DeleteBatchResponse, error) {
			return cl.Corrections.DeleteBatch(ctx, preview)
		})
		return answer(res, err, func(r *v1.DeleteBatchResponse) tea.Msg { return batchPreviewMsg{prev: r.GetPreview(), req: req} })
	})
}

// startBatch makes the job a batch preview gave its token for.
func startBatch(c *ctx, owner int, req *v1.DeleteBatchRequest, token string) tea.Cmd {
	cl := c.client
	real := &v1.DeleteBatchRequest{Tenant: req.GetTenant(), Targets: req.GetTargets(), Reason: req.GetReason(), Confirm: token}
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction delete-batch", func(ctx context.Context) (*v1.DeleteBatchResponse, error) {
			return cl.Corrections.DeleteBatch(ctx, real)
		})
		return answer(res, err, func(r *v1.DeleteBatchResponse) tea.Msg { return batchStartedMsg{job: r.GetJob()} })
	})
}

// batchTargetsPage asks for one page of a batch job's targets, in the order
// given, each with its outcome.
func batchTargetsPage(c *ctx, owner, gen int, token, tenant, job string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "correction batch-targets", func(ctx context.Context) (*v1.ListDeleteBatchTargetsResponse, error) {
			return cl.Corrections.ListDeleteBatchTargets(ctx, &v1.ListDeleteBatchTargetsRequest{Tenant: tenant, JobId: job, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListDeleteBatchTargetsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetTargets()))
			for _, t := range r.GetTargets() {
				rows = append(rows, row{cells: []string{itoa(t.GetPosition()), targetWord(t.GetTarget()), correctionWord(t.GetOutcome().String(), batchOutcomePrefix),
					output.Dash(t.GetCorrectionId()), itoa(t.GetInstances())}, ref: t})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// pauseJob pauses a batch delete job, or with resume lets it go on.
func pauseJob(c *ctx, owner int, tenant, id string, resume bool) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		if resume {
			res, err := client.Call(ctx, cl, "job resume", func(ctx context.Context) (*v1.ResumeJobResponse, error) {
				return cl.Jobs.ResumeJob(ctx, &v1.ResumeJobRequest{Tenant: tenant, Id: id})
			})
			return answer(res, err, func(r *v1.ResumeJobResponse) tea.Msg {
				return jobPausedMsg{job: r.GetJob(), note: "job " + r.GetJob().GetId() + " is running again"}
			})
		}
		res, err := client.Call(ctx, cl, "job pause", func(ctx context.Context) (*v1.PauseJobResponse, error) {
			return cl.Jobs.PauseJob(ctx, &v1.PauseJobRequest{Tenant: tenant, Id: id})
		})
		return answer(res, err, func(r *v1.PauseJobResponse) tea.Msg {
			return jobPausedMsg{job: r.GetJob(), note: "job " + r.GetJob().GetId() + " is paused; the target being deleted finishes"}
		})
	})
}

// maxPreviewItems is how many targets of a batch's preview are drawn; the rest
// are counted.
const maxPreviewItems = 200

// batchPreviewLines are the labeled lines of a batch delete's preview: its
// totals, then one line per target with what keeps it from being deleted.
func batchPreviewLines(c *ctx, ref any) []pair {
	m, _ := ref.(batchPreviewMsg)
	p, n := m.prev, m.prev.GetCounts()
	ps := []pair{
		{"Targets", itoa(int64(len(p.GetItems())))},
		{"Held (skipped)", itoa(p.GetTargetsHeld())},
		{"Kept by a rule (skipped)", itoa(p.GetTargetsKept())},
		{"Instances", itoa(n.GetInstances())},
		{"Bytes", output.Bytes(n.GetBytes())},
		{"Purged after", output.Time(p.GetPurgeTime(), c.loc) + " (each from its own delete)"},
		{"Confirm token", p.GetConfirm()},
	}
	for i, it := range p.GetItems() {
		if i == maxPreviewItems {
			ps = append(ps, pair{"More", itoa(int64(len(p.GetItems())-i)) + " targets not drawn here"})
			break
		}
		v := itoa(it.GetCounts().GetInstances()) + " instances, " + output.Bytes(it.GetCounts().GetBytes())
		if len(it.GetHolds()) > 0 {
			v += "; HELD by " + strings.Join(it.GetHolds(), ", ")
		}
		if it.GetSessionsUnderMinimum() > 0 {
			v += "; KEPT by a retention rule until " + output.Time(it.GetKeepUntil(), c.loc)
		}
		if len(it.GetReplicas()) > 0 {
			v += "; replicas " + strings.Join(it.GetReplicas(), ", ")
		}
		if len(it.GetSentTo()) > 0 {
			v += "; sent to " + strings.Join(it.GetSentTo(), ", ") + ", out of reach"
		}
		ps = append(ps, pair{targetWord(it.GetTarget()), v})
	}
	return ps
}
