package supervise_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAStowReceiverRunsAsAListener guards that a stow receiver is run as a
// DIMSE receiver is: an enabled one gets a listener made from its row, on
// no address, with the tenant's sink, and is up; a change of its keys
// restarts it with the new list; a change to de-identify restarts it with
// the row that names the profile (its answers then give no Retrieve URL);
// disabling it stops it.
func TestAStowReceiverRunsAsAListener(t *testing.T) {
	r := newRig(t)
	if _, err := r.ts.pipe().AddReceiver(context.Background(), tenant.Receiver{
		Name: "web", Type: tenant.ReceiverSTOW, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		STOW: tenant.STOWReceiver{Keys: []string{"key-a"}, MaxRequestBytes: 1 << 30},
	}, r.clock.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.waitFor("web up", r.isUp("web"))
	l := r.running("web")
	if len(l) != 1 || l[0].spec.Receiver.Type != tenant.ReceiverSTOW || l[0].spec.BindAddress != "" || l[0].spec.Sink == nil {
		t.Fatalf("listeners of web %+v", l)
	}
	if out := l[0].spec.Sink.Accept(context.Background(), receiveArrival()); out.Status != receivedStatus {
		t.Fatalf("the sink did not reach the pipeline: %+v", out)
	}
	first := l[0]
	r.set(tenant.Receiver{Name: "web", STOW: tenant.STOWReceiver{Keys: []string{"key-b"}}}, tenant.FieldSTOWKeys)
	r.waitFor("a listener with the new keys", func() bool {
		l := r.running("web")
		return first.stopped.Load() && len(l) == 1 && slices.Equal(l[0].spec.Receiver.STOW.Keys, []string{"key-b"}) && r.isUp("web")()
	})
	second := r.running("web")[0]
	ref := tenant.ProfileRef{Name: "research", Version: 1}
	r.set(tenant.Receiver{Name: "web", Profile: ref}, tenant.FieldDeidentification)
	r.waitFor("a listener that de-identifies", func() bool {
		l := r.running("web")
		return second.stopped.Load() && len(l) == 1 && l[0].spec.Receiver.Profile == ref && r.isUp("web")()
	})
	third := r.running("web")[0]
	r.set(tenant.Receiver{Name: "web", Enabled: false}, tenant.FieldEnabled)
	r.waitFor("web down, disabled", func() bool { return r.health("web").Reason == "disabled" && third.stopped.Load() })
}
