package receive

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAcceptTransferTakesOnlyWhatTheReceiverMayArchive guards the receiver of
// type job: a file a transfer marks as one that must be de-identified is
// refused, with nothing landed, by a receiver that archives as received; the
// same file unmarked is taken; a file given to a job receiver by anything
// but a transfer is refused; and a transfer's file given to a receiver of
// another type is refused.
func TestAcceptTransferTakesOnlyWhatTheReceiverMayArchive(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{})
	if _, err := r.store.AddReceiver(r.ctx, tenant.Receiver{Name: "xfer", Type: tenant.ReceiverJob, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30}, r.clock.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	file := func() Arrival {
		return Arrival{Receiver: "xfer", Body: bytes.NewReader(corpus(t, studyFiles[0]))}
	}
	landed := r.p.stats.Landed.Load()
	marked := file()
	marked.RequireDeidentified = true
	if out := r.p.AcceptTransfer(context.Background(), marked); out.Status != Failed || out.Reason != ReasonNotDeidentified {
		t.Fatalf("a file that must be de-identified, to a receiver that archives as received: %+v", out)
	}
	if got := r.p.stats.Landed.Load(); got != landed {
		t.Errorf("a refused file landed: %d files landed before, %d after", landed, got)
	}
	if out := r.p.AcceptTransfer(context.Background(), file()); out.Status != Received {
		t.Errorf("a file that need not be de-identified: %+v, want received", out)
	}
	if out := r.p.Accept(context.Background(), file()); out.Status != Failed {
		t.Errorf("a file handed to a job receiver by a source that is no transfer: %+v, want failed", out)
	}
	other := file()
	other.Receiver = "scanner"
	if out := r.p.AcceptTransfer(context.Background(), other); out.Status != Failed {
		t.Errorf("a transfer's file handed to a receiver of another type: %+v, want failed", out)
	}
}
