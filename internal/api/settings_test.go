package api_test

import (
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
)

// The sources of a setting's value, short.
const (
	builtIn  = v1.SettingSource_SETTING_SOURCE_BUILT_IN
	fromInst = v1.SettingSource_SETTING_SOURCE_INSTALLATION
	fromTen  = v1.SettingSource_SETTING_SOURCE_TENANT
)

// lifetime is the server's one real setting.
const lifetime = settings.KeyDefaultLifetime

// settingsFixture is the fixture with a tenant, down, whose database does
// not answer.
func settingsFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.downTenant("down")
	return f
}

// getSetting returns a call of GetSetting.
func getSetting(name string, s *v1.Scope) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.settings.GetSetting, &v1.GetSettingRequest{Name: name, Scope: s})
	}
}

// setSetting returns a call of SetSetting.
func setSetting(name string, s *v1.Scope, value string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: name, Scope: s, Value: value}})
	}
}

// unsetSetting returns a call of UnsetSetting.
func unsetSetting(name string, s *v1.Scope) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.settings.UnsetSetting, &v1.UnsetSettingRequest{Name: name, Scope: s})
	}
}

// then returns a call that makes first, fails the test if it fails, and
// then makes next.
func then(t *testing.T, first, next func(clients) outcome) func(clients) outcome {
	return func(c clients) outcome {
		if o := first(c); o.err != nil {
			t.Errorf("the first call: %v", o.err)
		}
		return next(c)
	}
}

// wantSetting returns a check that the answer's setting is name, from the
// scope from ("installation" or a tenant's name), with value and source.
func wantSetting(name, from, value string, source v1.SettingSource) func(t *testing.T, o outcome) {
	return func(t *testing.T, o outcome) {
		t.Helper()
		var s *v1.Setting
		switch r := o.res.(type) {
		case *v1.GetSettingResponse:
			s = r.GetSetting()
		case *v1.SetSettingResponse:
			s = r.GetSetting()
		case *v1.UnsetSettingResponse:
			s = r.GetSetting()
		}
		got := "installation"
		if s.GetScope().GetTenant() != "" {
			got = s.GetScope().GetTenant()
		}
		if s.GetName() != name || got != from || s.GetValue() != value || s.GetSource() != source {
			t.Errorf("got %s from %s = %q (%v), want %s from %s = %q (%v)", s.GetName(), got, s.GetValue(), s.GetSource(), name, from, value, source)
		}
	}
}

// testGetSetting guards GetSetting: reader on the scope asked for (a role on
// the installation gives nothing inside a tenant); the value that applies
// and which of the three it came from; a setting a tenant may not override
// answered from the installation; a name the server does not know not
// found; a tenant that does not exist, Denied or NotFound by who asks; a
// tenant not ready unavailable; the entry in the log of the scope asked for.
func testGetSetting(t *testing.T) {
	f := settingsFixture(t)
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: getSetting(lifetime, installation()), code: connect.CodeUnauthenticated},
		{name: "installation, just below the role", as: f.noGrant, call: getSetting(lifetime, installation()), code: connect.CodePermissionDenied},
		{name: "installation, the built-in default", as: f.instReader, call: getSetting(lifetime, installation()),
			audit: &wantAudit{outcome: master.OutcomeDone, object: lifetime, details: map[string]string{"scope": "installation"}},
			check: wantSetting(lifetime, "installation", "90d", builtIn)},
		{name: "installation, its own value, set by its admin", as: f.instAdmin, call: then(t, setSetting(lifetime, installation(), "30d"), getSetting(lifetime, installation())),
			check: wantSetting(lifetime, "installation", "30d", fromInst)},
		{name: "tenant, an installation admin", as: f.instAdmin, call: getSetting(testRetention, tenantScope("lab1")), code: connect.CodePermissionDenied},
		{name: "tenant, just below the role", as: f.noGrant, call: getSetting(testRetention, tenantScope("lab1")), code: connect.CodePermissionDenied},
		{name: "tenant, at the role, the built-in default", as: f.lab1Reader, call: getSetting(testRetention, tenantScope("lab1")),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", object: testRetention, details: map[string]string{"scope": "tenant/lab1"}},
			check: wantSetting(testRetention, "installation", "365d", builtIn)},
		{name: "tenant, the override", as: f.lab1Admin, call: then(t, setSetting(testRetention, tenantScope("lab1"), "30d"), getSetting(testRetention, tenantScope("lab1"))),
			check: wantSetting(testRetention, "lab1", "30d", fromTen)},
		{name: "tenant, a setting it may not override", as: f.lab1Reader, call: getSetting(lifetime, tenantScope("lab1")),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone},
			check: wantSetting(lifetime, "installation", "30d", fromInst)},
		{name: "no such setting", as: f.instReader, call: getSetting("no.such", installation()), code: connect.CodeNotFound, msg: "no setting", audit: failed("no setting")},
		{name: "no such setting on a tenant", as: f.lab1Reader, call: getSetting("no.such", tenantScope("lab1")), code: connect.CodeNotFound, audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "name at 128", as: f.instReader, call: getSetting(strings.Repeat("n", 128), installation()), code: connect.CodeNotFound},
		{name: "name at 129", as: f.instReader, call: getSetting(strings.Repeat("n", 129), installation()), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "a malformed name", as: f.instReader, call: getSetting("Key", installation()), code: connect.CodeInvalidArgument, msg: "name", audit: failed("name")},
		{name: "no scope, may read the installation", as: f.instReader, call: getSetting(lifetime, nil), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "no scope, may not", as: f.lab1Reader, call: getSetting(lifetime, nil), code: connect.CodePermissionDenied},
		{name: "no such tenant, no right to know", as: f.lab1Reader, call: getSetting(testRetention, tenantScope("nolab")), code: connect.CodePermissionDenied},
		{name: "no such tenant, may list", as: local, call: getSetting(testRetention, tenantScope("nolab")), code: connect.CodeNotFound},
		{name: "a malformed tenant, may list", as: f.instReader, call: getSetting(testRetention, tenantScope("Lab1")), code: connect.CodeInvalidArgument, msg: "scope.tenant"},
		{name: "a tenant that is not ready", as: local, call: getSetting(testRetention, tenantScope("down")), code: connect.CodeUnavailable, msg: "unreachable", audit: inMasterRerouted("down")},
	})
}

// testSetSetting guards SetSetting: admin on the scope set; the change and
// its entry in the scope's log, without the value; each setting's rule for
// its value, at its bounds; a tenant scope refused for a setting a tenant
// may not override; a name the server does not know not found; a tenant not
// ready unavailable.
func testSetSetting(t *testing.T) {
	f := settingsFixture(t)
	value := "v-" + strings.Repeat("x", 20)
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: setSetting(lifetime, installation(), "30d"), code: connect.CodeUnauthenticated},
		{name: "installation, just below the role", as: f.instOperator, call: setSetting(lifetime, installation(), "30d"), code: connect.CodePermissionDenied, audit: refused()},
		{name: "installation, at the role", as: f.instAdmin, call: setSetting(lifetime, installation(), "30d"),
			audit: &wantAudit{outcome: master.OutcomeDone, object: lifetime, details: map[string]string{"scope": "installation"}},
			check: wantSetting(lifetime, "installation", "30d", fromInst)},
		{name: "a lifetime at 1h", as: f.instAdmin, call: setSetting(lifetime, installation(), "1h")},
		{name: "a lifetime below 1h", as: f.instAdmin, call: setSetting(lifetime, installation(), "0h"), code: connect.CodeInvalidArgument, msg: "setting.value", audit: failed("setting.value")},
		{name: "a lifetime at 3650d", as: f.instAdmin, call: setSetting(lifetime, installation(), "3650d")},
		{name: "a lifetime at 3651d", as: f.instAdmin, call: setSetting(lifetime, installation(), "3651d"), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "a lifetime in minutes", as: f.instAdmin, call: setSetting(lifetime, installation(), "30m"), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "an empty lifetime", as: f.instAdmin, call: setSetting(lifetime, installation(), ""), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "a tenant scope for an installation setting", as: f.lab1Admin, call: setSetting(lifetime, tenantScope("lab1"), "30d"), code: connect.CodeInvalidArgument, msg: "setting.scope", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "tenant, an installation admin", as: f.instAdmin, call: setSetting(testRetention, tenantScope("lab1"), "1d"), code: connect.CodePermissionDenied},
		{name: "tenant, just below the role", as: f.lab1Operator, call: setSetting(testRetention, tenantScope("lab1"), "1d"), code: connect.CodePermissionDenied},
		{name: "tenant, at the role", as: f.lab1Admin, call: setSetting(testText, tenantScope("lab1"), value),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", object: testText, details: map[string]string{"scope": "tenant/lab1"}},
			check: wantSetting(testText, "lab1", value, fromTen)},
		{name: "tenant, repeated", as: f.lab1Admin, call: setSetting(testText, tenantScope("lab1"), value), check: wantSetting(testText, "lab1", value, fromTen)},
		{name: "tenant, a value the setting refuses", as: f.lab1Admin, call: setSetting(testRetention, tenantScope("lab1"), "soon"), code: connect.CodeInvalidArgument, msg: "setting.value", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "no such setting", as: f.instAdmin, call: setSetting("no.such", installation(), "1"), code: connect.CodeNotFound, msg: "no setting"},
		{name: "no such setting on a tenant", as: f.lab1Admin, call: setSetting("no.such", tenantScope("lab1"), "1"), code: connect.CodeNotFound, audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "value at 64 KiB", as: f.instAdmin, call: setSetting(testText, installation(), strings.Repeat("v", 64<<10))},
		{name: "value at 64 KiB and one", as: f.instAdmin, call: setSetting(testText, installation(), strings.Repeat("v", 64<<10+1)), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "an empty value the setting takes", as: f.instAdmin, call: setSetting(testText, installation(), ""), check: wantSetting(testText, "installation", "", fromInst)},
		{name: "reads per caller at 1", as: f.instAdmin, call: setSetting("read.per-caller", installation(), "1")},
		{name: "reads per caller at 1024 above the server's", as: f.instAdmin, call: setSetting("read.per-caller", installation(), "1024"), code: connect.CodeFailedPrecondition, msg: "read.per-caller"},
		{name: "reads per caller at 1025", as: f.instAdmin, call: setSetting("read.per-caller", installation(), "1025"), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "reads per caller at 0", as: f.instAdmin, call: setSetting("read.per-caller", installation(), "0"), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "reads per server at 15", as: f.instAdmin, call: setSetting("read.per-server", installation(), "15"), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "reads per server at 65537", as: f.instAdmin, call: setSetting("read.per-server", installation(), "65537"), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "reads per server at 65536", as: f.instAdmin, call: setSetting("read.per-server", installation(), "65536")},
		{name: "reads per caller at 1024 within the server's", as: f.instAdmin, call: setSetting("read.per-caller", installation(), "1024")},
		{name: "reads per server below the caller's", as: f.instAdmin, call: setSetting("read.per-server", installation(), "1023"), code: connect.CodeFailedPrecondition, msg: "read.per-server"},
		{name: "reads per server unset below the caller's", as: f.instAdmin, call: unsetSetting("read.per-server", installation()), code: connect.CodeFailedPrecondition, msg: "read.per-server"},
		{name: "reads per caller unset", as: f.instAdmin, call: unsetSetting("read.per-caller", installation()), check: wantSetting("read.per-caller", "installation", "16", builtIn)},
		{name: "reads per server unset", as: f.instAdmin, call: unsetSetting("read.per-server", installation()), check: wantSetting("read.per-server", "installation", "256", builtIn)},
		{name: "reads per caller on a tenant", as: f.lab1Admin, call: setSetting("read.per-caller", tenantScope("lab1"), "8"), code: connect.CodeInvalidArgument, msg: "setting.scope", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "no setting", as: f.instAdmin, call: func(c clients) outcome { return call(c.settings.SetSetting, &v1.SetSettingRequest{}) }, code: connect.CodeInvalidArgument, msg: "setting"},
		{name: "a malformed name", as: f.instAdmin, call: setSetting("A", installation(), "1"), code: connect.CodeInvalidArgument, msg: "setting.name"},
		{name: "no scope", as: f.instAdmin, call: setSetting(lifetime, nil, "1d"), code: connect.CodeInvalidArgument, msg: "setting.scope"},
		{name: "no such tenant, may list", as: local, call: setSetting(testRetention, tenantScope("nolab"), "1d"), code: connect.CodeNotFound},
		{name: "no such tenant, no right to know", as: f.lab1Admin, call: setSetting(testRetention, tenantScope("nolab"), "1d"), code: connect.CodePermissionDenied},
		{name: "a tenant that is not ready", as: local, call: setSetting(testRetention, tenantScope("down"), "1d"), code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
	})
	// The value is in no entry of either log.
	for _, en := range append(f.masterLog(), f.tenantLog("lab1")...) {
		if strings.Contains(en.Reason+strings.Join(mapValues(en.Details), " ")+en.Object, value) {
			t.Errorf("an audit entry holds a setting's value: %+v", en)
		}
	}
	if v, err := f.master.Setting(ctx, lifetime); err != nil || v != "3650d" {
		t.Errorf("the master holds %q, %v", v, err)
	}
}

// testUnsetSetting guards UnsetSetting: admin on the scope; on a tenant, the
// installation's value answered; on the installation, the built-in default;
// a repeat that succeeds; a tenant scope refused for a setting a tenant may
// not override; a name the server does not know not found; the entry in the
// scope's log.
func testUnsetSetting(t *testing.T) {
	f := settingsFixture(t)
	call(f.as(f.lab1Admin).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: testRetention, Scope: tenantScope("lab1"), Value: "30d"}})
	call(f.as(f.instAdmin).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: testRetention, Scope: installation(), Value: "100d"}})
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: unsetSetting(testRetention, tenantScope("lab1")), code: connect.CodeUnauthenticated},
		{name: "tenant, just below the role", as: f.lab1Operator, call: unsetSetting(testRetention, tenantScope("lab1")), code: connect.CodePermissionDenied},
		{name: "tenant, at the role", as: f.lab1Admin, call: unsetSetting(testRetention, tenantScope("lab1")),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", object: testRetention},
			check: wantSetting(testRetention, "installation", "100d", fromInst)},
		{name: "the installation's value applies again", as: f.lab1Reader, call: getSetting(testRetention, tenantScope("lab1")), check: wantSetting(testRetention, "installation", "100d", fromInst)},
		{name: "tenant, repeated", as: f.lab1Admin, call: unsetSetting(testRetention, tenantScope("lab1")), check: wantSetting(testRetention, "installation", "100d", fromInst)},
		{name: "installation, just below the role", as: f.instOperator, call: unsetSetting(testRetention, installation()), code: connect.CodePermissionDenied},
		{name: "installation, at the role", as: f.instAdmin, call: unsetSetting(testRetention, installation()),
			audit: &wantAudit{outcome: master.OutcomeDone, object: testRetention, details: map[string]string{"scope": "installation"}},
			check: wantSetting(testRetention, "installation", "365d", builtIn)},
		{name: "the built-in default applies again", as: f.lab1Reader, call: getSetting(testRetention, tenantScope("lab1")), check: wantSetting(testRetention, "installation", "365d", builtIn)},
		{name: "installation, repeated", as: f.instAdmin, call: unsetSetting(testRetention, installation()), check: wantSetting(testRetention, "installation", "365d", builtIn)},
		{name: "a tenant scope for an installation setting", as: f.lab1Admin, call: unsetSetting(lifetime, tenantScope("lab1")), code: connect.CodeInvalidArgument, msg: "scope", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "no such setting", as: f.lab1Admin, call: unsetSetting("no.such", tenantScope("lab1")), code: connect.CodeNotFound},
		{name: "a malformed name", as: f.lab1Admin, call: unsetSetting("", tenantScope("lab1")), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "no scope", as: f.instAdmin, call: unsetSetting(testRetention, nil), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "no such tenant, no right to know", as: f.lab1Admin, call: unsetSetting(testRetention, tenantScope("nolab")), code: connect.CodePermissionDenied},
		{name: "no such tenant, may list", as: local, call: unsetSetting(testRetention, tenantScope("nolab")), code: connect.CodeNotFound},
		{name: "a tenant that is not ready", as: local, call: unsetSetting(testRetention, tenantScope("down")), code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
	})
}

// testListSettings guards ListSettings: reader on the scope; on the
// installation every setting the server knows with its value and source, on
// a tenant only its overrides of settings the server knows; paging and a
// token refused with another scope.
func testListSettings(t *testing.T) {
	f := settingsFixture(t)
	call(f.as(f.instAdmin).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: testRetention, Scope: installation(), Value: "100d"}})
	call(f.as(f.lab1Admin).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: testText, Scope: tenantScope("lab1"), Value: "x"}})
	// An override of a name the server no longer knows, written straight into
	// the tenant's database as an older version would have left it.
	if st, err := f.registry.Store("lab1"); err != nil {
		t.Fatal(err)
	} else if err := st.SetSetting(ctx, "gone.setting", "old", f.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
	list := func(s *v1.Scope, size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.settings.ListSettings, &v1.ListSettingsRequest{Scope: s, PageSize: size, PageToken: token})
		}
	}
	names := func(o outcome) string {
		var out []string
		for _, s := range o.res.(*v1.ListSettingsResponse).GetSettings() {
			out = append(out, s.GetName()+"="+s.GetValue()+"@"+s.GetScope().GetTenant()+"/"+strings.TrimPrefix(s.GetSource().String(), "SETTING_SOURCE_"))
		}
		return strings.Join(out, ",")
	}
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(installation(), 0, ""), code: connect.CodeUnauthenticated},
		{name: "installation, just below the role", as: f.noGrant, call: list(installation(), 0, ""), code: connect.CodePermissionDenied},
		{name: "installation, at the role", as: f.instReader, call: list(installation(), 0, ""),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"scope": "installation"}},
			check: func(t *testing.T, o outcome) {
				want := "access.allowed-networks=@/BUILT_IN,access.block-time=15m@/BUILT_IN,access.denied-networks=@/BUILT_IN,access.failure-limit=10@/BUILT_IN," +
					"access.failure-window=1m@/BUILT_IN,access.forwarded-header=x-forwarded-for@/BUILT_IN,access.tenant-networks=@/BUILT_IN,access.trusted-proxies=@/BUILT_IN," +
					"api.cors-origins=@/BUILT_IN,audit.retention=0@/BUILT_IN,audit.seal-interval=10m@/BUILT_IN,backup.max-duration=6h@/BUILT_IN,correction.batch-max=10000@/BUILT_IN,deid.list-cache-bytes=256MiB@/BUILT_IN,deid.memory=2GiB@/BUILT_IN,deid.pixel-limit=1GiB@/BUILT_IN,delete.grace=7d@/BUILT_IN,events.retention=90d@/BUILT_IN," +
					"import.allowed-roots=@/BUILT_IN,import.max-dir-entries=200000@/BUILT_IN,import.max-units=4@/BUILT_IN,import.roots=@/BUILT_IN,import.server-max-units=8@/BUILT_IN," +
					"incoming.limit=0@/BUILT_IN,job.failures-kept=10000@/BUILT_IN,key.default-lifetime=90d@/BUILT_IN," +
					"key.expiry-warning=7d@/BUILT_IN,key.max-lifetime=3650d@/BUILT_IN,quarantine.warn-age=30d@/BUILT_IN,quarantine.warn-bytes=0@/BUILT_IN," +
					"quota.bytes=@/BUILT_IN,quota.instances=@/BUILT_IN,quota.warn=90%@/BUILT_IN," +
					"read.per-caller=16@/BUILT_IN,read.per-server=256@/BUILT_IN,recall.wait=48h@/BUILT_IN,receiver.allow-as-received=false@/BUILT_IN," +
					"receiver.bind-addresses=@/BUILT_IN,receiver.header-limit=64MiB@/BUILT_IN,receiver.ports=@/BUILT_IN," +
					"replica.lag-alert=1h@/BUILT_IN,replica.rate=0@/BUILT_IN," +
					"replica.repair=manual@/BUILT_IN,replica.repair-lease=1m@/BUILT_IN,retention.max-clock-step=2d@/BUILT_IN,retention.max-per-pass=1000@/BUILT_IN," +
					"retention.shorten-notice=30d@/BUILT_IN,scrub.archive-sample=0%@/BUILT_IN,scrub.interval=30d@/BUILT_IN,scrub.rate=64MiB@/BUILT_IN,sender.allow-plain-http=false@/BUILT_IN," +
					"sender.destinations=@/BUILT_IN,sender.max-associations=32@/BUILT_IN,sender.tenant-max-associations=8@/BUILT_IN,storage.directory-roots=@/BUILT_IN," +
					"storage.reserve=10%,10GiB@/BUILT_IN,storage.s3-endpoints=@/BUILT_IN,test.allotted=@/BUILT_IN,test.retention=100d@/INSTALLATION,test.text=@/BUILT_IN"
				if got := names(o); got != want {
					t.Errorf("got %q, want %q", got, want)
				}
			}},
		{name: "installation, a page of 15", as: f.instReader, call: list(installation(), 15, ""),
			check: func(t *testing.T, o outcome) {
				token = o.res.(*v1.ListSettingsResponse).GetNextPageToken()
				if got := names(o); !strings.HasPrefix(got, "access.allowed-networks=") || !strings.HasSuffix(got, "deid.memory=2GiB@/BUILT_IN") || token == "" {
					t.Errorf("got %q, token %q", got, token)
				}
			}},
		{name: "installation, the next page", as: f.instReader, call: func(c clients) outcome { return list(installation(), 15, token)(c) },
			check: func(t *testing.T, o outcome) {
				if got, next := names(o), o.res.(*v1.ListSettingsResponse).GetNextPageToken(); got != "deid.pixel-limit=1GiB@/BUILT_IN,delete.grace=7d@/BUILT_IN,events.retention=90d@/BUILT_IN,import.allowed-roots=@/BUILT_IN,import.max-dir-entries=200000@/BUILT_IN,import.max-units=4@/BUILT_IN,"+
					"import.roots=@/BUILT_IN,import.server-max-units=8@/BUILT_IN,incoming.limit=0@/BUILT_IN,"+
					"job.failures-kept=10000@/BUILT_IN,key.default-lifetime=90d@/BUILT_IN,"+
					"key.expiry-warning=7d@/BUILT_IN,key.max-lifetime=3650d@/BUILT_IN,quarantine.warn-age=30d@/BUILT_IN,quarantine.warn-bytes=0@/BUILT_IN" || next == "" {
					t.Errorf("got %q, token %q", got, next)
				}
			}},
		{name: "a token with another scope", as: f.lab1Reader, call: func(c clients) outcome { return list(tenantScope("lab1"), 2, token)(c) },
			code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "tenant, just below the role", as: f.noGrant, call: list(tenantScope("lab1"), 0, ""), code: connect.CodePermissionDenied},
		{name: "tenant, at the role, overrides only", as: f.lab1Reader, call: list(tenantScope("lab1"), 0, ""),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"scope": "tenant/lab1"}},
			check: func(t *testing.T, o outcome) {
				if got := names(o); got != "test.text=x@lab1/TENANT" {
					t.Errorf("got %q", got)
				}
			}},
		{name: "page_size at 1001", as: f.instReader, call: list(installation(), 1001, ""), code: connect.CodeInvalidArgument, msg: "page_size", audit: failed("page_size")},
		{name: "tenant, page_size at 1001", as: f.lab1Reader, call: list(tenantScope("lab1"), 1001, ""), code: connect.CodeInvalidArgument, msg: "page_size", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "page_token at 1025 bytes", as: f.instReader, call: list(installation(), 0, strings.Repeat("s", 1025)), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "no scope", as: f.instReader, call: list(nil, 0, ""), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "no such tenant, may list", as: local, call: list(tenantScope("nolab"), 0, ""), code: connect.CodeNotFound},
		{name: "no such tenant, no right to know", as: f.lab1Reader, call: list(tenantScope("nolab"), 0, ""), code: connect.CodePermissionDenied},
		{name: "a tenant that is not ready", as: local, call: list(tenantScope("down"), 0, ""), code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
	})
}
