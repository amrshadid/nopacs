package api

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// pagedObjects is a tenant.Replication whose StorageObjects answers one page.
type pagedObjects struct {
	tenant.Replication
	objs []tenant.StoredObject
}

// StorageObjects answers the one page.
func (p pagedObjects) StorageObjects(context.Context, tenant.StorageObjectQuery, master.Page) ([]tenant.StoredObject, string, error) {
	return p.objs, "", nil
}

// stateRecalls is a tenant.Recalls whose rows give each key the state set
// for it (none for a key not set), and that keeps the asks it is given.
type stateRecalls struct {
	tenant.Recalls
	states map[string]tenant.RecallState
	asks   []tenant.RecallAsk
}

// RecallsOf answers the rows of the keys a state is set for.
func (s *stateRecalls) RecallsOf(_ context.Context, storage string, keys []string) (map[string]tenant.Recall, error) {
	out := map[string]tenant.Recall{}
	for _, k := range keys {
		if st, ok := s.states[k]; ok {
			out[k] = tenant.Recall{Storage: storage, Key: k, State: st}
		}
	}
	return out, nil
}

// RequestRecalls keeps the ask and answers every key requested.
func (s *stateRecalls) RequestRecalls(_ context.Context, a tenant.RecallAsk) (tenant.RecallAsked, error) {
	s.asks = append(s.asks, a)
	return tenant.RecallAsked{Requested: len(a.Keys)}, nil
}

// TestConfirmedRecallIsAlwaysRecorded guards the entry of a confirmed
// recall: when every object became recalled by another request between the
// plan and the asks, nothing is asked and the call's own entry is recorded
// all the same, with no objects or bytes asked; otherwise the entry goes
// with the first ask, the call records nothing more, and the objects and
// bytes answered are both those it asked for, not the plan's.
func TestConfirmedRecallIsAlwaysRecorded(t *testing.T) {
	objs := []tenant.StoredObject{{Storage: "cold", Key: "data/a.dcm", Size: 10}, {Storage: "cold", Key: "data/b.dcm", Size: 32}}
	plan := recallPlan{storages: []recallStorage{{name: "cold", days: 3}}, askObjects: 2, askBytes: 42, path: label.Path{Subject: "sub-01"}}
	entry := master.AuditEntry{ID: "e1"}

	taken := &stateRecalls{states: map[string]tenant.RecallState{"data/a.dcm": tenant.RecallRequested, "data/b.dcm": tenant.RecallInProgress}}
	recorded := 0
	asked, audited, err := confirmRecall(context.Background(), pagedObjects{objs: objs}, taken, plan, entry, nil, time.Now(), func() { recorded++ })
	if err != nil || !audited || recorded != 1 || len(taken.asks) != 0 || asked != (recallAsked{}) {
		t.Errorf("every object taken meanwhile: asked %+v, audited %v, recorded %d, asks %d, %v; want nothing asked and the entry recorded once",
			asked, audited, recorded, len(taken.asks), err)
	}

	one := &stateRecalls{states: map[string]tenant.RecallState{"data/a.dcm": tenant.RecallInProgress}}
	recorded = 0
	asked, audited, err = confirmRecall(context.Background(), pagedObjects{objs: objs}, one, plan, entry, nil, time.Now(), func() { recorded++ })
	if err != nil || !audited || recorded != 0 || asked != (recallAsked{objects: 1, bytes: 32}) {
		t.Errorf("one object taken meanwhile: asked %+v, audited %v, recorded %d, %v; want the other asked, 1 object of 32 bytes", asked, audited, recorded, err)
	}
	if len(one.asks) != 1 || one.asks[0].Audit == nil || one.asks[0].Audit.ID != "e1" || len(one.asks[0].Keys) != 1 || one.asks[0].Keys[0] != "data/b.dcm" {
		t.Errorf("the asks were %+v, want one of data/b.dcm carrying the entry", one.asks)
	}
}
