package jobmeet

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// fakeArchive serves a stream's live and deleted instances in pages, in
// UID order, as a database does.
type fakeArchive struct {
	live, gone []string
}

func page(uids []string, after string, limit int) []tenant.ScanInstance {
	var out []tenant.ScanInstance
	for _, u := range uids {
		if u > after && len(out) < limit {
			out = append(out, tenant.ScanInstance{SOPInstanceUID: u})
		}
	}
	return out
}

func (f *fakeArchive) ResolveInstances(context.Context, []tenant.InstanceRef) ([]tenant.ResolvedInstance, error) {
	return nil, nil
}

func (f *fakeArchive) JobScanInstances(_ context.Context, _ tenant.WorkFence, after string, limit int) ([]tenant.ScanInstance, error) {
	return page(f.live, after, limit), nil
}

func (f *fakeArchive) DeletedJobInstances(_ context.Context, _ tenant.WorkFence, after string, limit int) ([]tenant.ScanInstance, error) {
	return page(f.gone, after, limit), nil
}

// uids returns n UIDs from start, step apart, in UID order.
func uids(start, n, step int) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf("1.2.3.%07d", start+i*step))
	}
	return out
}

// walk returns every item of a stream.
func walk(t *testing.T, s *Stream) (all []string, deleted []string) {
	t.Helper()
	for {
		it, ok, err := s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return all, deleted
		}
		all = append(all, it.Instance.SOPInstanceUID)
		if it.Deleted {
			deleted = append(deleted, it.Instance.SOPInstanceUID)
		}
	}
}

// TestStreamMergesDeletedInstancesIntoTheirPlaces guards the order a unit
// answers in: the instances the catalog holds and those a correction
// deleted come in one order of UIDs, over more than one page of each, so
// the unit's cursor never passes an instance it has not answered; one that
// is both held and listed deleted is answered once; and a scan with no
// deleted instance streams as it was.
func TestStreamMergesDeletedInstancesIntoTheirPlaces(t *testing.T) {
	live := uids(0, 2*Page+17, 3)
	gone := uids(1, Page+40, 5)
	gone = append(gone, live[10]) // held again: answered once, as held.
	sort.Strings(gone)
	gone = slices.Compact(gone)
	onlyGone := 0
	a := &fakeArchive{live: live, gone: gone}
	all, deleted := walk(t, NewStream(a, tenant.WorkFence{}, ""))
	want := slices.Clone(live)
	for _, g := range gone {
		if !slices.Contains(live, g) {
			want = append(want, g)
			onlyGone++
		}
	}
	sort.Strings(want)
	if !slices.Equal(all, want) {
		t.Errorf("the stream answers %d instances in an order that is not the merge of both, want %d", len(all), len(want))
	}
	for _, d := range deleted {
		if slices.Contains(live, d) {
			t.Errorf("instance %s is held and was answered deleted", d)
		}
	}
	if len(deleted) != onlyGone {
		t.Errorf("%d answered deleted, want %d", len(deleted), onlyGone)
	}
	after := live[Page]
	rest, _ := walk(t, NewStream(a, tenant.WorkFence{}, after))
	for _, u := range rest {
		if u <= after {
			t.Fatalf("a stream after %s answers %s", after, u)
		}
	}
	plain, none := walk(t, NewStream(&fakeArchive{live: live}, tenant.WorkFence{}, ""))
	if !slices.Equal(plain, live) || len(none) != 0 {
		t.Errorf("a scan with no deleted instance gives %d instances, %d deleted, want its %d held", len(plain), len(none), len(live))
	}
}
