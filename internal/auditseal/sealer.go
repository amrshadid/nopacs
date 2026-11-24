package auditseal

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/core/auditchain"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// DefaultInterval is the pass's interval when the setting cannot be read:
// audit.seal-interval's built-in default.
const DefaultInterval = 10 * time.Minute

// maxSealsPerLog bounds the checkpoints one pass makes in one log, so a log
// written faster than it is sealed does not hold the pass for ever; what is
// left waits for the next pass. 100 checkpoints of 10,000 entries are a
// million entries a log a pass.
const maxSealsPerLog = 100

// maxRetries bounds how often a pass reads a log again after a seal it made
// was refused as Conflict (another sealer sealed first).
const maxRetries = 3

// ServerUser is the local user of the entries the server writes for itself,
// on no request: a pruning's entry.
const ServerUser = "server"

// RetentionMethod is the method of a pruning's entry, which the server
// writes for itself (ServerUser) in the log it pruned. Verify reads these
// entries to tell a checkpoint retention pruned from one marked pruned by
// hand.
const RetentionMethod = "AuditRetention"

// TenantLog names one tenant's log: by its id, which the chain's signed
// texts name and a move keeps, and by its name, which the registry opens it
// by.
type TenantLog struct {
	// ID is the tenant's id: the log's name in its checkpoints.
	ID string
	// Name is the tenant's name.
	Name string
}

// Tenants is what a pass needs of the installation's tenants.
type Tenants interface {
	// List returns every tenant now.
	List(ctx context.Context) ([]TenantLog, error)
	// Log returns a tenant's open store, or an error when it is not open
	// (not ready, unreachable): the pass skips it until the next.
	Log(ctx context.Context, name string) (master.AuditSeals, error)
}

// Config is what New needs.
type Config struct {
	// Installation is the installation's id, which every checkpoint names.
	Installation string
	// Master is the master's store.
	Master master.AuditSeals
	// Tenants lists the tenants and opens their logs; nil seals the
	// master's log alone.
	Tenants Tenants
	// Signer signs the checkpoints: the installation's audit signer
	// (secret.Keys.Derive(secret.AuditSeal), then Signer).
	Signer auditchain.Signer
	// Interval returns audit.seal-interval as it is now; nil, or one that
	// fails, is DefaultInterval.
	Interval func(ctx context.Context) (time.Duration, error)
	// Retention returns audit.retention as it is now, 0 to keep everything;
	// nil keeps everything, and one that fails prunes nothing that pass. A
	// period below settings.MinAuditRetention is raised to it, so nothing
	// younger than the floor Verify holds pruned checkpoints to is pruned.
	Retention func(ctx context.Context) (time.Duration, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID makes the id of a pruning's audit entry.
	NewID func() string
	// Log receives a line per checkpoint made and per log a pass could not
	// seal; nil discards them.
	Log *slog.Logger
}

// Sealer runs the sealing pass on a timer, and the last pass when the
// server has drained (Last). Make it with New.
type Sealer struct {
	cfg   Config
	log   *slog.Logger
	stop  context.CancelFunc
	done  sync.WaitGroup
	stats stats
	// passing holds one token while a pass runs, so two passes never run at
	// once in one process (the timer's and the last one), and Pause can
	// wait for a running pass to end and still give up when its caller
	// does, which a mutex cannot.
	passing chan struct{}
	// mu guards held.
	mu sync.Mutex
	// held counts the Pause calls not yet resumed; a pass that starts while
	// it is above zero seals nothing.
	held int
}

// Pause waits for a pass that is running to end and keeps any other from
// sealing until the function it returns is called; that function may be
// called more than once. It gives up, with ctx's error and nothing paused,
// when ctx ends first: a pass stuck on a database that does not answer must
// not hold the caller for ever. A pass that comes due while paused is
// skipped, not waited for, so neither the timer nor Stop waits on what
// paused it; the next pass seals what was written meanwhile, since a seq is
// given at sealing. A backup made while the server runs reads the master
// and each tenant's database at moments apart: a pass between two of those
// reads would leave a database holding an anchor of a checkpoint another
// database's section does not hold yet, which a restore of that backup
// would then report as a log rolled back.
func (s *Sealer) Pause(ctx context.Context) (resume func(), err error) {
	select {
	case s.passing <- struct{}{}:
	case <-ctx.Done():
		return nil, fault.Wrap(fault.Unavailable, ctx.Err(), "auditseal: a sealing pass is running and did not end before the request did")
	}
	s.mu.Lock()
	s.held++
	s.mu.Unlock()
	<-s.passing
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.held--
			s.mu.Unlock()
		})
	}, nil
}

// paused reports whether a Pause is not yet resumed.
func (s *Sealer) paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held > 0
}

// New returns a Sealer that is not yet running.
func New(cfg Config) (*Sealer, error) {
	if cfg.Master == nil || cfg.Signer == nil || cfg.NewID == nil {
		return nil, fault.New(fault.Internal, "auditseal: a sealer needs the master, a signer and ids")
	}
	if err := master.ValidateID("installation id", cfg.Installation); err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sealer{cfg: cfg, log: log, passing: make(chan struct{}, 1)}, nil
}

// Start runs a pass at once and then one every interval, read again after
// each pass, in a goroutine that Stop ends.
func (s *Sealer) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.done.Go(func() {
		for {
			s.Pass(ctx)
			t := time.NewTimer(s.interval(ctx))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	})
	return nil
}

// Stop ends the timer and waits for a pass running. It runs no pass of its
// own: requests still draining and the audit entries the server holds in
// memory are written after it, so the server calls Last once they are, and
// before it closes the databases.
func (s *Sealer) Stop(context.Context) error {
	if s.stop != nil {
		s.stop()
	}
	s.done.Wait()
	return nil
}

// Last runs one last pass within ctx, so the entries written up to the
// stop, those of the requests drained and the audit writer's last batch
// included, are sealed. The server calls it after its drain and its audit
// flush, while the databases are still open.
func (s *Sealer) Last(ctx context.Context) error {
	s.Pass(ctx)
	return nil
}

// interval returns the pass's interval now.
func (s *Sealer) interval(ctx context.Context) time.Duration {
	if s.cfg.Interval == nil {
		return DefaultInterval
	}
	d, err := s.cfg.Interval(ctx)
	if err != nil || d <= 0 {
		return DefaultInterval
	}
	return d
}

// Pass seals every log once, anchors the master and the tenants to each
// other, and prunes what retention makes due. Start runs it on a timer and
// the server once more through Last; a test calls it directly. A log that
// fails is logged and left for the next pass.
func (s *Sealer) Pass(ctx context.Context) {
	s.passing <- struct{}{}
	defer func() { <-s.passing }()
	if s.paused() {
		s.log.Info("auditseal: a backup is reading the databases; this pass is skipped and the next seals what was written meanwhile")
		return
	}
	var oldest time.Time
	masterLast, masterOK := s.seal(ctx, s.cfg.Master, master.MasterAuditLog, "")
	// The anchors the master keeps, read once a pass: a read per tenant
	// would read every tenant's anchor for each.
	held, heldOK := s.heldAnchors(ctx, s.cfg.Master, "")
	var tenants []TenantLog
	if s.cfg.Tenants != nil {
		var err error
		if tenants, err = s.cfg.Tenants.List(ctx); err != nil {
			s.log.Warn("auditseal: the tenants could not be listed; only the installation's log was sealed", "reason", fault.Message(err))
			tenants = nil
		}
	}
	retention := s.retention(ctx)
	for _, t := range tenants {
		if ctx.Err() != nil {
			return
		}
		st, err := s.cfg.Tenants.Log(ctx, t.Name)
		if err != nil {
			continue
		}
		last, ok := s.seal(ctx, st, t.ID, t.Name)
		if masterOK {
			if tenantHeld, ok := s.heldAnchors(ctx, st, t.Name); ok {
				s.anchor(ctx, st, tenantHeld, s.cfg.Master, t.Name, masterLast)
			}
		}
		if ok && heldOK {
			s.anchor(ctx, s.cfg.Master, held, st, t.Name, last)
		}
		s.prune(ctx, st, t.ID, t.Name, retention)
		oldest = older(oldest, s.oldestUnsealed(ctx, st))
	}
	s.prune(ctx, s.cfg.Master, master.MasterAuditLog, "", retention)
	oldest = older(oldest, s.oldestUnsealed(ctx, s.cfg.Master))
	s.stats.setOldest(oldest)
}

// retention returns audit.retention now, or 0 (prune nothing) when it is
// not set up or cannot be read; a period below the floor is raised to it.
func (s *Sealer) retention(ctx context.Context) time.Duration {
	if s.cfg.Retention == nil {
		return 0
	}
	d, err := s.cfg.Retention(ctx)
	if err != nil {
		s.log.Warn("auditseal: audit.retention could not be read; nothing is pruned this pass", "reason", fault.Message(err))
		return 0
	}
	if d > 0 && d < settings.MinAuditRetention {
		d = settings.MinAuditRetention
	}
	return d
}

// logger returns the pass's logger for one log.
func (s *Sealer) logger(name, tenantName string) *slog.Logger {
	if tenantName == "" {
		return s.log.With("log", name)
	}
	return s.log.With("log", name, "tenant", tenantName)
}

// seal seals one log as far as it goes, and returns its latest checkpoint
// and whether it has one. An entry that cannot be encoded holds the log
// back until it is put right, since the entries are sealed in order: it is
// logged as an error naming the entry, and counted.
func (s *Sealer) seal(ctx context.Context, st master.AuditSeals, name, tenantName string) (master.AuditCheckpoint, bool) {
	log := s.logger(name, tenantName)
	last, ok, err := SealLog(ctx, st, s.cfg.Installation, name, s.cfg.Signer, s.cfg.Now, func(c master.AuditCheckpoint) {
		log.Info("auditseal: checkpoint made", "from_seq", c.From, "to_seq", c.To,
			"digest", hex.EncodeToString(c.Digest[:]), "key_version", c.KeyVersion)
	})
	var bad *UnencodableError
	switch {
	case err == nil:
	case errors.As(err, &bad):
		s.stats.unencodable.Add(1)
		log.Error("auditseal: an unsealed entry cannot be encoded; the log is not sealed past it until it is put right",
			"entry_id", bad.ID)
	default:
		s.stats.failed.Add(1)
		log.Warn("auditseal: the log could not be sealed; it is tried again next pass", "reason", fault.Message(err))
	}
	return last, ok
}

// UnencodableError is SealLog's failure when an entry not yet sealed cannot
// be encoded (its text is not valid UTF-8, as only a hand-made row can be):
// the entries are sealed in order, so the log is sealed no further until
// that row is put right. It names the entry by its id, never its content.
type UnencodableError struct {
	// ID is the entry's id.
	ID string
}

// Error implements error.
func (e *UnencodableError) Error() string {
	return "auditseal: the unsealed audit entry " + e.ID + " cannot be encoded"
}

// SealLog seals one log as far as it goes: the entries without a seq, in
// checkpoints of at most master.MaxAuditSealBatch, at most maxSealsPerLog
// of them. made is told of each checkpoint kept. It returns the log's
// latest checkpoint afterward and whether it has one, also when it fails
// part way, with what it kept. An entry that cannot be encoded is an
// *UnencodableError.
func SealLog(ctx context.Context, st master.AuditSeals, installation, name string, signer auditchain.Signer,
	now func() time.Time, made func(master.AuditCheckpoint)) (master.AuditCheckpoint, bool, error) {
	last, ok, err := latest(ctx, st)
	if err != nil {
		return last, ok, err
	}
	retries := 0
	for seals := 0; seals < maxSealsPerLog; {
		entries, err := st.UnsealedAudit(ctx, master.AuditPosition{}, master.MaxAuditSealBatch)
		if err != nil {
			return last, ok, err
		}
		if len(entries) == 0 {
			return last, ok, nil
		}
		from := auditchain.State{}
		if ok {
			from = stateAfter(last)
		}
		chained := make([]auditchain.Entry, len(entries))
		ids := make([]string, len(entries))
		for i, e := range entries {
			chained[i] = chainEntry(int64(from.Seq)+int64(i)+1, e) //nolint:gosec // G115: a seq read from a store.
			ids[i] = e.ID
		}
		c, _, err := auditchain.Seal(signer, installation, name, from, chained, now().UTC())
		if err != nil {
			var ce *auditchain.Error
			if errors.As(err, &ce) && ce.Problem == auditchain.ProblemEncoding {
				if i := ce.Seq - from.Seq - 1; i < uint64(len(ids)) {
					return last, ok, &UnencodableError{ID: ids[i]}
				}
			}
			return last, ok, fault.Wrap(fault.Internal, err, "auditseal: the entries cannot be sealed")
		}
		digest, err := c.Digest()
		if err != nil {
			return last, ok, fault.Wrap(fault.Internal, err, "auditseal: the checkpoint cannot be digested")
		}
		kept := storeCheckpoint(c, digest)
		err = st.SealAudit(ctx, master.AuditSeal{IDs: ids, Checkpoint: kept})
		if fault.KindOf(err) == fault.Conflict && retries < maxRetries {
			// Sealed by another since it was read: read the log again.
			retries++
			if last, ok, err = latest(ctx, st); err != nil {
				return last, ok, err
			}
			continue
		}
		if err != nil {
			return last, ok, err
		}
		// The store keeps the time to the microsecond; so does the signed
		// text, so the checkpoint as read back is this one.
		kept.Time = time.UnixMicro(kept.Time.UnixMicro()).UTC()
		last, ok, retries = kept, true, 0
		seals++
		if made != nil {
			made(kept)
		}
		if len(entries) < master.MaxAuditSealBatch {
			return last, ok, nil
		}
	}
	return last, ok, nil
}

// latest returns a log's latest checkpoint and whether it has one.
func latest(ctx context.Context, st master.AuditSealReads) (master.AuditCheckpoint, bool, error) {
	cs, err := st.RecentAuditCheckpoints(ctx, 0, 1)
	if err != nil || len(cs) == 0 {
		return master.AuditCheckpoint{}, false, err
	}
	return cs[0], true, nil
}

// oldestUnsealed returns the time of the oldest entry of st not yet sealed,
// or zero for none or when it cannot be read.
func (s *Sealer) oldestUnsealed(ctx context.Context, st master.AuditSeals) time.Time {
	es, err := st.UnsealedAudit(ctx, master.AuditPosition{}, 1)
	if err != nil || len(es) == 0 {
		return time.Time{}
	}
	return es[0].Time
}

// older returns the earlier of two times, a zero one counting as none.
func older(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// prune removes what retention makes due in one log, a pruning call at a
// time, until none is due or ctx ends, then seals the log again so the
// pruning's own entry, which Verify needs to accept the pruned checkpoints,
// is sealed at once and not a whole interval later.
//
// The cutoff is the earlier of now and the log's latest checkpoint's time,
// minus the retention: a clock stepped forward (by mistake, or by someone
// who wants entries gone) does not prune what the log's own signed times
// say is younger than the retention.
func (s *Sealer) prune(ctx context.Context, st master.AuditSeals, name, tenantName string, retention time.Duration) {
	if retention <= 0 {
		return
	}
	now := s.cfg.Now().UTC()
	last, ok, err := latest(ctx, st)
	if err != nil || !ok {
		return
	}
	before := now
	if last.Time.Before(before) {
		before = last.Time
	}
	before = before.Add(-retention)
	pruned := false
	for ctx.Err() == nil {
		got, err := st.PruneAudit(ctx, before, master.MaxAuditPruneBatch, master.AuditEntry{
			ID: s.cfg.NewID(), Time: now, LocalUser: ServerUser, RemoteAddress: "local", Method: RetentionMethod,
			TenantName: tenantName, Object: "audit", Outcome: master.OutcomeDone,
		})
		if err != nil {
			s.log.Warn("auditseal: the log could not be pruned; it is tried again next pass", "tenant", tenantName, "reason", fault.Message(err))
			break
		}
		if got.Checkpoints > 0 {
			pruned = true
			s.log.Info("auditseal: entries pruned by audit.retention", "tenant", tenantName,
				"checkpoints", got.Checkpoints, "entries", got.Entries)
		}
		if !got.More {
			break
		}
	}
	if pruned {
		s.seal(ctx, st, name, tenantName)
	}
}
