package api

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/master"
)

// auditService is AuditService: the installation's log from the master, a
// tenant's from its own store.
type auditService struct{ c *core }

var _ nopacsv1connect.AuditServiceHandler = auditService{}

// filterTime returns a filter's time, the zero time for an absent one, or an
// Invalid error naming field for a timestamp that is not valid or that the
// logs cannot hold (master.ValidateTime: the years 1970 to 9999).
func filterTime(field string, ts *timestamppb.Timestamp) (time.Time, error) {
	if ts == nil {
		return time.Time{}, nil
	}
	if ts.CheckValid() != nil || master.ValidateTime(field, ts.AsTime()) != nil {
		return time.Time{}, invalid(field, "is a time between the years 1970 and 9999")
	}
	return ts.AsTime(), nil
}

// ListAuditEntries answers a page of one log, newest first, filtered by key,
// outcome and time. The listing records itself in the log it read.
func (s auditService) ListAuditEntries(ctx context.Context, req *v1.ListAuditEntriesRequest) (*v1.ListAuditEntriesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.AuditServiceListAuditEntriesProcedure)
	if err != nil {
		return nil, err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	// Every record of the request from here on goes to the log of its scope.
	st, err := c.sinkFor(ctx, d, scope, auditFilterDetails(req))
	if err != nil {
		return nil, err
	}
	var filter master.AuditFilter
	if req.GetKeyId() != "" {
		if err := keyID("key_id", req.GetKeyId()); err != nil {
			return nil, c.failIn(d, st, err)
		}
		filter.KeyID = req.GetKeyId()
	}
	outcome, ok := masterOutcome(req.GetOutcome())
	if !ok {
		return nil, c.failIn(d, st, invalid("outcome", "is done, refused, failed or unspecified for every outcome"))
	}
	filter.Outcome = outcome
	if filter.Since, err = filterTime("start_time", req.GetStartTime()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if filter.Until, err = filterTime("end_time", req.GetEndTime()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if !filter.Since.IsZero() && !filter.Until.IsZero() && !filter.Until.After(filter.Since) {
		return nil, c.failIn(d, st, invalid("end_time", "is after start_time"))
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}

	var entries []master.AuditEntry
	var next string
	if st == nil {
		entries, next, err = c.master.ListAudit(ctx, filter, page)
		if err != nil {
			return nil, c.fail(d, listError(err, page.Cursor))
		}
	} else {
		entries, next, err = st.ListAudit(ctx, filter, page)
		if err != nil {
			return nil, c.failIn(d, st, listError(err, page.Cursor))
		}
	}
	c.done(d, st)
	out := &v1.ListAuditEntriesResponse{Entries: make([]*v1.AuditEntry, 0, len(entries)), NextPageToken: next}
	for _, e := range entries {
		out.Entries = append(out.Entries, entryMessage(e))
	}
	return out, nil
}
