package events

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Feed is what the Keeper needs of one tenant's event feed: a checkpoint
// and a pruning (tenant.Events).
type Feed interface {
	// CheckpointEvents is tenant.Events.CheckpointEvents.
	CheckpointEvents(ctx context.Context, at time.Time) error
	// PruneEvents is tenant.Events.PruneEvents.
	PruneEvents(ctx context.Context, before time.Time, limit int) (removed int, more bool, err error)
}

// KeptTenants lists the tenants whose feeds the Keeper keeps, and gives
// each one's feed and retention.
type KeptTenants interface {
	// TenantNames returns the names of the tenants now.
	TenantNames(ctx context.Context) ([]string, error)
	// Feed returns a tenant's feed, or an error when its database does not
	// answer, it is not ready, or it keeps no event feed: the Keeper skips
	// it until its next round.
	Feed(ctx context.Context, name string) (Feed, error)
	// Retention returns how long the tenant's events are kept, the setting
	// events.retention as it applies to the tenant.
	Retention(ctx context.Context, name string) (time.Duration, error)
}

// KeeperConfig is what NewKeeper needs.
type KeeperConfig struct {
	// Tenants lists the tenants and gives their feeds.
	Tenants KeptTenants
	// Clock is where the time of a checkpoint comes from; nil is the
	// system's clock. Checkpoints are the only time retention reads, so
	// this is the clock an event's age is measured by.
	Clock Clock
	// Interval is how often a round runs; 0 is tenant.CheckpointInterval.
	Interval time.Duration
	// Logger receives a line for each tenant a round could not keep, with
	// the tenant's name and a fixed phrase; nil discards them.
	Logger *slog.Logger
}

// Keeper keeps every tenant's event feed within its retention: a round, run
// at Start and then every Interval, records a checkpoint of each feed and
// prunes the events up to the newest checkpoint older than the tenant's
// retention, in pruning calls of at most tenant.MaxPruneBatch events, until
// none is due or the round's context ends. A tenant that fails is logged
// and left for the next round, which is safe: a pruning removes events in
// the feed's order and moves its mark with them, so a round cut short
// leaves no gap. Make it with NewKeeper.
type Keeper struct {
	cfg  KeeperConfig
	log  *slog.Logger
	stop context.CancelFunc
	done sync.WaitGroup
}

// NewKeeper returns a Keeper that is not yet running.
func NewKeeper(cfg KeeperConfig) *Keeper {
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = tenant.CheckpointInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Keeper{cfg: cfg, log: log}
}

// Start runs a round at once and then one every Interval, in a goroutine
// that Stop ends.
func (k *Keeper) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	k.stop = cancel
	k.done.Go(func() {
		ticker := k.cfg.Clock.NewTicker(k.cfg.Interval)
		defer ticker.Stop()
		for {
			k.Round(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C():
			}
		}
	})
	return nil
}

// Stop ends the rounds and waits for the one running, which stops between
// two pruning calls.
func (k *Keeper) Stop(context.Context) error {
	if k.stop != nil {
		k.stop()
	}
	k.done.Wait()
	return nil
}

// Round records a checkpoint of every tenant's feed and prunes what is due,
// once. Start runs it on a timer; a test calls it directly.
func (k *Keeper) Round(ctx context.Context) {
	names, err := k.cfg.Tenants.TenantNames(ctx)
	if err != nil {
		k.log.Warn("events: the tenants could not be listed to keep their feeds", "reason", fault.Message(err))
		return
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		k.keep(ctx, name)
	}
}

// keep keeps one tenant's feed.
func (k *Keeper) keep(ctx context.Context, name string) {
	log := k.log.With("tenant", name)
	feed, err := k.cfg.Tenants.Feed(ctx, name)
	if err != nil {
		return
	}
	now := k.cfg.Clock.Now()
	if err := feed.CheckpointEvents(ctx, now); err != nil {
		log.Warn("events: the feed's checkpoint could not be recorded", "reason", fault.Message(err))
		return
	}
	retention, err := k.cfg.Tenants.Retention(ctx, name)
	if err != nil {
		log.Warn("events: the feed's retention could not be read; nothing is pruned", "reason", fault.Message(err))
		return
	}
	before := now.Add(-retention)
	for ctx.Err() == nil {
		_, more, err := feed.PruneEvents(ctx, before, tenant.MaxPruneBatch)
		if err != nil {
			log.Warn("events: the feed could not be pruned", "reason", fault.Message(err))
			return
		}
		if !more {
			return
		}
	}
}
