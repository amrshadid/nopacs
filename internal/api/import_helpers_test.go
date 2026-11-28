package api_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/dirimport"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// fixtureTenants is the import worker's view of the fixture's tenants.
type fixtureTenants struct{ e *env }

// Names returns the tenants the tests make.
func (t fixtureTenants) Names(context.Context) ([]string, error) {
	return []string{"lab1", "lab2"}, nil
}

// Store returns a tenant's store from the registry.
func (t fixtureTenants) Store(name string) (tenant.Store, error) { return t.e.registry.Store(name) }

// holdingSink stands for the reception pipeline in the API's tests: it
// records every file an import hands it as held as it arrived, with its
// import arrival, as the pipeline holds a file whose label it cannot make;
// so the job's counts, its listed files and the quarantine's import fields
// are the store's own, without a pipeline.
type holdingSink struct {
	e    *env
	name string
}

// Accept holds the file and answers it received.
func (s holdingSink) Accept(ctx context.Context, a receive.Arrival) receive.Outcome {
	n, _ := io.Copy(io.Discard, a.Body)
	imp := *a.Import
	imp.Size = n
	p := s.e.pipelineOf(s.name)
	q := tenant.QuarantinedFile{ID: s.e.newID(), Reason: tenant.QuarantineBadLabel,
		Details: map[string]string{"label": "subject", "fault": "empty"}, Receiver: a.Receiver, Size: n,
		Quarantined: time.Now().UTC(), Import: &imp}
	if _, err := p.HoldFiles(ctx, []tenant.QuarantinedFile{q}, nil); err != nil {
		return receive.Outcome{Status: receive.Refused, Reason: "test: the file could not be held"}
	}
	return receive.Outcome{Status: receive.Received}
}

// runImports starts the import worker over the fixture's tenants, its
// roots found by the server's rules (place.ImportRoots) and its files held
// by holdingSink, and stops it at the test's end.
func (e *env) runImports(t *testing.T) {
	t.Helper()
	values := settingvalue.Reader{Master: e.master}
	s, err := dirimport.New(dirimport.Config{
		Tenants: fixtureTenants{e}, Installation: values,
		TenantSettings: func(st tenant.Store) dirimport.TenantSettings { return values.OnTenant(st) },
		Roots: place.ImportRoots{Values: values,
			Allotted: func(ctx context.Context, st tenant.Store) ([]string, error) {
				return values.OnTenant(st).ImportAllowedRoots(ctx)
			}},
		Sink:     func(name string) receive.Sink { return holdingSink{e: e, name: name} },
		Interval: 100 * time.Millisecond, PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
}
