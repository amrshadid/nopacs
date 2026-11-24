package supervise

import (
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// View is what the supervisor holds in memory of one tenant, for a status
// that must answer also when the tenant's database does not: its receivers
// as last read with their health, the space of each storage it last
// measured, and the pipeline's throughput.
type View struct {
	// Receivers are the receivers as last read, ordered by name.
	Receivers []ReceiverView
	// Storages are the storages whose space was last measured, by name: the
	// storages the tenant's enabled receivers write to.
	Storages map[string]StorageSpace
	// Throughput is the pipeline's, zero when none runs.
	Throughput receive.Throughput
}

// ReceiverView is one receiver and its health.
type ReceiverView struct {
	Receiver tenant.Receiver
	Health   Health
}

// StorageSpace is a storage's space as last measured: its free bytes, and
// whether they are below the reserve.
type StorageSpace struct {
	FreeBytes  int64
	NearlyFull bool
}

// viewState is what a tenant's worker publishes after each reconcile: the
// rows and space it holds, which only its goroutine changes, copied.
type viewState struct {
	rows     []tenant.Receiver
	storages map[string]StorageSpace
	pipeline Pipeline
}

// publish copies what the worker holds for View to read.
func (t *tenantRun) publish() {
	v := &viewState{rows: slices.Clone(t.rows), storages: make(map[string]StorageSpace, len(t.space.usage)), pipeline: t.pipeline}
	for name, s := range t.space.usage {
		v.storages[name] = s
	}
	t.view.Store(v)
}

// throughputOf is a pipeline that measures its throughput, as the reception
// pipeline does.
type throughputOf interface {
	Throughput() receive.Throughput
}

// View returns what the supervisor holds of the tenant named name, and
// whether it runs a worker for it. It reads memory only.
func (s *Supervisor) View(name string) (View, bool) {
	s.mu.Lock()
	t := s.tenants[name]
	s.mu.Unlock()
	if t == nil {
		return View{}, false
	}
	v := t.view.Load()
	if v == nil {
		return View{}, true
	}
	out := View{Storages: v.storages, Receivers: make([]ReceiverView, 0, len(v.rows))}
	for _, r := range v.rows {
		out.Receivers = append(out.Receivers, ReceiverView{Receiver: r, Health: s.Health(name, r)})
	}
	slices.SortFunc(out.Receivers, func(a, b ReceiverView) int { return strings.Compare(a.Receiver.Name, b.Receiver.Name) })
	if tp, ok := v.pipeline.(throughputOf); ok {
		out.Throughput = tp.Throughput()
	}
	return out, true
}
