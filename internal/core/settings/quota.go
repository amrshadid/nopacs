package settings

import (
	"errors"
	"strings"
)

// The settings of a tenant's storage quota (tenant.proto, TenantQuota). A
// quota bounds what one tenant holds on its storages, so one tenant cannot
// fill a storage that others share.
const (
	// QuotaBytes is the most bytes of DICOM files a tenant may hold on its
	// storages, "" for no bound (ParseQuotaBytes). Set on a tenant by the
	// installation's admin only: the storage is the installation's, and a
	// bound the tenant could raise would bound nothing.
	QuotaBytes = "quota.bytes"
	// QuotaInstances is the most instance files a tenant may hold on its
	// storages, "" for no bound (ParseQuotaInstances). Set on a tenant by
	// the installation's admin only, for the same reason.
	QuotaInstances = "quota.instances"
	// QuotaWarn is the share of a bound past which the tenant is warned, a
	// whole percentage (ParseQuotaWarn). Ordinary: a warning bounds nothing.
	QuotaWarn = "quota.warn"
)

// MaxQuotaInstances bounds quota.instances: 2^62, past any archive.
const MaxQuotaInstances = 1 << 62

// ParseQuotaBytes reads quota.bytes: "" for no bound, or a size (ParseSize)
// of at least one byte. The bound is returned as 0 for none.
func ParseQuotaBytes(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := ParseSize(v)
	if err != nil || n < 1 {
		return 0, errors.New(`is "" for no bound, or a size of at least one byte, such as 500GiB`)
	}
	return n, nil
}

// ParseQuotaInstances reads quota.instances: "" for no bound, or a whole
// number from 1 to 2^62, written without a sign or leading zeros. The bound
// is returned as 0 for none.
func ParseQuotaInstances(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := wholeNumber(v, MaxQuotaInstances)
	if err != nil || n < 1 {
		return 0, errors.New(`is "" for no bound, or a whole number of instances of at least 1`)
	}
	return n, nil
}

// ParseQuotaWarn reads quota.warn: a whole percentage from 1% to 100%, such
// as 90%.
func ParseQuotaWarn(v string) (int, error) {
	digits, ok := strings.CutSuffix(v, "%")
	n, err := wholeNumber(digits, 100)
	if !ok || err != nil || n < 1 {
		return 0, errors.New("is a whole percentage from 1% to 100%, such as 90%")
	}
	return int(n), nil
}

// quotaDefinitions returns the definitions of the quota's settings, for
// Builtin.
func quotaDefinitions() []Definition {
	return []Definition{{
		// No bound by default: a quota is something the installation's
		// admin hands out to a tenant, as ports are. The installation's
		// value, when one is set, is every tenant's default.
		Name:                    QuotaBytes,
		Default:                 "",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseQuotaBytes(v); return err },
	}, {
		Name:                    QuotaInstances,
		Default:                 "",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseQuotaInstances(v); return err },
	}, {
		// 90%: at the rate a busy tenant fills a bound, a tenth of it is
		// days of work, time for a person to raise the bound or to purge.
		Name:           QuotaWarn,
		Default:        "90%",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseQuotaWarn(v); return err },
	}}
}
