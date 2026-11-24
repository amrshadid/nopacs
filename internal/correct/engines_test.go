package correct

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// engines returns Engines over the rig's tenant, its pass every interval,
// each engine's grace period grace and its confirm key key (nil for none).
func (r *rig) engines(interval, grace time.Duration, key *secret.DerivedKey) *Engines {
	r.t.Helper()
	es, err := NewEngines(EnginesConfig{Tenants: oneTenant{r.p}, Interval: interval,
		Configure: func(context.Context, string, tenant.Corrections) (Config, func() error, error) {
			return Config{TenantID: rigTenantID, Keys: r.keys, OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil }, ConfirmKey: key,
				Grace: func(context.Context) (time.Duration, error) { return grace, nil }}, nil, nil
		}})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = es.Stop(context.Background()) })
	return es
}

// use returns the rig's tenant's engine from es, released at once: what a
// request holds only while it calls the engine.
func (r *rig) use(es *Engines) *Engine {
	r.t.Helper()
	e, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		r.t.Fatal(err)
	}
	release()
	return e
}

// waitState polls a correction until it reaches want, failing the test
// after half a minute.
func (r *rig) waitState(id string, want record.State) tenant.Correction {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		c := r.state(id)
		if c.State == want {
			return c
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the correction is %s (%s), want %s", c.State, c.StateReason, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestEnginesPassPurgesWhatIsDue guards the pass the server runs on a
// timer: a delete whose grace period (zero here) has ended is purged by the
// pass alone, its objects gone from the storage and every other object as
// it was; the same engine is answered to every request; and Stop ends the
// pass and refuses a later request.
func TestEnginesPassPurgesWhatIsDue(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	es := r.engines(50*time.Millisecond, 0, nil)
	e := r.use(es)
	if again := r.use(es); again != e {
		t.Fatal("a second request was given another engine")
	}
	subject := r.scanWith(2).SubjectLabel
	before := r.objects()
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	done := r.waitState(c.ID, record.StatePurged)
	if done.Counts.ObjectsRemoved == 0 || done.Counts.ObjectsOwned != done.Counts.ObjectsRemoved+done.Counts.ObjectsKept {
		t.Fatalf("the pass's purge counts %+v", done.Counts)
	}
	after := r.objects()
	for k, sum := range before {
		if got, ok := after[k]; ok && got != sum {
			t.Errorf("%s changed", k)
		}
	}
	if len(under(after, "data/"+subject+"/")) != 0 {
		t.Error("the purged subject's objects are still stored")
	}
	if err := es.Stop(r.ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := es.Use(r.ctx, "lab", r.p.(tenant.Store)); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a request after Stop: %v, want unavailable", err)
	}
}

// keyedConfirm returns a key derived for confirm tokens from a new key
// file.
func keyedConfirm(t *testing.T) *secret.DerivedKey {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
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

// TestKeyedTokenSentAgainAnswersTheDelete guards a delete sent again with
// the token of its preview, the server's keyed one: the delete hid its
// target, so no preview can be made again, and the repeat is answered with
// the delete it made rather than as a target not found; a token another
// installation's key made is refused as a precondition.
func TestKeyedTokenSentAgainAnswersTheDelete(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	e := r.use(r.engines(time.Hour, time.Hour, keyedConfirm(t)))
	other := r.engineWith(func(c *Config) { c.ConfirmKey = keyedConfirm(t) })
	target := tenant.CorrectionTarget{Subject: r.scanWith(2).SubjectLabel}
	p, err := e.Preview(r.ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Delete(r.ctx, DeleteRequest{Target: target, Reason: "x", Confirm: p.Confirm, Audit: r.audit()}); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("another key's token: %v, want precondition", err)
	}
	first, err := e.Delete(r.ctx, DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: p.Confirm, Audit: r.audit()})
	if err != nil || first.State != record.StateCommitted {
		t.Fatalf("the delete: %+v, %v", first, err)
	}
	again, err := e.Delete(r.ctx, DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: p.Confirm, Audit: r.audit()})
	if err != nil || again.ID != first.ID {
		t.Fatalf("the same token again: %+v, %v; want %s", again, err, first.ID)
	}
}

// TestStartPurgeLeavesThePurgeToContinuePurge guards the admin's purge as
// the API makes it: StartPurge moves a committed delete to purging and
// removes nothing; ContinuePurge then removes its objects and ends it
// purged; a correction not purging is answered by ContinuePurge as it
// stands.
func TestStartPurgeLeavesThePurgeToContinuePurge(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	e := r.engine()
	subject := r.scanWith(2).SubjectLabel
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.ContinuePurge(r.ctx, c.ID); err != nil || got.State != record.StateCommitted {
		t.Fatalf("ContinuePurge of a committed delete: %+v, %v", got, err)
	}
	started, err := e.StartPurge(r.ctx, c.ID, r.audit(), nil)
	if err != nil || started.State != record.StatePurging {
		t.Fatalf("StartPurge: %+v, %v", started, err)
	}
	if len(under(r.objects(), "data/"+subject+"/")) == 0 {
		t.Fatal("StartPurge removed objects")
	}
	done, err := e.ContinuePurge(r.ctx, c.ID)
	if err != nil || done.State != record.StatePurged {
		t.Fatalf("ContinuePurge: %+v, %v", done, err)
	}
	if len(under(r.objects(), "data/"+subject+"/")) != 0 {
		t.Fatal("the purge left objects")
	}
}

// TestEngineAuthenticatesItsRecords guards that the record the engine puts
// on a storage carries a MAC for its tenant under the installation keys: it
// reads back under them and is refused, as unauthenticated, for another
// tenant's id or under another key file; and that no engine is made
// without the keys or the tenant's id, so none writes a record every reader
// would refuse.
func TestEngineAuthenticatesItsRecords(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	s := r.scanWith(1)
	c, err := r.deleteNow(r.engine(), tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.CorrectionKey(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := readAll(r.ctx, r.storage, key, record.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := record.DecodeCorrection(bytes.NewReader(b), rigTenantID, r.keys); err != nil || rec.ID != c.ID {
		t.Fatalf("the record does not read back under the installation keys: %+v, %v", rec, err)
	}
	for name, read := range map[string]func() error{
		"another tenant": func() error {
			_, err := record.DecodeCorrection(bytes.NewReader(b), "T0000000000002", r.keys)
			return err
		},
		"another key file": func() error {
			_, err := record.DecodeCorrection(bytes.NewReader(b), rigTenantID, testkit.InstallationKeys(t))
			return err
		},
	} {
		if err := read(); !errors.Is(err, record.ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want unauthenticated", name, err)
		}
	}
	for name, change := range map[string]func(*Config){
		"no keys":      func(c *Config) { c.Keys = nil },
		"empty keys":   func(c *Config) { c.Keys = &secret.Keys{} },
		"no tenant id": func(c *Config) { c.TenantID = "" },
	} {
		cfg := Config{Tenant: "lab", TenantID: rigTenantID, Keys: r.keys, Store: r.p,
			OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil }}
		change(&cfg)
		if _, err := New(cfg); fault.KindOf(err) != fault.Internal {
			t.Errorf("%s: New = %v, want Internal", name, err)
		}
	}
}
