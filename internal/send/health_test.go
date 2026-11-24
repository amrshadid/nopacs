package send

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestPeerDownUsesNoAttempts guards a sender whose peer is down: after
// three connections in a row fail the sender is down for peer_failing, its
// unit is given back without spending an attempt and is not claimed while
// it is, however long that lasts; the peer is tried again on the backoff,
// and once it answers the sender is up and the job ends done. Each change
// of health is a sender_health event.
func TestPeerDownUsesNoAttempts(t *testing.T) {
	f := newFixture(t)
	addr := freePort(t)
	f.archive("ses-1", scanFiles{"mr", generated(t, 2, 256)})
	f.addDIMSE("peer", addr)
	rec := f.record()
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	f.stepOnEach(s, "the sender's going down", time.Minute, 2*time.Minute, failedConnections(s, "peer"), func() bool {
		h, _ := s.Health("lab", "peer")
		return h.State == tenant.SenderDown
	})
	h, _ := s.Health("lab", "peer")
	if h.DownReason != tenant.SenderDownPeerFailing || h.Fault != tenant.SenderFaultUnreachable || h.FailedConnections < DownAfter || h.NextTry.IsZero() {
		t.Errorf("health %+v, want down for peer_failing, unreachable, with a next try", h)
	}
	// An hour of the clock passes while the peer stays down: the probes
	// fail and no unit is worked.
	for range 20 {
		f.clock.add(5 * time.Minute)
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
	// Two workers may each make a connection while the sender goes down;
	// only those before it spend an attempt.
	released, failed := rec.counts()
	if failed > DownAfter-1 || released < 1 {
		t.Errorf("given back %d times with an attempt and %d without, want at most %d and at least 1", failed, released, DownAfter-1)
	}
	if j, _ := f.p.Job(f.ctx, job.ID); j.State.Ended() {
		t.Fatalf("job ended %s while its peer was down", j.State)
	}
	// The peer comes up on its address.
	ap := netip.MustParseAddrPort(addr)
	peer := &dimsePeer{}
	peer.data, peer.sums, peer.syntax, peer.count = map[string][]byte{}, map[string][32]byte{}, map[string]string{}, map[string]int{}
	srv, err := network.StartServer(context.Background(), network.SCPConfig{AETitle: "PEER", Port: int(ap.Port()), BindAddress: "127.0.0.1"}, peer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	srv.SetSupportedTransferSyntaxes(network.AllTransferSyntaxes())
	// Past the next try, which is at most an hour away; then no more, so
	// no lease runs out under a unit that sends.
	f.clock.add(2 * time.Hour)
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || peer.arrivals() != 2 {
		t.Errorf("job %s with the peer holding %d, want done with 2", j.State, peer.arrivals())
	}
	if h, _ := s.Health("lab", "peer"); h.State != tenant.SenderUp || h.FailedConnections != 0 {
		t.Errorf("health %+v after the peer answered, want up", h)
	}
	page, err := f.p.(tenant.Events).ReadEvents(f.ctx, "", []tenant.EventType{tenant.EventSenderHealth}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, e := range page.Events {
		if p, ok := e.Payload.(*tenant.SenderHealthChanged); ok {
			states = append(states, string(p.State)+"/"+string(p.DownReason))
		}
	}
	if !containsInOrder(states, "down/peer_failing", "up/") {
		t.Errorf("sender_health events %v, want down for peer_failing, then up", states)
	}
}

// containsInOrder reports whether want appear in got in that order.
func containsInOrder(got []string, want ...string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// TestDamagedBytesNeverReachThePeerWhole guards the checked read: an
// archived file changed on the storage near its end is aborted before its
// last fragment (the peer sees the data set cut and never answers it), a
// file gone from the storage is not sent at all, both are refused as
// damaged_in_storage, and the others are sent.
func TestDamagedBytesNeverReachThePeerWhole(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	addr := startPeer(t, peer)
	files := generated(t, 4, 1<<20)
	f.archive("ses-1", scanFiles{"mr", files})
	uids := make([]string, len(files))
	for i, b := range files {
		uids[i] = uidOf(t, b)
	}
	damaged, missing := uids[1], uids[2]
	bad := bytes.Clone(f.files[damaged])
	bad[len(bad)-100] ^= 0xFF
	if _, err := f.store.Put(f.ctx, f.keys[damaged], bytes.NewReader(bad)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(f.ctx, f.keys[missing]); err != nil {
		t.Fatal(err)
	}
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 2 || j.Counts.InstancesRefused != 2 {
		t.Fatalf("job %s %+v, want partial with 2 sent and 2 refused", j.State, j.Counts)
	}
	for _, fl := range f.failures(job.ID) {
		if fl.Code != tenant.FailureDamagedInStorage || (fl.SOPInstanceUID != damaged && fl.SOPInstanceUID != missing) {
			t.Errorf("failure %+v, want damaged_in_storage of the changed and the missing file", fl)
		}
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.count[damaged] != 0 || peer.count[missing] != 0 {
		t.Errorf("the peer answered the changed file %d times and the missing one %d times, want never", peer.count[damaged], peer.count[missing])
	}
	if peer.cut != 1 {
		t.Errorf("the peer saw %d data sets cut short, want the changed one", peer.cut)
	}
}

// TestStowCredentialRefusedHoldsBack guards a stow peer that answers 401
// (RFC 9110 15.5.2): the answer refuses no instance; each is a connection
// that failed with the fault rejected, counted toward the sender's health,
// so after three in a row the sender is down and its unit is held back
// without spending its attempts; once the credential is mended the job
// sends what was held back and ends done.
func TestStowCredentialRefusedHoldsBack(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	peer := &webPeer{auth: "Bearer token-right"}
	url := startWebPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 2, 256)})
	row := f.addStow("pacs", url)
	rec := f.record()
	job := f.createJob("pacs", "ses-1")
	s := f.start(f.config())
	f.stepOnEach(s, "the sender's going down", time.Minute, 2*time.Minute, failedConnections(s, "pacs"), func() bool {
		h, _ := s.Health("lab", "pacs")
		return h.State == tenant.SenderDown
	})
	if h, _ := s.Health("lab", "pacs"); h.DownReason != tenant.SenderDownPeerFailing || h.Fault != tenant.SenderFaultRejected || h.FailedConnections < DownAfter {
		t.Errorf("health %+v, want down for peer_failing with the fault rejected", h)
	}
	if fl := f.failures(job.ID); len(fl) != 0 {
		t.Errorf("failures %+v while the peer refused the credential, want none", fl)
	}
	if j, _ := f.p.Job(f.ctx, job.ID); j.State.Ended() || j.Counts.InstancesRefused != 0 {
		t.Fatalf("job %s %+v while the peer refused the credential, want not ended with nothing refused", j.State, j.Counts)
	}
	// The unit whose connection made the sender down is given back after
	// the health changes.
	waitFor(t, 10*time.Second, "the unit to be given back", func() bool { released, _ := rec.counts(); return released >= 1 })
	if released, failed := rec.counts(); failed > DownAfter-1 || released < 1 {
		t.Errorf("given back %d times with an attempt and %d without, want at most %d and at least 1", failed, released, DownAfter-1)
	}
	row.STOW.SealedCredential = []byte("token-right")
	if _, err := f.p.SetSender(f.ctx, row, []tenant.SenderField{tenant.FieldSTOWCredential}, f.clock.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	f.clock.add(2 * time.Hour)
	s.Poke("lab")
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	peer.mu.Lock()
	arrived := len(peer.parts)
	peer.mu.Unlock()
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 2 || arrived != 2 {
		t.Errorf("job %s %+v with the peer holding %d, want done with the 2 held back sent", j.State, j.Counts, arrived)
	}
}

// TestIdleWaitEndsAtTheNextTry guards the wait of an idle worker of a
// sender down for its failures: it ends when the peer is due to be tried
// again (NextTry), not up to the poll interval's bound later; a try due
// already waits a millisecond; a sender that is not down waits the poll
// interval as it stands.
func TestIdleWaitEndsAtTheNextTry(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	sr := &senderRun{name: "pacs", health: newHealth("pacs", now)}
	w := &worker{s: sr}
	if got := w.idleWait(MaxPollInterval, now); got != MaxPollInterval {
		t.Errorf("a sender not down waits %s, want %s", got, MaxPollInterval)
	}
	for range DownAfter {
		sr.health.failed(tenant.SenderFaultUnreachable, phraseUnreachable, now)
	}
	h := sr.health.get()
	if h.State != tenant.SenderDown || !h.NextTry.After(now) {
		t.Fatalf("health %+v, want down with a next try", h)
	}
	if got, want := w.idleWait(MaxPollInterval, now), h.NextTry.Sub(now); got != want || want >= MaxPollInterval {
		t.Errorf("a down sender's worker waits %s, want %s (the next try)", got, want)
	}
	if got := w.idleWait(MaxPollInterval, h.NextTry.Add(time.Second)); got != time.Millisecond {
		t.Errorf("a try due already waits %s, want 1ms", got)
	}
	if got := w.idleWait(time.Second, now); got != time.Second {
		t.Errorf("a wait shorter than the next try is %s, want 1s", got)
	}
}

// TestTenantRemovedStopsItsSendsAtOnce guards TenantRemoved: a tenant no
// longer in the list of tenants is let go as soon as the registry says so,
// long before the next reading of the list (an hour here).
func TestTenantRemovedStopsItsSendsAtOnce(t *testing.T) {
	f := newFixture(t)
	f.addDIMSE("pacs", freePort(t))
	cfg := f.config()
	cfg.Interval = time.Hour
	s := f.start(cfg)
	waitFor(t, 10*time.Second, "the tenant's first reading of its senders", func() bool {
		_, ok := s.Health("lab", "pacs")
		return ok
	})
	f.ten.gone.Store(true)
	s.TenantRemoved("lab")
	waitFor(t, 5*time.Second, "the removed tenant to be let go", func() bool {
		_, ok := s.Health("lab", "pacs")
		return !ok
	})
}
