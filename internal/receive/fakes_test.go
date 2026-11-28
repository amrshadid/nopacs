package receive

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// faultyStorage is a storage that fails, lies or stalls on demand, over a
// real one.
type faultyStorage struct {
	blob.Store
	// putFails and readFails fail that many next Puts or Reads of an
	// instance's object as Unavailable.
	putFails, readFails atomic.Int64
	// corrupt makes a Read of an instance's object return other bytes.
	corrupt atomic.Bool
	// missing makes a Stat of an instance's object answer NotFound.
	missing atomic.Bool
	// stall, when set, makes a Put of an instance's object wait until it
	// is closed or the Put's context ends.
	mu    sync.Mutex
	stall chan struct{}
	// stalled is signaled when a Put starts to wait.
	stalled chan struct{}
}

func newFaulty(s blob.Store) *faultyStorage {
	return &faultyStorage{Store: s, stalled: make(chan struct{}, 16)}
}

// instance reports whether key is an instance's object.
func instance(key string) bool { return strings.HasPrefix(key, "data/") }

func (f *faultyStorage) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if instance(key) {
		if f.putFails.Add(-1) >= 0 {
			return blob.Written{}, fault.New(fault.Unavailable, "the test storage refuses this write")
		}
		f.mu.Lock()
		stall := f.stall
		f.mu.Unlock()
		if stall != nil {
			f.stalled <- struct{}{}
			select {
			case <-stall:
			case <-ctx.Done():
				return blob.Written{}, fault.Wrap(fault.Unavailable, ctx.Err(), "the test storage's write was stopped")
			}
		}
	}
	return f.Store.Put(ctx, key, r)
}

func (f *faultyStorage) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if instance(key) {
		if f.readFails.Add(-1) >= 0 {
			return nil, fault.New(fault.Unavailable, "the test storage refuses this read")
		}
		if f.corrupt.Load() {
			return io.NopCloser(bytes.NewReader([]byte("other bytes than were written"))), nil
		}
	}
	return f.Store.Read(ctx, key, offset, length)
}

func (f *faultyStorage) Stat(ctx context.Context, key string) (blob.Info, error) {
	if instance(key) && f.missing.Load() {
		return blob.Info{}, fault.New(fault.NotFound, "the test storage hides this object")
	}
	return f.Store.Stat(ctx, key)
}

// setStall makes instance Puts wait until the returned function is called.
func (f *faultyStorage) setStall() func() {
	ch := make(chan struct{})
	f.mu.Lock()
	f.stall = ch
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.stall = nil
		f.mu.Unlock()
		close(ch)
	}
}

// faultyDB is the tenant's database with refusals on demand: a fake of the
// port over the real driver.
type faultyDB struct {
	tenant.Pipeline
	// commitFails fails that many next CommitScan calls as Unavailable.
	commitFails atomic.Int64
	// claimFails fails that many next ClaimInstances calls.
	claimFails atomic.Int64
	// leaseLost makes every RenewWork answer that the lease was lost.
	leaseLost atomic.Bool
	// claims counts ClaimInstances calls.
	claims atomic.Int64
	// addFails fails that many next AddReceivedFiles calls as Unavailable.
	addFails atomic.Int64
	// addTries counts AddReceivedFiles calls.
	addTries atomic.Int64
	// markMust records every received file as one its source required
	// de-identified (ReceivedFile.MustDeidentify).
	markMust atomic.Bool
}

func (d *faultyDB) AddReceivedFiles(ctx context.Context, files []tenant.ReceivedFile, at time.Time, fence *tenant.WorkFence) (int, error) {
	d.addTries.Add(1)
	if d.markMust.Load() {
		files = slices.Clone(files)
		for i := range files {
			files[i].MustDeidentify = true
		}
	}
	if d.addFails.Add(-1) >= 0 {
		return 0, fault.New(fault.Unavailable, "the test database refuses these received files")
	}
	return d.Pipeline.AddReceivedFiles(ctx, files, at, fence)
}

func (d *faultyDB) CommitScan(ctx context.Context, c tenant.ScanCommit) (tenant.Scan, error) {
	if d.commitFails.Add(-1) >= 0 {
		return tenant.Scan{}, fault.New(fault.Unavailable, "the test database refuses this commit")
	}
	return d.Pipeline.CommitScan(ctx, c)
}

func (d *faultyDB) ClaimInstances(ctx context.Context, id string, files []tenant.InstanceClaim, now time.Time, fence *tenant.WorkFence) ([]tenant.Duplicate, error) {
	d.claims.Add(1)
	if d.claimFails.Add(-1) >= 0 {
		return nil, fault.New(fault.Unavailable, "the test database refuses this claim")
	}
	return d.Pipeline.ClaimInstances(ctx, id, files, now, fence)
}

func (d *faultyDB) RenewWork(ctx context.Context, fence tenant.WorkFence, now time.Time, lease time.Duration) (tenant.Work, error) {
	if d.leaseLost.Load() {
		return tenant.Work{}, tenant.LeaseLost()
	}
	return d.Pipeline.RenewWork(ctx, fence, now, lease)
}
