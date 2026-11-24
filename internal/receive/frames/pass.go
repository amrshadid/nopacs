package frames

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Clock is the pass's source of time: Now for an event's time, After for
// each wait. The reception pipeline's clock is one.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

// Catalog lists a tenant's scans and fills in the header values of one
// archived without them, which is all the pass asks of its database.
type Catalog interface {
	// ListScans is tenant.ScanReads.ListScans.
	ListScans(ctx context.Context, filter tenant.ScanFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogScan], error)
	tenant.ScanValueFill
}

// The pass's bounds and pace.
const (
	// scanPage is how many scans one read of the catalog lists.
	scanPage = 500
	// batchRanges and batchInstances bound one batch of a scan's tables
	// held at once: a scan with more is rewritten once a batch, the last
	// rewrite making it complete.
	batchRanges    = 1 << 20
	batchInstances = 1 << 16
	// busyFactor is how many times as long as an instance took the pass
	// waits after it while the pipeline is busy; when it is not, it waits
	// as long as the instance took.
	busyFactor = 9
	// busyFloor is the least it waits after an instance while the
	// pipeline is busy.
	busyFloor = 50 * time.Millisecond
)

// Config is what a pass is handed. Every port is given; none is opened
// here.
type Config struct {
	// Catalog lists the tenant's scans.
	Catalog Catalog
	// Events is the tenant's feed, for the pass's progress; nil writes no
	// event.
	Events tenant.Events
	// OpenStorage opens one of the tenant's storages by name. The store is
	// the caller's: the pass never closes it.
	OpenStorage func(ctx context.Context, name string) (blob.Store, error)
	// Lock takes the lock the archive step holds while it writes a scan's
	// metadata and manifest on a storage, and returns its release.
	Lock func(storage string, path label.Path) (unlock func())
	// HeaderLimit is the most bytes of a file read as its header (the
	// setting receiver.header-limit).
	HeaderLimit func(ctx context.Context) int64
	// Busy says the pipeline has a step in flight or files arriving; the
	// pass then slows to a fraction of its pace. Nil is never busy.
	Busy func() bool
	// Wake is called after the pass wrote an event. Nil does nothing.
	Wake func()
	// Clock is the pass's clock.
	Clock Clock
	// Logger receives the pass's log lines: labels, counts and fault
	// messages only. Nil discards them.
	Logger *slog.Logger

	// seams are a test's changes to the pass's bounds.
	seams seams
}

// seams are the bounds a test makes smaller, to see a scan done in several
// batches without a scan of that many instances; zero is the default.
type seams struct {
	// batchInstances is the most lines one batch gives a table.
	batchInstances int
}

// Report is what a pass did.
type Report struct {
	// ScansChecked counts the scans whose metadata version was read.
	ScansChecked int64
	// ScansDone counts the scans whose metadata was rewritten complete.
	ScansDone int64
	// InstancesDone counts the instances given a table.
	InstancesDone int64
	// ScansFilled counts the scans listed without a modality whose header
	// values were handed to the catalog.
	ScansFilled int64
}

// Run makes one pass over every scan of the tenant, in label order, and
// gives each scan's metadata of version 1 the frame tables its lines lack,
// and each scan the catalog lists without a modality, whatever its
// metadata's version, the header values its files say (the catalog's
// values, kept since a later version than the scans may have been archived
// by).
// It returns when every scan was checked, or at the first failure that is
// not a fact of a file (a storage or the database not answering, ctx
// ending), with what it did so far; a pass run again goes on from the
// first scan not done, and a pass over scans all done changes nothing.
func Run(ctx context.Context, cfg Config) (Report, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	r := &runner{cfg: cfg, stores: map[string]blob.Store{}}
	cursor := ""
	for {
		page, err := cfg.Catalog.ListScans(ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: scanPage, Cursor: cursor})
		if err != nil {
			return r.rep, err
		}
		for _, sc := range page.Items {
			path := label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
			if err := r.scan(ctx, sc.Storage, path); err != nil {
				return r.rep, err
			}
			if sc.Values.Modality == "" {
				if err := r.fill(ctx, sc, path); err != nil {
					return r.rep, err
				}
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if r.rep.ScansDone > 0 {
		r.event(ctx, &tenant.FramesAdded{ScansDone: r.rep.ScansDone, InstancesDone: r.rep.InstancesDone, Finished: true}, "end")
	}
	return r.rep, nil
}

// runner is one pass in progress.
type runner struct {
	cfg Config
	rep Report
	// stores are the storages opened so far, by name.
	stores map[string]blob.Store
}

// storage returns the storage of that name, opened once a pass.
func (r *runner) storage(ctx context.Context, name string) (blob.Store, error) {
	if s, ok := r.stores[name]; ok {
		return s, nil
	}
	s, err := r.cfg.OpenStorage(ctx, name)
	if err != nil {
		return nil, err
	}
	r.stores[name] = s
	return s, nil
}

// pause waits after a piece of work that took worked: as long again, or
// busyFactor times as long and at least busyFloor while the pipeline is
// busy, so reception always has the larger share.
func (r *runner) pause(ctx context.Context, worked time.Duration) error {
	d := worked
	if r.cfg.Busy != nil && r.cfg.Busy() {
		d = max(worked*busyFactor, busyFloor)
	}
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.cfg.Clock.After(d):
		return nil
	}
}

// event appends one progress event, at the clock's time. A failure is
// logged: the progress itself is in the sidecars, which an event only
// reports.
func (r *runner) event(ctx context.Context, p *tenant.FramesAdded, keyed string) {
	if r.cfg.Events == nil {
		return
	}
	ev, err := tenant.NewEvent(p, r.cfg.Clock.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		r.cfg.Logger.Error("frames: an event the pass made is not valid; it is not written", "reason", fault.Message(err))
		return
	}
	sum := sha256.Sum256([]byte(keyed + "\x00" + ev.Payload() + "\x00" + ev.Time().String()))
	if _, err := r.cfg.Events.AppendEvents(ctx, []tenant.EventAppend{{Key: "frames-" + hex.EncodeToString(sum[:20]), Event: ev}}); err != nil {
		r.cfg.Logger.Warn("frames: a progress event could not be written", "reason", fault.Message(err))
		return
	}
	if r.cfg.Wake != nil {
		r.cfg.Wake()
	}
}

// metadataKey returns the key of the scan's sidecar.
func metadataKey(path label.Path) (string, error) {
	key, err := layout.MetadataKey(path)
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	return key, nil
}

// readHead reads the first bytes of the scan's sidecar, the ones that say
// its version, or reports that there is none.
func readHead(ctx context.Context, store blob.Store, key string, n int) ([]byte, bool, error) {
	rc, err := store.Read(ctx, key, 0, int64(n))
	if fault.KindOf(err) == fault.NotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, storageError(err, "the scan's metadata cannot be read")
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, int64(n)))
	if err != nil {
		return nil, false, storageError(err, "the scan's metadata cannot be read")
	}
	return b, true, nil
}

// storageError keeps a storage's failure with its kind: one the storage
// does not classify is the storage not answering.
func storageError(err error, message string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	k := fault.KindOf(err)
	if k == fault.Internal {
		k = fault.Unavailable
	}
	return fault.Wrap(k, err, message)
}
