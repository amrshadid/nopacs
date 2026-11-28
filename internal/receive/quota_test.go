package receive

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// quotaRig is an env whose pipeline reads its quota from bounds, which a
// test changes as it goes.
type quotaRig struct {
	*env
	bounds atomic.Pointer[tenant.QuotaBounds]
}

// newQuotaRig makes an env and starts its pipeline again over the same
// store with a quota of b.
func newQuotaRig(t *testing.T, b tenant.QuotaBounds) *quotaRig {
	t.Helper()
	r := &quotaRig{env: newEnv(t, envOptions{})}
	r.bounds.Store(&b)
	if err := r.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.cfg.Quota = func(context.Context) (tenant.QuotaBounds, error) { return *r.bounds.Load(), nil }
	r.start()
	return r
}

// setBounds changes the quota and moves the clock past the time a bound
// read is kept, so the next file reads it.
func (r *quotaRig) setBounds(b tenant.QuotaBounds) {
	r.bounds.Store(&b)
	r.clock.Advance(HeaderLimitInterval + time.Millisecond)
}

// use reads the tenant's use from its database.
func (r *quotaRig) use() tenant.QuotaUse {
	r.t.Helper()
	u, err := r.store.(tenant.Quotas).QuotaUse(r.ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return u
}

// sendOver hands a corpus file to the pipeline and requires it refused for
// the quota.
func (r *quotaRig) sendOver(name string) {
	r.t.Helper()
	if out := r.sendBytes(corpus(r.t, name), Refused); out.Reason != ReasonOverQuota {
		r.t.Fatalf("a file past the quota is refused for %q, want %q", out.Reason, ReasonOverQuota)
	}
}

// TestQuotaRefusesNewFilesAtTheBound guards the check at the start of each
// file: a bound of three instances takes three files and refuses the
// fourth as a temporary failure keeping nothing; once the three are
// archived the fourth is still refused, the database counting them held;
// a raised bound takes it; and every admission is given back once its row
// is written.
func TestQuotaRefusesNewFilesAtTheBound(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{Instances: 3, WarnPercent: 90})
	r.send(studyFiles[:3]...)
	before := r.incomingFiles()
	r.sendOver(studyFiles[3])
	if got := r.incomingFiles(); got != before {
		t.Errorf("a refused file left %d files in the incoming area, want %d", got, before)
	}
	rs := r.settle(1)
	if len(rs) != 1 || rs[0].State != reception.Done || rs[0].Counts.FilesArchived != 3 {
		t.Fatalf("the receptions are %+v, want one done with three archived", rs)
	}
	if u := r.use(); u.HeldInstances != 3 || u.Instances() != 3 || u.ReceivingInstances != 0 {
		t.Errorf("the use after the archive is %+v, want three held", u)
	}
	r.clock.Advance(HeaderLimitInterval + time.Millisecond)
	r.sendOver(studyFiles[3])
	if files, landed := r.p.admittedForQuota(); files != 0 || landed != 0 {
		t.Errorf("once the use counts every row the gate holds %d files of %d bytes, want none", files, landed)
	}
	r.setBounds(tenant.QuotaBounds{Instances: 4, WarnPercent: 90})
	r.send(studyFiles[3])
	r.sendOver(studyFiles[4])
}

// TestQuotaReceptionInProgressFinishes guards that nothing received is lost
// when the bound is reached: three files of a study taken under the bound,
// the bound then lowered below what they make, refuses the next file, and
// the reception still archives all three, which leaves the tenant past its
// bound by what it had taken.
func TestQuotaReceptionInProgressFinishes(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{Instances: 10, WarnPercent: 90})
	r.send(studyFiles[:3]...)
	r.setBounds(tenant.QuotaBounds{Instances: 1, WarnPercent: 90})
	r.sendOver(studyFiles[3])
	rs := r.settle(1)
	if len(rs) != 1 || rs[0].State != reception.Done || rs[0].Counts.FilesArchived != 3 || rs[0].Counts.FilesQuarantined != 0 {
		t.Fatalf("the receptions are %+v, want one done with its three files archived", rs)
	}
	if u := r.use(); u.HeldInstances != 3 {
		t.Errorf("the use is %+v, want the three archived", u)
	}
}

// TestQuotaTwoAtTheEdge guards the lock the check is made under: files
// sent at once to a tenant with room for one instance more are admitted
// one at a time, so exactly one is taken and the rest refused for the
// quota, never two seeing the same room. A bytes bound with room for one
// byte takes at least one file and refuses the rest past the files then in
// flight, its stated slack.
func TestQuotaTwoAtTheEdge(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{Instances: 1, WarnPercent: 90})
	outcomes := sendAtOnce(r.env, studyFiles)
	taken, refused := 0, 0
	for _, out := range outcomes {
		switch {
		case out.Status == Received:
			taken++
		case out.Status == Refused && out.Reason == ReasonOverQuota:
			refused++
		default:
			t.Errorf("a file sent at the edge answered %+v", out)
		}
	}
	if taken != 1 || refused != len(studyFiles)-1 {
		t.Fatalf("at a bound with room for one instance, %d files were taken and %d refused, want 1 and %d", taken, refused, len(studyFiles)-1)
	}
	r.settle(1)
	if u := r.use(); u.Instances() != 1 {
		t.Errorf("the use is %+v, want the one instance the bound allows", u)
	}

	b := newQuotaRig(t, tenant.QuotaBounds{Bytes: 1, WarnPercent: 90})
	taken = 0
	for _, out := range sendAtOnce(b.env, studyFiles) {
		if out.Status == Received {
			taken++
		}
	}
	// Every file at once may be admitted before any has landed: that is the
	// stated slack of a bytes bound, so only the first is required.
	if taken < 1 {
		t.Errorf("at a bound with room for one byte, %d of %d files sent at once were taken, want at least one", taken, len(studyFiles))
	}
	b.sendOver(studyFiles[0])
}

// sendAtOnce hands every named corpus file to the env's pipeline at the
// same moment, each from its own goroutine, and returns their outcomes in
// order.
func sendAtOnce(e *env, names []string) []Outcome {
	e.t.Helper()
	bodies := make([][]byte, len(names))
	for i, n := range names {
		bodies[i] = corpus(e.t, n)
	}
	out := make([]Outcome, len(names))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i] = e.p.Accept(e.ctx, Arrival{Receiver: "dimse", Body: bytes.NewReader(bodies[i]),
				Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"}})
		}()
	}
	close(start)
	wg.Wait()
	return out
}

// TestQuotaUnreadRefuses guards that a quota that cannot be read refuses
// rather than lets a file through: the bounds unread since the pipeline
// started answer the file refused for the database, keeping nothing.
func TestQuotaUnreadRefuses(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{})
	if err := r.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.cfg.Quota = func(context.Context) (tenant.QuotaBounds, error) {
		return tenant.QuotaBounds{}, errors.New("settings unreadable")
	}
	r.start()
	if out := r.sendBytes(corpus(t, studyFiles[0]), Refused); out.Reason != reasonUnavailable {
		t.Errorf("a file whose quota cannot be read is refused for %q, want %q", out.Reason, reasonUnavailable)
	}
	if n := r.incomingFiles(); n != 0 {
		t.Errorf("the refused file left %d files in the incoming area", n)
	}
}

// TestQuotaBoundsErrorAfterGoodReadRefuses guards that a quota that cannot
// be read refuses even after it was read once: a good read of the bounds
// (here, no bound), then a failing one once the cached read has expired,
// then a file, which must be refused rather than admitted on the stale
// bounds.
func TestQuotaBoundsErrorAfterGoodReadRefuses(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{})
	if err := r.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	r.cfg.Quota = func(context.Context) (tenant.QuotaBounds, error) {
		if fail.Load() {
			return tenant.QuotaBounds{}, errors.New("settings unreadable")
		}
		return tenant.QuotaBounds{WarnPercent: 90}, nil
	}
	r.start()
	r.send(studyFiles[0])
	fail.Store(true)
	r.clock.Advance(HeaderLimitInterval + time.Millisecond)
	if out := r.sendBytes(corpus(t, studyFiles[1]), Refused); out.Reason != reasonUnavailable {
		t.Errorf("a file whose bounds cannot be read again is refused for %q, want %q", out.Reason, reasonUnavailable)
	}
}

// failingUse is a store whose use read fails or blocks when told to.
type failingUse struct {
	tenant.Pipeline
	fail  atomic.Bool
	block chan struct{}
	reads atomic.Int64
}

// QuotaUse reads the wrapped store's use unless told to fail or block.
func (f *failingUse) QuotaUse(ctx context.Context) (tenant.QuotaUse, error) {
	f.reads.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return tenant.QuotaUse{}, ctx.Err()
		}
	}
	if f.fail.Load() {
		return tenant.QuotaUse{}, errors.New("use unreadable")
	}
	return f.Pipeline.(tenant.Quotas).QuotaUse(ctx)
}

// wrapUse starts the rig's pipeline again over a store whose use read can
// fail or block on the channel.
func (r *quotaRig) wrapUse(block chan struct{}) *failingUse {
	r.t.Helper()
	if err := r.p.Stop(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	f := &failingUse{block: block}
	r.dbWrap = func(s tenant.Pipeline) tenant.Pipeline { f.Pipeline = s; return f }
	r.start()
	return f
}

// TestQuotaUseErrorAfterGoodReadRefuses guards that a use that cannot be
// read refuses even after a good read: the cached use expires, its read
// fails, and the next file is refused rather than admitted on the old use.
func TestQuotaUseErrorAfterGoodReadRefuses(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{Instances: 100, WarnPercent: 90})
	f := r.wrapUse(nil)
	r.send(studyFiles[0])
	f.fail.Store(true)
	r.clock.Advance(HeaderLimitInterval + time.Millisecond)
	if out := r.sendBytes(corpus(t, studyFiles[1]), Refused); out.Reason != reasonUnavailable {
		t.Errorf("a file whose use cannot be read again is refused for %q, want %q", out.Reason, reasonUnavailable)
	}
}

// TestQuotaCanceledAcceptDoesNotWait guards that a caller that gave up
// leaves at once: another file's use read that never returns does not hold
// a canceled Accept, which is refused.
func TestQuotaCanceledAcceptDoesNotWait(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{Instances: 100, WarnPercent: 90})
	f := r.wrapUse(make(chan struct{}))
	arrival := func(name string) Arrival {
		return Arrival{Receiver: "dimse", Body: bytes.NewReader(corpus(t, name)),
			Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"}}
	}
	stuck := make(chan Outcome, 1)
	go func() { stuck <- r.p.Accept(r.ctx, arrival(studyFiles[0])) }()
	for f.reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(r.ctx)
	cancel()
	done := make(chan Outcome, 1)
	go func() { done <- r.p.Accept(ctx, arrival(studyFiles[1])) }()
	select {
	case out := <-done:
		if out.Status != Refused {
			t.Errorf("a canceled Accept answered %+v, want refused", out)
		}
	case <-time.After(5 * time.Second):
		t.Error("a canceled Accept is stuck behind another file's use read")
	}
	close(f.block)
	<-stuck
}

// TestQuotaManyAtTheEdge guards the instance bound under load: fifty files
// sent at once to a tenant with room for ten instances take exactly ten,
// the use read held for a second between them and the files admitted in
// memory not yet counted by it.
func TestQuotaManyAtTheEdge(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{Instances: 10, WarnPercent: 90})
	names := make([]string, 50)
	for i := range names {
		names[i] = studyFiles[i%len(studyFiles)]
	}
	taken := 0
	for _, out := range sendAtOnce(r.env, names) {
		switch {
		case out.Status == Received:
			taken++
		case out.Status == Refused && out.Reason == ReasonOverQuota:
		default:
			t.Errorf("a file sent at the edge answered %+v", out)
		}
	}
	if taken != 10 {
		t.Fatalf("at a bound with room for ten instances, %d of 50 files were taken, want 10", taken)
	}
}

// importStore is a store that records the received files of an import as
// plain ones, so a test can drive the import path (the row written before
// Accept returns) without a job and its lease.
type importStore struct{ tenant.Pipeline }

// QuotaUse passes the use through, so the gate still sees a store that
// counts.
func (s importStore) QuotaUse(ctx context.Context) (tenant.QuotaUse, error) {
	return s.Pipeline.(tenant.Quotas).QuotaUse(ctx)
}

// AddReceivedFiles writes the files without the import's arrival.
func (s importStore) AddReceivedFiles(ctx context.Context, fs []tenant.ReceivedFile, now time.Time) (int, error) {
	for i := range fs {
		fs[i].Import = nil
	}
	return s.Pipeline.AddReceivedFiles(ctx, fs, now)
}

// TestQuotaImportsKeepCountingAfterTheirRow guards the files an import
// hands in, whose row is written before Accept returns: the admission must
// stay counted until a read of the use has seen the row. Ten imports within
// one cache interval at a bound of three instances take exactly three.
func TestQuotaImportsKeepCountingAfterTheirRow(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{Instances: 3, WarnPercent: 90})
	if err := r.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AddReceiver(r.ctx, tenant.Receiver{
		Name: "folder", Type: tenant.ReceiverImport, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		Import: tenant.ImportReceiver{Root: "r", Layout: importpath.Plain},
	}, r.clock.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.dbWrap = func(s tenant.Pipeline) tenant.Pipeline { return importStore{s} }
	r.start()
	// Read the use once, so every import below runs on the cached one.
	r.clock.Advance(HeaderLimitInterval + time.Millisecond)
	taken := 0
	for i := range 10 {
		out := r.p.Accept(r.ctx, Arrival{Receiver: "folder", Body: bytes.NewReader(corpus(t, studyFiles[i%len(studyFiles)])),
			Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"},
			Import: &tenant.ImportArrival{Fence: tenant.WorkFence{ID: "unit", Attempt: 1}, Path: "a/b.dcm"}})
		switch {
		case out.Status == Received:
			taken++
		case out.Status == Refused && out.Reason == ReasonOverQuota:
		default:
			t.Fatalf("import %d answered %+v", i, out)
		}
	}
	if taken != 3 {
		t.Errorf("at a bound of three instances, %d of 10 imports were taken, want 3", taken)
	}
}
