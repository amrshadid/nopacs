package send

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxSenderPages bounds the pages of one reading of a tenant's senders, so
// a store that never ends its list cannot hold the worker.
const maxSenderPages = 1000

// tenantRun is the worker of one tenant: its senders' pools, its bound on
// associations, and the database they work through.
type tenantRun struct {
	sup  *Supervisor
	name string
	log  *slog.Logger

	poke    chan struct{}
	retired chan struct{}
	once    sync.Once

	// p is the tenant's database as last found, nil while it is not ready.
	p     atomic.Pointer[pipelineRef]
	limit *assocLimit
	kept  atomic.Int64
	// gen counts the pokes, so an idle worker sees one it missed.
	gen atomic.Uint64
	// reclaimed says the tenant's send units were reclaimed once, as the
	// process started working it.
	reclaimed bool

	mu      sync.Mutex
	senders map[string]*senderRun
}

// pipelineRef holds a tenant's database for an atomic pointer.
type pipelineRef struct{ p tenant.Pipeline }

// newTenantRun returns the worker of the tenant named name.
func newTenantRun(s *Supervisor, name string) *tenantRun {
	t := &tenantRun{
		sup: s, name: name, log: s.log.With("tenant", name),
		poke: make(chan struct{}, 1), retired: make(chan struct{}),
		limit: newAssocLimit(DefaultTenantAssociations), senders: map[string]*senderRun{},
	}
	t.kept.Store(tenant.DefaultFailuresKept)
	return t
}

// wake makes the worker read its senders, and its idle workers look for
// work, at once. It never blocks.
func (t *tenantRun) wake() {
	t.gen.Add(1)
	select {
	case t.poke <- struct{}{}:
	default:
	}
	t.mu.Lock()
	for _, sr := range t.senders {
		sr.nudge()
	}
	t.mu.Unlock()
}

// retire ends the worker: it stops its senders' workers and returns.
func (t *tenantRun) retire() { t.once.Do(func() { close(t.retired) }) }

// pipeline returns the tenant's database, or nil while it is not ready.
func (t *tenantRun) pipeline() tenant.Pipeline {
	if r := t.p.Load(); r != nil {
		return r.p
	}
	return nil
}

// health returns the health of a sender of the tenant.
func (t *tenantRun) health(name string) (tenant.SenderHealthChanged, bool) {
	t.mu.Lock()
	sr := t.senders[name]
	t.mu.Unlock()
	if sr == nil {
		return tenant.SenderHealthChanged{}, false
	}
	return sr.health.get(), true
}

// run reconciles the tenant until retired, then stops its senders.
func (t *tenantRun) run() {
	defer t.sup.wg.Done()
	defer t.shutdown()
	for {
		t.reconcile()
		timer := time.NewTimer(t.sup.cfg.Interval)
		select {
		case <-t.retired:
			timer.Stop()
			return
		case <-t.poke:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// reconcile makes the tenant's pools match its senders' rows.
func (t *tenantRun) reconcile() {
	now := t.sup.cfg.Now()
	st, err := t.sup.cfg.Tenants.Store(t.name)
	var p tenant.Pipeline
	if err == nil {
		p, _ = st.(tenant.Pipeline)
	}
	if p == nil {
		t.p.Store(nil)
		t.eachSender(func(sr *senderRun) {
			sr.report(sr.health.set(tenant.SenderDown, tenant.SenderDownTenantNotReady, phraseTenantNotReady, now), now)
		})
		return
	}
	t.p.Store(&pipelineRef{p: p})
	ctx, cancel := context.WithTimeout(t.sup.hard, opTimeout)
	defer cancel()
	ts := t.sup.cfg.TenantSettings(st)
	if n, err := ts.MaxAssociations(ctx); err == nil {
		t.limit.setMax(n)
	} else {
		t.log.Warn("senders: sender.tenant-max-associations could not be read; the last value holds", "reason", fault.Message(err))
	}
	if n, err := ts.FailuresKept(ctx); err == nil {
		t.kept.Store(int64(n))
	} else {
		t.log.Warn("senders: job.failures-kept could not be read; the last value holds", "reason", fault.Message(err))
	}
	rows, err := t.readSenders(ctx, p)
	if err != nil {
		t.log.Warn("senders: the tenant's senders could not be read; nothing changes", "reason", fault.Message(err))
		return
	}
	t.reclaim(ctx, p, rows, now)
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		seen[row.Name] = true
		credential, err := credentialDigest(ctx, p, row)
		if err != nil {
			t.log.Warn("senders: a sender's credential could not be read; it is left as it runs", "sender", row.Name, "reason", fault.Message(err))
			continue
		}
		t.mu.Lock()
		sr := t.senders[row.Name]
		if sr == nil {
			sr = newSenderRun(t, row, now)
			t.senders[row.Name] = sr
		}
		t.mu.Unlock()
		sr.update(row, credential, now)
	}
	t.mu.Lock()
	var gone []*senderRun
	for name, sr := range t.senders {
		if !seen[name] {
			delete(t.senders, name)
			gone = append(gone, sr)
		}
	}
	t.mu.Unlock()
	for _, sr := range gone {
		sr.stopAll(false, reasonRemoved)
	}
}

// credentialDigest returns the SHA-256 of a stow sender's sealed
// credential, so a change of it is seen without the credential being held;
// zero for a sender that has none.
func credentialDigest(ctx context.Context, p tenant.Pipeline, row tenant.Sender) ([32]byte, error) {
	if row.Type != tenant.SenderSTOW || row.STOW.Authentication == tenant.StowAuthNone || row.STOW.Authentication == "" {
		return [32]byte{}, nil
	}
	sealed, err := p.SenderCredential(ctx, row.Name)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return [32]byte{}, nil
	case err != nil:
		return [32]byte{}, err
	}
	return sha256.Sum256(sealed), nil
}

// reclaim gives back, once per process and before any worker of the tenant
// claims, the send units a server that is gone left leased, so they are
// taken up at once rather than when their lease runs out. Only the kinds of
// the tenant's senders (tenant.WorkReclaim.ReclaimWorkOfKinds), so a unit
// the reception pipeline's live workers hold is never touched; a tenant
// with more senders than one call names is reclaimed in several.
func (t *tenantRun) reclaim(ctx context.Context, p tenant.Pipeline, rows []tenant.Sender, now time.Time) {
	if t.reclaimed {
		return
	}
	t.reclaimed = true
	kinds := make([]string, len(rows))
	for i, row := range rows {
		kinds[i] = tenant.SendKind(row.Name)
	}
	total := 0
	for chunk := range slices.Chunk(kinds, tenant.MaxReclaimKinds) {
		n, err := p.ReclaimWorkOfKinds(ctx, chunk, now)
		if err != nil {
			t.log.Warn("senders: the units a stopped process held could not be taken back; their leases run out", "reason", fault.Message(err))
			return
		}
		total += n
	}
	if total > 0 {
		t.log.Info("senders: units a stopped process held were taken back", "units", total)
	}
}

// readSenders reads every sender of the tenant, page after page.
func (t *tenantRun) readSenders(ctx context.Context, p tenant.Pipeline) ([]tenant.Sender, error) {
	var out []tenant.Sender
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxSenderPages {
		batch, next, err := p.ListSenders(ctx, page)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
	return nil, fault.New(fault.Internal, "the tenant's list of senders did not end")
}

// eachSender calls fn for every sender the worker runs.
func (t *tenantRun) eachSender(fn func(*senderRun)) {
	t.mu.Lock()
	all := make([]*senderRun, 0, len(t.senders))
	for _, sr := range t.senders {
		all = append(all, sr)
	}
	t.mu.Unlock()
	for _, sr := range all {
		fn(sr)
	}
}

// shutdown marks every sender stopping and stops its workers, each giving
// its unit back.
func (t *tenantRun) shutdown() {
	now := t.sup.cfg.Now()
	t.eachSender(func(sr *senderRun) {
		sr.report(sr.health.set(tenant.SenderDown, tenant.SenderDownStopping, phraseStopping, now), now)
	})
	t.eachSender(func(sr *senderRun) { sr.stopAll(true, reasonStopping) })
}

// acquire takes one of the installation's associations and one of the
// tenant's, or reports false when either is at its bound; the function
// returned gives both back.
func (t *tenantRun) acquire() (func(), bool) {
	if !t.sup.inst.take() {
		return nil, false
	}
	if !t.limit.take() {
		t.sup.inst.give()
		return nil, false
	}
	return func() {
		t.limit.give()
		t.sup.inst.give()
	}, true
}

// appendEvents writes events the supervisor reports into the tenant's
// feed, all at the time at, and wakes its watchers. A failure is logged:
// the state it reports is still in memory.
func (t *tenantRun) appendEvents(ps []tenant.EventPayload, at time.Time) {
	feed, ok := t.pipeline().(tenant.Events)
	if !ok || len(ps) == 0 {
		return
	}
	batch := make([]tenant.EventAppend, 0, len(ps))
	for _, pl := range ps {
		ev, err := tenant.NewEvent(pl, at.UTC().Truncate(time.Microsecond))
		if err != nil {
			t.log.Error("senders: an event the supervisor made is not valid; it is not written", "reason", fault.Message(err))
			continue
		}
		batch = append(batch, tenant.EventAppend{Key: newKey(), Event: ev})
	}
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.sup.hard), opTimeout)
	defer cancel()
	if _, err := feed.AppendEvents(ctx, batch); err != nil {
		t.log.Warn("senders: events could not be written", "count", len(batch), "reason", fault.Message(err))
		return
	}
	t.sup.cfg.Wake(t.name)
}
