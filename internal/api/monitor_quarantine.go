package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
	"google.golang.org/protobuf/types/known/durationpb"
)

// quarantineStatus fills a tenant's status with what it holds in quarantine
// and whether that is past quarantine.warn-age or quarantine.warn-bytes. It
// is measured at the request, from the held files themselves, so a status
// warns the moment a limit is passed, whether or not the server's periodic
// check has run since. A part that cannot be read is left out: a warning
// that cannot be measured is not claimed absent.
func (c *core) quarantineStatus(ctx context.Context, st tenant.Store, ts *v1.TenantStatus) {
	r, ok := st.(tenant.Retention)
	if !ok {
		return
	}
	aging, err := r.QuarantineAging(ctx)
	if err != nil {
		return
	}
	v := c.values().OnTenant(st)
	age, err := v.QuarantineWarnAge(ctx)
	if err != nil {
		return
	}
	bytes, err := v.QuarantineWarnBytes(ctx)
	if err != nil {
		return
	}
	w := aging.Warning(c.now(), age, bytes)
	ts.Quarantine = &v1.QuarantineWarnings{OldestAge: durationpb.New(w.OldestAge), HeldFiles: w.Files, HeldBytes: w.Bytes,
		AgePassed: w.AgePassed, BytesPassed: w.BytesPassed}
}
