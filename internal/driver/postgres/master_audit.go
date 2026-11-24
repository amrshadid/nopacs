package postgres

import (
	"context"

	"github.com/amrshadid/nopacs/internal/master"
)

// AppendAudit implements master.Store.
func (m *Master) AppendAudit(ctx context.Context, entries ...master.AuditEntry) error {
	return appendAudit(ctx, &m.base, entries)
}

// ListAudit implements master.Store. The cursor is bound to the whole filter.
func (m *Master) ListAudit(ctx context.Context, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error) {
	return listAuditPage(ctx, &m.base, masterLists, filter, page)
}
