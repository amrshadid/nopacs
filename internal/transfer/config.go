package transfer

import (
	"context"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Source is the tenant a job reads from: its transfer jobs, its catalog and
// its corrections.
type Source interface {
	tenant.Transfers
	tenant.SessionReads
	tenant.ScanReads
	tenant.InstanceReads
	// Correction answers the correction with that id, which a move's delete
	// made.
	Correction(ctx context.Context, id string) (tenant.Correction, error)
	// ListCorrections lists corrections newest first; a move that finds its
	// source gone looks there for its own delete.
	ListCorrections(ctx context.Context, filter tenant.CorrectionFilter, page master.Page) ([]tenant.Correction, string, error)
}

// Storages reads the source's archived objects, as package checkedread's
// Stores does: from the object's storage, or from a replica in step.
type Storages interface {
	// Open opens length bytes of the object from offset.
	Open(ctx context.Context, obj checkedread.Object, offset, length int64) (checkedread.Opened, error)
	// Mismatch records an object of a replica found wrong, and returns what
	// recording it failed with.
	Mismatch(ctx context.Context, o checkedread.Opened, err error) error
}

// Target is the tenant a job copies into: its receiver of type job, the row
// the catalog will hold for an instance, and its provenance.
type Target interface {
	tenant.TransferTargets
	// Receiver answers the receiver of that name.
	Receiver(ctx context.Context, name string) (tenant.Receiver, error)
	// AcceptTransfer hands one file to the receiver of type job the arrival
	// names (receive.Pipeline.AcceptTransfer).
	AcceptTransfer(ctx context.Context, a receive.Arrival) receive.Outcome
	// TransferRow answers the row the catalog will hold for the file
	// (receive.Pipeline.TransferRow).
	TransferRow(ctx context.Context, receiver, uid string, sum tenant.SHA256) (receive.TransferRow, error)
}

// Deleter is the source's corrections engine, whose ordinary preview and
// delete a move uses; *correct.Engine is one.
type Deleter interface {
	// Preview answers what a delete of target would remove.
	Preview(ctx context.Context, target tenant.CorrectionTarget) (correct.Preview, error)
	// Delete makes the delete.
	Delete(ctx context.Context, r correct.DeleteRequest) (tenant.Correction, error)
}

// Config is what a Runner needs.
type Config struct {
	// Source is the tenant the job lives in, and SourceID its id, which the
	// target's provenance names.
	Source   Source
	SourceID string
	// Storages opens the source's storages for one pass and returns what
	// closes them.
	Storages func(ctx context.Context) (Storages, func(), error)
	// Target opens the tenant the job copies into by its name: Unavailable
	// while its reception pipeline does not run, NotFound for a tenant that
	// is gone.
	Target func(ctx context.Context, name string) (Target, error)
	// Deleter opens the source's corrections engine for a move's delete,
	// and returns what releases it.
	Deleter func(ctx context.Context) (Deleter, func(), error)
	// Authorize checks again, at each pass, that the key that made the job
	// is still admin on the source and writer on the target. A refusal it
	// answers as a *correct.KeyRefused ends the job failed. Nil asks
	// nothing (the local operator's jobs hold no key).
	Authorize func(ctx context.Context, keyID, targetTenantID string) error
	// DeleteGuard returns the check a move's delete makes, in its own
	// transaction, of the key that made the job; nil for none.
	DeleteGuard func(ctx context.Context, keyID string) (tenant.Guard, error)
	// DeleteAudit returns the audit entry of a move's delete at now.
	DeleteAudit func(j tenant.TransferJob, now time.Time) master.AuditEntry
	// Reads, when set, bounds the reads open at once as the API's streams
	// and the senders are.
	Reads *checkedread.Slots
	// Workers is how many instances are copied at once; 0 is DefaultWorkers.
	Workers int
	// ConfirmWait is how long a copy the target took may stay unfound in its
	// catalog before it is failed; 0 is DefaultConfirmWait.
	ConfirmWait time.Duration
	// Wait returns transfer.wait as it applies on the source tenant now: how
	// long a job may wait on refusals that pass by themselves before it ends
	// failed. Nil, or an error, is DefaultWait.
	Wait func(ctx context.Context) (time.Duration, error)
	// Waiting remembers since when each job has been waiting, across the
	// passes of one server; nil keeps it in the runner, which a pass that
	// makes its runner anew then forgets.
	Waiting *Waiting
	// Now is the clock.
	Now func() time.Time
	// Log takes what a pass reports; nil for none.
	Log *slog.Logger
}

// The defaults.
const (
	// DefaultWorkers is how many instances are copied at once.
	DefaultWorkers = 4
	// DefaultWait is transfer.wait's built-in default.
	DefaultWait = 24 * time.Hour
	// DefaultConfirmWait is how long the target is given to archive a file
	// it took before the copy is failed as unaccounted: its quiet period and
	// its steps come first.
	DefaultConfirmWait = time.Hour
	// pageSize is how many items a pass reads at a time.
	pageSize = 500
	// retryWait is how long a pass that met a temporary failure waits.
	retryWait = 5 * time.Second
)
