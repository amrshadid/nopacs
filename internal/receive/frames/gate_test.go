package frames

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// copyPlace returns a second archive whose storage holds a copy of every
// object of a's, as a replica in step does, its sidecar puts checked
// against a's lock: the place a switch makes the storage's name name.
func copyPlace(t *testing.T, a *archive) *archive {
	t.Helper()
	b := newArchive(t)
	b.store.locked = a.locks
	page, err := a.store.List(a.ctx, blob.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Next != "" {
		t.Fatal("the test's archive does not fit one page")
	}
	for _, key := range page.Keys {
		rc, err := a.store.Store.Read(a.ctx, key, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.store.Store.Put(a.ctx, key, rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// switching is the place a storage's name names and the gate of writes to
// it, as a switch of the storage to its replica moves them: closed, the
// gate refuses every write; swapping, it moves the name to the new place
// before it admits a write, as a gate does that held the write through the
// swap.
type switching struct {
	mu                sync.Mutex
	current, next     blob.Store
	closed, swapping  bool
	entered, released int
}

// open returns the store the name names now.
func (s *switching) open(context.Context, string) (blob.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, nil
}

// enter is the gate.
func (s *switching) enter(_ context.Context, storage string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fault.New(fault.Unavailable, "STORAGE_SWITCHING: the storage "+storage+" is being switched to its replica")
	}
	if s.swapping {
		s.current = s.next
	}
	s.entered++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.released++
			s.mu.Unlock()
		})
	}, nil
}

// metadataPuts counts the puts of sidecars made on a store.
func metadataPuts(c *counted) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for key, puts := range c.puts {
		if strings.HasPrefix(key, layout.MetaRoot) {
			n += puts
		}
	}
	return n
}

// TestPassWritesOnlyWhereTheGateAdmits guards the pass of frame tables
// during a switch of a storage to its replica: while the gate of writes
// refuses, the pass stops before any sidecar is written and leaves the
// scan at version 1; when the gate holds a rewrite through the swap, the
// pass writes the sidecar on the place the storage's name names after it,
// never on the old place it read from, and releases every entry.
func TestPassWritesOnlyWhereTheGateAdmits(t *testing.T) {
	a := newArchive(t)
	a.scan(scanA, true, filesA...)
	b := copyPlace(t, a)
	sw := &switching{current: a.store, next: b.store, closed: true}
	cfg := a.config()
	cfg.OpenStorage, cfg.Enter = sw.open, sw.enter
	rep, err := Run(a.ctx, cfg)
	if fault.KindOf(err) != fault.Unavailable || rep.ScansDone != 0 || metadataPuts(a.store) != 0 || metadataPuts(b.store) != 0 {
		t.Fatalf("a pass the gate refuses: %+v, %v, %d and %d sidecar puts", rep, err, metadataPuts(a.store), metadataPuts(b.store))
	}
	if _, version, _ := a.sidecarOf(scanA); version != 1 {
		t.Fatalf("a refused pass left the sidecar at version %d", version)
	}
	sw.mu.Lock()
	sw.closed, sw.swapping = false, true
	sw.mu.Unlock()
	if rep, err = Run(a.ctx, cfg); err != nil || rep.ScansDone != 1 {
		t.Fatalf("a pass held through the swap: %+v, %v", rep, err)
	}
	if n := metadataPuts(a.store); n != 0 {
		t.Errorf("the pass wrote %d sidecars on the place the switch left", n)
	}
	if _, version, _ := a.sidecarOf(scanA); version != 1 {
		t.Errorf("the old place's sidecar is at version %d", version)
	}
	b.wantTables(scanA, filesA...)
	if b.store.unlocked != 0 {
		t.Errorf("%d sidecars were written without the scan's lock", b.store.unlocked)
	}
	if sw.entered == 0 || sw.released != sw.entered {
		t.Errorf("the gate was entered %d times and released %d", sw.entered, sw.released)
	}
}

// TestUpgradeWritesOnlyWhereTheGateAdmits guards the rewrite of older
// sidecars during a switch, as the pass of frame tables: refused by the
// gate, it writes nothing; held through the swap, it writes on the new
// place only, and releases every entry.
func TestUpgradeWritesOnlyWhereTheGateAdmits(t *testing.T) {
	u := newUpArchive(t, 2)
	nb := &upArchive{archive: copyPlace(t, u.archive), cat: u.cat}
	sw := &switching{current: u.store, next: nb.store, closed: true}
	cfg := u.config()
	cfg.OpenStorage, cfg.Enter = sw.open, sw.enter
	rep, err := Upgrade(u.ctx, cfg)
	if fault.KindOf(err) != fault.Unavailable || rep.ScansDone != 0 || metadataPuts(u.store) != 0 || metadataPuts(nb.store) != 0 {
		t.Fatalf("a rewrite the gate refuses: %+v, %v", rep, err)
	}
	sw.mu.Lock()
	sw.closed, sw.swapping = false, true
	sw.mu.Unlock()
	if rep, err = Upgrade(u.ctx, cfg); err != nil || rep.ScansDone != 2 {
		t.Fatalf("a rewrite held through the swap: %+v, %v", rep, err)
	}
	if n := metadataPuts(u.store); n != 0 {
		t.Errorf("the rewrite wrote %d sidecars on the place the switch left", n)
	}
	for i := 1; i <= 2; i++ {
		nb.wantV3(i)
		if _, version, _ := u.sidecarOf(label.Path{Subject: "SUB", Session: "SES", Scan: scanLabel(i)}); version != sidecar.VersionFrames {
			t.Errorf("scan %d on the old place is at version %d", i, version)
		}
	}
	if sw.entered == 0 || sw.released != sw.entered {
		t.Errorf("the gate was entered %d times and released %d", sw.entered, sw.released)
	}
}
