package send

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// readCounter is the fixture's storage that counts the reads of each
// instance, so a test sees which instances a unit opened at all.
type readCounter struct {
	storages
	mu    sync.Mutex
	reads map[string]int
}

// Open counts the read and reads the object.
func (r *readCounter) Open(ctx context.Context, obj checkedread.Object, offset, length int64) (checkedread.Opened, error) {
	r.mu.Lock()
	r.reads[obj.SOPInstanceUID]++
	r.mu.Unlock()
	return r.storages.Open(ctx, obj, offset, length)
}

// count returns how many times the instance was opened.
func (r *readCounter) count(uid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads[uid]
}

// eligibilityFiles archives three instances in session ses-1, one of each
// kind the default rule judges, and returns their UIDs: archived as
// received, de-identified with an accepted pixel risk, and de-identified.
func eligibilityFiles(t *testing.T, f *fixture) (asIs, risky, clean string) {
	t.Helper()
	files := generated(t, 3, 32<<10)
	asIs, risky, clean = uidOf(t, files[0]), uidOf(t, files[1]), uidOf(t, files[2])
	f.provenance = func(uid string) tenant.InstanceProvenance {
		switch uid {
		case asIs:
			return tenant.InstanceProvenance{}
		case risky:
			return tenant.InstanceProvenance{Profile: fixtureProfile, PixelRiskAccepted: true}
		}
		return tenant.InstanceProvenance{Profile: fixtureProfile}
	}
	f.archive("ses-1", scanFiles{"mr", files})
	return asIs, risky, clean
}

// refusals returns a job's refusals as "<uid> <code>", sorted, each
// checked to carry the fixed phrase of its reason and no value.
func refusals(t *testing.T, f *fixture, id string) []string {
	t.Helper()
	var out []string
	for _, fl := range f.failures(id) {
		if fl.Detail != eligibilityDetail(fl.Code) && fl.Code != tenant.FailureDeletedInArchive {
			t.Errorf("the refusal of %s reads %q, not the fixed phrase of %s", fl.SOPInstanceUID, fl.Detail, fl.Code)
		}
		out = append(out, fl.SOPInstanceUID+" "+string(fl.Code))
	}
	sort.Strings(out)
	return out
}

// wantRefused fails unless the job refused exactly asIs and risky for their
// reasons, sent clean alone to peer, and never opened a refused instance.
func wantRefused(t *testing.T, f *fixture, what string, j tenant.Job, peer *dimsePeer, reads *readCounter, asIs, risky, clean string) {
	t.Helper()
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 2 {
		t.Errorf("%s: job %s %+v, want partial with 1 sent and 2 refused", what, j.State, j.Counts)
	}
	want := []string{asIs + " " + string(tenant.FailureAsReceived), risky + " " + string(tenant.FailurePixelRiskAccepted)}
	sort.Strings(want)
	if got := refusals(t, f, j.ID); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: refusals %v, want %v", what, got, want)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.count[asIs] != 0 || peer.count[risky] != 0 || peer.count[clean] == 0 {
		t.Errorf("%s: the peer received as received %d, accepted risk %d, de-identified %d times; want only the de-identified one",
			what, peer.count[asIs], peer.count[risky], peer.count[clean])
	}
	if reads != nil && (reads.count(asIs) != 0 || reads.count(risky) != 0) {
		t.Errorf("%s: a refused instance was read from the storage (%d, %d times)", what, reads.count(asIs), reads.count(risky))
	}
}

// startCounting starts a supervisor whose storage reads are counted.
func (f *fixture) startCounting() (*Supervisor, *readCounter) {
	reads := &readCounter{storages: storages{s: f.store}, reads: map[string]int{}}
	cfg := f.config()
	cfg.OpenStorages = func(string, tenant.Pipeline) Storages { return reads }
	return f.start(cfg), reads
}

// setMark sets or clears a sender's allow_identified in the store.
func (f *fixture) setMark(name string, mark bool) {
	f.t.Helper()
	if _, err := f.p.SetSender(f.ctx, tenant.Sender{Name: name, AllowIdentified: mark},
		[]tenant.SenderField{tenant.FieldSenderAllowIdentified}, f.clock.Now(), f.audit(), nil); err != nil {
		f.t.Fatal(err)
	}
}

// TestAnUnmarkedSenderSendsOnlyDeidentified guards the default rule on a
// job a person makes: to a sender not marked, an instance archived as
// received and one with an accepted pixel risk are refused for their
// reasons, never read, while the de-identified one is sent and the job ends
// partial; a marked sender sends all three.
func TestAnUnmarkedSenderSendsOnlyDeidentified(t *testing.T) {
	f := newFixture(t)
	plainPeer, openPeer := &dimsePeer{}, &dimsePeer{}
	asIs, risky, clean := eligibilityFiles(t, f)
	f.addDIMSE("plain", startPeer(t, plainPeer))
	f.addDIMSE("open", startPeer(t, openPeer), func(s *tenant.Sender) { s.AllowIdentified = true })
	refused := f.createJob("plain", "ses-1")
	all := f.createJob("open", "ses-1")
	// The marked sender reads every instance, so the reads are not counted
	// here; the tests below count them with an unmarked sender alone.
	s := f.start(f.config())
	wantRefused(t, f, "unmarked", f.waitJob(s, refused.ID, 30*time.Second, ended), plainPeer, nil, asIs, risky, clean)
	j := f.waitJob(s, all.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 3 {
		t.Errorf("marked: job %s %+v, want done with all 3 sent", j.State, j.Counts)
	}
	openPeer.mu.Lock()
	defer openPeer.mu.Unlock()
	for _, u := range []string{asIs, risky, clean} {
		if openPeer.count[u] == 0 {
			t.Errorf("the marked sender's peer did not receive %s", u)
		}
	}
}

// TestAForwardAppliesTheRule guards the rule on a receiver's forward: the
// job a reception makes for an unmarked sender refuses what the default
// rule refuses, as a person's job does.
func TestAForwardAppliesTheRule(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	f.addDIMSE("plain", startPeer(t, peer))
	if _, err := f.p.SetReceiver(f.ctx, tenant.Receiver{Name: "recv", ForwardTo: []string{"plain"}}, []tenant.ReceiverField{tenant.FieldForwardTo},
		f.clock.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	asIs, risky, clean := eligibilityFiles(t, f)
	id := tenant.ForwardJobID(f.reception, "plain")
	if _, err := f.p.Job(f.ctx, id); err != nil {
		t.Fatalf("the reception made no forward: %v", err)
	}
	s, reads := f.startCounting()
	wantRefused(t, f, "forward", f.waitJob(s, id, 30*time.Second, ended), peer, reads, asIs, risky, clean)
}

// TestARetryAppliesTheRuleWhenItSends guards the rule on retries: a retry
// of a job that refused instances to an unmarked sender refuses them again,
// and a retry made after the installation's admin marked the sender sends
// them, so the rule is the sender's mark when the instance is sent.
func TestARetryAppliesTheRuleWhenItSends(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	asIs, risky, clean := eligibilityFiles(t, f)
	f.addDIMSE("plain", startPeer(t, peer))
	first := f.createJob("plain", "ses-1")
	s := f.start(f.config())
	wantRefused(t, f, "first", f.waitJob(s, first.ID, 30*time.Second, ended), peer, nil, asIs, risky, clean)
	_, retry, err := f.p.RetryJob(f.ctx, first.ID, f.id("job"), f.clock.Now(), f.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	j := f.waitJob(s, retry.ID, 30*time.Second, ended)
	if j.State != tenant.JobPartial || j.Counts.InstancesRefused != 2 || j.Counts.InstancesSent != 0 {
		t.Errorf("a retry to the unmarked sender: %s %+v, want partial with both refused again", j.State, j.Counts)
	}
	want := []string{asIs + " " + string(tenant.FailureAsReceived), risky + " " + string(tenant.FailurePixelRiskAccepted)}
	sort.Strings(want)
	if got := refusals(t, f, retry.ID); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("a retry's refusals %v, want %v", got, want)
	}
	f.setMark("plain", true)
	s.Poke("lab")
	_, again, err := f.p.RetryJob(f.ctx, retry.ID, f.id("job"), f.clock.Now(), f.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	j = f.waitJob(s, again.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 2 {
		t.Errorf("a retry once the sender is marked: %s %+v, want done with both sent", j.State, j.Counts)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.count[asIs] != 1 || peer.count[risky] != 1 {
		t.Errorf("the peer received as received %d and accepted risk %d times, want once each, after the mark", peer.count[asIs], peer.count[risky])
	}
}

// TestAJobIsJudgedWhenItSends guards that the rule is not taken from when a
// job was made: a job made while its sender was marked, left queued by a
// server that stopped, and taken up by a new process over the same
// database after the mark was cleared (as a restored database hands its
// queued jobs to the server that starts on it), refuses what the sender may
// no longer send.
func TestAJobIsJudgedWhenItSends(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	asIs, risky, clean := eligibilityFiles(t, f)
	f.addDIMSE("plain", startPeer(t, peer), func(s *tenant.Sender) { s.AllowIdentified = true })
	j := f.createJob("plain", "ses-1")
	f.setMark("plain", false)
	// The database is closed and opened again by a new store, as the
	// process that starts on a restored database opens it.
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := (driver.SQLiteTenants{DataDir: f.dir}).Open(f.ctx, "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f.p = s.(tenant.Pipeline)
	f.ten.p = f.p
	sup, reads := f.startCounting()
	wantRefused(t, f, "a job made while marked", f.waitJob(sup, j.ID, 30*time.Second, ended), peer, reads, asIs, risky, clean)
}

// TestAMarkClearedStopsTheNextInstance guards the unit's reading of the
// mark: it may send what the default rule refuses only while both the row
// it started with and the row the supervisor read last are marked, so a
// mark cleared while a unit sends refuses its next instance, and one set
// meanwhile waits for the next unit.
func TestAMarkClearedStopsTheNextInstance(t *testing.T) {
	marked, unmarked := tenant.Sender{Name: "pacs", AllowIdentified: true}, tenant.Sender{Name: "pacs"}
	for _, c := range []struct {
		name          string
		started, last *tenant.Sender
		want          bool
	}{
		{"marked throughout", &marked, &marked, true},
		{"cleared since the unit started", &marked, &unmarked, false},
		{"set since the unit started", &unmarked, &marked, false},
		{"no row read", &marked, nil, false},
	} {
		sr := &senderRun{}
		if c.last != nil {
			sr.row.Store(c.last)
		}
		u := &unitRun{sr: sr, row: *c.started}
		if got := u.allowIdentified(); got != c.want {
			t.Errorf("%s: may send identified %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRefusalsNameNoValue guards the refusals' phrases: each names the fact
// of the catalog that refused the instance and fits a failure's detail.
func TestRefusalsNameNoValue(t *testing.T) {
	for _, code := range []tenant.JobFailureCode{tenant.FailureAsReceived, tenant.FailureNoProfileProvenance, tenant.FailurePixelRiskAccepted} {
		d := eligibilityDetail(code)
		f := tenant.JobFailure{SOPInstanceUID: "1.2.3", Code: code, Detail: d, Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		if err := f.Validate(); err != nil || !strings.HasPrefix(d, "not sent: ") {
			t.Errorf("%s: %q, %v", code, d, err)
		}
	}
	if eligibilityDetail(tenant.FailureAsReceived) == eligibilityDetail(tenant.FailurePixelRiskAccepted) ||
		eligibilityDetail(tenant.FailureNoProfileProvenance) == eligibilityDetail(tenant.FailurePixelRiskAccepted) {
		t.Error("two reasons share one phrase")
	}
}
