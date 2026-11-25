package storageswitch

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Owners is what a switch reads and writes of the generations of the
// owner records the tenant's database wrote (tenant.Restoring).
type Owners interface {
	// StorageOwners returns the generation the database wrote for each
	// storage that has one.
	StorageOwners(ctx context.Context) ([]tenant.StorageOwner, error)
	// SetStorageOwner records that the database wrote the owner record of
	// storage at generation.
	SetStorageOwner(ctx context.Context, storage string, generation int64, now time.Time) error
}

// DB is what a Switcher needs of the tenant's database.
type DB interface {
	tenant.Switching
	Owners
	Jobs
}

// Reseal returns the sealed secrets a swap of primary's and replica's
// places writes (tenant.SwitchSwap.PrimarySecret, ReplicaSecret);
// place.SwapSecrets is the server's.
type Reseal func(ctx context.Context, primary, replica tenant.Storage) (forPrimary, forReplica []byte, err error)

// OpenPlace opens the place a storage's row names, to write a few objects
// beside the stores already open on it (place.Opener.OpenBeside).
type OpenPlace func(ctx context.Context, row tenant.Storage) (blob.Store, error)

// The bounds and defaults of a switch.
const (
	// DefaultDrainTimeout is the drain's bound when a request names none;
	// MinDrainTimeout and MaxDrainTimeout bound one it names
	// (SwitchStorageRequest.drain_timeout).
	DefaultDrainTimeout = 10 * time.Minute
	MinDrainTimeout     = time.Second
	MaxDrainTimeout     = time.Hour
	// DefaultPoll is how often a drain reads how many scans are still not
	// in step on the replica.
	DefaultPoll = time.Second
)

// Method is the method the server's own audit entries of a switch name.
const Method = "storage.switch"

// Config is what a Switcher is given. Every port is given, none opened here.
type Config struct {
	// DB is the tenant's database.
	DB DB
	// Tenant is the tenant whose storages are switched: its id, which an
	// owner record and a sealed secret are bound to, and its name, which
	// its audit entries name.
	Tenant place.Tenant
	// Installation is this installation's id, which an owner record names.
	Installation string
	// OwnerKeys authenticates an owner record (secret.Keys).
	OwnerKeys record.OwnerKeyring
	// OpenPlace opens the replica's place to write its owner record.
	OpenPlace OpenPlace
	// Reseal seals each S3 place's secret again for the name it comes to;
	// nil seals nothing, and a swap of an S3 place is then refused by the
	// database, changing nothing.
	Reseal Reseal
	// ConfirmKey makes the confirm tokens.
	ConfirmKey MACKey
	// CompareFrom tells the storage a rebuild compare was built from
	// alone; nil, no compare is told from the replica, and the blocker of
	// an empty compare holds unless skip_verification lifts it.
	CompareFrom CompareFrom
	// Wake asks the replicator to claim again at once (replicate.Worker's
	// Wake); nil does nothing, and the drain waits for its next poll.
	Wake func()
	// WriteRecordsOwed writes the correction records owed to the replica,
	// asked at each turn of the drain: the swap is refused while one is
	// owed (tenant.RefusalRecordsOwed), since the replica's place becomes
	// the primary's. Nil writes none, and the drain waits for whatever
	// else writes them.
	WriteRecordsOwed func(ctx context.Context, replica string) error
	// Clock is the clock; nil is the machine's.
	Clock Clock
	// Poll is how often a drain reads its progress; 0 is DefaultPoll.
	Poll time.Duration
	// GateWait and GateRowTTL are the gate's (GateConfig).
	GateWait, GateRowTTL time.Duration
	// NewID returns a new id for a switch and an audit entry; nil makes a
	// random one.
	NewID func() string
	// Log takes what the Switcher reports: names, phrases and counts.
	Log *slog.Logger
}

// Switcher runs a tenant's switches and holds the gate of its writes. Make
// one with New, call Recover once its tenant is open, and Stop at the end.
type Switcher struct {
	cfg  Config
	gate *Gate
	log  *slog.Logger

	// root is the context every run's waits end with; cancel ends it.
	root   context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	stopped bool
	running map[string]chan struct{}
	wg      sync.WaitGroup

	// seams are a test's hooks into the Switcher.
	seams seams
}

// seams are the points a test steps in at, to place a stop where a request
// could meet one; nil is no hook.
type seams struct {
	// beforeLaunch is called after a switch is accepted in the database and
	// before its run is started.
	beforeLaunch func()
}

// New returns a Switcher for the tenant cfg names, with its gate open.
func New(cfg Config) (*Switcher, error) {
	switch {
	case cfg.DB == nil:
		return nil, fault.New(fault.Internal, "storageswitch: no tenant database")
	case cfg.ConfirmKey == nil:
		return nil, fault.New(fault.Internal, "storageswitch: no confirm key")
	case cfg.OwnerKeys == nil || cfg.OpenPlace == nil:
		return nil, fault.New(fault.Internal, "storageswitch: no way to write an owner record")
	}
	if err := master.ValidateID("tenant id", cfg.Tenant.ID); err != nil {
		return nil, err
	}
	if err := master.ValidateID("installation id", cfg.Installation); err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	if cfg.Wake == nil {
		cfg.Wake = func() {}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	root, cancel := context.WithCancel(context.Background())
	return &Switcher{cfg: cfg, log: cfg.Log.With("tenant", cfg.Tenant.Name), root: root, cancel: cancel, running: map[string]chan struct{}{},
		gate: NewGate(GateConfig{Rows: cfg.DB, Clock: cfg.Clock, RowTTL: cfg.GateRowTTL, Wait: cfg.GateWait})}, nil
}

// Gate returns the gate every write to the tenant's primary storages asks.
func (s *Switcher) Gate() *Gate { return s.gate }

// Preview is what Validate answers (SwitchStorageResponse with
// validate_only).
type Preview struct {
	// Ready says the switch could start now: no blocker holds.
	Ready bool
	// Blockers are what stands in its way, in a fixed order.
	Blockers []string
	// ScansPending counts the primary's scans not in step on the replica.
	ScansPending int64
	// Receivers name the receivers that pause during the switch.
	Receivers []string
	// Confirm is the token the switch must carry.
	Confirm string
	// Generation is the primary's switch generation the token holds.
	Generation int64
}

// Validate answers whether a switch of primary to replica could start now,
// what stands in its way, and its confirm token, changing nothing. With
// skip, the blockers of verification are passed over.
func (s *Switcher) Validate(ctx context.Context, primary, replica string, skip bool) (Preview, error) {
	f, err := s.cfg.DB.SwitchFacts(ctx, primary, replica)
	if err != nil {
		return Preview{}, err
	}
	blockers := tenant.SwitchBlockers(f)
	if !skip {
		more, err := verification(ctx, s.cfg.DB, s.cfg.CompareFrom, replica, f.BackfillDone)
		if err != nil {
			return Preview{}, err
		}
		blockers = append(blockers, more...)
	}
	token, err := makeToken(s.cfg.ConfirmKey, s.cfg.Tenant.ID, s.now(), f)
	if err != nil {
		return Preview{}, err
	}
	return Preview{Ready: len(blockers) == 0, Blockers: blockers, ScansPending: f.ScansNotInStep, Receivers: f.Receivers, Confirm: token,
		Generation: f.Primary.SwitchGeneration}, nil
}

// Request is a switch a person asks for, after its preview.
type Request struct {
	// Primary and Replica name the storages.
	Primary, Replica string
	// Confirm is the preview's token.
	Confirm string
	// SkipVerification and SkipReason pass over the checks of the replica.
	SkipVerification bool
	SkipReason       string
	// DrainTimeout bounds the drain; 0 is DefaultDrainTimeout.
	DrainTimeout time.Duration
	// Audit is the request's entry; Guard the check of who asks.
	Audit master.AuditEntry
	Guard tenant.Guard
}

// errTokenMismatch answers a token that is not one a preview of these
// storages gave, or whose places or generation changed since.
var errTokenMismatch = fault.New(fault.Precondition,
	"the confirm token is not one this server's preview of these storages gave, or either place or the primary's switch generation changed since")

// Begin accepts a switch under its token and starts it, then answers it,
// accepted; Wait answers its end. From the accept on, it runs on the
// Switcher's own goroutine to its end or its refusal, whatever becomes of
// ctx. A token that began a switch already, sent again within its life,
// answers that switch through the database (tenant.Switching.BeginSwitch),
// which checks the caller's guard and records its audit entry as for the
// first request.
func (s *Switcher) Begin(ctx context.Context, r Request) (tenant.StorageSwitch, error) {
	drain := r.DrainTimeout
	if drain == 0 {
		drain = DefaultDrainTimeout
	}
	if drain < MinDrainTimeout || drain > MaxDrainTimeout {
		return tenant.StorageSwitch{}, fault.New(fault.Invalid, "drain_timeout: is 1 second to 1 hour")
	}
	issued, mac, ok := splitToken(r.Confirm)
	if !ok {
		return tenant.StorageSwitch{}, errTokenMismatch
	}
	now := s.now()
	if now.Sub(issued) > TokenLife || issued.Sub(now) > tokenSkew {
		return tenant.StorageSwitch{}, fault.New(fault.Precondition, "the confirm token is more than an hour old; preview the switch again")
	}
	f, err := s.cfg.DB.SwitchFacts(ctx, r.Primary, r.Replica)
	if err != nil {
		return tenant.StorageSwitch{}, err
	}
	match, err := tokenMatches(s.cfg.ConfirmKey, s.cfg.Tenant.ID, issued, mac, f)
	switch {
	case err != nil:
		return tenant.StorageSwitch{}, err
	case !match:
		// The swap of the switch a token began moves the places and raises
		// the generation its MAC was made over, so a token sent again after
		// it no longer matches: it is answered by the switch it began only
		// when the database holds that very token.
		used, err := s.usedToken(ctx, r)
		if err != nil {
			return tenant.StorageSwitch{}, err
		}
		if !used {
			return tenant.StorageSwitch{}, errTokenMismatch
		}
	case !r.SkipVerification:
		more, err := verification(ctx, s.cfg.DB, s.cfg.CompareFrom, r.Replica, f.BackfillDone)
		if err != nil {
			return tenant.StorageSwitch{}, err
		}
		if len(more) > 0 {
			return tenant.StorageSwitch{}, tenant.Precondition(more[0])
		}
	}
	if !s.open() {
		return tenant.StorageSwitch{}, fault.New(fault.Unavailable, "the server is stopping; try again shortly")
	}
	sw, made, err := s.cfg.DB.BeginSwitch(ctx, tenant.SwitchStart{ID: s.cfg.NewID(), Primary: r.Primary, Replica: r.Replica, Token: r.Confirm,
		SkipVerification: r.SkipVerification, SkipReason: r.SkipReason, Now: now}, r.Audit, r.Guard)
	if err != nil || !made {
		return sw, err
	}
	if s.seams.beforeLaunch != nil {
		s.seams.beforeLaunch()
	}
	if !s.launch(sw, drain) {
		return sw, fault.New(fault.Unavailable, "the server is stopping; the switch accepted does not run and is refused when the server starts again")
	}
	return sw, nil
}

// usedToken reports whether the last switch of r's storages was accepted
// with the token r carries, compared in constant time: a token sent again
// after the swap of the switch it began.
func (s *Switcher) usedToken(ctx context.Context, r Request) (bool, error) {
	last, found, err := s.cfg.DB.LastSwitch(ctx, r.Primary)
	if err != nil || !found {
		return false, err
	}
	return last.Primary == r.Primary && last.Replica == r.Replica && hmac.Equal([]byte(last.Token), []byte(r.Confirm)), nil
}

// Wait waits until the switch of id has ended, here or before, and answers
// it: done, or refused with its phrase. An ended ctx answers the switch as
// it stands with ctx's error; the switch runs on.
func (s *Switcher) Wait(ctx context.Context, id string) (tenant.StorageSwitch, error) {
	s.mu.Lock()
	done, ok := s.running[id]
	s.mu.Unlock()
	if ok {
		select {
		case <-done:
		case <-ctx.Done():
			sw, err := s.cfg.DB.Switch(context.WithoutCancel(ctx), id)
			if err != nil {
				return sw, err
			}
			return sw, ctx.Err()
		}
	}
	return s.cfg.DB.Switch(ctx, id)
}

// Recover ends the switches a server that stopped left unfinished: one
// accepted, its swap not committed, is refused ("server restarted"), and
// every write resumes on the old place; one swapped is finished. It is
// called once the tenant's database is open, before Begin.
func (s *Switcher) Recover(ctx context.Context) error {
	left, err := s.cfg.DB.UnfinishedSwitches(ctx)
	if err != nil {
		return err
	}
	for _, sw := range left {
		s.mu.Lock()
		_, mine := s.running[sw.ID]
		s.mu.Unlock()
		if mine {
			continue
		}
		switch sw.State {
		case tenant.SwitchAccepted:
			if _, err := s.cfg.DB.RefuseSwitch(ctx, sw.ID, tenant.RefusalRestarted, s.now(), s.audit(sw, "refused")); err != nil {
				return err
			}
			s.log.Warn("a switch the last server left unfinished was refused; writes resume on the old place", "primary", sw.Primary,
				"replica", sw.Replica, "refusal", tenant.RefusalRestarted)
		case tenant.SwitchSwapped:
			if _, err := s.cfg.DB.FinishSwitch(ctx, sw.ID, s.now()); err != nil {
				return err
			}
			s.log.Info("a switch whose swap committed before the last server stopped was finished", "primary", sw.Primary,
				"replica", sw.Replica)
		}
		s.gate.resume(sw.Primary)
	}
	return nil
}

// Stop ends every run's waits and waits, within ctx, for them to end: a
// run before its swap refuses its switch ("server restarted"), as the next
// start would; one past it finishes.
func (s *Switcher) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// open reports whether the Switcher takes a new switch.
func (s *Switcher) open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.stopped
}

// launch starts the run of an accepted switch on the Switcher's goroutine,
// and reports false, starting nothing, once Stop has begun: the run is
// counted under the same lock Stop marks the Switcher stopped with, so no
// run is added to the wait group after Stop has begun to wait on it. A
// switch accepted and not run stays accepted for the next start to refuse
// (Recover).
func (s *Switcher) launch(sw tenant.StorageSwitch, drain time.Duration) bool {
	done := make(chan struct{})
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return false
	}
	s.running[sw.ID] = done
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, sw.ID)
			s.mu.Unlock()
			close(done)
		}()
		s.run(sw, drain)
	}()
	return true
}

// now returns the clock's time, in UTC and to the microsecond a store
// keeps.
func (s *Switcher) now() time.Time { return s.cfg.Clock.Now().UTC().Truncate(time.Microsecond) }

// audit returns the server's audit entry of a step of switch sw it took
// itself.
func (s *Switcher) audit(sw tenant.StorageSwitch, step string) master.AuditEntry {
	return master.AuditEntry{ID: s.cfg.NewID(), Time: s.now(), LocalUser: "server", Method: Method, TenantName: s.cfg.Tenant.Name,
		Object: sw.Primary, Outcome: master.OutcomeDone, Details: map[string]string{"replica": sw.Replica, "step": step}}
}

// randomID returns 32 random lowercase hex digits, an id master.ValidateID
// takes.
func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24 and later).
	return hex.EncodeToString(b[:])
}
