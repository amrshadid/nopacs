package tenant

import (
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// RepairWorkKind is the kind of a repair job's one unit of work, queued in
// the transaction that makes the job: one kind for every repair job, so one
// worker of the tenant claims them as every other job's units are claimed.
const RepairWorkKind = "repair-jobs"

// MaxRepairFindings is the most findings a repair job names, the
// contract's.
const MaxRepairFindings = 1000

// RepairJob is what a repair job mends, the contract's RepairJob: a manual
// job names Findings or All; an automatic one (the tenant's setting
// replica.repair "auto") names neither, and mends every finding that marks
// an instance.
type RepairJob struct {
	// Storage names the primary it repairs.
	Storage string
	// From names the replica it reads from; empty for any replica in step
	// for each object's scan.
	From string
	// Findings are the ids of the findings it mends, at most
	// MaxRepairFindings, each once.
	Findings []string
	// All says it mends every finding that holds on the storage of a kind
	// a copy mends.
	All bool
	// Automatic says the server made it by itself.
	Automatic bool
}

// Validate returns an Invalid error unless r can be made.
func (r RepairJob) Validate() error {
	if err := ValidateName("storage", r.Storage); err != nil {
		return err
	}
	if r.From != "" {
		if err := ValidateName("replica", r.From); err != nil {
			return err
		}
	}
	named := len(r.Findings) > 0
	switch {
	case r.Automatic && (named || r.All):
		return invalid("an automatic repair names no findings and not all")
	case !r.Automatic && named == r.All:
		return invalid("a repair names findings or all, and not both")
	case len(r.Findings) > MaxRepairFindings:
		return invalid("a repair names at most " + strconv.Itoa(MaxRepairFindings) + " findings")
	}
	seen := make(map[string]bool, len(r.Findings))
	for _, id := range r.Findings {
		if err := master.ValidateID("finding id", id); err != nil {
			return err
		}
		if seen[id] {
			return invalid("a repair names each finding once")
		}
		seen[id] = true
	}
	return nil
}

// validateRepairJob returns an Invalid error unless a repair job's fields
// can be made: what it mends, and no sender, mode, profile, target,
// scheduled flag or rebuild.
func validateRepairJob(j NewJob) error {
	if j.Sender != "" || j.Mode != "" || !j.Profile.IsZero() || !j.Target.Whole() || j.Storage != "" || j.Scheduled || j.Rebuild != nil {
		return invalid("a repair job names what it mends alone")
	}
	if j.Repair == nil {
		return invalid("a repair job names its storage and what it mends")
	}
	return j.Repair.Validate()
}

// RepairOutcomeKind is what became of one finding a repair job met, among
// those its counts and tallies keep.
type RepairOutcomeKind string

// The outcomes a job counts, the contract's tally keys: repaired and
// already good count sent, no good copy counts refused.
const (
	// RepairRepaired is an object put back from a replica, its finding
	// cleared.
	RepairRepaired RepairOutcomeKind = "repaired"
	// RepairAlreadyGood is an object a new read found whole, its finding
	// cleared without a put.
	RepairAlreadyGood RepairOutcomeKind = "already_good"
	// RepairNoGoodCopy is an object no replica in step holds good, left as
	// it is (FailureDamagedInStorage, the detail "no good copy").
	RepairNoGoodCopy RepairOutcomeKind = "no_good_copy"
)

// Sent reports whether the outcome counts among a job's sent, rather than
// its refused.
func (k RepairOutcomeKind) Sent() bool { return k == RepairRepaired || k == RepairAlreadyGood }

// RepairOutcome is one finding's outcome, written under the job's unit.
type RepairOutcome struct {
	// Fence is the repair job's unit's lease.
	Fence WorkFence
	// Finding is the finding's id; Kind what became of it.
	Finding string
	Kind    RepairOutcomeKind
	// Bytes is the object's size, counted with it; 0 when unknown.
	Bytes int64
	// Now is when it is written.
	Now time.Time
}

// Validate returns an Invalid error unless o can be written.
func (o RepairOutcome) Validate() error {
	if err := o.Fence.Validate(); err != nil {
		return err
	}
	if err := master.ValidateID("finding id", o.Finding); err != nil {
		return err
	}
	if o.Kind != RepairRepaired && o.Kind != RepairAlreadyGood && o.Kind != RepairNoGoodCopy {
		return invalid("a repair's outcome is repaired, already_good or no_good_copy")
	}
	if o.Bytes < 0 {
		return invalid("an outcome's bytes are not negative")
	}
	return master.ValidateTime("outcome time", o.Now)
}
