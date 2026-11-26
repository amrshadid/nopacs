package supervise_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// switchGate is a gate of writes whose storages switching a test sets, and
// which counts the refusals it is told of.
type switchGate struct {
	on      atomic.Bool
	mu      sync.Mutex
	refused map[string]int
}

// Switching reports the switch the test set, for the storage "main" only.
func (g *switchGate) Switching(_ context.Context, storage string) (bool, error) {
	return storage == "main" && g.on.Load(), nil
}

// CountRefused counts one refusal for storage.
func (g *switchGate) CountRefused(storage string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused[storage]++
}

// count returns the refusals counted for storage.
func (g *switchGate) count(storage string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused[storage]
}

// TestASwitchingStorageRefusesAndCounts guards a receiver whose storage is
// being switched to its replica: it refuses new associations as storage
// switching, each refusal counted on the switch's gate, and takes them
// again once the switch ends; its listener keeps its port.
func TestASwitchingStorageRefusesAndCounts(t *testing.T) {
	r := newRig(t)
	gate := &switchGate{refused: map[string]int{}}
	r.edit = func(c *supervise.Config) {
		c.Switches = func(string) supervise.SwitchGate { return gate }
	}
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	l := r.running("ct")[0]
	if f := l.spec.Refusal(); f != "" {
		t.Fatalf("refusal before the switch %q", f)
	}
	gate.on.Store(true)
	r.waitFor("ct refusing while its storage switches", func() bool {
		return l.spec.Refusal() == tenant.FailureStorageSwitching
	})
	r.waitFor("ct down while its storage switches", func() bool {
		h := r.health("ct")
		return h.State == tenant.ReceiverDown && h.Reason == tenant.DownOther && h.Detail == tenant.PhraseStorageSwitching
	})
	before := gate.count("main")
	if f := l.spec.Refusal(); f != tenant.FailureStorageSwitching || gate.count("main") != before+1 {
		t.Errorf("a refusal while switching answered %q and counted %d; want storage_switching, counted once more",
			f, gate.count("main")-before)
	}
	gate.on.Store(false)
	r.waitFor("ct taking associations again", func() bool { return l.spec.Refusal() == "" })
	r.waitFor("ct up again", r.isUp("ct"))
	if got := r.running("ct"); len(got) != 1 || got[0] != l {
		t.Error("the listener was stopped while its storage switched; it holds its port")
	}
}
