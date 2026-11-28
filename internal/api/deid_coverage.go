package api

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// GetCoverage answers a page of the tenant's coverage report: the elements
// no rule of a profile named, with their counts, and what the report was
// made from. It holds no value of any file; a private creator is given only
// where the engine judged it a vendor's name.
func (s deidService) GetCoverage(ctx context.Context, req *v1.GetCoverageRequest) (*v1.GetCoverageResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.DeidServiceGetCoverageProcedure)
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
	out := &v1.GetCoverageResponse{}
	if running, ok, err := p.DeidCoverageInProgress(ctx); err != nil {
		return nil, c.failIn(d, st, err)
	} else if ok {
		out.Refreshing = c.newJobViews(p, req.GetTenant()).message(ctx, running)
	}
	rep, found, err := p.DeidCoverageReport(ctx)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if !found {
		if page.Cursor != "" {
			return nil, c.failIn(d, st, invalid("page_token", "is not one this list returned for these arguments"))
		}
		c.done(d, st)
		return out, nil
	}
	var after *tenant.CoverageKey
	if page.Cursor != "" {
		last, err := readAfterToken(page.Cursor, tokenCoverage, req.GetTenant()+":"+rep.JobID)
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		if after, err = coverageKeyOf(last); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	items, err := p.DeidCoverageElements(ctx, rep.JobID, after, page.Limit+1)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if len(items) > page.Limit {
		items = items[:page.Limit]
		out.NextPageToken = afterToken(tokenCoverage, req.GetTenant()+":"+rep.JobID, coverageKeyText(items[len(items)-1].CoverageKey))
	}
	c.done(d, st)
	out.Report = &v1.CoverageReport{
		JobId: rep.JobID, EndTime: timestamppb.New(rep.Ended), ScansRead: rep.Scans, ScansSkipped: rep.ScansSkipped,
		Instances: rep.Instances, InstancesRecorded: rep.Recorded, InstancesCapped: rep.Capped, ElementsNotListed: rep.NotListed,
		DistinctElements: rep.Elements,
	}
	out.Elements = make([]*v1.CoverageElement, len(items))
	for i, e := range items {
		out.Elements[i] = coverageElementMessage(e)
	}
	return out, nil
}

// coverageElementMessage returns an element of a report as the contract
// writes it.
func coverageElementMessage(e tenant.CoverageElement) *v1.CoverageElement {
	tag := fmt.Sprintf("%04X,%04X", e.Tag>>16, e.Tag&0xFFFF)
	if e.Private() {
		tag = fmt.Sprintf("%04X,xx%02X", e.Tag>>16, e.Tag&0xFF)
	}
	return &v1.CoverageElement{Tag: tag, Private: e.Private(), PrivateCreator: e.Creator, CreatorWithheld: e.Withheld, Vr: e.VR,
		Instances: e.Instances, Occurrences: e.Occurrences}
}

// coverageKeyText writes a key as a page token holds it: the tag, the
// withheld flag, the VR and the creator in hexadecimal, which holds no "/".
func coverageKeyText(k tenant.CoverageKey) string {
	w := "0"
	if k.Withheld {
		w = "1"
	}
	return fmt.Sprintf("%08X.%s.%s.%s", k.Tag, w, k.VR, hex.EncodeToString([]byte(k.Creator))+"x")
}

// coverageKeyOf reads a key coverageKeyText wrote, or the API's Invalid for
// a token that is not one.
func coverageKeyOf(s string) (*tenant.CoverageKey, error) {
	bad := invalid("page_token", "is not one this list returned for these arguments")
	parts := strings.Split(s, ".")
	if len(parts) != 4 || len(parts[0]) != 8 || (parts[1] != "0" && parts[1] != "1") || !strings.HasSuffix(parts[3], "x") {
		return nil, bad
	}
	tag, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return nil, bad
	}
	creator, err := hex.DecodeString(strings.TrimSuffix(parts[3], "x"))
	if err != nil {
		return nil, bad
	}
	k := tenant.CoverageKey{Tag: uint32(tag), Creator: string(creator), Withheld: parts[1] == "1", VR: parts[2]}
	if k.Validate() != nil {
		return nil, bad
	}
	return &k, nil
}

// RefreshCoverage makes the job that builds the coverage report again. The
// job's runner is the server's: it reads each scan's metadata file and writes
// the tenant's database only.
func (s deidService) RefreshCoverage(ctx context.Context, req *v1.RefreshCoverageRequest) (*v1.RefreshCoverageResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.DeidServiceRefreshCoverageProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failWith(d, st, facts, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	facts.Object = id
	facts.Details = map[string]string{detailJob: id}
	job, err := p.QueueDeidCoverage(ctx, tenant.DeidCoverageQueue{ID: id, KeyID: callerKeyID(d), Now: c.now()}, changeEntry(d, facts), c.roleGuard(d, master.RoleOperator))
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.RefreshCoverageResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, job)}, nil
}
