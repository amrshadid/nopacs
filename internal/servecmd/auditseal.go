package servecmd

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/auditseal"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
)

// newSealer returns the sealing pass of every audit log of the installation
// (package auditseal), signed with the audit key derived from the key
// file's newest version, run every audit.seal-interval and once more at
// stop, after the server's drain (Built.AfterDrain).
func newSealer(d Deps, reg *registry.Registry) (*auditseal.Sealer, error) {
	id, err := server.ReadInstallationID(d.DataDir)
	if err != nil {
		return nil, err
	}
	derived, err := d.Keys.Derive(secret.AuditSeal)
	if err != nil {
		return nil, err
	}
	signer, err := derived.Signer()
	if err != nil {
		return nil, err
	}
	values := settingvalue.Reader{Master: d.Master}
	return auditseal.New(auditseal.Config{
		Installation: id,
		Master:       d.Master,
		Tenants:      sealedTenants{master: d.Master, registry: reg},
		Signer:       signer,
		Interval:     values.AuditSealInterval,
		Retention:    values.AuditRetention,
		NewID:        server.NewID,
		Log:          d.Log,
	})
}

// sealCounters returns the sealing passes' failures as metrics, one counter
// a kind: a rolled-back log (an anchor kept because the log no longer
// descends from it) is tampering or a restore, an unencodable entry is a
// row put in by hand, and the rest is a database that did not answer.
func sealCounters(s auditseal.Stats) []obs.Counter {
	return []obs.Counter{
		{Name: "nopacs_audit_seal_failures_rolled_back_total", Help: "Anchors a sealing pass kept because the log they name no longer descends from them.", Value: s.RolledBack},
		{Name: "nopacs_audit_seal_failures_unencodable_total", Help: "Sealing passes held back by an unsealed audit entry that cannot be encoded.", Value: s.Unencodable},
		{Name: "nopacs_audit_seal_failures_other_total", Help: "Audit logs a sealing pass could not seal, anchor or check for another reason, mostly a database that did not answer.", Value: s.Failed},
	}
}

// sealGauges returns the age of the oldest audit entry the last pass left
// unsealed, 0 for none: past two intervals, the passes are not sealing a
// log.
func sealGauges(s auditseal.Stats, now time.Time) []obs.Gauge {
	age := 0.0
	if !s.OldestUnsealed.IsZero() {
		age = max(now.Sub(s.OldestUnsealed).Seconds(), 0)
	}
	return []obs.Gauge{{Name: "nopacs_audit_unsealed_oldest_age_seconds", Help: "Age of the oldest audit entry the last sealing pass left unsealed, 0 for none.", Value: age}}
}

// sealedTenants is the sealing pass's view of the tenants: their ids and
// names from the master, each one's open store from the registry.
type sealedTenants struct {
	master   master.Store
	registry *registry.Registry
}

var _ auditseal.Tenants = sealedTenants{}

// List implements auditseal.Tenants, reading the master page after page.
func (s sealedTenants) List(ctx context.Context) ([]auditseal.TenantLog, error) {
	var out []auditseal.TenantLog
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		batch, next, err := s.master.ListTenants(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, t := range batch {
			out = append(out, auditseal.TenantLog{ID: t.ID, Name: t.Name})
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
}

// Log implements auditseal.Tenants.
func (s sealedTenants) Log(_ context.Context, name string) (master.AuditSeals, error) {
	return s.registry.Store(name)
}
