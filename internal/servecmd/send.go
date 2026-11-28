package servecmd

import (
	"context"
	"sync"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/clock"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deidjob"
	"github.com/amrshadid/nopacs/internal/dirimport"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/forward"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/rebuild"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/scrub"
	"github.com/amrshadid/nopacs/internal/send"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// newSending returns the supervisor of every tenant's senders, over the
// registry's tenants. Its reads of archived bytes count in reads, the
// server's one count, so the bound on reads open covers the API's and the
// senders' together, and those a replica served count in counts. Storages
// are opened as the API opens them for a read; a stow sender's credential is opened with the installation key
// under the context it was sealed with (place.CredentialContext).
func newSending(d Deps, reg *registry.Registry, wake func(tenantName string), reads *checkedread.Slots, counts *checkedread.Counts) (*send.Supervisor, error) {
	values := settingvalue.Reader{Master: d.Master}
	opener := place.Opener{Values: values, Keys: d.Keys, StaleUploadAge: -1}
	tenantID := func(ctx context.Context, name string) (string, error) {
		row, err := d.Master.TenantByName(ctx, name)
		return row.ID, err
	}
	return send.New(send.Config{
		Tenants:        registryTenants{master: d.Master, registry: reg},
		Installation:   values,
		TenantSettings: func(st tenant.Store) send.TenantSettings { return values.OnTenant(st) },
		OpenStorages: func(name string, p tenant.Pipeline) send.Storages {
			return checkedread.NewStores(opener, tenantID, p, name).WithCounts(counts)
		},
		Credential: func(ctx context.Context, tenantName, sender string, sealed []byte) (string, error) {
			id, err := tenantID(ctx, tenantName)
			if err != nil {
				return "", err
			}
			plain, err := d.Keys.Open(place.CredentialContext(id, sender), sealed)
			if err != nil {
				return "", fault.Wrap(fault.Internal, err, "the sender's sealed credential does not open with the installation key")
			}
			return string(plain), nil
		},
		Reads:  reads,
		Now:    clock.Now,
		TLSDir: func(tenantName string) string { return server.TLSDir(d.DataDir, tenantName) },
		Wake:   wake,
		Logger: d.Log,
		Copies: destinationCopies(values, reg),
	})
}

// destinationCopies returns the makers of the copies a forward rule's
// destination profile asks for, one per tenant, kept so each compiles a
// profile version once: under the tenant's keys of de-identification, as
// its receptions, within the reception's bounds (the header limit, the
// tenant's pixel limit) and checked after as a reception's file is.
func destinationCopies(values settingvalue.Reader, reg *registry.Registry) func(string, tenant.Pipeline) send.Copier {
	var mu sync.Mutex
	made := map[string]*forward.Copies{}
	return func(name string, p tenant.Pipeline) send.Copier {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := made[name]; ok {
			return c
		}
		c := &forward.Copies{
			Profiles: p,
			Keys:     func(ctx context.Context) (engine.Keys, error) { return reg.DeidKeys(ctx, name) },
			Options: func(ctx context.Context) (receive.TrialOptions, error) {
				headers, err := values.HeaderLimit(ctx)
				if err != nil {
					return receive.TrialOptions{}, err
				}
				src, ok := p.(settingvalue.Source)
				if !ok {
					return receive.TrialOptions{}, fault.New(fault.Internal, "the tenant's database keeps no settings")
				}
				pixels, err := values.OnTenant(src).PixelLimit(ctx)
				if err != nil {
					return receive.TrialOptions{}, err
				}
				return receive.TrialOptions{HeaderLimit: headers, PixelLimit: pixels,
					Check: func(n int64) receive.Checker { return check.New(check.Options{MaxFileBytes: n}) }}, nil
			},
		}
		made[name] = c
		return c
	}
}

// newDeidJobs returns the supervisor of every tenant's deid jobs, over the
// registry's tenants. Its reads of archived bytes count in reads, the
// server's one count, and those a replica served in counts; storages are
// opened as the senders open them; a
// check reads the tenant's UID key from the registry, as a reception does.
func newDeidJobs(d Deps, reg *registry.Registry, wake func(tenantName string), reads *checkedread.Slots, counts *checkedread.Counts, memory check.Memory) (*deidjob.Supervisor, error) {
	values := settingvalue.Reader{Master: d.Master}
	opener := place.Opener{Values: values, Keys: d.Keys, StaleUploadAge: -1}
	tenantID := func(ctx context.Context, name string) (string, error) {
		row, err := d.Master.TenantByName(ctx, name)
		return row.ID, err
	}
	return deidjob.New(deidjob.Config{
		Tenants:        registryTenants{master: d.Master, registry: reg},
		Installation:   values,
		TenantSettings: func(st tenant.Store) deidjob.TenantSettings { return values.OnTenant(st) },
		OpenStorages: func(name string, p tenant.Pipeline) deidjob.Storages {
			return checkedread.NewStores(opener, tenantID, p, name).WithCounts(counts)
		},
		UIDKey: reg.UIDKey,
		Reads:  reads,
		Now:    clock.Now,
		Memory: memory,
		Wake:   wake,
		Logger: d.Log,
	})
}

// senderHealths answers the API's question of a sender's health from the
// send supervisor, and passes the API's notice of a change of a tenant's
// senders or jobs to it and to the supervisors of deid jobs, of scrubs and
// of imports.
type senderHealths struct {
	s        *send.Supervisor
	deid     *deidjob.Supervisor
	scrub    *scrub.Supervisor
	imports  *dirimport.Supervisor
	rebuilds *rebuild.Supervisor
}

var (
	_ api.SenderHealths = senderHealths{}
	_ api.SenderChanges = senderHealths{}
	_ api.SenderTester  = senderTests{}
)

// SendersChanged makes the supervisor read the tenant's senders, and its
// idle workers look for work, at once; and the tenant's deid jobs and
// scrubs be looked for at once.
func (h senderHealths) SendersChanged(tenantName string) {
	h.s.Poke(tenantName)
	if h.deid != nil {
		h.deid.Poke(tenantName)
	}
	if h.scrub != nil {
		h.scrub.Poke(tenantName)
	}
	if h.imports != nil {
		// A job made, canceled or retried: the import units are looked for
		// at once (dirimport.Supervisor.Poke).
		h.imports.Poke(tenantName)
	}
	if h.rebuilds != nil {
		h.rebuilds.Poke(tenantName)
	}
}

// SenderHealth returns the supervisor's health of the sender. One the
// supervisor has not read yet (just added, or its tenant just ready) is
// down when disabled and unknown otherwise, as the contract says of a
// sender the server has not connected to.
func (h senderHealths) SenderHealth(tenantName string, s tenant.Sender) api.SenderHealth {
	got, ok := h.s.Health(tenantName, s.Name)
	if !ok {
		if !s.Enabled {
			return api.SenderHealth{State: tenant.SenderDown, DownReason: tenant.SenderDownDisabled, Since: s.Updated}
		}
		return api.SenderHealth{State: tenant.SenderUnknown, Since: s.Updated}
	}
	return api.SenderHealth{State: got.State, DownReason: got.DownReason, Fault: got.Fault, Detail: got.Detail, Since: got.Since,
		FailedConnections: got.FailedConnections, NextTry: got.NextTry, LastSuccess: got.LastSuccess,
		OpenAssociations: h.s.OpenAssociations(tenantName, s.Name)}
}

// senderTests runs the API's TestSender through the send supervisor,
// which connects as a send would.
type senderTests struct{ s *send.Supervisor }

// TestSender runs the steps and gives them in the API's terms.
func (t senderTests) TestSender(ctx context.Context, tenantName string, s tenant.Sender) ([]api.SenderTestResult, error) {
	steps, err := t.s.TestSender(ctx, tenantName, s)
	if err != nil {
		return nil, err
	}
	out := make([]api.SenderTestResult, len(steps))
	for i, st := range steps {
		out[i] = api.SenderTestResult{Step: api.SenderTestStep(st.Step), Outcome: api.SenderTestOutcome(st.Outcome),
			Duration: st.Duration, Fault: st.Fault, Detail: st.Detail}
	}
	return out, nil
}
