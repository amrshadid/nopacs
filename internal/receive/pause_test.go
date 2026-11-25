package receive

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
)

// fakePause is a write gate a test closes and opens: closed, it refuses
// every write as a storage being switched does; open, it admits each and
// counts the releases.
type fakePause struct {
	mu                         sync.Mutex
	closed                     bool
	refused, entered, released int
	storages                   []string
}

// Enter refuses while closed, and otherwise admits the write.
func (f *fakePause) Enter(ctx context.Context, storage string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storages = append(f.storages, storage)
	if f.closed {
		f.refused++
		return nil, fault.New(fault.Unavailable, "STORAGE_SWITCHING: the storage "+storage+" is being switched to its replica")
	}
	f.entered++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.released++
			f.mu.Unlock()
		})
	}, nil
}

// set changes the gate under its lock.
func (f *fakePause) set(change func(f *fakePause)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// counts returns the refusals, admissions and releases so far.
func (f *fakePause) counts() (refused, entered, released int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused, f.entered, f.released
}

// TestArchiveStepAsksTheWriteGate guards the one check every reception's
// archive makes before it lands bytes on its storage: while the gate of the
// reception's storage refuses (a switch of it to its replica), the step
// fails before any object is put and the reception stays identified; once
// the gate admits it, the step archives every file, and releases the gate
// once, after its commit.
func TestArchiveStepAsksTheWriteGate(t *testing.T) {
	pause := &fakePause{closed: true}
	e := newEnv(t, envOptions{pause: pause})
	e.send(studyFiles...)
	e.waitFor("the archive step to be refused", 3*time.Second, func() bool {
		refused, _, _ := pause.counts()
		return refused >= 1 && e.p.Stats().StepsFailed.Load() >= 1
	})
	page, err := e.storage.List(e.ctx, blob.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Keys) != 0 {
		t.Fatalf("a refused archive step put %d objects on the storage: %v", len(page.Keys), page.Keys)
	}
	rs := e.receptions()
	if len(rs) != 1 || rs[0].State != reception.Identified {
		t.Fatalf("the receptions while the gate refuses are %+v; want one identified", rs)
	}
	pause.set(func(f *fakePause) { f.closed = false })
	rs = e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != 7 {
		t.Fatalf("the reception once the gate admits it is %+v", rs[0])
	}
	refused, entered, released := pause.counts()
	if entered < 1 || released != entered || refused < 1 {
		t.Fatalf("the gate was refused %d, entered %d and released %d times; want every entry released", refused, entered, released)
	}
	pause.mu.Lock()
	defer pause.mu.Unlock()
	for _, s := range pause.storages {
		if s != "main" {
			t.Fatalf("the archive step asked the gate of %q, not of its reception's storage", s)
		}
	}
}
