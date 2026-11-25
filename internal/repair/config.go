package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Findings is the part of the tenant's database a job reads its findings
// from (tenant.Scrubbing, tenant.Repairs).
type Findings interface {
	ListScrubFindings(ctx context.Context, filter tenant.ScrubFindingFilter, page master.Page) ([]tenant.ScrubFinding, string, error)
	RepairFindings(ctx context.Context, ids []string) ([]tenant.ScrubFinding, error)
}

// Catalog is the part of the catalog a job reads the bytes expected from:
// a scan's manifest digest and Series Instance UID, an instance's key, size
// and SHA-256 (tenant.ScanReads, tenant.InstanceReads), and the storages'
// rows (tenant.Pipeline's Storage).
type Catalog interface {
	CatalogScan(ctx context.Context, path label.Path) (tenant.CatalogScan, error)
	HeldInstance(ctx context.Context, path label.Path, sopInstanceUID string) (tenant.ScanInstance, error)
	Storage(ctx context.Context, name string) (tenant.Storage, error)
}

// Replicas is the part of the tenant's database that says which replicas
// are in step for a scan and takes a replica's damage found on the way
// (tenant.Replication).
type Replicas interface {
	InStepReplicas(ctx context.Context, seriesUIDs []string) (map[string][]string, error)
	RecordReplicaDamage(ctx context.Context, d tenant.ReplicaDamageRecord) (bool, error)
}

// Repairs is the record of each repair and the job's counts
// (tenant.Repairs).
type Repairs interface {
	RecordRepair(ctx context.Context, r tenant.RepairRecord) (tenant.Repair, bool, error)
	ReleaseRepair(ctx context.Context, fence tenant.WorkFence, id string) error
	CompleteRepair(ctx context.Context, fence *tenant.WorkFence, id string, now time.Time, audit master.AuditEntry) (tenant.Repair, error)
	RecordRepairOutcome(ctx context.Context, o tenant.RepairOutcome) (tenant.Job, error)
	EndRepairJob(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error)
}

// Units is the part of the tenant's database that leases the job's unit of
// work and starts its job (tenant.Pipeline, tenant.JobWork).
type Units interface {
	ClaimWork(ctx context.Context, kind, owner string, now time.Time, lease time.Duration) (tenant.Work, error)
	RenewWork(ctx context.Context, fence tenant.WorkFence, now time.Time, lease time.Duration) (tenant.Work, error)
	FailWork(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Work, error)
	StartJobUnit(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.JobUnit, error)
}

// DB is every part of the tenant's database a job uses; a tenant.Store is
// one.
type DB interface {
	Findings
	Catalog
	Replicas
	Repairs
	Units
}

// Source is all a job does with a replica: read an object. A job holds a
// replica as this type, so a write to a replica does not compile in this
// package.
type Source interface {
	Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
}

// OpenFunc opens the tenant's storage of this name. The job closes what it
// opens.
type OpenFunc func(ctx context.Context, storage string) (blob.Store, error)

// WritePause is the gate every write to a tenant's storage asks before it
// lands bytes there (storageswitch.Gate), the shape of receive.WritePause.
// Enter waits while the storage is paused, then admits the write and
// returns its release, or answers why not: a switch of the storage to its
// replica holds new writes, waits for those admitted to release, and swaps
// the storage's place only then, so a put never lands on a place its
// storage's name no longer names.
type WritePause interface {
	// Enter admits a write to the storage named and returns its release,
	// called once after the write's last effect; an error refuses it.
	Enter(ctx context.Context, storage string) (func(), error)
}

// Mode is the tenant's setting replica.repair: whether a person starts each
// repair, or the server starts one by itself when scrubs find damage.
type Mode string

// The modes.
const (
	// ModeManual is the built-in default: damage is a person's to see
	// before the server writes to the storage again, and reads already
	// turn to a replica, so waiting loses nothing.
	ModeManual Mode = "manual"
	// ModeAuto lets the server start a repair by itself.
	ModeAuto Mode = "auto"
)

// ParseMode returns the mode text names; empty is ModeManual.
func ParseMode(text string) (Mode, error) {
	switch Mode(text) {
	case "", ModeManual:
		return ModeManual, nil
	case ModeAuto:
		return ModeAuto, nil
	}
	return "", fault.New(fault.Invalid, "replica.repair is manual or auto")
}

// Job is one repair job: the storage it repairs and what it mends, as its
// row in the jobs table holds it (tenant.Job's Repair).
type Job struct {
	// ID is the job's id, the one its row in the jobs table has.
	ID string
	// Storage names the primary it repairs.
	Storage string
	// From names the replica it reads from; empty for any replica in step
	// for each object's scan.
	From string
	// Mode is how the job was made. A manual job mends Findings, or every
	// mendable finding with All; an automatic one mends every finding that
	// marks an instance, and names none of them.
	Mode Mode
	// Findings are ids of findings to mend, at most MaxFindings; the job
	// reads these alone.
	Findings []string
	// All mends every finding that holds on the storage, of a kind a copy
	// mends (MendedKinds).
	All bool
}

// MaxFindings is the most findings a job names, the contract's.
const MaxFindings = tenant.MaxRepairFindings

// NewJob returns the job's row to make (tenant.Jobs.CreateJob), made at
// created. The row queues the job's one unit, which a Run claims.
func (j Job) NewJob(created time.Time) tenant.NewJob {
	return tenant.NewJob{ID: j.ID, Kind: tenant.JobRepair, Created: created, Repair: &tenant.RepairJob{Storage: j.Storage, From: j.From,
		Findings: j.Findings, All: j.All, Automatic: j.Mode == ModeAuto}}
}

// jobOf returns the repair job a row of the jobs table holds.
func jobOf(t tenant.Job) (Job, error) {
	if t.Kind != tenant.JobRepair || t.Repair == nil {
		return Job{}, fault.New(fault.Internal, "repair: the unit's job is not a repair job")
	}
	j := Job{ID: t.ID, Storage: t.Repair.Storage, From: t.Repair.From, Mode: ModeManual, Findings: t.Repair.Findings, All: t.Repair.All}
	if t.Repair.Automatic {
		j.Mode = ModeAuto
	}
	return j, j.Validate()
}

// MendedKinds returns the kinds of finding a repair mends by putting an
// object back: an instance's object missing or damaged, and a scan's
// manifest damaged. A finding that a scan's manifest disagrees with the
// catalog is not among them: a replica in step holds the same manifest, so
// no copy mends it.
func MendedKinds() []tenant.ScrubFindingKind {
	return []tenant.ScrubFindingKind{tenant.FindingMissing, tenant.FindingDamaged, tenant.FindingManifestDamaged}
}

// autoKinds are the kinds an automatic job mends: those that mark an
// instance, which every read of it fails on.
func autoKinds() []tenant.ScrubFindingKind {
	return []tenant.ScrubFindingKind{tenant.FindingMissing, tenant.FindingDamaged}
}

// Validate returns an Invalid error unless j can run.
func (j Job) Validate() error {
	if err := master.ValidateID("job id", j.ID); err != nil {
		return err
	}
	if err := tenant.ValidateName("storage", j.Storage); err != nil {
		return err
	}
	if j.From != "" {
		if err := tenant.ValidateName("replica", j.From); err != nil {
			return err
		}
	}
	named := len(j.Findings) > 0
	switch j.Mode {
	case ModeAuto:
		if named || j.All {
			return fault.New(fault.Invalid, "an automatic repair names no findings and not all")
		}
	case ModeManual:
		if named == j.All {
			return fault.New(fault.Invalid, "a repair names findings or all, and not both")
		}
	default:
		return fault.New(fault.Invalid, "a repair's mode is manual or auto")
	}
	if len(j.Findings) > MaxFindings {
		return fault.New(fault.Invalid, "a repair names at most "+strconv.Itoa(MaxFindings)+" findings")
	}
	for _, id := range j.Findings {
		if err := master.ValidateID("finding id", id); err != nil {
			return err
		}
	}
	return nil
}

// kinds returns the kinds of finding the job walks.
func (j Job) kinds() []tenant.ScrubFindingKind {
	if j.Mode == ModeAuto {
		return autoKinds()
	}
	return MendedKinds()
}

// AuditID returns the id of the audit entry of the repair of that id: made
// from it, so an end sent again names the same entry.
func AuditID(repairID string) string { return "rpa-" + hashOf(repairID)[:40] }

// hashOf returns the SHA-256 of s in lowercase hex.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// CompleteMethod is the method of the audit entry the server writes for
// each object it repaired.
const CompleteMethod = "repair.object"

// The defaults of a Config's bounds.
const (
	// DefaultLease is how long a claim holds the unit; a timer renews it
	// every third of it, whatever the job is doing.
	DefaultLease = time.Minute
	// DefaultObjectTimeout bounds one read or put of an object, so a
	// storage that hangs cannot hold the unit for ever; a put is cut sooner
	// still, before its lease's due (Config.PutMargin).
	DefaultObjectTimeout = 30 * time.Minute
	// DefaultAttempts is the attempts the unit has before it is held as
	// failed for a person.
	DefaultAttempts = tenant.DefaultWorkAttempts
	// DefaultPutMargin is how long before its lease's due every put to the
	// primary is cut: room for the worker's clock running behind the
	// database's.
	DefaultPutMargin = 15 * time.Second
)

// opTimeout bounds one write to the database.
const opTimeout = 30 * time.Second

// releaseTimeout bounds the release of a repair's hold, made on the way out
// of an object even when the run's context has ended.
const releaseTimeout = 10 * time.Second

// findingsPage is how many findings one page of the walk reads.
const findingsPage = 200

// copyBuffer is what one stream holds of an object at a time.
const copyBuffer = 256 << 10

// Config is what a Runner is made with.
type Config struct {
	// DB is the tenant's database.
	DB DB
	// Tenant is the tenant's name, for the audit entries.
	Tenant string
	// Open opens a storage by name.
	Open OpenFunc
	// Now is the clock every time the job records comes from.
	Now func() time.Time
	// Owner names this worker in its claims.
	Owner string
	// Lease, Attempts and ObjectTimeout are the bounds above; zero takes
	// the default.
	Lease         time.Duration
	Attempts      int
	ObjectTimeout time.Duration
	// PutMargin is how long before the due of the unit's lease each put to
	// the primary is cut, whatever ObjectTimeout allows: a repair's hold of
	// its scan runs out tenant.RepairHoldGrace after that due, and a put must
	// have ended by then, by the database's clock. Zero takes
	// DefaultPutMargin; it is less than Lease, so a put has time at all. An
	// object a put cannot carry within a lease (less this margin) is left
	// for the next attempt; a longer Lease carries it.
	PutMargin time.Duration
	// Pause, when not nil, is entered before each put to the primary and
	// released after it: a switch of the storage waits for a put admitted,
	// and a put that comes while the storage is switched waits, or is
	// refused and tried again at the unit's next attempt. Nil admits every
	// put.
	Pause WritePause
	// Outcome, when set, is told the outcome of each finding the job met.
	// The job's counts and tallies are written to its row whatever it is.
	Outcome func(Outcome)
	// Log receives what the job does; nil discards it.
	Log *slog.Logger
}

// Runner runs repair jobs of one tenant.
type Runner struct {
	cfg Config
	log *slog.Logger
}

// New returns a runner over cfg, its zero bounds given their defaults.
func New(cfg Config) (*Runner, error) {
	if cfg.DB == nil || cfg.Open == nil || cfg.Now == nil {
		return nil, fault.New(fault.Invalid, "repair: the database, the opener and the clock are required")
	}
	if cfg.Owner == "" {
		return nil, fault.New(fault.Invalid, "repair: the runner needs an owner name for its claims")
	}
	if err := tenant.ValidateName("tenant", cfg.Tenant); err != nil {
		return nil, err
	}
	if cfg.Lease == 0 {
		cfg.Lease = DefaultLease
	}
	if cfg.Attempts == 0 {
		cfg.Attempts = DefaultAttempts
	}
	if cfg.ObjectTimeout == 0 {
		cfg.ObjectTimeout = DefaultObjectTimeout
	}
	if cfg.ObjectTimeout < 0 {
		return nil, fault.New(fault.Invalid, "repair: the bound of one object's read or put is negative")
	}
	if err := tenant.ValidateLease(time.Unix(0, 0).UTC(), cfg.Lease); err != nil {
		return nil, err
	}
	if cfg.PutMargin == 0 {
		cfg.PutMargin = DefaultPutMargin
	}
	if cfg.PutMargin < 0 || cfg.PutMargin >= cfg.Lease {
		return nil, fault.New(fault.Invalid, "repair: the margin a put ends by before its lease's due is positive and less than the lease")
	}
	if cfg.Attempts < 0 || cfg.Attempts > tenant.MaxWorkAttempts {
		return nil, fault.New(fault.Invalid, "repair: a unit has 1 to "+strconv.Itoa(tenant.MaxWorkAttempts)+" attempts")
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{cfg: cfg, log: log}, nil
}

// OutcomeKind is what became of one finding.
type OutcomeKind string

// The outcomes, the contract's tallies of a repair job where it names one.
const (
	// Repaired is an object put back from a replica and its finding
	// cleared.
	Repaired OutcomeKind = "repaired"
	// AlreadyGood is an object a new read found whole, whose finding is
	// cleared without a put.
	AlreadyGood OutcomeKind = "already_good"
	// NoGoodCopy is an object no replica in step holds good: left as it is,
	// refused (tenant.FailureDamagedInStorage, the detail "no good copy").
	NoGoodCopy OutcomeKind = "no_good_copy"
	// Skipped is a finding the job does not mend now: cleared or moved
	// meanwhile, of a kind a copy does not mend, or naming a key the
	// catalog no longer names whole.
	Skipped OutcomeKind = "skipped"
	// Retry is a finding a storage or the database did not let the job
	// finish; the unit is tried again.
	Retry OutcomeKind = "retry"
)

// Outcome is what became of one finding.
type Outcome struct {
	// Finding is the finding's id; Kind what became of it.
	Finding string
	Kind    OutcomeKind
	// Bytes is the size of the object put, for Repaired.
	Bytes int64
	// Reason is a phrase of the server's saying why, for every kind but
	// Repaired and AlreadyGood.
	Reason string
}

// Result is what one Run did.
type Result struct {
	// Job is the id of the job whose unit the Run claimed; empty when none
	// was due.
	Job string
	// Counts counts the findings met by what became of each.
	Counts map[OutcomeKind]int64
	// Bytes is the bytes put back.
	Bytes int64
	// Done says the job ended with its unit: every finding was met and none
	// is to try again. A job canceled before its unit ran is Done with
	// nothing counted.
	Done bool
}
