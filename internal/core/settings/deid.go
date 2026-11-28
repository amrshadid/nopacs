package settings

import (
	"errors"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/pathname"
)

// DeidBaseProfile is the tenant's base profile: the profile version "name@version"
// that every receiver's profile, every archive rule's profile and every
// destination's profile of the tenant must be at least as strict as ("" for
// none). It is a tenant's own value with no installation value, since a
// profile is the tenant's own document, and the tenant's admin sets it: the
// tenant's admin is who answers for what the tenant's receivers let through.
// A masked key never sets it (the API refuses it), as a service outside the
// site must not loosen or drop what a site requires of every receiver.
const DeidBaseProfile = "deid.base-profile"

// MaxBaseProfileVersion is the largest version number of a profile, the
// profile language's bound restated here because the core imports nothing of
// NoPACs'.
const MaxBaseProfileVersion = 1<<31 - 1

// ParseBaseProfile reads deid.base-profile: "" for no base, or "name@version"
// naming a version of a profile of the tenant, the name under the rule of a
// tenant's name and the version a whole number from 1, written without
// leading zeros. Whether the tenant has that version is the API's to judge.
func ParseBaseProfile(v string) (name string, version int32, err error) {
	if v == "" {
		return "", 0, nil
	}
	bad := errors.New(`is "" for no base, or a profile version as name@version, such as research@2`)
	name, num, ok := strings.Cut(v, "@")
	if !ok || !pathname.ValidName(name) || num == "" || num[0] == '0' || strings.Trim(num, "0123456789") != "" {
		return "", 0, bad
	}
	n, perr := strconv.ParseInt(num, 10, 32)
	if perr != nil || n < 1 || n > MaxBaseProfileVersion {
		return "", 0, bad
	}
	return name, int32(n), nil
}

// deidDefinitions returns the definitions of the settings of the layers of
// de-identification, for Builtin.
func deidDefinitions() []Definition {
	return []Definition{{
		// Empty by default: a tenant has no base until its admin names one.
		// No installation value, since a profile version exists in one
		// tenant's database only.
		Name:           DeidBaseProfile,
		Default:        "",
		TenantOverride: true,
		TenantOnly:     true,
		Validate:       func(v string) error { _, _, err := ParseBaseProfile(v); return err },
	}}
}
