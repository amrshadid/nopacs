package api

import (
	"context"
	"slices"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// importRoots returns where the server finds an import root for a tenant
// and checks it again at each use (place.ImportRoots): the installation's
// import.roots, the tenant's import.allowed-roots, and the directories no
// import root may touch.
func (c *core) importRoots() place.ImportRoots {
	v := c.values()
	return place.ImportRoots{
		Values: v,
		Allotted: func(ctx context.Context, st tenant.Store) ([]string, error) {
			return v.OnTenant(st).ImportAllowedRoots(ctx)
		},
		Guarded: c.importGuarded,
	}
}

// checkImportPlace applies the rules that keep import roots apart from the
// archive's own directories, for a change of import.roots or of
// storage.directory-roots, value being what would apply after it: no import
// root is, contains or lies inside a storage root, a guarded directory of
// the server's or another import root, once every symbolic link is followed
// (place.CheckImportRoots). It returns the value to store: for import.roots,
// each path resolved as it leads now (place.ResolveImportRoots), so a root
// whose path later resolves elsewhere is refused at its use; any other
// value as it is. The caller holds core.places for writing.
func (c *core) checkImportPlace(ctx context.Context, name, value string) (string, error) {
	var imports, storage settings.DirectoryRoots
	var err error
	switch name {
	case settings.ImportRoots:
		if imports, err = settings.ParseImportRoots(value); err != nil {
			return "", invalid("setting.value", err.Error())
		}
		if storage, err = c.values().DirectoryRoots(ctx); err != nil {
			return "", err
		}
	case settings.StorageDirectoryRoots:
		if storage, err = settings.ParseDirectoryRoots(value); err != nil {
			return "", invalid("setting.value", err.Error())
		}
		if imports, err = c.values().ImportRoots(ctx); err != nil {
			return "", err
		}
	default:
		return value, nil
	}
	if err := place.CheckImportRoots(imports, storage, c.importGuarded); err != nil {
		switch fault.KindOf(err) {
		case fault.Invalid:
			return "", invalid("setting.value", fault.Message(err))
		case fault.Precondition:
			return "", fault.Wrap(fault.Precondition, err, "setting.value: "+fault.Message(err))
		}
		return "", err
	}
	if name != settings.ImportRoots {
		return value, nil
	}
	resolved, err := place.ResolveImportRoots(imports)
	if err != nil {
		return "", fault.Wrap(fault.Precondition, err, "setting.value: "+fault.Message(err))
	}
	return resolved.String(), nil
}

// importTakenAway refuses, as a precondition, a value of import.roots that
// removes or moves a root an import receiver of any tenant names, or a
// tenant's import.allowed-roots that takes back a root one of its import
// receivers names. value is what would apply on scope after the change.
// The caller holds core.places for writing, so no receiver is added
// meanwhile on what is taken away.
func (c *core) importTakenAway(ctx context.Context, scope access.Scope, name, value string) error {
	switch name {
	case settings.ImportRoots:
		old, err := c.values().ImportRoots(ctx)
		if err != nil {
			return err
		}
		next, err := settings.ParseImportRoots(value)
		if err != nil {
			return invalid("setting.value", err.Error())
		}
		return c.eachTenant(ctx, func(tenantName string, p tenant.Pipeline) error {
			receivers, err := tenantReceivers(ctx, p)
			if err != nil {
				return err
			}
			for _, r := range receivers {
				if r.Type != tenant.ReceiverImport {
					continue
				}
				was, _ := old.Lookup(r.Import.Root)
				now, ok := next.Lookup(r.Import.Root)
				if !ok || now.Path != was.Path {
					return precondition("the root " + r.Import.Root + " is read by the import receiver " + r.Name + " of the tenant " + tenantName + "; remove the receiver, or change its root, before the root is changed or removed")
				}
			}
			return nil
		})
	case settings.ImportAllowedRoots:
		if scope.IsInstallation() {
			return nil
		}
		allowed, err := settings.ParseAllowedRoots(value)
		if err != nil {
			return invalid("setting.value", err.Error())
		}
		st, err := c.reg.Store(scope.Tenant)
		if err != nil {
			return err
		}
		p, err := c.pipeline(st)
		if err != nil {
			return err
		}
		receivers, err := tenantReceivers(ctx, p)
		if err != nil {
			return err
		}
		for _, r := range receivers {
			if r.Type == tenant.ReceiverImport && !slices.Contains(allowed, r.Import.Root) {
				return precondition("the root " + r.Import.Root + " is read by the import receiver " + r.Name + " of the tenant " + scope.Tenant + "; remove the receiver, or change its root, before the root is taken back")
			}
		}
	}
	return nil
}
