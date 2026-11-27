package settings

import (
	"errors"
	"testing"
	"time"
)

// TestBuiltin guards the built-in list: it is a valid table, the key's
// default lifetime is installation-only with a default of 90 days, and each
// setting the reception pipeline reads has the kind, the default and the
// scope the API contract gives it (storage.proto, receiver.proto,
// monitor.proto, and the walk-through's table of settings).
func TestBuiltin(t *testing.T) {
	tab := MustBuiltin()
	d, ok := tab.Lookup(KeyDefaultLifetime)
	if !ok || d.TenantOverride {
		t.Fatalf("%+v %v", d, ok)
	}
	if v, err := ParseDuration(d.Default); err != nil || v != 90*24*time.Hour {
		t.Errorf("default %q is %v, %v", d.Default, v, err)
	}
	if _, ok := tab.Lookup("no.such"); ok {
		t.Error("an unknown name is found")
	}
	want := map[string]struct {
		kind       Kind
		def        string
		tenantOnly bool
	}{
		KeyDefaultLifetime:      {InstallationOnly, "90d", false},
		StorageDirectoryRoots:   {InstallationOnly, "", false},
		StorageS3Endpoints:      {InstallationOnly, "", false},
		StorageReserve:          {InstallationOnly, "10%,10GiB", false},
		ReceiverPorts:           {SetByInstallation, "", true},
		ReceiverBindAddresses:   {InstallationOnly, "", false},
		IncomingLimit:           {SetByInstallation, "0", false},
		EventsRetention:         {Ordinary, "90d", false},
		ReceiverHeaderLimit:     {InstallationOnly, "64MiB", false},
		ReceiverAllowAsReceived: {SetByInstallation, "false", false},
		DeidPixelLimit:          {SetByInstallation, "1GiB", false},
		DeidMemory:              {InstallationOnly, "2GiB", false},
		DeidListCacheBytes:      {InstallationOnly, "256MiB", false},
		APICORSOrigins:          {InstallationOnly, "", false},
		AccessTrustedProxies:    {InstallationOnly, "", false},
		AccessForwardedHeader:   {InstallationOnly, "x-forwarded-for", false},
		AccessAllowedNetworks:   {InstallationOnly, "", false},
		AccessDeniedNetworks:    {InstallationOnly, "", false},
		AccessTenantNetworks:    {Ordinary, "", false},
		AccessFailureLimit:      {InstallationOnly, "10", false},
		AccessFailureWindow:     {InstallationOnly, "1m", false},
		AccessBlockTime:         {InstallationOnly, "15m", false},
		KeyMaxLifetime:          {InstallationOnly, "3650d", false},
		KeyExpiryWarning:        {InstallationOnly, "7d", false},

		SenderDestinations:          {InstallationOnly, "", false},
		SenderAllowPlainHTTP:        {InstallationOnly, "false", false},
		SenderMaxAssociations:       {InstallationOnly, "32", false},
		SenderTenantMaxAssociations: {SetByInstallation, "8", false},
		JobFailuresKept:             {Ordinary, "10000", false},

		AuditSealInterval: {InstallationOnly, "10m", false},
		AuditRetention:    {InstallationOnly, "0", false},

		ReadPerCaller: {InstallationOnly, "16", false},
		ReadPerServer: {InstallationOnly, "256", false},

		ScrubRate:     {Ordinary, "64MiB", false},
		ScrubInterval: {Ordinary, "30d", false},

		ScrubArchiveSample: {Ordinary, "0%", false},
		ReplicaLagAlert:    {Ordinary, "1h", false},
		ReplicaRate:        {Ordinary, "0", false},
		RecallWait:         {Ordinary, "48h", false},

		DeleteGrace: {Ordinary, "7d", false},

		BackupMaxDuration: {InstallationOnly, "6h", false},

		ImportRoots:          {InstallationOnly, "", false},
		ImportAllowedRoots:   {SetByInstallation, "", true},
		ImportMaxUnits:       {SetByInstallation, "4", false},
		ImportServerMaxUnits: {InstallationOnly, "8", false},
		ImportMaxDirEntries:  {InstallationOnly, "200000", false},

		ReplicaRepair:          {Ordinary, "manual", false},
		ReplicaRepairLease:     {Ordinary, "1m", false},
		RetentionShortenNotice: {SetByInstallation, "30d", false},
		RetentionMaxClockStep:  {InstallationOnly, "2d", false},
		RetentionMaxPerPass:    {SetByInstallation, "1000", false},
		QuarantineWarnAge:      {Ordinary, "30d", false},
		QuarantineWarnBytes:    {Ordinary, "0", false},
	}
	if got := tab.Names(); len(got) != len(want) {
		t.Errorf("names %v, want the %d this test knows", got, len(want))
	}
	for name, w := range want {
		d, ok := tab.Lookup(name)
		if !ok {
			t.Errorf("%s is not built in", name)
			continue
		}
		if d.Kind() != w.kind || d.Default != w.def || d.TenantOnly != w.tenantOnly {
			t.Errorf("%s: kind %v, default %q, tenant only %v; want %v, %q, %v", name, d.Kind(), d.Default, d.TenantOnly, w.kind, w.def, w.tenantOnly)
		}
	}
}

// TestKeyLifetimeBounds guards the key lifetime's rule at each bound and one
// past it, in both units.
func TestKeyLifetimeBounds(t *testing.T) {
	d, _ := MustBuiltin().Lookup(KeyDefaultLifetime)
	for v, ok := range map[string]bool{
		"1h": true, "0h": false, "3650d": true, "3651d": false, "87600h": true, "87601h": false,
		"2160h": true, "90d": true, "60m": true, "3600s": true, "59m": false, "": false, "90": false, "d": false, "-1d": false, "1.5d": false,
		"090d": false, "90D": false, "1d1h": false, " 90d": false, "99999999999999999999d": false,
	} {
		if err := d.Validate(v); (err == nil) != ok {
			t.Errorf("%q: accepted %v, want %v (%v)", v, err == nil, ok, err)
		}
	}
}

// TestNewRefusesMistakes guards New against a table it could not serve: a bad
// or repeated name, no rule, and a default its own rule refuses.
func TestNewRefusesMistakes(t *testing.T) {
	ok := func(string) error { return nil }
	no := func(string) error { return errors.New("no") }
	for name, defs := range map[string][]Definition{
		"bad name":    {{Name: "Bad", Validate: ok}},
		"empty name":  {{Name: "", Validate: ok}},
		"long name":   {{Name: string(make([]byte, 129)), Validate: ok}},
		"twice":       {{Name: "a", Validate: ok}, {Name: "a", Validate: ok}},
		"no rule":     {{Name: "a"}},
		"bad default": {{Name: "a", Validate: no}},
		"reserved":    {{Name: ReservedPrefix + "x", Validate: ok}},
		"set by the installation without a tenant value": {{Name: "a", Validate: ok, TenantSetByInstallation: true}},
		"tenant only, set by its own admin":              {{Name: "a", Validate: ok, TenantOverride: true, TenantOnly: true}},
	} {
		if _, err := New(defs...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tab, err := New(Definition{Name: "b.two", Validate: ok}, Definition{Name: "a.one", Validate: ok, TenantOverride: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := tab.Names(); len(n) != 2 || n[0] != "a.one" {
		t.Errorf("names are not in order: %v", n)
	}
}
