package fakeserver

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// settingsHandler implements SettingsService, as the real server's: a fixed
// list of settings (Server.Settings), each with a built-in default; the
// value that applies is the tenant's override, else the installation's
// value, else the built-in default, and each answer says which.
type settingsHandler struct{ s *Server }

// settingScope reads a settings scope into the key of the settings map: ""
// for the installation, the tenant name otherwise, after checking the tenant
// exists.
func (s *Server) settingScope(sc *v1.Scope) (string, error) {
	tenant, _, err := scopeOf(sc)
	if err != nil {
		return "", err
	}
	if tenant != "" {
		if err := s.checkTenant(tenant); err != nil {
			return "", err
		}
	}
	return tenant, nil
}

// definition applies the contract's rule for a name and the fixed list.
func (s *Server) definition(name string) (settings.Definition, error) {
	if !settingName.MatchString(name) {
		return settings.Definition{}, fault.New(fault.Invalid, "name: is 1 to 128 of a-z, 0-9, ., _ and -")
	}
	def, ok := s.known().Lookup(name)
	if !ok {
		return settings.Definition{}, fault.New(fault.NotFound, "no setting has this name")
	}
	return def, nil
}

// onTenantOnly refuses a tenant scope for a setting a tenant may not
// override.
func onTenantOnly(scopeKey string, def settings.Definition) error {
	if scopeKey != "" && !def.TenantOverride {
		return fault.New(fault.Invalid, "scope: is the installation: a tenant may not override "+def.Name)
	}
	return nil
}

// applies returns the value of def that applies on the installation and its
// source.
func (s *Server) applies(def settings.Definition) *v1.Setting {
	if v, ok := s.settings[""][def.Name]; ok {
		return settingOf(def.Name, "", v, v1.SettingSource_SETTING_SOURCE_INSTALLATION)
	}
	return settingOf(def.Name, "", def.Default, v1.SettingSource_SETTING_SOURCE_BUILT_IN)
}

// settingOf builds a Setting for a scope key.
func settingOf(name, scopeKey, value string, source v1.SettingSource) *v1.Setting {
	sc := &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}
	if scopeKey != "" {
		sc = &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: scopeKey}}
	}
	return &v1.Setting{Name: name, Scope: sc, Value: value, Source: source}
}

// GetSetting answers the value that applies to a scope and where it came from.
func (h settingsHandler) GetSetting(_ context.Context, req *v1.GetSettingRequest) (*v1.GetSettingResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, err := s.settingScope(req.GetScope())
	if err != nil {
		return nil, err
	}
	def, err := s.definition(req.GetName())
	if err != nil {
		return nil, err
	}
	if v, ok := s.settings[sk][def.Name]; ok && sk != "" && def.TenantOverride {
		return &v1.GetSettingResponse{Setting: settingOf(def.Name, sk, v, v1.SettingSource_SETTING_SOURCE_TENANT)}, nil
	}
	return &v1.GetSettingResponse{Setting: s.applies(def)}, nil
}

// SetSetting sets the installation's value or a tenant's override.
func (h settingsHandler) SetSetting(_ context.Context, req *v1.SetSettingRequest) (*v1.SetSettingResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st := req.GetSetting()
	if st == nil {
		return nil, fault.New(fault.Invalid, "setting: is required")
	}
	sk, err := s.settingScope(st.GetScope())
	if err != nil {
		return nil, err
	}
	def, err := s.definition(st.GetName())
	if err != nil {
		return nil, err
	}
	if err := onTenantOnly(sk, def); err != nil {
		return nil, err
	}
	if len(st.GetValue()) > settings.MaxValueLength {
		return nil, fault.New(fault.Invalid, "setting.value: is at most 64 KiB")
	}
	if err := def.Validate(st.GetValue()); err != nil {
		return nil, fault.New(fault.Invalid, "setting.value: "+err.Error())
	}
	if s.settings[sk] == nil {
		s.settings[sk] = map[string]string{}
	}
	s.settings[sk][def.Name] = st.GetValue()
	source := v1.SettingSource_SETTING_SOURCE_INSTALLATION
	if sk != "" {
		source = v1.SettingSource_SETTING_SOURCE_TENANT
	}
	return &v1.SetSettingResponse{Setting: settingOf(def.Name, sk, st.GetValue(), source)}, nil
}

// UnsetSetting removes a tenant's override or the installation's value and
// answers what applies again.
func (h settingsHandler) UnsetSetting(_ context.Context, req *v1.UnsetSettingRequest) (*v1.UnsetSettingResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, err := s.settingScope(req.GetScope())
	if err != nil {
		return nil, err
	}
	def, err := s.definition(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := onTenantOnly(sk, def); err != nil {
		return nil, err
	}
	delete(s.settings[sk], def.Name)
	return &v1.UnsetSettingResponse{Setting: s.applies(def)}, nil
}

// ListSettings answers, ordered by name, every setting the fake knows as it
// applies on the installation, or a tenant's overrides of settings it knows.
func (h settingsHandler) ListSettings(_ context.Context, req *v1.ListSettingsRequest) (*v1.ListSettingsResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, err := s.settingScope(req.GetScope())
	if err != nil {
		return nil, err
	}
	var all []*v1.Setting
	for _, n := range s.known().Names() {
		def, _ := s.known().Lookup(n)
		if sk == "" {
			all = append(all, s.applies(def))
		} else if v, ok := s.settings[sk][n]; ok {
			all = append(all, settingOf(n, sk, v, v1.SettingSource_SETTING_SOURCE_TENANT))
		}
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListSettingsResponse{Settings: all[from:to], NextPageToken: next}, nil
}
