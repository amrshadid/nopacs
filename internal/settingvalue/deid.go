package settingvalue

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// BaseProfile returns deid.base-profile as it applies on the tenant: the
// name and version of the tenant's base profile, and whether it has one.
func (t Tenant) BaseProfile(ctx context.Context) (name string, version int32, ok bool, err error) {
	v, err := t.r.Tenant(ctx, t.st, settings.DeidBaseProfile)
	if err != nil {
		return "", 0, false, err
	}
	name, version, perr := settings.ParseBaseProfile(v)
	if perr != nil {
		return "", 0, false, errBroken("tenant's ", settings.DeidBaseProfile)
	}
	return name, version, name != "", nil
}
