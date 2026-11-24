package api

import (
	"context"
	"strings"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageNoSetting answers a name that is not in the list of settings the
// server knows.
const messageNoSetting = "no setting has this name"

// settingsService is SettingsService. The list of settings is the server's
// (package core/settings); the installation's values are in the master and a
// tenant's overrides in its own store, which only the registry hands out, and
// only for a tenant that is ready. The value that applies is the tenant's
// override, else the installation's value, else the built-in default.
type settingsService struct{ c *core }

var _ nopacsv1connect.SettingsServiceHandler = settingsService{}

// definition returns the setting named name, or NotFound when the server
// knows no such setting.
func (c *core) definition(name string) (settings.Definition, error) {
	def, ok := c.settings.Lookup(name)
	if !ok {
		return settings.Definition{}, fault.New(fault.NotFound, messageNoSetting)
	}
	return def, nil
}

// onInstallation returns the value of def that applies on the installation
// and where it comes from: the installation's value in the master, else the
// built-in default.
func (c *core) onInstallation(ctx context.Context, def settings.Definition) (string, v1.SettingSource, error) {
	v, err := c.master.Setting(ctx, def.Name)
	switch {
	case err == nil:
		return v, v1.SettingSource_SETTING_SOURCE_INSTALLATION, nil
	case fault.KindOf(err) == fault.NotFound:
		return def.Default, v1.SettingSource_SETTING_SOURCE_BUILT_IN, nil
	}
	return "", v1.SettingSource_SETTING_SOURCE_UNSPECIFIED, err
}

// values returns the reader of the settings that apply, over the server's
// list and the master (package settingvalue, which the receivers read
// through too).
func (c *core) values() settingvalue.Reader {
	return settingvalue.Reader{Table: c.settings, Master: c.master}
}

// installationValue returns the value of the setting named name that
// applies on the installation. A setting the server's list does not hold, or
// a stored value its rule refuses, is the server's fault: every value is
// checked before it is stored.
func (c *core) installationValue(ctx context.Context, name string) (string, error) {
	return c.values().Installation(ctx, name)
}

// keyLifetime returns the lifetime of a key made with no expiry: the
// installation's key.default-lifetime, else its built-in default. ok is
// false when the server's list has no such setting, and the access package's
// own default applies.
func (c *core) keyLifetime(ctx context.Context) (time.Duration, bool, error) {
	def, known := c.settings.Lookup(settings.KeyDefaultLifetime)
	if !known {
		return 0, false, nil
	}
	v, _, err := c.onInstallation(ctx, def)
	if err != nil {
		return 0, false, err
	}
	d, err := settings.ParseDuration(v)
	if err != nil || def.Validate(v) != nil {
		// Every value is validated before it is stored, so one that does
		// not read is the server's fault, not the caller's.
		return 0, false, fault.New(fault.Internal, "api: the stored "+settings.KeyDefaultLifetime+" does not read as a lifetime")
	}
	return d, true, nil
}

// settingMessage returns a setting as the contract writes it.
func settingMessage(name string, scope access.Scope, value string, source v1.SettingSource) *v1.Setting {
	return &v1.Setting{Name: name, Scope: scopeMessage(scope), Value: value, Source: source}
}

// shownValue returns the value of the setting named name as the caller d
// may read it. The paths of storage.directory-roots are the installation's:
// where its disks are mounted is no tenant's business, and the rest of the
// server never shows one (an error, a log line, an audit entry). So a caller
// who is not an admin on the installation is answered the roots' names
// alone, one per line, which is all a storage names; the installation's
// admin, who sets them, reads them whole.
func shownValue(d access.Decision, name, value string) string {
	// import.roots is the same: a tenant's roles see its roots' names only.
	if (name != settings.StorageDirectoryRoots && name != settings.ImportRoots) || d.Principal().Role(master.Installation).Includes(master.RoleAdmin) {
		return value
	}
	roots, err := settings.ParseDirectoryRoots(value)
	if err != nil {
		// Every stored value follows its rule; one that does not is shown
		// as nothing rather than as it is.
		return ""
	}
	names := make([]string, len(roots))
	for i, r := range roots {
		names[i] = r.Name
	}
	return strings.Join(names, "\n")
}

// tenantScoped returns the Invalid error of a tenant scope for a setting a
// tenant may not override, naming field, or nil.
func tenantScoped(field string, scope access.Scope, def settings.Definition) error {
	if !scope.IsInstallation() && !def.TenantOverride {
		return invalid(field, "is the installation: a tenant may not override "+def.Name)
	}
	return nil
}

// scopeDetails returns the audit details of a request about one scope.
func scopeDetails(scope access.Scope) map[string]string {
	return map[string]string{detailScope: scope.String()}
}

// GetSetting answers the value of a setting that applies on a scope, and
// where it came from.
func (s settingsService) GetSetting(ctx context.Context, req *v1.GetSettingRequest) (*v1.GetSettingResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.SettingsServiceGetSettingProcedure)
	if err != nil {
		return nil, err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	// Every record of the request from here on goes to the log of its scope.
	st, err := c.sinkFor(ctx, d, scope, scopeDetails(scope))
	if err != nil {
		return nil, err
	}
	name := req.GetName()
	if err := settingName("name", name); err != nil {
		return nil, c.failIn(d, st, err)
	}
	def, err := c.definition(name)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if st != nil && def.TenantOverride {
		v, err := st.Setting(ctx, name)
		switch {
		case err == nil:
			c.done(d, st)
			return &v1.GetSettingResponse{Setting: settingMessage(name, scope, v, v1.SettingSource_SETTING_SOURCE_TENANT)}, nil
		case fault.KindOf(err) != fault.NotFound:
			return nil, c.failIn(d, st, err)
		}
	}
	// A setting a tenant may not override is answered from the installation
	// on a tenant scope too; its read is still recorded in the tenant's log.
	v, source, err := c.onInstallation(ctx, def)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	return &v1.GetSettingResponse{Setting: settingMessage(name, access.InstallationScope, shownValue(d, name, v), source)}, nil
}

// SetSetting sets the installation's value of a setting or a tenant's
// override of it, with its audit entry in the same transaction. The entry
// holds the setting's name and scope, never its value. The caller's key is
// checked again as the change is made: in the master's transaction for the
// installation, and for a tenant, whose database holds no keys, by the
// master from inside the tenant's transaction (adminGuard). A value of a
// setting that says where data may go is checked against what every
// tenant's storages and receivers use (takenAway).
func (s settingsService) SetSetting(ctx context.Context, req *v1.SetSettingRequest) (*v1.SetSettingResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.SettingsServiceSetSettingProcedure)
	if err != nil {
		return nil, err
	}
	in := req.GetSetting()
	if in == nil {
		return nil, c.fail(d, invalid("setting", "is required"))
	}
	scope, err := scopeOf("setting.scope", in.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	st, err := c.sinkFor(ctx, d, scope, scopeDetails(scope))
	if err != nil {
		return nil, err
	}
	facts := access.Facts{TenantName: scope.Tenant}
	name, value := in.GetName(), in.GetValue()
	def, err := c.settingToChange(d, scope, "setting", name, &value)
	if err != nil {
		return nil, c.failWith(d, refusalSink(err, st), facts, err)
	}
	if err := def.Validate(value); err != nil {
		return nil, c.failWith(d, st, facts, invalid("setting.value", err.Error()))
	}
	if err := checkSettingValue("setting.value", name, value); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if err := c.checkReadBounds(ctx, name, value); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if placeSettings[name] {
		c.places.Lock()
		defer c.places.Unlock()
		// import.roots is stored resolved, so a link changed later is
		// told at the root's use.
		if value, err = c.checkImportPlace(ctx, name, value); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		if err := c.takenAway(ctx, scope, name, value); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
	}
	entry := d.Entry(master.OutcomeDone, "", facts)
	if st == nil {
		if err := c.master.SetSetting(ctx, name, value, entry, d.Actor(master.Installation, master.RoleAdmin)); err != nil {
			return nil, c.fail(d, err)
		}
		return &v1.SetSettingResponse{Setting: settingMessage(name, scope, value, v1.SettingSource_SETTING_SOURCE_INSTALLATION)}, nil
	}
	if err := st.SetSetting(ctx, name, value, entry, c.settingGuard(d, def)); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.SetSettingResponse{Setting: settingMessage(name, scope, value, v1.SettingSource_SETTING_SOURCE_TENANT)}, nil
}

// settingToChange returns the definition of the setting a change names,
// after the checks every change of a setting makes, in the contract's
// order: the name; a value, when value is not nil, within the bound every
// setting has; a setting the server knows; a scope the setting takes (a
// tenant only when it allows an override, the installation only when it
// has a value there); and, for a setting the installation's admin alone
// sets on a tenant, a caller who is that admin. prefix is the request's
// field that holds the name, "" when the name is a field of its own.
func (c *core) settingToChange(d access.Decision, scope access.Scope, prefix, name string, value *string) (settings.Definition, error) {
	field := func(f string) string {
		if prefix == "" {
			return f
		}
		return prefix + "." + f
	}
	if err := settingName(field("name"), name); err != nil {
		return settings.Definition{}, err
	}
	if value != nil {
		if err := text(field("value"), *value, 0, master.MaxSettingValueLength); err != nil {
			return settings.Definition{}, err
		}
	}
	def, err := c.definition(name)
	if err != nil {
		return settings.Definition{}, err
	}
	if err := tenantScoped(field("scope"), scope, def); err != nil {
		return settings.Definition{}, err
	}
	if scope.IsInstallation() && def.TenantOnly {
		return settings.Definition{}, invalid(field("scope"), "is a tenant: "+name+" has no installation value")
	}
	if def.TenantSetByInstallation && !d.Principal().Role(master.Installation).Includes(master.RoleAdmin) {
		// The table asks this of the request before the handler runs; this
		// holds the handler to it too, should the two lists of settings
		// ever differ.
		return settings.Definition{}, fault.New(fault.Denied, access.MessageDenied)
	}
	return def, nil
}

// refusalSink returns the log a failure of a setting's change is recorded
// in: a refusal in the installation's, as every refusal is, and anything
// else in the log of the scope, st.
func refusalSink(err error, st tenant.Store) tenant.Store {
	if fault.KindOf(err) == fault.Denied {
		return nil
	}
	return st
}

// settingGuard returns the check a change of a tenant's value of def makes
// of its caller: admin on the installation for a setting only the
// installation's admin sets on a tenant, and otherwise admin on the tenant
// (adminGuard).
func (c *core) settingGuard(d access.Decision, def settings.Definition) tenant.Guard {
	if def.TenantSetByInstallation {
		by := d.Actor(master.Installation, master.RoleAdmin)
		return func(ctx context.Context) error { return c.master.CheckActor(ctx, by) }
	}
	return c.adminGuard(d)
}

// UnsetSetting removes a value set on a scope and answers what applies there
// again: for a tenant, what applies on the installation; for the
// installation, the built-in default. Removing a value that is not set
// succeeds. Removing a value that a tenant's storage or receiver uses is
// refused, as setting one that leaves it out is.
func (s settingsService) UnsetSetting(ctx context.Context, req *v1.UnsetSettingRequest) (*v1.UnsetSettingResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.SettingsServiceUnsetSettingProcedure)
	if err != nil {
		return nil, err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	st, err := c.sinkFor(ctx, d, scope, scopeDetails(scope))
	if err != nil {
		return nil, err
	}
	facts := access.Facts{TenantName: scope.Tenant}
	name := req.GetName()
	def, err := c.settingToChange(d, scope, "", name, nil)
	if err != nil {
		return nil, c.failWith(d, refusalSink(err, st), facts, err)
	}
	if err := c.checkReadBounds(ctx, name, def.Default); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if placeSettings[name] {
		c.places.Lock()
		defer c.places.Unlock()
		after := def.Default
		if !scope.IsInstallation() && !def.TenantOnly {
			if after, err = c.installationValue(ctx, name); err != nil {
				return nil, c.failWith(d, st, facts, err)
			}
		}
		if err := c.takenAway(ctx, scope, name, after); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
	}
	entry := d.Entry(master.OutcomeDone, "", facts)
	if st == nil {
		if _, err := c.master.UnsetSetting(ctx, name, entry, d.Actor(master.Installation, master.RoleAdmin)); err != nil {
			return nil, c.fail(d, err)
		}
		return &v1.UnsetSettingResponse{Setting: settingMessage(name, scope, def.Default, v1.SettingSource_SETTING_SOURCE_BUILT_IN)}, nil
	}
	if _, err := st.UnsetSetting(ctx, name, entry, c.settingGuard(d, def)); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	// The override is gone and its entry written; what applies now is read
	// for the answer. If that read fails the caller is told so, and may
	// repeat the call, which changes nothing more.
	v, source, err := c.onInstallation(ctx, def)
	if err != nil {
		return nil, c.toAPI(d, err)
	}
	return &v1.UnsetSettingResponse{Setting: settingMessage(name, access.InstallationScope, v, source)}, nil
}

// adminGuard returns the check a change in the tenant d names makes of its
// caller (tenant.Guard): the master's check that d's key still holds admin on
// that tenant, the one each change of the master makes in its own
// transaction (master.Store.CheckActor). The tenant's store runs it last in
// its transaction, before the commit; the tenant's database holds no keys,
// so the check reads another database, and a revocation in the moment
// between that read and the tenant's commit is not seen by that one change
// (package tenant's documentation). A refusal comes back from the change
// with a master.Refusal in its chain, and failWith answers and records it as
// the check would.
func (c *core) adminGuard(d access.Decision) tenant.Guard {
	ref, _ := d.Tenant()
	by := d.Actor(master.TenantScope(ref.ID), master.RoleAdmin)
	return func(ctx context.Context) error {
		if err := c.master.CheckActor(ctx, by); err != nil {
			return err
		}
		return c.writable(ref.Name)
	}
}

// writable returns the refusal of a change in the tenant named name when it
// is held read only (moved or fenced), and nil otherwise. A tenant's guard
// asks it last in the change's transaction, so a change that began before
// an export made the tenant moved, and commits after, is refused rather
// than written after the export's snapshot.
func (c *core) writable(name string) error {
	if _, err := c.reg.Store(name); fault.KindOf(err) == fault.Precondition {
		return err
	}
	return nil
}

// ListSettings answers a page of settings, ordered by name: on the
// installation, every setting the server knows with the value that applies
// there; on a tenant, its overrides of settings the server knows.
func (s settingsService) ListSettings(ctx context.Context, req *v1.ListSettingsRequest) (*v1.ListSettingsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.SettingsServiceListSettingsProcedure)
	if err != nil {
		return nil, err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return nil, c.fail(d, err)
	}
	st, err := c.sinkFor(ctx, d, scope, scopeDetails(scope))
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if st == nil {
		out, err := c.installationSettings(ctx, d, page)
		if err != nil {
			return nil, c.fail(d, err)
		}
		c.done(d, nil)
		return out, nil
	}
	overrides, next, err := st.ListSettings(ctx, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListSettingsResponse{Settings: make([]*v1.Setting, 0, len(overrides)), NextPageToken: next}
	for _, o := range overrides {
		// An override of a name the server no longer knows (a setting a
		// later version dropped) is kept in the tenant's database, not
		// deleted, and not shown, as the installation's listing and
		// GetSetting do not show it either. A page may so hold fewer than
		// page_size items; its token still goes on from where the store
		// stopped.
		if _, known := c.settings.Lookup(o.Name); !known {
			continue
		}
		out.Settings = append(out.Settings, settingMessage(o.Name, scope, o.Value, v1.SettingSource_SETTING_SOURCE_TENANT))
	}
	return out, nil
}

// installationSettings returns a page of every setting the server knows,
// each with the value that applies on the installation. It pages the
// server's list, not the master's rows, so a setting with no value set is
// listed with its built-in default; a row of a setting the server no longer
// knows is not listed.
func (c *core) installationSettings(ctx context.Context, d access.Decision, page master.Page) (*v1.ListSettingsResponse, error) {
	after := ""
	if page.Cursor != "" {
		var err error
		if after, err = readAfterToken(page.Cursor, tokenInstallationSettings, "installation"); err != nil {
			return nil, err
		}
	}
	var names []string
	for _, n := range c.settings.Names() {
		if n > after {
			names = append(names, n)
		}
	}
	out := &v1.ListSettingsResponse{}
	if len(names) > page.Limit {
		names = names[:page.Limit]
		out.NextPageToken = afterToken(tokenInstallationSettings, "installation", names[len(names)-1])
	}
	out.Settings = make([]*v1.Setting, 0, len(names))
	for _, n := range names {
		def, _ := c.settings.Lookup(n)
		v, source, err := c.onInstallation(ctx, def)
		if err != nil {
			return nil, err
		}
		out.Settings = append(out.Settings, settingMessage(n, access.InstallationScope, shownValue(d, n, v), source))
	}
	return out, nil
}
