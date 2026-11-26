package checkedread_test

import (
	"context"
	"errors"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// recallsOf answers a recall per storage as a test sets it, and counts the
// lookups.
type recallsOf struct {
	rows    map[string]tenant.Recall
	fail    error
	lookups int
}

// Recall answers the row the test set for the storage.
func (r *recallsOf) Recall(_ context.Context, storage, _ string) (tenant.Recall, bool, error) {
	r.lookups++
	if r.fail != nil {
		return tenant.Recall{}, false, r.fail
	}
	row, ok := r.rows[storage]
	return row, ok, nil
}

// TestNotOnlineCarriesTheRecallState guards what a read no storage serves
// because the object is held cold says of its recall: the primary not
// online with no replica asks the recall of the primary's object; the
// primary's object missing and the replica in step holding it cold answers
// not online (never not found: the bytes are not missing) with the
// replica's recall; each state the database holds is passed on, an online
// row whose object is cold again read as none, and Waiting only while a
// recall is requested or in progress; a database that does not answer
// leaves the plain not-online error; and a read the primary serves asks
// nothing of recalls.
func TestNotOnlineCarriesTheRecallState(t *testing.T) {
	f := newFixture(t, 1, 5000)
	in := f.held(uid(1))
	obj := checkedread.InstanceObject(in, f.series, f.path)

	s, all := f.stores()
	rc := &recallsOf{rows: map[string]tenant.Recall{"main": {State: tenant.RecallInProgress}}}
	checkedread.UseRecalls(s, rc)
	if _, _, err := readAll(f.ctx, s, obj, in); err != nil || rc.lookups != 0 {
		t.Fatalf("a read the primary serves: %v, %d recall lookups", err, rc.lookups)
	}
	all["main"].set(func(st *faulty) { st.fail = notOnline })
	_, _, err := readAll(f.ctx, s, obj, in)
	var no *checkedread.NotOnline
	if !errors.As(err, &no) || no.RecallState != checkedread.RecallInProgress || !no.Waiting() ||
		!errors.Is(err, blob.ErrNotOnline) || fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("the primary not online: %v (%+v); want unavailable, not online, in_progress", err, no)
	}

	f.attach("copy1")
	f.inStep("copy1")
	for _, c := range []struct {
		row     *tenant.Recall
		want    string
		waiting bool
	}{
		{nil, checkedread.RecallNone, false},
		{&tenant.Recall{State: tenant.RecallRequested}, checkedread.RecallRequested, true},
		{&tenant.Recall{State: tenant.RecallInProgress}, checkedread.RecallInProgress, true},
		{&tenant.Recall{State: tenant.RecallFailed}, checkedread.RecallFailed, false},
		{&tenant.Recall{State: tenant.RecallOnline}, checkedread.RecallNone, false},
	} {
		s, all := f.stores()
		rc := &recallsOf{rows: map[string]tenant.Recall{}}
		if c.row != nil {
			rc.rows["copy1"] = *c.row
		}
		checkedread.UseRecalls(s, rc)
		for _, n := range []string{"main", "copy1"} {
			if _, err := s.Get(f.ctx, n); err != nil {
				t.Fatal(err)
			}
		}
		all["main"].set(func(st *faulty) { st.fail = fault.New(fault.NotFound, "the test's object is not there") })
		all["copy1"].set(func(st *faulty) { st.fail = notOnline })
		_, _, err := readAll(f.ctx, s, obj, in)
		var no *checkedread.NotOnline
		if !errors.As(err, &no) || no.RecallState != c.want || no.Waiting() != c.waiting || fault.KindOf(err) != fault.Unavailable {
			t.Errorf("with the replica's recall %+v: %v (%+v); want %s, waiting %v", c.row, err, no, c.want, c.waiting)
		}
	}

	s2, all2 := f.stores()
	checkedread.UseRecalls(s2, &recallsOf{fail: fault.New(fault.Unavailable, "the test's database does not answer")})
	if _, err := s2.Get(f.ctx, "main"); err != nil {
		t.Fatal(err)
	}
	all2["main"].set(func(st *faulty) { st.fail = notOnline })
	all2["copy1"].set(func(st *faulty) { st.fail = notOnline })
	_, _, err = readAll(f.ctx, s2, obj, in)
	if errors.As(err, &no) || !errors.Is(err, blob.ErrNotOnline) {
		t.Errorf("with the database not answering: %v; want the plain not-online error", err)
	}
}
