package correct

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// shifted returns a clock that reads the machine's time moved by d.
func shifted(d time.Duration) func() time.Time { return func() time.Time { return time.Now().Add(d) } }

// TestPurgeGoesByTrustedClock guards the purge's clock: a clock set back
// before the delete's commit refuses the server's purge and an admin's; a
// clock that jumped forward more than the step since the last trusted pass
// pauses the server's purges, the delete left committed, and the tenant's
// retention with them; an admin's purge then runs, and the server's purges
// go on once a person confirms the time.
func TestPurgeGoesByTrustedClock(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	subject := r.scanWith(2).SubjectLabel
	c, err := r.deleteNow(r.engine(), tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	back := r.engineWith(func(cfg *Config) { cfg.Now = shifted(-time.Hour) })
	if _, err := back.PurgeDue(r.ctx); !errors.Is(err, tenant.ErrClock) {
		t.Fatalf("PurgeDue at a clock set back answered %v, want the clock's refusal", err)
	}
	if _, err := back.PurgeNow(r.ctx, c.ID, r.audit(), nil); !errors.Is(err, tenant.ErrClock) {
		t.Fatalf("PurgeNow at a clock set back answered %v, want the clock's refusal", err)
	}
	if n, err := r.engine().PurgeDue(r.ctx); err != nil || n != 0 {
		t.Fatalf("PurgeDue in the grace = %d, %v", n, err)
	}
	jumped := r.engineWith(func(cfg *Config) { cfg.Now = shifted(3 * 24 * time.Hour) })
	if _, err := jumped.PurgeDue(r.ctx); !errors.Is(err, tenant.ErrClock) {
		t.Fatalf("PurgeDue after the clock jumped answered %v, want the clock's refusal", err)
	}
	if st := r.state(c.ID); st.State != record.StateCommitted {
		t.Fatalf("after a paused pass the delete is %s", st.State)
	}
	if p, err := jumped.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || p.State != record.StatePurged {
		t.Fatalf("an admin's purge after the jump = %+v, %v", p, err)
	}
	// The jump the server's pass saw paused retention, and with it every
	// automatic purge, until a person confirms the time.
	if _, err := jumped.PurgeDue(r.ctx); !errors.Is(err, tenant.ErrClock) {
		t.Fatalf("PurgeDue before the time is confirmed answered %v, want the clock's refusal", err)
	}
	now := time.Now().Add(3 * 24 * time.Hour)
	if _, _, err := r.p.(tenant.Retention).ConfirmClock(r.ctx, tenant.ClockConfirm{Observed: now, Now: now, Reason: "checked"}, r.audit(),
		nil); err != nil {
		t.Fatal(err)
	}
	if _, err := jumped.PurgeDue(r.ctx); err != nil {
		t.Fatalf("PurgeDue at the clock a person confirmed: %v", err)
	}
}

// confirmKey derives a confirm-token key from a new installation key file.
func confirmKey(t *testing.T) *secret.DerivedKey {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nopacs.key")
	if err := secret.CreateKeyFile(path); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.Derive(secret.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestConfirmTokenIsTheServers guards a delete's token under the confirm
// key: the store's own token (which anyone who saw the counts could
// compute) and a token with a changed MAC are refused; the server's token
// is taken even though a job over the whole tenant started between the
// preview and the delete.
func TestConfirmTokenIsTheServers(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	subject := r.scanWith(2).SubjectLabel
	target := tenant.CorrectionTarget{Subject: subject}
	e := r.engineWith(func(cfg *Config) { cfg.ConfirmKey = confirmKey(t) })
	pv, err := e.Preview(r.ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pv.Confirm, "c2.") {
		t.Fatalf("the preview's token is %q, want a keyed one", pv.Confirm)
	}
	plain, err := r.p.PreviewDelete(r.ctx, target, pv.Issued)
	if err != nil {
		t.Fatal(err)
	}
	last := pv.Confirm[len(pv.Confirm)-1]
	forged := pv.Confirm[:len(pv.Confirm)-1] + map[bool]string{true: "1", false: "0"}[last == '0']
	for _, tok := range []string{plain.Confirm, forged} {
		if _, err := e.Delete(r.ctx, DeleteRequest{Target: target, Reason: "r", Confirm: tok, Audit: r.audit()}); fault.KindOf(err) != fault.Precondition {
			t.Fatalf("a delete with the token %q answered %v, want a precondition failure", tok, err)
		}
	}
	if _, err := r.p.CreateJob(r.ctx, tenant.NewJob{ID: "scrub0000009", Kind: tenant.JobScrub, Storage: "main", Created: time.Now().UTC()},
		r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	c, err := e.Delete(r.ctx, DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: pv.Confirm, Audit: r.audit()})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("a delete with the server's token = %+v, %v", c, err)
	}
}

// retaining is a storage that keeps versions: the first instance object a
// purge asks for is retained by its lock until a time, and a version purge
// after removes it.
type retaining struct {
	blob.Store
	mu       sync.Mutex
	until    time.Time
	held     string
	versions []string
}

// Purge implements blob.Store.
func (s *retaining) Purge(ctx context.Context, key string) ([]blob.Removal, error) {
	s.mu.Lock()
	first := s.held == "" && strings.HasSuffix(key, ".dcm")
	if first {
		s.held = key
	}
	s.mu.Unlock()
	if first {
		return []blob.Removal{{Version: "v1", Disposition: blob.Retained, RetainUntil: s.until}}, nil
	}
	return s.Store.Purge(ctx, key)
}

// PurgeOlder implements blob.VersionPurger: none older.
func (s *retaining) PurgeOlder(context.Context, string) ([]blob.Removal, error) { return nil, nil }

// PurgeVersion implements blob.VersionPurger.
func (s *retaining) PurgeVersion(ctx context.Context, key, version string) (blob.Removal, error) {
	s.mu.Lock()
	s.versions = append(s.versions, key+"@"+version)
	s.mu.Unlock()
	if _, err := s.Store.Purge(ctx, key); err != nil {
		return blob.Removal{}, err
	}
	return blob.Removal{Version: version, Disposition: blob.Removed}, nil
}

// TestRetainedVersionsArePurgedLater guards a version a storage retained:
// the delete is purged with it counted retained; a pass before its
// retention ends leaves it; the first pass after purges that version alone
// and the delete then counts it removed.
func TestRetainedVersionsArePurgedLater(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	s := r.scanWith(2)
	st := &retaining{Store: r.storage, until: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	open := func(context.Context, string) (blob.Store, error) { return st, nil }
	e := r.engineWith(func(cfg *Config) { cfg.OpenStorage = open })
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != record.StatePurged || len(p.Dispositions) != 1 || p.Dispositions[0].Retained != 1 {
		t.Fatalf("the purged delete is %+v, want one object retained", p.Dispositions)
	}
	if _, err := e.PurgeDue(r.ctx); err != nil || len(st.versions) != 0 {
		t.Fatalf("a pass in the retention purged %q, %v", st.versions, err)
	}
	later := r.engineWith(func(cfg *Config) { cfg.OpenStorage, cfg.Now = open, shifted(2*time.Hour) })
	if _, err := later.PurgeDue(r.ctx); err != nil {
		t.Fatal(err)
	}
	if len(st.versions) != 1 || st.versions[0] != st.held+"@v1" {
		t.Fatalf("the pass after the retention purged %q, want %s@v1", st.versions, st.held)
	}
	got := r.state(c.ID)
	if got.State != record.StatePurged || got.Dispositions[0].Retained != 0 || got.Dispositions[0].Removed != p.Dispositions[0].Removed+1 {
		t.Fatalf("after the later pass the delete counts %+v", got.Dispositions)
	}
}
