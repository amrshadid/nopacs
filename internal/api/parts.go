package api

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// HealthState is whether a receiver accepts data now (receiver.proto,
// ReceiverState).
type HealthState int

// The states.
const (
	// HealthStarting: the server is starting the receiver, or restarting it
	// after a change.
	HealthStarting HealthState = iota + 1
	// HealthUp: it accepts data from the callers it admits.
	HealthUp
	// HealthDown: it does not; the reason says why.
	HealthDown
	// HealthIdle: it holds no listener and takes only what its jobs hand
	// it, an import receiver that is enabled.
	HealthIdle
)

// DownReason is why a receiver is down (receiver.proto, ReceiverDownReason).
type DownReason int

// The reasons, each the contract's of the same name.
const (
	DownNone DownReason = iota
	DownDisabled
	DownAddressInUse
	DownPermissionDenied
	DownAddressNotAvailable
	DownTLS
	DownNearlyFull
	DownTenantNotReady
	DownStopping
	DownOther
	DownIncomingLimit
)

// Health is what the server knows of a receiver now.
type Health struct {
	// State is its state.
	State HealthState
	// Reason is why it is down, DownNone unless State is HealthDown.
	Reason DownReason
	// Detail is a fixed phrase of the server's for a person, at most 256
	// bytes; never an operating system's or a library's own text, a path,
	// a secret, or anything of another program or tenant. Empty when up.
	Detail string
	// Since is when it entered this state with this reason.
	Since time.Time
}

// Receivers answers a receiver's health, which only the part of the server
// that runs receivers knows. It is called on the request's path, once for
// each receiver answered, so it reads memory and never a network or a disk.
type Receivers interface {
	// Health returns the health of the receiver r of the tenant named
	// tenantName, as configured now.
	Health(tenantName string, r tenant.Receiver) Health
}

// notStarted is the Receivers of a server that runs no receivers yet: every
// receiver is down, and says why in the contract's terms. A receiver that
// is not enabled is down because it is disabled, which is true whatever
// runs it; one that is enabled is down for a reason no other value names,
// with the detail "not started", since nothing starts it. Since is when its
// configuration last changed, which is when it entered that state. The
// part of the server that runs receivers replaces this with the health it
// sees: starting, up, or down with the reason it met.
type notStarted struct{}

// detailNotStarted is the phrase of an enabled receiver nothing starts.
const detailNotStarted = "not started"

// Health answers down, disabled or not started.
func (notStarted) Health(_ string, r tenant.Receiver) Health {
	if !r.Enabled {
		return Health{State: HealthDown, Reason: DownDisabled, Since: r.Updated}
	}
	return Health{State: HealthDown, Reason: DownOther, Detail: detailNotStarted, Since: r.Updated}
}

// Incoming is what follows a person's decision in the server's incoming
// area and the tenant's work queue, which the reception pipeline owns. A
// release and a retry run through it whole, with the tenant's store the
// handler resolved, so the pipeline gives a released file's bytes their
// landed name before the store records the release and queues a retry's
// first unit at once; a release recorded by the store alone would leave a
// file that joins no reception under a name nothing keeps, which the
// pipeline's next start removes. A rejection or a discard the handler
// records through the store itself, and hands the removal of the bytes
// here; a handler never deletes a file itself.
type Incoming interface {
	// Release releases a held file (tenant.QuarantineReleases.ReleaseFile)
	// with what follows it.
	Release(ctx context.Context, tenantName string, store tenant.Pipeline, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error)
	// Retry makes a new reception of a failed one's files
	// (tenant.Pipeline.RetryReception) with what follows it.
	Retry(ctx context.Context, tenantName string, store tenant.Pipeline, id, newReceptionID, receiver string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (failed, retry tenant.Reception, err error)
	// RemoveDiscarded removes the bytes of the files of a discarded
	// reception that no quarantine record keeps, as the tenant's store lists
	// them (tenant.Pipeline.DiscardedFiles), after the store has recorded the
	// discard. It may return before the bytes are gone; a file it does not
	// remove is removed when the pipeline next starts, which finds the
	// discard recorded. An error is logged, never answered: the discard is
	// already recorded.
	RemoveDiscarded(ctx context.Context, tenantName, receptionID string) error
	// RemoveRejected removes the bytes of a rejected quarantined file, held
	// at incoming in the incoming area, after the store has recorded the
	// rejection; as RemoveDiscarded, it may finish later.
	RemoveRejected(ctx context.Context, tenantName, incoming string) error
}

// leaveForPipeline is the Incoming of a server that runs no reception
// pipeline, as the API's own tests do: a release and a retry are the
// store's alone, and no bytes are removed. It is not for a server that
// receives: there a released file that joins no reception would be left
// under a name nothing keeps (Incoming); serve gives the API the running
// pipelines instead.
type leaveForPipeline struct{}

// Release releases through the store alone.
func (leaveForPipeline) Release(ctx context.Context, _ string, store tenant.Pipeline, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	return store.ReleaseFile(ctx, rel, audit, guard)
}

// Retry retries through the store alone.
func (leaveForPipeline) Retry(ctx context.Context, _ string, store tenant.Pipeline, id, newReceptionID, receiver string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Reception, tenant.Reception, error) {
	return store.RetryReception(ctx, id, newReceptionID, receiver, now, audit, guard)
}

// RemoveDiscarded removes nothing.
func (leaveForPipeline) RemoveDiscarded(context.Context, string, string) error { return nil }

// RemoveRejected removes nothing.
func (leaveForPipeline) RemoveRejected(context.Context, string, string) error { return nil }

// MaxHeaderBytes is the longest header a quarantined file's answer holds,
// the contract's 1 MiB; a longer one is left out, never cut.
const MaxHeaderBytes = 1 << 20

// ErrNoHeader is what a Headers returns for a file whose header cannot be
// given: its bytes are not held, its data set cannot be read, or nothing on
// this server reads one. The answer then has no header and no too-large
// flag, which is what the contract answers for a header it cannot give.
var ErrNoHeader = errors.New("the file's header cannot be read")

// Headers reads the header of a quarantined file, which only the reception
// pipeline can: it owns the incoming area the file is held in and reads
// DICOM through the DICOM library.
type Headers interface {
	// Header returns the data set of the held file f of the tenant named
	// tenantName in the DICOM JSON model (PS3.18 Annex F), without its bulk
	// data: Pixel Data and every attribute with a value longer than 1 KiB
	// left out entirely, with no BulkDataURI and no InlineBinary. When the
	// result is longer than limit bytes it returns tooLarge and no text,
	// never a part of one. It returns ErrNoHeader for a file whose header
	// cannot be given, and any other error for a failure, which the request
	// answers as one.
	Header(ctx context.Context, tenantName string, f tenant.QuarantinedFile, limit int) (json string, tooLarge bool, err error)
}

// noHeaders is the Headers of a server whose reception pipeline does not
// run yet: no header can be read, so every request for one is answered
// without it.
type noHeaders struct{}

// Header returns ErrNoHeader.
func (noHeaders) Header(context.Context, string, tenant.QuarantinedFile, int) (string, bool, error) {
	return "", false, ErrNoHeader
}

// ErrNoHeldFile is what HeldFiles answers for a held file whose bytes
// cannot be read: they are not kept, or nothing on this server reads them.
var ErrNoHeldFile = errors.New("the held file's bytes cannot be read")

// HeldFiles reads a held file's bytes, as the sender sent them, for a
// trial run of a profile over it.
type HeldFiles interface {
	// HeldFile returns the bytes of the held file f of the tenant named
	// tenantName, at most limit bytes: a longer file is an error, never a
	// part of one. It returns ErrNoHeldFile for a file whose bytes cannot
	// be read, and any other error for a failure.
	HeldFile(ctx context.Context, tenantName string, f tenant.QuarantinedFile, limit int64) ([]byte, error)
}

// noHeldFiles is the HeldFiles of a server whose reception pipeline does
// not run: no held file can be read.
type noHeldFiles struct{}

// HeldFile returns ErrNoHeldFile.
func (noHeldFiles) HeldFile(context.Context, string, tenant.QuarantinedFile, int64) ([]byte, error) {
	return nil, ErrNoHeldFile
}

// Live answers what the server holds in memory of a tenant, for a status
// that must answer also when the tenant's database does not.
type Live interface {
	// TenantLive returns what is held of the tenant named tenantName, and
	// whether anything is.
	TenantLive(tenantName string) (TenantLive, bool)
}

// TenantLive is what the server holds in memory of one tenant: the
// receivers as the part that runs them last read them, the senders as the
// part that sends last read them, the space it last measured of the
// storages the receivers write to, and the pipeline's throughput.
type TenantLive struct {
	Receivers  []tenant.Receiver
	Senders    []tenant.Sender
	Storages   map[string]StorageSpace
	Throughput Throughput
}

// StorageSpace is a storage's free space as last measured, and whether it
// is below its reserve.
type StorageSpace struct {
	FreeBytes  int64
	NearlyFull bool
}

// Throughput is how fast files moved through a tenant over the last ten
// seconds, per second.
type Throughput struct {
	FilesReceived, BytesReceived, FilesArchived, BytesArchived float64
}

// noLive is the Live of a server that runs no receivers: nothing is held.
type noLive struct{}

// TenantLive implements Live.
func (noLive) TenantLive(string) (TenantLive, bool) { return TenantLive{}, false }
