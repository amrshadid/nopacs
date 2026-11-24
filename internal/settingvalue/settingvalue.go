package settingvalue

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Source answers the value stored for a setting: master.Store for the
// installation, tenant.Store for a tenant.
type Source interface {
	// Setting returns the value stored for name, NotFound when none is.
	Setting(ctx context.Context, name string) (string, error)
}

// Reader reads the value of a setting that applies. Its zero Table is the
// server's built-in list.
type Reader struct {
	// Table is the list of settings the server knows; nil is
	// settings.MustBuiltin.
	Table *settings.Table
	// Master is the installation's store of settings, the master database.
	Master Source
}

// table returns the reader's list of settings.
func (r Reader) table() *settings.Table {
	if r.Table == nil {
		return builtin
	}
	return r.Table
}

// builtin is the server's own list, made once: it never changes.
var builtin = settings.MustBuiltin()

// definition returns the setting named name, or an Internal fault: a name
// the server reads that its own list does not hold is a mistake of the code.
func (r Reader) definition(name string) (settings.Definition, error) {
	def, ok := r.table().Lookup(name)
	if !ok {
		return settings.Definition{}, fault.New(fault.Internal, "settings: the server's list of settings has no "+name)
	}
	return def, nil
}

// errBroken is the fault of a stored value its rule refuses: every value is
// checked before it is stored, so one that fails now is the server's fault.
func errBroken(where, name string) error {
	return fault.New(fault.Internal, "settings: the "+where+"stored "+name+" does not follow its rule")
}

// Installation returns the value of the setting named name that applies on
// the installation: its stored value, else the built-in default.
func (r Reader) Installation(ctx context.Context, name string) (string, error) {
	def, err := r.definition(name)
	if err != nil {
		return "", err
	}
	return r.onInstallation(ctx, def)
}

// onInstallation is Installation for a definition already looked up.
func (r Reader) onInstallation(ctx context.Context, def settings.Definition) (string, error) {
	if r.Master == nil {
		return "", fault.New(fault.Internal, "settings: the reader is given no installation store")
	}
	v, err := r.Master.Setting(ctx, def.Name)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		v = def.Default
	case err != nil:
		return "", err
	}
	if def.Validate(v) != nil {
		return "", errBroken("", def.Name)
	}
	return v, nil
}

// Tenant returns the value of the setting named name that applies on the
// tenant whose store is st: its own, else the installation's (none for a
// setting with no installation value), else the built-in default.
func (r Reader) Tenant(ctx context.Context, st Source, name string) (string, error) {
	def, err := r.definition(name)
	if err != nil {
		return "", err
	}
	v, err := st.Setting(ctx, name)
	switch {
	case fault.KindOf(err) == fault.NotFound && def.TenantOnly:
		v = def.Default
	case fault.KindOf(err) == fault.NotFound:
		return r.onInstallation(ctx, def)
	case err != nil:
		return "", err
	}
	if def.Validate(v) != nil {
		return "", errBroken("tenant's ", name)
	}
	return v, nil
}

// typed parses v, the value read of the setting named name (or the error
// of reading it), with parse; a value read that does not parse is the
// server's fault.
func typed[T any](name string, v string, err error, parse func(string) (T, error)) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}
	out, err := parse(v)
	if err != nil {
		return zero, errBroken("", name)
	}
	return out, nil
}

// DirectoryRoots returns the installation's storage.directory-roots.
func (r Reader) DirectoryRoots(ctx context.Context) (settings.DirectoryRoots, error) {
	v, err := r.Installation(ctx, settings.StorageDirectoryRoots)
	return typed(settings.StorageDirectoryRoots, v, err, settings.ParseDirectoryRoots)
}

// S3Endpoints returns the installation's storage.s3-endpoints.
func (r Reader) S3Endpoints(ctx context.Context) (settings.S3Endpoints, error) {
	v, err := r.Installation(ctx, settings.StorageS3Endpoints)
	return typed(settings.StorageS3Endpoints, v, err, settings.ParseS3Endpoints)
}

// BindAddresses returns the installation's receiver.bind-addresses.
func (r Reader) BindAddresses(ctx context.Context) (settings.BindAddresses, error) {
	v, err := r.Installation(ctx, settings.ReceiverBindAddresses)
	return typed(settings.ReceiverBindAddresses, v, err, settings.ParseBindAddresses)
}

// Reserve returns the installation's storage.reserve.
func (r Reader) Reserve(ctx context.Context) (settings.Reserve, error) {
	v, err := r.Installation(ctx, settings.StorageReserve)
	return typed(settings.StorageReserve, v, err, settings.ParseReserve)
}

// HeaderLimit returns the installation's receiver.header-limit in bytes.
func (r Reader) HeaderLimit(ctx context.Context) (int64, error) {
	v, err := r.Installation(ctx, settings.ReceiverHeaderLimit)
	return typed(settings.ReceiverHeaderLimit, v, err, settings.ParseHeaderLimit)
}

// DeidMemory returns the installation's deid.memory in bytes.
func (r Reader) DeidMemory(ctx context.Context) (int64, error) {
	v, err := r.Installation(ctx, settings.DeidMemory)
	return typed(settings.DeidMemory, v, err, settings.ParseDeidMemory)
}

// CORSOrigins returns the installation's api.cors-origins: the web origins
// whose pages may call the API from a browser.
func (r Reader) CORSOrigins(ctx context.Context) (settings.Origins, error) {
	v, err := r.Installation(ctx, settings.APICORSOrigins)
	return typed(settings.APICORSOrigins, v, err, settings.ParseOrigins)
}

// SenderDestinations returns the installation's sender.destinations: where
// a tenant's senders may send.
func (r Reader) SenderDestinations(ctx context.Context) (settings.Destinations, error) {
	v, err := r.Installation(ctx, settings.SenderDestinations)
	return typed(settings.SenderDestinations, v, err, settings.ParseDestinations)
}

// AllowPlainHTTP returns the installation's sender.allow-plain-http.
func (r Reader) AllowPlainHTTP(ctx context.Context) (bool, error) {
	v, err := r.Installation(ctx, settings.SenderAllowPlainHTTP)
	return typed(settings.SenderAllowPlainHTTP, v, err, settings.ParseBool)
}

// MaxAssociations returns the installation's sender.max-associations: the
// most associations and requests all senders of all tenants hold open at
// once.
func (r Reader) MaxAssociations(ctx context.Context) (int, error) {
	v, err := r.Installation(ctx, settings.SenderMaxAssociations)
	return typed(settings.SenderMaxAssociations, v, err, settings.ParseAssociations)
}

// ReadLimits returns the installation's read.per-caller and
// read.per-server: the most reads of archived bytes one caller, and the
// whole server, hold open at once.
func (r Reader) ReadLimits(ctx context.Context) (perCaller, perServer int, err error) {
	v, err := r.Installation(ctx, settings.ReadPerCaller)
	if perCaller, err = typed(settings.ReadPerCaller, v, err, settings.ParseReadPerCaller); err != nil {
		return 0, 0, err
	}
	v, err = r.Installation(ctx, settings.ReadPerServer)
	if perServer, err = typed(settings.ReadPerServer, v, err, settings.ParseReadPerServer); err != nil {
		return 0, 0, err
	}
	return perCaller, perServer, nil
}

// AuditSealInterval returns the installation's audit.seal-interval.
func (r Reader) AuditSealInterval(ctx context.Context) (time.Duration, error) {
	v, err := r.Installation(ctx, settings.AuditSealInterval)
	return typed(settings.AuditSealInterval, v, err, settings.ParseSealInterval)
}

// AuditRetention returns the installation's audit.retention; 0 keeps every
// entry.
func (r Reader) AuditRetention(ctx context.Context) (time.Duration, error) {
	v, err := r.Installation(ctx, settings.AuditRetention)
	return typed(settings.AuditRetention, v, err, settings.ParseAuditRetention)
}

// OnTenant returns the reader of the settings that apply on the tenant
// whose store is st.
func (r Reader) OnTenant(st Source) Tenant { return Tenant{r: r, st: st} }

// Tenant reads the settings that apply on one tenant.
type Tenant struct {
	r  Reader
	st Source
}

// Ports returns receiver.ports as it applies on the tenant.
func (t Tenant) Ports(ctx context.Context) (settings.Ports, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ReceiverPorts)
	return typed(settings.ReceiverPorts, v, err, settings.ParsePorts)
}

// BindAddresses returns the installation's receiver.bind-addresses, which
// no tenant overrides.
func (t Tenant) BindAddresses(ctx context.Context) (settings.BindAddresses, error) {
	return t.r.BindAddresses(ctx)
}

// IncomingLimit returns incoming.limit as it applies on the tenant, in
// bytes; 0 is no limit.
func (t Tenant) IncomingLimit(ctx context.Context) (int64, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.IncomingLimit)
	return typed(settings.IncomingLimit, v, err, settings.ParseSize)
}

// AllowAsReceived returns receiver.allow-as-received as it applies on the
// tenant: whether a receiver of it may archive as received.
func (t Tenant) AllowAsReceived(ctx context.Context) (bool, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ReceiverAllowAsReceived)
	return typed(settings.ReceiverAllowAsReceived, v, err, settings.ParseBool)
}

// PixelLimit returns deid.pixel-limit as it applies on the tenant, in
// bytes of an image decoded.
func (t Tenant) PixelLimit(ctx context.Context) (int64, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.DeidPixelLimit)
	return typed(settings.DeidPixelLimit, v, err, settings.ParsePixelLimit)
}

// Networks returns the installation's value of a setting that lists
// networks (access.trusted-proxies, access.allowed-networks,
// access.denied-networks).
func (r Reader) Networks(ctx context.Context, name string) (settings.Networks, error) {
	v, err := r.Installation(ctx, name)
	return typed(name, v, err, settings.ParseNetworks)
}

// ForwardedHeader returns the installation's access.forwarded-header.
func (r Reader) ForwardedHeader(ctx context.Context) (settings.ForwardedHeader, error) {
	v, err := r.Installation(ctx, settings.AccessForwardedHeader)
	return typed(settings.AccessForwardedHeader, v, err, settings.ParseForwardedHeader)
}

// FailureLimit returns the installation's access.failure-limit.
func (r Reader) FailureLimit(ctx context.Context) (int, error) {
	v, err := r.Installation(ctx, settings.AccessFailureLimit)
	return typed(settings.AccessFailureLimit, v, err, settings.ParseFailureLimit)
}

// FailureWindow returns the installation's access.failure-window.
func (r Reader) FailureWindow(ctx context.Context) (time.Duration, error) {
	v, err := r.Installation(ctx, settings.AccessFailureWindow)
	return typed(settings.AccessFailureWindow, v, err, settings.ParseFailureWindow)
}

// BlockTime returns the installation's access.block-time.
func (r Reader) BlockTime(ctx context.Context) (time.Duration, error) {
	v, err := r.Installation(ctx, settings.AccessBlockTime)
	return typed(settings.AccessBlockTime, v, err, settings.ParseBlockTime)
}

// KeyMaxLifetime returns the installation's key.max-lifetime.
func (r Reader) KeyMaxLifetime(ctx context.Context) (time.Duration, error) {
	v, err := r.Installation(ctx, settings.KeyMaxLifetime)
	return typed(settings.KeyMaxLifetime, v, err, settings.ParseKeyLifetime)
}

// KeyExpiryWarning returns the installation's key.expiry-warning.
func (r Reader) KeyExpiryWarning(ctx context.Context) (time.Duration, error) {
	v, err := r.Installation(ctx, settings.KeyExpiryWarning)
	return typed(settings.KeyExpiryWarning, v, err, settings.ParseExpiryWarning)
}

// TenantNetworks returns access.tenant-networks as it applies on the
// tenant: its own, else the installation's, else none.
func (t Tenant) TenantNetworks(ctx context.Context) (settings.Networks, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.AccessTenantNetworks)
	return typed(settings.AccessTenantNetworks, v, err, settings.ParseNetworks)
}

// MaxAssociations returns sender.tenant-max-associations as it applies on
// the tenant: the most associations and requests its senders hold open at
// once.
func (t Tenant) MaxAssociations(ctx context.Context) (int, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.SenderTenantMaxAssociations)
	return typed(settings.SenderTenantMaxAssociations, v, err, settings.ParseAssociations)
}

// ScrubRate returns scrub.rate as it applies on the tenant: the most bytes a
// second a scrub reads from one storage.
func (t Tenant) ScrubRate(ctx context.Context) (int64, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ScrubRate)
	return typed(settings.ScrubRate, v, err, settings.ParseScrubRate)
}

// ScrubInterval returns scrub.interval as it applies on the tenant: how
// often the server makes a scrub of each storage by itself, zero for never.
func (t Tenant) ScrubInterval(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ScrubInterval)
	return typed(settings.ScrubInterval, v, err, settings.ParseScrubInterval)
}

// DeleteGrace returns delete.grace as it applies on the tenant: how long a
// delete's bytes stay on the storage, unserved, before they are purged.
func (t Tenant) DeleteGrace(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.DeleteGrace)
	return typed(settings.DeleteGrace, v, err, settings.ParseDeleteGrace)
}

// FailuresKept returns job.failures-kept as it applies on the tenant: how
// many failures a job lists.
func (t Tenant) FailuresKept(ctx context.Context) (int, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.JobFailuresKept)
	return typed(settings.JobFailuresKept, v, err, settings.ParseFailuresKept)
}
