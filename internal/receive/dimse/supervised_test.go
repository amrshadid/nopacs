package dimse_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/dimse"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// oneTenant is the supervisor's view of an archive's one tenant.
type oneTenant struct{ a *archive }

func (o oneTenant) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }
func (o oneTenant) Store(string) (tenant.Store, error)      { return o.a.store, nil }

// noSettings is an installation with no setting stored.
type noSettings struct{}

func (noSettings) Setting(context.Context, string) (string, error) {
	return "", fault.New(fault.NotFound, "no such setting")
}

// freePort returns a port no one listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// TestTheSupervisorRunsRealListeners guards the supervisor with the real
// listener and pipeline: a receiver whose port another program holds is
// down with that reason while another receiver receives a study; a change
// of the receiving one's AE title through the tenant's database restarts
// its listener, which then answers to the new title only.
func TestTheSupervisorRunsRealListeners(t *testing.T) {
	py := python(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	port := freePort(t)
	r := row(func(r *tenant.Receiver) { r.DIMSE.Port = port; r.DIMSE.BindAddress = "127.0.0.1" })
	a := newArchive(t, r)
	blocked, err := a.store.AddReceiver(a.ctx, row(func(r *tenant.Receiver) {
		r.Name = "blocked"
		r.DIMSE.Port = held.Addr().(*net.TCPAddr).Port
		r.DIMSE.BindAddress = "127.0.0.1"
	}), t0, a.audit("b"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sup, err := supervise.New(supervise.Config{
		Tenants: oneTenant{a}, Installation: noSettings{}, Interval: time.Hour, MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond,
		StartPipeline: func(context.Context, string, tenant.Pipeline, supervise.Gates) (supervise.Pipeline, error) {
			return a.p, nil
		},
		NewListener: func(spec supervise.ListenerSpec) (supervise.Listener, error) {
			return dimse.New(dimse.Config{Receiver: spec.Receiver, BindAddress: spec.BindAddress, Sink: spec.Sink,
				Refusal: dimse.Refusal(spec.Refusal), Report: dimse.Report(spec.Report)})
		},
		OpenStorage: func(context.Context, string, tenant.Pipeline, string) (blob.Store, error) {
			return nil, fault.New(fault.Unavailable, "none")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Stop(context.Background()) })
	stored := func(name string) tenant.Receiver {
		row, err := a.store.Receiver(a.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	waitFor(t, "dimse up and blocked down", func() bool {
		up := sup.Health("lab", stored("dimse")).State == tenant.ReceiverUp
		h := sup.Health("lab", blocked)
		return up && h.State == tenant.ReceiverDown && h.Reason == tenant.DownAddressInUse
	})
	s := &started{host: "127.0.0.1", port: strconv.Itoa(port)}
	paths := make([]string, len(studyFiles))
	for i, f := range studyFiles {
		paths[i] = corpusPath(f)
	}
	res := scu(t, py, s, "store", "SCANNER", "ARCHIVE", paths...)
	for _, st := range res.Statuses {
		if !isStatus(st, statusSuccess) {
			t.Fatalf("a store while another receiver is down: %+v", res)
		}
	}
	if rs := a.settle(1); rs[0].Counts.FilesArchived != int64(len(studyFiles)) {
		t.Fatalf("reception %+v", rs[0])
	}

	changed, err := a.store.SetReceiver(a.ctx, tenant.Receiver{Name: "dimse", DIMSE: tenant.DIMSEReceiver{AETitle: "RENAMED"}},
		[]tenant.ReceiverField{tenant.FieldDIMSEAETitle}, t0.Add(time.Minute), a.audit("c"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := sup.Health("lab", changed); h.State != tenant.ReceiverStarting {
		t.Fatalf("health right after the change %+v", h)
	}
	waitFor(t, "the listener to answer to its new title", func() bool {
		return scu(t, py, s, "echo", "SCANNER", "RENAMED").Assoc == "accepted"
	})
	if res := scu(t, py, s, "echo", "SCANNER", "ARCHIVE"); res.Assoc != "rejected" || res.RJ[2] != rjCalledNotKnown {
		t.Fatalf("the old title after the change: %+v", res)
	}
	if h := sup.Health("lab", stored("blocked")); h.Reason != tenant.DownAddressInUse {
		t.Fatalf("the blocked receiver's health %+v", h)
	}
}
