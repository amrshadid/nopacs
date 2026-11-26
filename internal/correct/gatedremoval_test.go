package correct

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// swappingGate is a write gate that, like a switch, swaps the storage's
// place before it admits a write: each Enter makes a new place current,
// then admits. It counts the writes it holds admitted now.
type swappingGate struct {
	mu       sync.Mutex
	place    int
	admitted int
}

// Enter swaps the place, then admits.
func (g *swappingGate) Enter(context.Context, string) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.place++
	g.admitted++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.admitted--
			g.mu.Unlock()
		})
	}, nil
}

// now answers the current place and how many writes are admitted.
func (g *swappingGate) now() (place, admitted int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.place, g.admitted
}

// removal is one removal a placeStore saw: whether it went to a place no
// longer current, and whether no write was admitted when it ran.
type removal struct {
	key     string
	stale   bool
	outside bool
}

// removals records the removals of every placeStore of a test.
type removals struct {
	mu   sync.Mutex
	list []removal
	// retain, when set, makes the first Purge of a data object answer one
	// version retained until a time already past, so a later repurge has a
	// version to remove.
	retain   bool
	retained string
}

// add records one removal through a store of place p.
func (rs *removals) add(g *swappingGate, p int, key string) {
	cur, admitted := g.now()
	rs.mu.Lock()
	rs.list = append(rs.list, removal{key: key, stale: p != cur, outside: admitted == 0})
	rs.mu.Unlock()
}

// placeStore is the rig's storage as opened while place was current.
type placeStore struct {
	blob.Store
	place int
	gate  *swappingGate
	seen  *removals
}

// Purge records the removal, then removes.
func (s *placeStore) Purge(ctx context.Context, key string) ([]blob.Removal, error) {
	s.seen.add(s.gate, s.place, key)
	out, err := s.Store.Purge(ctx, key)
	if err != nil {
		return out, err
	}
	s.seen.mu.Lock()
	defer s.seen.mu.Unlock()
	if s.seen.retain && s.seen.retained == "" && strings.HasPrefix(key, "data/") {
		s.seen.retained = key
		return []blob.Removal{{Version: "v1", Disposition: blob.Retained, RetainUntil: time.Now().Add(-time.Hour)}}, nil
	}
	return out, nil
}

// PurgeOlder records the removal; the directory keeps no older versions.
func (s *placeStore) PurgeOlder(_ context.Context, key string) ([]blob.Removal, error) {
	s.seen.add(s.gate, s.place, key)
	return nil, nil
}

// PurgeVersion records the removal and answers it removed.
func (s *placeStore) PurgeVersion(_ context.Context, key, version string) (blob.Removal, error) {
	s.seen.add(s.gate, s.place, key)
	return blob.Removal{Version: version, Disposition: blob.Removed}, nil
}

// gatedEngine returns an engine over the rig whose gate swaps the place at
// each admission and whose storage records each removal and the place it
// was opened at.
func gatedEngine(r *rig) (*Engine, *swappingGate, *removals) {
	g, seen := &swappingGate{}, &removals{}
	e := r.engineWith(func(c *Config) {
		c.Pause = g
		c.OpenStorage = func(context.Context, string) (blob.Store, error) {
			p, _ := g.now()
			return &placeStore{Store: r.storage, place: p, gate: g, seen: seen}, nil
		}
	})
	return e, g, seen
}

// wantGated fails the test for each removal that went to a place swapped
// away before it ran, or that ran with no write admitted by the gate.
func wantGated(t *testing.T, seen *removals, what string) {
	t.Helper()
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if len(seen.list) == 0 {
		t.Fatalf("%s removed nothing", what)
	}
	for _, r := range seen.list {
		if r.stale || r.outside {
			t.Fatalf("%s removed %s through a place swapped away (%v) or outside the gate (%v)", what, r.key, r.stale, r.outside)
		}
	}
}

// TestEveryPurgeRemovalLandsOnThePlaceTheGateAdmitted guards a purge's
// removal of each object: the gate is entered before the storage is
// opened, so a switch that swaps the place while the removal waits sends
// the removal to the new place, never the old.
func TestEveryPurgeRemovalLandsOnThePlaceTheGateAdmitted(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	e, _, seen := gatedEngine(r)
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: r.scans()[0].SubjectLabel})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err != nil || got.State != record.StatePurged {
		t.Fatalf("the purge: %+v %v", got, err)
	}
	wantGated(t, seen, "the purge")
}

// TestAnObjectFoundAgainIsRemovedInsideTheGate guards the purge's last
// check of every copy: an object found there again is removed inside the
// storage's gate, through the storage opened once admitted, not through
// the store its presence was read from.
func TestAnObjectFoundAgainIsRemovedInsideTheGate(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	e, _, seen := gatedEngine(r)
	subject := r.scans()[0].SubjectLabel
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	var back, body string
	for k := range r.objects() {
		if strings.HasPrefix(k, "data/") && strings.Contains(k, subject+"/") {
			back = k
			break
		}
	}
	if back == "" {
		t.Fatalf("no object of the subject %s", subject)
	}
	b, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(back))) // #nosec G304 -- the test's own storage.
	if err != nil {
		t.Fatal(err)
	}
	body = string(b)
	e.SetCrashAt(crashAt("purge-objects"))
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err == nil {
		t.Fatal("the purge did not stop where the test stops it")
	}
	if _, ok := r.objects()[back]; ok {
		t.Fatalf("%s was not removed by the purge", back)
	}
	// The object comes back, as a copy put while the purge ran would.
	if err := os.WriteFile(filepath.Join(r.root, filepath.FromSlash(back)), []byte(body), 0o600); err != nil { // #nosec G703 -- the test's own storage and key.
		t.Fatal(err)
	}
	seen.mu.Lock()
	seen.list = nil
	seen.mu.Unlock()
	got, err := e.ContinuePurge(r.ctx, c.ID)
	if err != nil || got.State != record.StatePurged {
		t.Fatalf("the purge run again: %+v %v", got, err)
	}
	if _, ok := r.objects()[back]; ok {
		t.Fatalf("%s found again was not removed", back)
	}
	wantGated(t, seen, "the check of every copy")
}

// TestARetainedVersionIsRemovedInsideTheGate guards the retry of a version
// a storage retained: once its retention has ended it is removed inside
// the storage's gate, through the storage opened once admitted.
func TestARetainedVersionIsRemovedInsideTheGate(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	e, _, seen := gatedEngine(r)
	seen.retain = true
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: r.scans()[0].SubjectLabel})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if seen.retained == "" {
		t.Fatal("no version was retained")
	}
	seen.mu.Lock()
	seen.list = nil
	seen.mu.Unlock()
	if err := e.repurge(r.ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantGated(t, seen, "the retry of a retained version")
}
