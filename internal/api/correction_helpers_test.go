package api_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// envTenants is the installation's tenants as the corrections' engines
// reach them: names from the master, databases from the registry.
type envTenants struct{ e *env }

// Names reads every tenant's name.
func (t envTenants) Names(ctx context.Context) ([]string, error) {
	rows, _, err := t.e.master.ListTenants(ctx, master.Page{Limit: master.MaxPageLimit})
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, err
}

// Store returns a tenant's database.
func (t envTenants) Store(name string) (tenant.Store, error) { return t.e.registry.Store(name) }

// newTestEngines returns the corrections' engines the test installation's
// handlers act through: each tenant's storages opened by the rules the
// handlers apply, its delete.grace read from its settings, the server's
// clock. Their pass is not started: what a request leaves to the
// background runs, and a test asks for the rest.
func newTestEngines(t testing.TB, e *env) *correct.Engines {
	t.Helper()
	values := settingvalue.Reader{Master: e.faulty}
	opener := place.Opener{Values: values, Keys: e.keys, StaleUploadAge: -1}
	key, err := e.keys.Derive(secret.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	es, err := correct.NewEngines(correct.EnginesConfig{
		Tenants: envTenants{e},
		Configure: func(ctx context.Context, name string, cs tenant.Corrections) (correct.Config, func() error, error) {
			st := cs.(tenant.Store)
			p := cs.(tenant.Pipeline)
			var mu sync.Mutex
			open := map[string]blob.Store{}
			tr, err := e.master.TenantByName(ctx, name)
			if err != nil {
				return correct.Config{}, nil, err
			}
			cfg := correct.Config{
				TenantID:   tr.ID,
				Keys:       e.keys,
				Grace:      values.OnTenant(cs.(settingvalue.Source)).DeleteGrace,
				ConfirmKey: key,
				OpenStorage: func(ctx context.Context, storage string) (blob.Store, error) {
					mu.Lock()
					defer mu.Unlock()
					if b, ok := open[storage]; ok {
						return b, nil
					}
					row, err := e.master.TenantByName(ctx, name)
					if err != nil {
						return nil, err
					}
					s, err := p.Storage(ctx, storage)
					if err != nil {
						return nil, err
					}
					b, err := opener.Open(ctx, place.TenantOf(row.ID, name, st), s)
					if err != nil {
						return nil, err
					}
					open[storage] = b
					return b, nil
				},
			}
			return cfg, func() error {
				mu.Lock()
				defer mu.Unlock()
				for _, b := range open {
					_ = b.Close()
				}
				return nil
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = es.Stop(ctx)
	})
	return es
}

// testReplaceSources makes a replace's file through lab1's running
// pipeline, once a test runs one (env.pipe).
type testReplaceSources struct{ e *env }

// ReplaceSource answers through the running pipeline, or unavailable.
func (s testReplaceSources) ReplaceSource(ctx context.Context, _ string, q tenant.QuarantinedFile) (receive.ReplaceFile, error) {
	p := s.e.pipe.Load()
	if p == nil {
		return receive.ReplaceFile{}, fault.New(fault.Unavailable, "no pipeline runs")
	}
	return p.ReplaceSource(ctx, q)
}

// sendAgain hands b to lab1's running pipeline, as a sender sending a file
// again, and waits until the quarantine holds a file of reason, which it
// returns.
func (a *archive) sendAgain(t *testing.T, b []byte, reason v1.QuarantineReason) *v1.QuarantinedFile {
	t.Helper()
	out := a.pipe.Accept(context.Background(), receive.Arrival{Receiver: "scanner1", Body: bytes.NewReader(b),
		Caller: tenant.Caller{CallingAETitle: "PRISMA1", Address: "10.20.0.31:50288"}})
	if out.Status != receive.Received {
		t.Fatalf("a file sent again was %v (%s)", out.Status, out.Reason)
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		a.wait.clock.advance(time.Second)
		res, err := a.f.as(local).quarantine.ListQuarantinedFiles(context.Background(), &v1.ListQuarantinedFilesRequest{Tenant: "lab1", Reason: reason,
			State: v1.QuarantineState_QUARANTINE_STATE_HELD})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.GetFiles()) > 0 {
			return res.GetFiles()[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no file was quarantined as %v", reason)
	return nil
}

// settle waits until no reception of lab1 is in progress, moving the
// pipeline's clock: a delete's preview counts the receptions that ended,
// so one ending between a preview and its delete would change the token.
func (a *archive) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		going, _, err := a.p.ListReceptions(context.Background(), tenant.ReceptionFilter{InProgress: true}, master.Page{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(going) == 0 {
			return
		}
		a.wait.clock.advance(10 * time.Second)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a reception of lab1 did not end")
}

// waitCorrection polls lab1's correction id until cond holds, failing the
// test after half a minute.
func (f *fixture) waitCorrection(t *testing.T, id string, cond func(*v1.Correction) bool) *v1.Correction {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := f.as(local).corrections.GetCorrection(context.Background(), &v1.GetCorrectionRequest{Tenant: "lab1", Id: id})
		if err != nil {
			t.Fatal(err)
		}
		if cond(res.GetCorrection()) {
			return res.GetCorrection()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the correction is %v", res.GetCorrection())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
