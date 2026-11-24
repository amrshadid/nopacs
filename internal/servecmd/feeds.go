package servecmd

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/events"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// keptFeeds is the events Keeper's view of the installation: the tenants'
// names from the master, each one's feed from the registry, and its
// retention from the setting events.retention as it applies to the tenant.
type keptFeeds struct {
	tenants  registryTenants
	registry *registry.Registry
	values   settingvalue.Reader
}

var _ events.KeptTenants = keptFeeds{}

// TenantNames implements events.KeptTenants.
func (k keptFeeds) TenantNames(ctx context.Context) ([]string, error) { return k.tenants.Names(ctx) }

// errNoFeed is a tenant whose database keeps no event feed.
var errNoFeed = fault.New(fault.Unavailable, "the tenant's database keeps no event feed")

// Feed implements events.KeptTenants: the tenant's store, when it keeps an
// event feed.
func (k keptFeeds) Feed(_ context.Context, name string) (events.Feed, error) {
	st, err := k.registry.Store(name)
	if err != nil {
		return nil, err
	}
	feed, ok := st.(tenant.Events)
	if !ok {
		return nil, errNoFeed
	}
	return feed, nil
}

// Retention implements events.KeptTenants.
func (k keptFeeds) Retention(ctx context.Context, name string) (time.Duration, error) {
	st, err := k.registry.Store(name)
	if err != nil {
		return 0, err
	}
	v, err := k.values.Tenant(ctx, st, settings.EventsRetention)
	if err != nil {
		return 0, err
	}
	return settings.ParseRetention(v)
}

// runners starts several runners in order and stops them in the reverse
// order, so what started last, and may depend on the others, stops first.
type runners []Runner

// Start starts each runner; when one fails, those already started are
// stopped and its error returned.
func (rs runners) Start(ctx context.Context) error {
	for i, r := range rs {
		if err := r.Start(ctx); err != nil {
			return errors.Join(err, rs[:i].Stop(ctx))
		}
	}
	return nil
}

// Stop stops every runner, the last started first, and joins their errors.
func (rs runners) Stop(ctx context.Context) error {
	var errs []error
	for i := len(rs) - 1; i >= 0; i-- {
		errs = append(errs, rs[i].Stop(ctx))
	}
	return errors.Join(errs...)
}

// closer is a runner that only closes something at stop, last of the
// runners it is listed first among: the broadcaster, whose open streams end
// when the server stops.
type closer struct{ c interface{ Close() error } }

// Start does nothing.
func (closer) Start(context.Context) error { return nil }

// Stop closes it.
func (c closer) Stop(context.Context) error { return c.c.Close() }
