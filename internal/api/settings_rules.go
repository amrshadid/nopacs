package api

import (
	"context"
	"net/netip"
	"strconv"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// placeSettings are the settings that say where the installation lets data
// go: a change of one is checked against what every tenant's storages and
// receivers use, under the write lock of core.places.
var placeSettings = map[string]bool{
	settings.StorageDirectoryRoots: true,
	settings.StorageS3Endpoints:    true,
	settings.ReceiverBindAddresses: true,
	settings.ReceiverPorts:         true,
	settings.SenderDestinations:    true,
	settings.SenderAllowPlainHTTP:  true,
	settings.ImportRoots:           true,
	settings.ImportAllowedRoots:    true,
}

// checkSettingValue applies the rules of a setting's value that need more
// than its own text (settings.Definition.Validate has that), which package
// place holds: each root's path absolute on this server's system and no two
// roots nesting once their symbolic links are followed (place.CheckRoots),
// and each S3 endpoint one a storage could use, with no two entries spelling
// one endpoint (place.CheckEndpoints). A refusal of the text is Invalid,
// naming field; one that depends on what is on the server's disk is a
// precondition.
func checkSettingValue(field, name, value string) error {
	var err error
	switch name {
	case settings.StorageDirectoryRoots:
		roots, perr := settings.ParseDirectoryRoots(value)
		if perr != nil {
			return invalid(field, perr.Error())
		}
		err = place.CheckRoots(roots)
	case settings.StorageS3Endpoints:
		entries, perr := settings.ParseS3Endpoints(value)
		if perr != nil {
			return invalid(field, perr.Error())
		}
		err = place.CheckEndpoints(entries)
	}
	switch fault.KindOf(err) {
	case fault.Invalid:
		return invalid(field, fault.Message(err))
	case fault.Precondition:
		return fault.Wrap(fault.Precondition, err, field+": "+fault.Message(err))
	}
	return err
}

// checkReadBounds refuses, as a precondition naming the setting, a value of
// read.per-caller or read.per-server after which one caller could hold more
// reads open than the server does. value is what would apply after the
// change; the other setting is read as it applies now. Any other name
// passes.
func (c *core) checkReadBounds(ctx context.Context, name, value string) error {
	if name != settings.ReadPerCaller && name != settings.ReadPerServer {
		return nil
	}
	perCaller, perServer, err := c.values().ReadLimits(ctx)
	if err != nil {
		return err
	}
	var perr error
	if name == settings.ReadPerCaller {
		perCaller, perr = settings.ParseReadPerCaller(value)
	} else {
		perServer, perr = settings.ParseReadPerServer(value)
	}
	if perr != nil {
		return invalid("setting.value", perr.Error())
	}
	if perCaller > perServer {
		return precondition(name + ": " + settings.ReadPerCaller + " (" + strconv.Itoa(perCaller) + ") may not exceed " + settings.ReadPerServer + " (" + strconv.Itoa(perServer) + "); change the other one first")
	}
	return nil
}

// takenAway refuses, as a precondition, a value of a place setting that
// would take away what a tenant's storage or receiver uses, or a tenant's
// ports that overlap another tenant's; value is what would apply on scope
// after the change. A tenant that is not ready cannot be read, so whether
// it uses the value cannot be known: the change is refused as Unavailable
// until it is. The caller holds core.places for writing, so no storage or
// receiver is added meanwhile on what is being taken away.
func (c *core) takenAway(ctx context.Context, scope access.Scope, name, value string) error {
	switch name {
	case settings.StorageDirectoryRoots:
		old, err := installationPlaces{c}.DirectoryRoots(ctx)
		if err != nil {
			return err
		}
		next, err := settings.ParseDirectoryRoots(value)
		if err != nil {
			return invalid("setting.value", err.Error())
		}
		return c.eachTenant(ctx, func(name string, p tenant.Pipeline) error {
			storages, err := tenantStorages(ctx, p)
			if err != nil {
				return err
			}
			for _, s := range storages {
				if s.Type != tenant.StorageDirectory {
					continue
				}
				was, _ := old.Lookup(s.Directory.Root)
				now, ok := next.Lookup(s.Directory.Root)
				if !ok || now.Path != was.Path {
					return precondition("the root " + s.Directory.Root + " holds the storage " + s.Name + " of the tenant " + name + "; move or remove the storage before the root is changed or removed")
				}
			}
			return nil
		})
	case settings.StorageS3Endpoints:
		next, err := settings.ParseS3Endpoints(value)
		if err != nil {
			return invalid("setting.value", err.Error())
		}
		return c.eachTenant(ctx, func(name string, p tenant.Pipeline) error {
			storages, err := tenantStorages(ctx, p)
			if err != nil {
				return err
			}
			for _, s := range storages {
				if s.Type != tenant.StorageS3 {
					continue
				}
				if _, err := place.AllowS3(next, s.S3); err != nil {
					return precondition("the S3 storage " + s.Name + " of the tenant " + name + " uses an endpoint this value no longer allows (" + fault.Message(err) + "); move or remove the storage first")
				}
			}
			return nil
		})
	case settings.SenderDestinations:
		next, err := settings.ParseDestinations(value)
		if err != nil {
			return invalid("setting.value", err.Error())
		}
		return c.eachSender(ctx, func(tenantName string, s tenant.Sender) error {
			host, port, _ := senderEndpoint(s)
			if next.Allowed(host, netip.Addr{}, port) != nil {
				return precondition("the sender " + s.Name + " of the tenant " + tenantName + " uses a destination this value no longer allows; move or remove the sender first")
			}
			return nil
		})
	case settings.SenderAllowPlainHTTP:
		allowed, err := settings.ParseBool(value)
		if err != nil {
			return invalid("setting.value", err.Error())
		}
		if allowed {
			return nil
		}
		return c.eachSender(ctx, func(tenantName string, s tenant.Sender) error {
			if _, _, plain := senderEndpoint(s); plain {
				return precondition("the sender " + s.Name + " of the tenant " + tenantName + " sends over plain http; move or remove the sender before plain http is turned off")
			}
			return nil
		})
	case settings.ReceiverBindAddresses:
		next, err := settings.ParseBindAddresses(value)
		if err != nil {
			return invalid("setting.value", err.Error())
		}
		return c.eachTenant(ctx, func(name string, p tenant.Pipeline) error {
			receivers, err := tenantReceivers(ctx, p)
			if err != nil {
				return err
			}
			for _, r := range receivers {
				if r.DIMSE.BindAddress == "" {
					continue
				}
				a, err := netip.ParseAddr(r.DIMSE.BindAddress)
				if err != nil || !next.Contains(a) {
					return precondition("the receiver " + r.Name + " of the tenant " + name + " listens on an address this value no longer lists; move or remove the receiver first")
				}
			}
			return nil
		})
	case settings.ReceiverPorts:
		if scope.IsInstallation() {
			return nil
		}
		return c.portsTakenAway(ctx, scope.Tenant, value)
	case settings.ImportRoots, settings.ImportAllowedRoots:
		return c.importTakenAway(ctx, scope, name, value)
	}
	return nil
}

// portsTakenAway refuses ports for the tenant named name that leave out a
// port its receivers use, or that overlap the ports another tenant holds.
func (c *core) portsTakenAway(ctx context.Context, name, value string) error {
	next, err := settings.ParsePorts(value)
	if err != nil {
		return invalid("setting.value", err.Error())
	}
	return c.eachTenant(ctx, func(other string, p tenant.Pipeline) error {
		if other == name {
			receivers, err := tenantReceivers(ctx, p)
			if err != nil {
				return err
			}
			for _, r := range receivers {
				if r.Type == tenant.ReceiverDIMSE && !next.Contains(r.DIMSE.Port) {
					return precondition("the receiver " + r.Name + " of the tenant " + name + " uses the port " + strconv.Itoa(r.DIMSE.Port) + ", which this value leaves out; move or remove the receiver first")
				}
			}
			return nil
		}
		v, err := p.Setting(ctx, settings.ReceiverPorts)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			return nil
		case err != nil:
			return err
		}
		held, err := settings.ParsePorts(v)
		if err != nil {
			return fault.New(fault.Internal, "api: the stored "+settings.ReceiverPorts+" of a tenant does not follow its rule")
		}
		if port, overlap := next.Overlap(held); overlap {
			return precondition("the port " + strconv.Itoa(port) + " is allotted to the tenant " + other + "; two tenants' receivers never share a port")
		}
		return nil
	})
}

// eachTenant calls fn with the name and pipeline of every tenant the master
// lists, in order of name, and stops at its first error. A tenant removed
// meanwhile is passed over; one that is not ready fails the walk as
// Unavailable, naming it: the caller is an admin on the installation, who
// may list every tenant.
func (c *core) eachTenant(ctx context.Context, fn func(name string, p tenant.Pipeline) error) error {
	page := master.Page{Limit: master.MaxPageLimit}
	for i := 0; ; i++ {
		if i == maxWholeListPages {
			return fault.New(fault.Internal, "api: the master's tenants did not end")
		}
		rows, next, err := c.master.ListTenants(ctx, page)
		if err != nil {
			return err
		}
		for _, row := range rows {
			st, err := c.reg.Store(row.Name)
			switch kind := fault.KindOf(err); {
			case kind == fault.NotFound:
				continue
			case kind == fault.Unavailable:
				return fault.Wrap(fault.Unavailable, err, "the tenant "+row.Name+" is not ready, so whether it uses this value cannot be checked; try again once it is")
			case err != nil:
				return err
			}
			p, err := c.pipeline(st)
			if err != nil {
				return err
			}
			if err := fn(row.Name, p); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// eachSender calls fn with every sender of every tenant the master lists,
// and stops at its first error (eachTenant).
func (c *core) eachSender(ctx context.Context, fn func(tenantName string, s tenant.Sender) error) error {
	return c.eachTenant(ctx, func(name string, p tenant.Pipeline) error {
		senders, err := allSenders(ctx, p)
		if err != nil {
			return err
		}
		for _, s := range senders {
			if err := fn(name, s); err != nil {
				return err
			}
		}
		return nil
	})
}
