package settingvalue

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// TransferWait returns transfer.wait as it applies on the tenant.
func (t Tenant) TransferWait(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.TransferWait)
	return typed(settings.TransferWait, v, err, settings.ParseTransferWait)
}
