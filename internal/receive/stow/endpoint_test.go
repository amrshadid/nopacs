package stow

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestDirectoryFindsTheReceiverOfAKey guards that a request reaches only
// the receiver that lists its key: a key no running receiver lists, the
// empty id of the local operator and another tenant's receiver find
// nothing, and a stopped receiver is no longer found.
func TestDirectoryFindsTheReceiverOfAKey(t *testing.T) {
	r := newRig(t, 1<<20, 1<<24, nil)
	other, err := New(Config{Tenant: "lab2", Sink: r.sink, Directory: r.dir,
		Receiver: tenant.Receiver{Name: "web2", Type: tenant.ReceiverSTOW, STOW: tenant.STOWReceiver{Keys: []string{"key-b"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ep, ok := r.dir.Find("lab1", "key-a"); !ok || ep != r.ep {
		t.Errorf("key-a in lab1 found %v %v", ep, ok)
	}
	for _, c := range []struct{ tenant, key string }{{"lab1", "key-b"}, {"lab1", ""}, {"lab2", "key-a"}, {"lab3", "key-a"}} {
		if _, ok := r.dir.Find(c.tenant, c.key); ok {
			t.Errorf("Find(%q, %q) found a receiver", c.tenant, c.key)
		}
	}
	if err := other.Start(context.Background()); err == nil {
		t.Error("a second Start was taken")
	}
	_ = other.Stop(context.Background())
	if _, ok := r.dir.Find("lab2", "key-b"); ok {
		t.Error("a stopped receiver is still found")
	}
	if _, err := New(Config{Tenant: "lab1", Sink: r.sink, Directory: r.dir, Receiver: tenant.Receiver{Type: tenant.ReceiverDIMSE}}); err == nil {
		t.Error("an endpoint was made of a DIMSE receiver")
	}
}

// blockingSink holds every file until its request's context ends, as a
// pipeline writing to a disk that does not answer would.
type blockingSink struct{ entered chan struct{} }

// Accept implements receive.Sink.
func (s blockingSink) Accept(ctx context.Context, _ receive.Arrival) receive.Outcome {
	s.entered <- struct{}{}
	<-ctx.Done()
	return receive.Outcome{Status: receive.Refused, Reason: "canceled"}
}

// TestStopDrainsThenCuts guards Stop: a request in flight is let run for
// the drain bound, then cut, its part not kept; Stop returns once it has
// ended, and a request after Stop is refused 503.
func TestStopDrainsThenCuts(t *testing.T) {
	sink := blockingSink{entered: make(chan struct{}, 1)}
	r := newRig(t, 1<<20, 1<<24, func(c *Config) { c.Sink = sink; c.Drain = 100 * time.Millisecond })
	body, ct := multipartBody(t, dicomParts(corpus(t, "ct5n-2062.dcm")))
	done := make(chan Result, 1)
	go func() { _, res := r.serve("", body, ct); done <- res }()
	<-sink.entered
	start := time.Now()
	if err := r.ep.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Errorf("Stop returned after %v, before the drain bound", d)
	}
	if res := <-done; res.Stored != 0 || res.Status != http.StatusServiceUnavailable {
		t.Errorf("the request cut short: %+v", res)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	if res := r.ep.Serve(context.Background(), w, req, Request{}); res.Status != http.StatusServiceUnavailable {
		t.Errorf("a request after Stop: %+v", res)
	}
}
