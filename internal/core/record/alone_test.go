package record

import "testing"

// TestAlone guards the one rule a rebuild and a restore read a record with
// no row by: a delete committed, purging, purged or still prepared is
// committed and unresolved, one abandoned or undone keeps its state and is
// not; a relabel or a replace published is committed and unresolved, one
// prepared or abandoned is abandoned.
func TestAlone(t *testing.T) {
	for _, c := range []struct {
		kind       Kind
		state      State
		want       State
		unresolved bool
	}{
		{KindDelete, StatePrepared, StateCommitted, true},
		{KindDelete, StateCommitted, StateCommitted, true},
		{KindDelete, StatePurging, StateCommitted, true},
		{KindDelete, StatePurged, StateCommitted, true},
		{KindDelete, StateAbandoned, StateAbandoned, false},
		{KindDelete, StateUndone, StateUndone, false},
		{KindRelabel, StatePrepared, StateAbandoned, false},
		{KindRelabel, StateAbandoned, StateAbandoned, false},
		{KindRelabel, StateCommitted, StateCommitted, true},
		{KindRelabel, StatePurged, StateCommitted, true},
		{KindReplace, StatePrepared, StateAbandoned, false},
		{KindReplace, StateCommitted, StateCommitted, true},
		{KindReplace, StatePurging, StateCommitted, true},
	} {
		got, unresolved := Correction{Kind: c.kind, State: c.state}.Alone()
		if got != c.want || unresolved != c.unresolved {
			t.Errorf("a %s record %s alone reads %s, unresolved %v; want %s, unresolved %v", c.kind, c.state, got, unresolved, c.want, c.unresolved)
		}
	}
}
