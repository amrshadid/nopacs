package tenant

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// Switching is what the tenant's database keeps of a move off a storage: the
// switch that makes a replica the primary, recorded when it is accepted
// with each step it reaches, and the one transaction that swaps the two
// storages' places.
//
// # The swap
//
// The primary's name, which the catalog, the receivers and the routes use,
// comes to name the replica's place, and the replica's name the primary's
// former place, still a replica of the primary: so no catalog row changes.
// SwapStorages does it in one transaction, which also refuses it unless
// every scan of the primary is in step on the replica at the catalog's
// manifest digest and no finding holds on the replica; moves with each place
// what belongs to the place (its owner record's generation, its open scrub
// findings, the marks those findings set on instances, which are lifted,
// since reads now go to the place the switch's checks read whole); raises
// both names' switch generation; records every scan of the primary in step
// on the replica's new place, which held the primary's bytes until the
// pause, but those its moved findings name, which are damaged there; and
// writes the switch's event and audit entry.
//
// # A restart
//
// A switch whose swap has not committed when its server stops is refused
// at the next start ("server restarted", RefuseSwitch): every write resumes
// on the old place. A swap that committed stands, and what follows it is
// finished (FinishSwitch). UnfinishedSwitches answers both kinds.
type Switching interface {
	// SwitchFacts reads what a switch of primary to replica depends on in
	// the tenant's database, for SwitchBlockers and for the confirm token:
	// both storages as stored, the scans of the primary not in step on the
	// replica, the findings holding on the replica, whether a correction of
	// the primary is prepared, whether a rebuild is queued or running, the
	// switch running if any, the receivers that pause, and when the
	// replica's backfill was owed nothing more. A storage the tenant does
	// not have is NotFound.
	SwitchFacts(ctx context.Context, primary, replica string) (SwitchFacts, error)
	// BeginSwitch records a switch accepted, under CheckSwitch's rules on
	// the facts read in its transaction, with its audit entry. A call whose
	// token a switch already used answers that switch and false, recording
	// nothing more but its audit entry; otherwise it answers the switch
	// made and true.
	BeginSwitch(ctx context.Context, s SwitchStart, audit master.AuditEntry, guard Guard) (StorageSwitch, bool, error)
	// SwitchStep records the step an accepted switch has reached. A switch
	// not accepted is a precondition failure.
	SwitchStep(ctx context.Context, id string, step SwitchStepName, now time.Time) (StorageSwitch, error)
	// SwapStorages swaps the places of an accepted switch's storages, as
	// Switching says, with audit (the server's), and answers the switch,
	// now swapped. A switch swapped or finished already is answered as it
	// stands; a refused one is a precondition failure with its refusal; a
	// rule that fails (among them a correction of the primary prepared, or
	// a rebuild queued, since the switch began, and an S3 place without its
	// secret sealed for its new name) changes nothing and leaves it
	// accepted, for the caller to refuse.
	SwapStorages(ctx context.Context, s SwitchSwap, audit master.AuditEntry) (StorageSwitch, error)
	// RefuseSwitch ends an accepted switch refused, with refusal (one of
	// SwitchRefusals), its event and audit (the server's), and answers it.
	// One refused already is answered as it stands; one swapped is a
	// precondition failure: a swap that committed stands.
	RefuseSwitch(ctx context.Context, id, refusal string, now time.Time, audit master.AuditEntry) (StorageSwitch, error)
	// FinishSwitch ends a swapped switch done and answers it; one done
	// already is answered as it stands, one not swapped is a precondition
	// failure.
	FinishSwitch(ctx context.Context, id string, now time.Time) (StorageSwitch, error)
	// Switch answers the switch of id, or NotFound.
	Switch(ctx context.Context, id string) (StorageSwitch, error)
	// UnfinishedSwitches answers the switches accepted or swapped, oldest
	// first: those a start refuses or finishes.
	UnfinishedSwitches(ctx context.Context) ([]StorageSwitch, error)
	// LastSwitch answers the switch of the storage named, as primary or as
	// replica, accepted last, and whether there is one: a status's
	// switching and last switch time.
	LastSwitch(ctx context.Context, storage string) (StorageSwitch, bool, error)
}

// SwitchState is where a switch is.
type SwitchState string

// The states.
const (
	// SwitchAccepted is a switch begun, its swap not committed.
	SwitchAccepted SwitchState = "accepted"
	// SwitchSwapped is a switch whose swap committed, what follows not yet
	// finished.
	SwitchSwapped SwitchState = "swapped"
	// SwitchDone is a switch made and finished.
	SwitchDone SwitchState = "done"
	// SwitchRefused is a switch refused before its swap.
	SwitchRefused SwitchState = "refused"
)

// Running reports whether a switch in s holds its storages: accepted or
// swapped.
func (s SwitchState) Running() bool { return s == SwitchAccepted || s == SwitchSwapped }

// SwitchStepName is a step an accepted switch reaches before its swap.
type SwitchStepName string

// The steps, in order.
const (
	SwitchPausing  SwitchStepName = "pausing"
	SwitchDraining SwitchStepName = "draining"
	SwitchOwning   SwitchStepName = "owning"
)

// The refusals of a switch, from the contract's fixed set of phrases.
const (
	RefusalRestarted       = "server restarted"
	RefusalNotInStep       = "replica not in step when drain_timeout passed"
	RefusalRecordsOwed     = "records owed to the replica when drain_timeout passed"
	RefusalOwnerNotTaken   = "the owner record of the replica's place could not be taken"
	RefusalStorageNoAnswer = "a storage did not answer"
	RefusalFindingHolds    = "a scrub finding holds on the replica"
)

// SwitchRefusals returns every refusal phrase.
func SwitchRefusals() []string {
	return []string{RefusalRestarted, RefusalNotInStep, RefusalRecordsOwed, RefusalOwnerNotTaken, RefusalStorageNoAnswer,
		RefusalFindingHolds}
}

// StorageSwitch is one switch of a primary to its replica.
type StorageSwitch struct {
	// ID names it, given by the caller.
	ID string
	// Primary and Replica name its storages, as they were named when it was
	// accepted.
	Primary, Replica string
	// State is where it is; Step the last step it reached before its swap.
	State SwitchState
	Step  SwitchStepName
	// Token is the confirm token it was accepted with, used once.
	Token string
	// Generation is the primary's switch generation when it was accepted.
	Generation int64
	// SkipVerification and SkipReason are the person's passing over of the
	// checks of the replica.
	SkipVerification bool
	SkipReason       string
	// KeyID is the key that asked for it; empty for the local operator.
	KeyID string
	// Accepted is when it was accepted; Swapped when its swap committed;
	// Ended when it was done or refused; each zero until then.
	Accepted, Swapped, Ended time.Time
	// Refusal is why it was refused, empty otherwise.
	Refusal string
	// ScansDrained, AssociationsRefused and Pause are its swap's counts.
	ScansDrained, AssociationsRefused int64
	Pause                             time.Duration
}

// SwitchStart is what BeginSwitch records.
type SwitchStart struct {
	// ID names the switch.
	ID string
	// Primary and Replica name the storages.
	Primary, Replica string
	// Token is the confirm token, 1 to MaxSwitchToken bytes.
	Token string
	// SkipVerification and SkipReason go together, the reason 1 to 1024
	// bytes.
	SkipVerification bool
	SkipReason       string
	// Now is when it is accepted.
	Now time.Time
}

// MaxSwitchToken bounds a switch's confirm token, the contract's.
const MaxSwitchToken = 128

// Validate returns an Invalid error unless s is well formed.
func (s SwitchStart) Validate() error {
	if err := master.ValidateID("switch id", s.ID); err != nil {
		return err
	}
	if err := ValidateName("storage", s.Primary); err != nil {
		return err
	}
	if err := ValidateName("storage", s.Replica); err != nil {
		return err
	}
	if s.Primary == s.Replica {
		return invalid("a switch names two storages")
	}
	if err := validateText("confirm token", s.Token, 1, MaxSwitchToken); err != nil {
		return err
	}
	if s.SkipVerification != (s.SkipReason != "") {
		return invalid("skip_verification and its reason go together")
	}
	if s.SkipVerification {
		if err := validateReason("skip reason", s.SkipReason); err != nil {
			return err
		}
	}
	return master.ValidateTime("time", s.Now)
}

// SwitchSwap is what SwapStorages records with the swap.
type SwitchSwap struct {
	// ID names the switch.
	ID string
	// ScansDrained, AssociationsRefused and Pause are the counts the
	// caller kept while the writes paused.
	ScansDrained, AssociationsRefused int64
	Pause                             time.Duration
	// PrimarySecret and ReplicaSecret are the sealed secret access keys
	// the two names hold after the swap when the place each comes to name
	// is an S3 bucket: the replica's secret sealed again for the primary's
	// name, the primary's for the replica's. A seal is bound to its
	// storage's name, so the sealed bytes cannot move with the place; the
	// database holds no key to seal them again, so the caller does
	// (place.SwapSecrets). Nil for a directory place. A swap that would
	// move an S3 place without its secret sealed for the new name is
	// refused, changing nothing.
	PrimarySecret, ReplicaSecret []byte
	// Now is when it swaps.
	Now time.Time
}

// Validate returns an Invalid error unless s is well formed.
func (s SwitchSwap) Validate() error {
	if err := master.ValidateID("switch id", s.ID); err != nil {
		return err
	}
	if s.ScansDrained < 0 || s.AssociationsRefused < 0 || s.Pause < 0 {
		return invalid("a switch's counts are not negative")
	}
	if len(s.PrimarySecret) > MaxSealedSecretLength || len(s.ReplicaSecret) > MaxSealedSecretLength {
		return invalid("a sealed secret access key is longer than the store keeps")
	}
	return master.ValidateTime("time", s.Now)
}

// RefusalPlaceNotResealed is the precondition failure of a swap that would
// move an S3 place to a name without the place's secret sealed for that
// name (SwitchSwap.PrimarySecret, ReplicaSecret).
const RefusalPlaceNotResealed = "an S3 place moves to a name without its secret access key sealed for that name"

// ValidateRefusal returns an Invalid error unless refusal is one of
// SwitchRefusals.
func ValidateRefusal(refusal string) error {
	if !slices.Contains(SwitchRefusals(), refusal) {
		return invalid("a switch's refusal is one of the server's phrases")
	}
	return nil
}

// SwitchFacts is what SwitchFacts reads.
type SwitchFacts struct {
	// Primary and Replica are the storages as stored.
	Primary, Replica Storage
	// ScansNotInStep counts the primary's scans not in step on the replica
	// at the catalog's manifest digest.
	ScansNotInStep int64
	// ReplicaFindings counts the scrub findings holding on the replica.
	ReplicaFindings int64
	// CorrectionPrepared says a correction with a scan on the primary is
	// prepared.
	CorrectionPrepared bool
	// RebuildRunning says a rebuild is queued or running.
	RebuildRunning bool
	// Running is the switch of the tenant accepted or swapped, if any.
	Running *StorageSwitch
	// Receivers names the receivers whose storage, or a route of whose,
	// is the primary: those that pause. Ordered by name.
	Receivers []string
	// BackfillDone is when the replica's backfill was owed nothing more;
	// zero while one is, or for a storage that is not a replica.
	BackfillDone time.Time
}

// The blockers of a switch the tenant's database knows, from the
// contract's fixed set of phrases (SwitchStorageResponse.blockers). Those
// of verification (a clean scrub, an empty compare) are the caller's to
// add, from the jobs.
const (
	BlockerNotReplica         = "the replica is not a replica of the primary"
	BlockerArchive            = "the replica is of the archive tier"
	BlockerCorrectionPrepared = "correction prepared"
	BlockerRebuild            = "a rebuild is queued or running"
	BlockerSwitch             = "another switch is running"
	BlockerFinding            = "a scrub finding holds on the replica"
)

// BlockerNotInStep returns the blocker of n scans not in step.
func BlockerNotInStep(n int64) string {
	return "replica not in step: " + strconv.FormatInt(n, 10) + " scans"
}

// SwitchBlockers returns, in a fixed order, what in f stands in the way of
// a switch starting now; none when it could. A replica not in step blocks
// a preview only: the switch's drain copies what is pending before its
// swap.
func SwitchBlockers(f SwitchFacts) []string {
	var out []string
	if f.Replica.ReplicaOf != f.Primary.Name {
		out = append(out, BlockerNotReplica)
	}
	if f.Replica.Tier.Archive() {
		out = append(out, BlockerArchive)
	}
	if f.ScansNotInStep > 0 {
		out = append(out, BlockerNotInStep(f.ScansNotInStep))
	}
	if f.ReplicaFindings > 0 {
		out = append(out, BlockerFinding)
	}
	if f.CorrectionPrepared {
		out = append(out, BlockerCorrectionPrepared)
	}
	if f.RebuildRunning {
		out = append(out, BlockerRebuild)
	}
	if f.Running != nil {
		out = append(out, BlockerSwitch)
	}
	return out
}

// CheckSwitch returns a precondition failure, its message the first
// blocker that holds, unless a switch may be accepted on f: every blocker
// of SwitchBlockers but a replica not in step and a finding on the replica,
// which the swap checks again after the drain.
func CheckSwitch(f SwitchFacts) error {
	for _, b := range SwitchBlockers(f) {
		if b == BlockerNotInStep(f.ScansNotInStep) || b == BlockerFinding {
			continue
		}
		return Precondition(b)
	}
	return nil
}

// StorageSwitched is a switch's end, the contract's message of that name: a
// replica made the primary, or a switch refused.
type StorageSwitched struct {
	// Primary and Replica name the storages.
	Primary string `json:"primary"`
	// Replica is the replica's name, on the former primary's place once
	// made.
	Replica string `json:"replica"`
	// ScansDrained counts the scans the drain copied before the swap.
	ScansDrained int64 `json:"scans_drained"`
	// AssociationsRefused counts the associations refused during the pause.
	AssociationsRefused int64 `json:"associations_refused"`
	// PauseMicros is how long the receivers refused new data, in
	// microseconds.
	PauseMicros int64 `json:"pause_us"`
	// Refused says the switch was refused, Refusal why.
	Refused bool   `json:"refused"`
	Refusal string `json:"refusal"`
	// SwitchGeneration is the primary's after the event.
	SwitchGeneration int64 `json:"switch_generation"`
}

// EventType implements EventPayload.
func (*StorageSwitched) EventType() EventType { return EventStorageSwitched }

// validate implements EventPayload.
func (p *StorageSwitched) validate() error {
	if err := ValidateName("storage", p.Primary); err != nil {
		return err
	}
	if err := ValidateName("storage", p.Replica); err != nil {
		return err
	}
	if p.ScansDrained < 0 || p.AssociationsRefused < 0 || p.PauseMicros < 0 || p.SwitchGeneration < 0 {
		return invalid("a switch's counts are not negative")
	}
	if p.Refused != (p.Refusal != "") {
		return invalid("a refused switch has its refusal, and only it")
	}
	if p.Refused {
		return ValidateRefusal(p.Refusal)
	}
	return nil
}

// SwitchEvent returns the event of switch s ending, at the time given, with
// the primary's generation after.
func SwitchEvent(s StorageSwitch, generation int64, at time.Time) (EventRecord, error) {
	return NewEvent(&StorageSwitched{Primary: s.Primary, Replica: s.Replica, ScansDrained: s.ScansDrained,
		AssociationsRefused: s.AssociationsRefused, PauseMicros: s.Pause.Microseconds(), Refused: s.State == SwitchRefused,
		Refusal: s.Refusal, SwitchGeneration: generation}, at)
}
