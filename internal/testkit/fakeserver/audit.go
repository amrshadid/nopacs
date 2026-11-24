package fakeserver

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// auditHandler implements AuditService.
type auditHandler struct{ s *Server }

// ListAuditEntries answers a page of the entries added with AddAudit that
// match the scope and the filters, newest first as they were added.
func (h auditHandler) ListAuditEntries(_ context.Context, req *v1.ListAuditEntriesRequest) (*v1.ListAuditEntriesResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant, installation, err := scopeOf(req.GetScope())
	if err != nil {
		return nil, err
	}
	if !installation {
		if err := s.checkTenant(tenant); err != nil {
			return nil, err
		}
	}
	if req.GetStartTime() != nil && req.GetEndTime() != nil && !req.GetEndTime().AsTime().After(req.GetStartTime().AsTime()) {
		return nil, fault.New(fault.Invalid, "end_time must be after start_time")
	}
	var entries []*v1.AuditEntry
	for _, e := range s.audit {
		switch {
		case e.GetTenant() != tenant:
		case req.GetKeyId() != "" && e.GetKeyId() != req.GetKeyId():
		case req.GetOutcome() != v1.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED && e.GetOutcome() != req.GetOutcome():
		case req.GetStartTime() != nil && e.GetTime().AsTime().Before(req.GetStartTime().AsTime()):
		case req.GetEndTime() != nil && !e.GetTime().AsTime().Before(req.GetEndTime().AsTime()):
		default:
			entries = append(entries, e)
		}
	}
	from, to, next, err := pageOf(len(entries), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListAuditEntriesResponse{Entries: entries[from:to], NextPageToken: next}, nil
}
