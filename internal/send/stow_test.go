package send

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// webPeer is a STOW-RS service on httptest: it reads each request's parts
// as they come, records each instance it received whole, and answers what
// a test chooses for them (PS3.18 10.5.3).
type webPeer struct {
	t testing.TB
	// auth is the Authorization a request must carry; others are 401.
	auth string
	// answer gives the HTTP status and the body of a request whose
	// instances are uids, n being how often each arrived before; nil
	// stores them all.
	answer func(uids []string, n map[string]int) (int, []byte)

	mu       sync.Mutex
	parts    map[string][]byte
	count    map[string]int
	requests int
	sizes    []int
}

// ServeHTTP reads a store request and answers it.
func (p *webPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/dicom-web/studies" {
		// The service probe, a search for studies (PS3.18 10.6): none.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/dicom-web/studies" {
		http.Error(w, "not here", http.StatusNotFound)
		return
	}
	if p.auth != "" && r.Header.Get("Authorization") != p.auth {
		http.Error(w, "who are you", http.StatusUnauthorized)
		return
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/related" || params["type"] != "application/dicom" {
		http.Error(w, "not a STOW-RS body", http.StatusUnsupportedMediaType)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var uids []string
	got := map[string][]byte{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A body cut short: nothing of it is kept.
			return
		}
		b, err := io.ReadAll(part)
		if err != nil {
			return
		}
		u := uidOf(p.t, b)
		uids = append(uids, u)
		got[u] = b
	}
	p.mu.Lock()
	p.requests++
	p.sizes = append(p.sizes, len(uids))
	before := map[string]int{}
	for _, u := range uids {
		before[u] = p.count[u]
		p.count[u]++
		p.parts[u] = got[u]
	}
	p.mu.Unlock()
	status, body := http.StatusOK, storeAnswer(uids, nil, nil)
	if p.answer != nil {
		status, body = p.answer(uids, before)
	}
	w.Header().Set("Content-Type", "application/dicom+json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// startWebPeer serves p and returns the base URL of its DICOMweb service.
func startWebPeer(t testing.TB, p *webPeer) string {
	t.Helper()
	p.t, p.parts, p.count = t, map[string][]byte{}, map[string]int{}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv.URL + "/dicom-web"
}

// storeAnswer is a store response (PS3.18 10.5.3): stored lists the
// instances kept, with the Warning Reason of those in warned; failed those
// refused, with their Failure Reason.
func storeAnswer(stored []string, warned, failed map[string]uint16) []byte {
	item := func(uid string, tag string, reason uint16) map[string]any {
		m := map[string]any{
			"00081150": map[string]any{"vr": "UI", "Value": []string{"1.2.840.10008.5.1.4.1.1.4"}},
			"00081155": map[string]any{"vr": "UI", "Value": []string{uid}},
		}
		if reason != 0 {
			m[tag] = map[string]any{"vr": "US", "Value": []int{int(reason)}}
		}
		return m
	}
	out := map[string]any{}
	var ref, fail []map[string]any
	for _, u := range stored {
		ref = append(ref, item(u, "00081196", warned[u]))
	}
	for u, reason := range failed {
		fail = append(fail, item(u, "00081197", reason))
	}
	if len(ref) > 0 {
		out["00081199"] = map[string]any{"vr": "SQ", "Value": ref}
	}
	if len(fail) > 0 {
		out["00081198"] = map[string]any{"vr": "SQ", "Value": fail}
	}
	b, _ := json.Marshal(out)
	return b
}

// addStow adds an enabled stow sender named name to the service at url,
// with bearer authentication by the token "token-xyz".
func (f *fixture) addStow(name, url string, mods ...func(*tenant.Sender)) tenant.Sender {
	f.t.Helper()
	s := tenant.Sender{Name: name, Type: tenant.SenderSTOW, Enabled: true, STOW: tenant.STOWSender{
		URL: url, Authentication: tenant.StowAuthBearer, SealedCredential: []byte("token-xyz")}}
	for _, m := range mods {
		m(&s)
	}
	out, err := f.p.AddSender(f.ctx, s, f.clock.Now(), f.audit(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// TestStowSendsTheArchivedFiles guards the stow sender's main line: each
// instance goes as one part, the whole file as stored, in requests of at
// most the sender's instances, each with the credential; the job ends done.
func TestStowSendsTheArchivedFiles(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	peer := &webPeer{auth: "Bearer token-xyz"}
	url := startWebPeer(t, peer)
	var files [][]byte
	for _, n := range []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "MR_small_RLE.dcm", "liver_1frame.dcm"} {
		files = append(files, corpus(t, n))
	}
	f.archive("ses-1", scanFiles{"a", files})
	f.addStow("pacs", url, func(s *tenant.Sender) { s.STOW.MaxInstancesPerRequest = 2 })
	job := f.createJob("pacs", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 5 {
		t.Fatalf("job %s %+v, want done with 5 sent", j.State, j.Counts)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for uid, file := range f.files {
		if !bytes.Equal(peer.parts[uid], file) {
			t.Errorf("instance %s: the peer holds %d bytes, not the %d archived", uid, len(peer.parts[uid]), len(file))
		}
	}
	for _, n := range peer.sizes {
		if n > 2 {
			t.Errorf("a request carried %d instances, more than the sender's 2", n)
		}
	}
	if h, _ := s.Health("lab", "pacs"); h.State != tenant.SenderUp {
		t.Errorf("health %+v, want up", h)
	}
}

// TestStowReadsTheAnswerPerInstance guards the reading of a store
// response: an instance in the Referenced SOP Sequence is sent, with a
// warning when it has a Warning Reason; one in the Failed SOP Sequence is
// refused with its Failure Reason; one in neither is owed, sent once more,
// and sent when then accounted for, or refused as peer_unaccounted when
// left out again.
func TestStowReadsTheAnswerPerInstance(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	files := generated(t, 6, 256)
	uids := make([]string, len(files))
	for i, b := range files {
		uids[i] = uidOf(t, b)
	}
	sortStrings(uids)
	peer := &webPeer{answer: func(got []string, n map[string]int) (int, []byte) {
		var stored []string
		failed := map[string]uint16{}
		for _, u := range got {
			switch {
			case u == uids[2]:
				failed[u] = 0xC000
			case u == uids[3] && n[u] == 0, u == uids[4]:
				// Left unaccounted.
			default:
				stored = append(stored, u)
			}
		}
		return http.StatusAccepted, storeAnswer(stored, map[string]uint16{uids[1]: 0xB000}, failed)
	}}
	url := startWebPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", files})
	f.addStow("pacs", url, func(s *tenant.Sender) { s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil })
	job := f.createJob("pacs", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	c := j.Counts
	if j.State != tenant.JobPartial || c.InstancesSent != 3 || c.InstancesWarned != 1 || c.InstancesRefused != 2 || c.InstancesRemaining() != 0 {
		t.Fatalf("job %s %+v, want partial with 3 sent, 1 warned, 2 refused", j.State, c)
	}
	codes := map[string]tenant.JobFailure{}
	for _, fl := range f.failures(job.ID) {
		codes[fl.SOPInstanceUID] = fl
	}
	if fl := codes[uids[1]]; fl.Code != tenant.FailurePeerWarning || fl.PeerStatus != 0xB000 {
		t.Errorf("warned instance: %+v", fl)
	}
	if fl := codes[uids[2]]; fl.Code != tenant.FailurePeerRefused || fl.PeerStatus != 0xC000 {
		t.Errorf("failed instance: %+v", fl)
	}
	if fl := codes[uids[4]]; fl.Code != tenant.FailurePeerUnaccounted {
		t.Errorf("instance left out twice: %+v, want peer_unaccounted", fl)
	}
	if _, ok := codes[uids[3]]; ok {
		t.Errorf("the instance accounted for at its second try is listed as a failure")
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for i, u := range uids {
		want := 1
		if i == 3 || i == 4 {
			want = 2
		}
		if peer.count[u] != want {
			t.Errorf("instance %d arrived %d times, want %d", i, peer.count[u], want)
		}
	}
}

// TestStowFailuresOfTheWholeRequest guards the answers that are not per
// instance: a 503 is a temporary failure, tried again after the wait, and
// then sent; a 400 with no account refuses every instance of the request as
// peer_refused with the status; a 404 (a URL that names no DICOMweb
// service) fails the sender and refuses nothing; a redirect is not
// followed; plain http
// while the installation does not allow it reaches nothing.
func TestStowFailuresOfTheWholeRequest(t *testing.T) {
	t.Run("503 then stored", func(t *testing.T) {
		f := newFixture(t)
		f.inst.plain = true
		peer := &webPeer{}
		first := true
		var mu sync.Mutex
		peer.answer = func(uids []string, _ map[string]int) (int, []byte) {
			mu.Lock()
			defer mu.Unlock()
			if first {
				first = false
				return http.StatusServiceUnavailable, nil
			}
			return http.StatusOK, storeAnswer(uids, nil, nil)
		}
		url := startWebPeer(t, peer)
		f.archive("ses-1", scanFiles{"mr", generated(t, 2, 256)})
		f.addStow("pacs", url, func(s *tenant.Sender) { s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil })
		rec := f.record()
		job := f.createJob("pacs", "ses-1")
		s := f.start(f.config())
		waitFor(t, 30*time.Second, "the first failed attempt", func() bool { _, failed := rec.counts(); return failed == 1 })
		f.clock.add(15 * time.Second)
		j := f.waitJob(s, job.ID, 30*time.Second, ended)
		if _, failed := rec.counts(); j.State != tenant.JobDone || failed != 1 {
			t.Errorf("job %s after %d failed attempts, want done after 1", j.State, failed)
		}
	})
	t.Run("400 refuses the request", func(t *testing.T) {
		f := newFixture(t)
		f.inst.plain = true
		peer := &webPeer{answer: func([]string, map[string]int) (int, []byte) { return http.StatusBadRequest, nil }}
		url := startWebPeer(t, peer)
		f.archive("ses-1", scanFiles{"mr", generated(t, 2, 256)})
		f.addStow("pacs", url, func(s *tenant.Sender) { s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil })
		job := f.createJob("pacs", "ses-1")
		s := f.start(f.config())
		j := f.waitJob(s, job.ID, 30*time.Second, ended)
		if j.State != tenant.JobPartial || j.Counts.InstancesRefused != 2 {
			t.Fatalf("job %s %+v, want both refused", j.State, j.Counts)
		}
		for _, fl := range f.failures(job.ID) {
			if fl.Code != tenant.FailurePeerRefused || fl.PeerStatus != http.StatusBadRequest {
				t.Errorf("failure %+v, want peer_refused 400", fl)
			}
		}
	})
	t.Run("404 fails the sender, not the instances", func(t *testing.T) {
		f := newFixture(t)
		f.inst.plain = true
		peer := &webPeer{answer: func([]string, map[string]int) (int, []byte) { return http.StatusNotFound, nil }}
		url := startWebPeer(t, peer)
		f.archive("ses-1", scanFiles{"mr", generated(t, 2, 256)})
		f.addStow("pacs", url, func(s *tenant.Sender) { s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil })
		rec := f.record()
		job := f.createJob("pacs", "ses-1")
		s := f.start(f.config())
		waitFor(t, 30*time.Second, "a failed request", func() bool {
			h, _ := s.Health("lab", "pacs")
			return h.Fault == tenant.SenderFaultWrongAnswer && h.Detail == phraseNoService
		})
		waitFor(t, 30*time.Second, "the unit given back", func() bool { released, failed := rec.counts(); return released+failed >= 1 })
		j, err := f.p.Job(f.ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if j.State.Ended() || j.Counts.InstancesRefused != 0 || len(f.failures(job.ID)) != 0 {
			t.Errorf("job %s %+v after a 404, want it running with nothing refused", j.State, j.Counts)
		}
	})
	t.Run("redirect not followed", func(t *testing.T) {
		f := newFixture(t)
		f.inst.plain = true
		target := &webPeer{}
		targetURL := startWebPeer(t, target)
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			http.Redirect(w, r, targetURL+"/studies", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redirect.Close)
		f.archive("ses-1", scanFiles{"mr", generated(t, 1, 256)})
		f.addStow("pacs", redirect.URL+"/dicom-web", func(s *tenant.Sender) {
			s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil
		})
		f.createJob("pacs", "ses-1")
		s := f.start(f.config())
		waitFor(t, 30*time.Second, "a failed request", func() bool {
			h, _ := s.Health("lab", "pacs")
			return h.Fault == tenant.SenderFaultWrongAnswer
		})
		target.mu.Lock()
		defer target.mu.Unlock()
		if target.requests != 0 {
			t.Errorf("the redirect's target got %d requests", target.requests)
		}
	})
	t.Run("plain http not allowed", func(t *testing.T) {
		f := newFixture(t)
		peer := &webPeer{}
		url := startWebPeer(t, peer)
		f.archive("ses-1", scanFiles{"mr", generated(t, 1, 256)})
		f.addStow("pacs", url)
		f.createJob("pacs", "ses-1")
		s := f.start(f.config())
		waitFor(t, 30*time.Second, "a refused connection", func() bool {
			h, _ := s.Health("lab", "pacs")
			return h.Fault == tenant.SenderFaultAddressNotAllowed && strings.Contains(h.Detail, "plain http")
		})
		peer.mu.Lock()
		defer peer.mu.Unlock()
		if peer.requests != 0 {
			t.Errorf("the peer got %d requests over plain http", peer.requests)
		}
	})
}

// TestStowCutsTheRequestAtDamage guards the checked read in a request: an
// instance whose stored bytes changed near their end cuts the request
// before its part ends, so the peer keeps none of it; it is refused as
// damaged_in_storage, and the instance sent before it in the cut request,
// whose fate the cut left unknown, is sent again in the next one.
func TestStowCutsTheRequestAtDamage(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	peer := &webPeer{}
	url := startWebPeer(t, peer)
	files := generated(t, 3, 1<<20)
	f.archive("ses-1", scanFiles{"mr", files})
	uids := make([]string, len(files))
	for i, b := range files {
		uids[i] = uidOf(t, b)
	}
	sortStrings(uids)
	bad := bytes.Clone(f.files[uids[1]])
	bad[len(bad)-100] ^= 0xFF
	if _, err := f.store.Put(f.ctx, f.keys[uids[1]], bytes.NewReader(bad)); err != nil {
		t.Fatal(err)
	}
	f.addStow("pacs", url, func(s *tenant.Sender) { s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil })
	job := f.createJob("pacs", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 2 || j.Counts.InstancesRefused != 1 {
		t.Fatalf("job %s %+v, want partial with 2 sent and the changed one refused", j.State, j.Counts)
	}
	if fl := f.failures(job.ID); len(fl) != 1 || fl[0].SOPInstanceUID != uids[1] || fl[0].Code != tenant.FailureDamagedInStorage {
		t.Errorf("failures %+v, want the changed instance damaged_in_storage", fl)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.count[uids[1]] != 0 || peer.count[uids[0]] != 1 || peer.count[uids[2]] != 1 {
		t.Errorf("the peer kept %v, want the two whole instances once each and never the changed one", peer.count)
	}
}

// TestStowPeerDownUsesNoAttempts guards a stow peer that does not answer
// at all (nothing listens on its port): after three connections in a row
// fail the sender is down for peer_failing, its unit is given back without
// spending an attempt and is not worked while it is; once the peer
// listens, the probe brings the sender up and the job ends done.
func TestStowPeerDownUsesNoAttempts(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	addr := freePort(t)
	f.archive("ses-1", scanFiles{"mr", generated(t, 2, 256)})
	f.addStow("pacs", "http://"+addr+"/dicom-web", func(s *tenant.Sender) {
		s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil
	})
	rec := f.record()
	job := f.createJob("pacs", "ses-1")
	s := f.start(f.config())
	f.stepOnEach(s, "the sender's going down", time.Minute, 2*time.Minute, failedConnections(s, "pacs"), func() bool {
		h, _ := s.Health("lab", "pacs")
		return h.State == tenant.SenderDown
	})
	if h, _ := s.Health("lab", "pacs"); h.DownReason != tenant.SenderDownPeerFailing || h.Fault != tenant.SenderFaultUnreachable {
		t.Errorf("health %+v, want down for peer_failing, unreachable", h)
	}
	for range 20 {
		f.clock.add(5 * time.Minute)
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
	released, failed := rec.counts()
	if failed > DownAfter-1 || released < 1 {
		t.Errorf("given back %d times with an attempt and %d without, want at most %d and at least 1", failed, released, DownAfter-1)
	}
	peer := &webPeer{}
	peer.t, peer.parts, peer.count = t, map[string][]byte{}, map[string]int{}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: peer, ReadHeaderTimeout: time.Minute}}
	srv.Start()
	t.Cleanup(srv.Close)
	f.clock.add(2 * time.Hour)
	if j := f.waitJob(s, job.ID, 30*time.Second, ended); j.State != tenant.JobDone {
		t.Errorf("job %s once the peer listens, want done", j.State)
	}
	if h, _ := s.Health("lab", "pacs"); h.State != tenant.SenderUp {
		t.Errorf("health %+v, want up", h)
	}
}

// TestStowTLSFailure guards a stow peer whose certificate does not verify
// against the sender's roots: the connection fails as tls_failed, nothing
// is stored, and no request reaches the peer's handler.
func TestStowTLSFailure(t *testing.T) {
	f := newFixture(t)
	peer := &webPeer{}
	peer.t, peer.parts, peer.count = t, map[string][]byte{}, map[string]int{}
	srv := httptest.NewTLSServer(peer)
	t.Cleanup(srv.Close)
	f.archive("ses-1", scanFiles{"mr", generated(t, 1, 256)})
	f.addStow("pacs", srv.URL+"/dicom-web", func(s *tenant.Sender) {
		s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil
	})
	f.createJob("pacs", "ses-1")
	s := f.start(f.config())
	waitFor(t, 30*time.Second, "a failed handshake", func() bool {
		h, _ := s.Health("lab", "pacs")
		return h.Fault == tenant.SenderFaultTLSFailed
	})
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.requests != 0 {
		t.Errorf("the peer's handler got %d requests over a connection that did not verify", peer.requests)
	}
}

// TestStowCredentialGoesOnlyWithItsURL guards a stow sender whose URL and
// credential change together while the supervisor still holds the row it
// read before (no notice of the change): the unit reads the sender as
// stored, so the new credential goes to the new URL only, and the old URL
// receives nothing.
func TestStowCredentialGoesOnlyWithItsURL(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	oldPeer, newPeer := &webPeer{}, &webPeer{auth: "Bearer token-new"}
	oldURL, newURL := startWebPeer(t, oldPeer), startWebPeer(t, newPeer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 2, 256)})
	row := f.addStow("pacs", oldURL)
	cfg := f.config()
	cfg.Interval = time.Hour
	s := f.start(cfg)
	waitFor(t, 10*time.Second, "the sender's first reading", func() bool { _, ok := s.Health("lab", "pacs"); return ok })
	row.STOW.URL, row.STOW.SealedCredential = newURL, []byte("token-new")
	if _, err := f.p.SetSender(f.ctx, row, []tenant.SenderField{tenant.FieldSTOWURL, tenant.FieldSTOWCredential}, f.clock.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	job := f.createJob("pacs", "ses-1")
	if j := f.waitJob(nil, job.ID, 30*time.Second, ended); j.State != tenant.JobDone {
		t.Fatalf("job %s, want done", j.State)
	}
	oldPeer.mu.Lock()
	defer oldPeer.mu.Unlock()
	if oldPeer.requests != 0 {
		t.Errorf("the old URL received %d requests after the change", oldPeer.requests)
	}
}
