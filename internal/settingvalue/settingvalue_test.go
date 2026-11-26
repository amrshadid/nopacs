package settingvalue_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/settingvalue"
)

// stored is a store of settings: a value per name, and an error for every
// read when fail is set.
type stored struct {
	v    map[string]string
	fail error
}

func (s stored) Setting(_ context.Context, name string) (string, error) {
	if s.fail != nil {
		return "", s.fail
	}
	if v, ok := s.v[name]; ok {
		return v, nil
	}
	return "", fault.New(fault.NotFound, "no such setting")
}

// TestTheValueThatAppliesIsReadInOrder guards the order of the values: a
// tenant's own, then the installation's, then the built-in default; and a
// setting with no installation value goes from the tenant's straight to the
// default.
func TestTheValueThatAppliesIsReadInOrder(t *testing.T) {
	ctx := context.Background()
	master := stored{v: map[string]string{settings.IncomingLimit: "10GiB", settings.ReceiverPorts: "4000-4010"}}
	r := settingvalue.Reader{Master: master}
	none := stored{v: map[string]string{}}
	own := stored{v: map[string]string{settings.IncomingLimit: "1GiB", settings.ReceiverPorts: "5000"}}
	for _, c := range []struct {
		st   stored
		name string
		want string
	}{
		{own, settings.IncomingLimit, "1GiB"},
		{none, settings.IncomingLimit, "10GiB"},
		{none, settings.EventsRetention, "90d"},
		{own, settings.ReceiverPorts, "5000"},
		// receiver.ports has no installation value: what the master holds
		// under that name never applies to a tenant.
		{none, settings.ReceiverPorts, ""},
	} {
		got, err := r.Tenant(ctx, c.st, c.name)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	if got, err := r.Installation(ctx, settings.ReceiverHeaderLimit); err != nil || got != "64MiB" {
		t.Errorf("header limit %q, %v; want the default", got, err)
	}
	n, err := r.OnTenant(none).IncomingLimit(ctx)
	if err != nil || n != 10<<30 {
		t.Errorf("incoming limit %d, %v", n, err)
	}
}

// TestABrokenValueIsTheServersFault guards the check as a value is read: a
// stored value its rule refuses, or a name the list does not hold, is an
// Internal fault naming the setting and never the value; a store that does
// not answer is its own error, passed on.
func TestABrokenValueIsTheServersFault(t *testing.T) {
	ctx := context.Background()
	broken := "not-a-size-but-a-value-to-keep-out-of-errors"
	r := settingvalue.Reader{Master: stored{v: map[string]string{settings.ReceiverHeaderLimit: broken}}}
	_, err := r.HeaderLimit(ctx)
	if fault.KindOf(err) != fault.Internal || !strings.Contains(err.Error(), settings.ReceiverHeaderLimit) || strings.Contains(err.Error(), broken) {
		t.Fatalf("a broken value: %v", err)
	}
	if _, err := r.Tenant(ctx, stored{v: map[string]string{settings.IncomingLimit: broken}}, settings.IncomingLimit); fault.KindOf(err) != fault.Internal ||
		strings.Contains(err.Error(), broken) {
		t.Fatalf("a broken tenant value: %v", err)
	}
	if _, err := r.Installation(ctx, "no.such"); fault.KindOf(err) != fault.Internal {
		t.Fatalf("an unknown name: %v", err)
	}
	down := errors.New("the master does not answer")
	if _, err := (settingvalue.Reader{Master: stored{fail: down}}).Reserve(ctx); !errors.Is(err, down) {
		t.Fatalf("a store that fails: %v", err)
	}
}

// TestAChangeIsReadAtOnce guards that nothing is cached: a value changed in
// the store is the one the next read returns.
func TestAChangeIsReadAtOnce(t *testing.T) {
	ctx := context.Background()
	m := stored{v: map[string]string{}}
	r := settingvalue.Reader{Master: m}
	if n, _ := r.HeaderLimit(ctx); n != 64<<20 {
		t.Fatalf("default %d", n)
	}
	m.v[settings.ReceiverHeaderLimit] = "128MiB"
	if n, err := r.HeaderLimit(ctx); err != nil || n != 128<<20 {
		t.Fatalf("after a change %d, %v", n, err)
	}
	if o, err := r.CORSOrigins(ctx); err != nil || o.Any || len(o.List) != 0 {
		t.Fatalf("CORS origins by default %+v, %v; want none", o, err)
	}
	m.v[settings.APICORSOrigins] = "https://viewer.example.org"
	if o, err := r.CORSOrigins(ctx); err != nil || !o.Allows("https://viewer.example.org") || o.Allows("https://other.example.org") {
		t.Fatalf("CORS origins after a change %+v, %v", o, err)
	}
}

// TestAccessValues guards the typed readers of the access settings: the
// built-in defaults when nothing is stored, a stored value read, a tenant's
// networks over the installation's, and a stored value its rule refuses
// answered as the server's fault.
func TestAccessValues(t *testing.T) {
	ctx := context.Background()
	r := settingvalue.Reader{Master: stored{v: map[string]string{
		settings.AccessDeniedNetworks: "192.0.2.0/24", settings.AccessTenantNetworks: "10.0.0.0/8",
		settings.AccessFailureLimit: "3",
	}}}
	if n, err := r.Networks(ctx, settings.AccessDeniedNetworks); err != nil || n.String() != "192.0.2.0/24" {
		t.Errorf("denied %v, %v", n, err)
	}
	if n, err := r.Networks(ctx, settings.AccessTrustedProxies); err != nil || len(n) != 0 {
		t.Errorf("proxies %v, %v", n, err)
	}
	if v, err := r.FailureLimit(ctx); err != nil || v != 3 {
		t.Errorf("limit %v, %v", v, err)
	}
	if v, err := r.FailureWindow(ctx); err != nil || v.String() != "1m0s" {
		t.Errorf("window %v, %v", v, err)
	}
	if v, err := r.BlockTime(ctx); err != nil || v.String() != "15m0s" {
		t.Errorf("block %v, %v", v, err)
	}
	if v, err := r.KeyMaxLifetime(ctx); err != nil || v.Hours() != 3650*24 {
		t.Errorf("max lifetime %v, %v", v, err)
	}
	if v, err := r.KeyExpiryWarning(ctx); err != nil || v.Hours() != 7*24 {
		t.Errorf("warning %v, %v", v, err)
	}
	if n, err := r.OnTenant(stored{v: map[string]string{}}).TenantNetworks(ctx); err != nil || n.String() != "10.0.0.0/8" {
		t.Errorf("tenant without its own %v, %v", n, err)
	}
	if n, err := r.OnTenant(stored{v: map[string]string{settings.AccessTenantNetworks: "198.51.100.0/24"}}).TenantNetworks(ctx); err != nil || n.String() != "198.51.100.0/24" {
		t.Errorf("tenant with its own %v, %v", n, err)
	}
	broken := settingvalue.Reader{Master: stored{v: map[string]string{settings.AccessAllowedNetworks: "10.1.2.3/8"}}}
	if _, err := broken.Networks(ctx, settings.AccessAllowedNetworks); fault.KindOf(err) != fault.Internal {
		t.Errorf("a stored value its rule refuses: %v", err)
	}
}

// TestSenderValues guards the readers of the sender and job settings: the
// built-in defaults, a tenant's share over the installation's default, a
// tenant's own count of failures kept, and a stored destination list that
// its rule refuses answered as the server's fault.
func TestSenderValues(t *testing.T) {
	ctx := context.Background()
	r := settingvalue.Reader{Master: stored{v: map[string]string{
		settings.SenderDestinations: "pacs.lab.example ports=104", settings.SenderMaxAssociations: "64",
		settings.SenderTenantMaxAssociations: "4",
	}}}
	if d, err := r.SenderDestinations(ctx); err != nil || d.String() != "pacs.lab.example ports=104" {
		t.Errorf("destinations %v, %v", d, err)
	}
	if v, err := r.AllowPlainHTTP(ctx); err != nil || v {
		t.Errorf("plain http %v, %v: off by default", v, err)
	}
	if v, err := r.MaxAssociations(ctx); err != nil || v != 64 {
		t.Errorf("max associations %v, %v", v, err)
	}
	if v, err := r.OnTenant(stored{v: map[string]string{}}).MaxAssociations(ctx); err != nil || v != 4 {
		t.Errorf("a tenant without its own share takes the installation's: %v, %v", v, err)
	}
	if v, err := r.OnTenant(stored{v: map[string]string{settings.SenderTenantMaxAssociations: "2"}}).MaxAssociations(ctx); err != nil || v != 2 {
		t.Errorf("a tenant's own share: %v, %v", v, err)
	}
	if v, err := r.OnTenant(stored{v: map[string]string{}}).FailuresKept(ctx); err != nil || v != 10000 {
		t.Errorf("failures kept by default: %v, %v", v, err)
	}
	if v, err := r.OnTenant(stored{v: map[string]string{settings.JobFailuresKept: "500"}}).FailuresKept(ctx); err != nil || v != 500 {
		t.Errorf("a tenant's own failures kept: %v, %v", v, err)
	}
	broken := settingvalue.Reader{Master: stored{v: map[string]string{settings.SenderDestinations: "10.0.0.1/8"}}}
	if _, err := broken.SenderDestinations(ctx); fault.KindOf(err) != fault.Internal {
		t.Errorf("a stored value its rule refuses: %v", err)
	}
}

// TestCopyAndRecallValues guards the readers of the settings of replicas'
// copies, recalls and archive-tier scrubs: each built-in default when
// nothing is stored, and a tenant's own value over the installation's.
func TestCopyAndRecallValues(t *testing.T) {
	ctx := context.Background()
	r := settingvalue.Reader{Master: stored{v: map[string]string{settings.ReplicaRate: "64MiB"}}}
	none := r.OnTenant(stored{v: map[string]string{}})
	if v, err := none.LagAlert(ctx); err != nil || v.String() != "1h0m0s" {
		t.Errorf("lag alert by default: %v, %v", v, err)
	}
	if v, err := none.ReplicaRate(ctx); err != nil || v != 64<<20 {
		t.Errorf("replica rate from the installation: %v, %v", v, err)
	}
	if v, err := none.RecallWait(ctx); err != nil || v.String() != "48h0m0s" {
		t.Errorf("recall wait by default: %v, %v", v, err)
	}
	if v, err := none.ArchiveSample(ctx); err != nil || v != 0 {
		t.Errorf("archive sample by default: %v, %v", v, err)
	}
	own := r.OnTenant(stored{v: map[string]string{settings.ReplicaLagAlert: "5m", settings.ReplicaRate: "0",
		settings.RecallWait: "2h", settings.ScrubArchiveSample: "25%"}})
	if v, err := own.LagAlert(ctx); err != nil || v.String() != "5m0s" {
		t.Errorf("a tenant's own lag alert: %v, %v", v, err)
	}
	if v, err := own.ReplicaRate(ctx); err != nil || v != 0 {
		t.Errorf("a tenant's own unbounded rate: %v, %v", v, err)
	}
	if v, err := own.RecallWait(ctx); err != nil || v.String() != "2h0m0s" {
		t.Errorf("a tenant's own recall wait: %v, %v", v, err)
	}
	if v, err := own.ArchiveSample(ctx); err != nil || v != 0.25 {
		t.Errorf("a tenant's own archive sample: %v, %v", v, err)
	}
}
