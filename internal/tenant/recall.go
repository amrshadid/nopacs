package tenant

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/master"
)

// Recalls is what the tenant's database keeps of the recalls of objects
// from its archive-tier storages: one row per object, shared by every
// request and job waiting for it, so however many wait, one recall is
// asked of the store.
//
// A row is requested (recorded, the store not asked yet), in progress (the
// store took a recall and is working on it), online (the object reads, until
// OnlineUntil when the store said), or failed (the store refused it or did
// not complete it). Who asks: an operator through RequestRecalls with an
// audit entry, after a preview; a job unit that meets an object not online
// through RequestRecalls with none, the server's own. A reader's read starts
// none. The worker that asks the store reads the rows due (DueRecalls),
// asks, and records what the store said (RecordRecall), then polls the
// object until it is online.
//
// A row's key is the object's key on its storage, which holds a SOP
// Instance UID as received: the store keeps it in its own rows only, and no
// error, log or event carries it.
type Recalls interface {
	// RequestRecalls records a recall of each key of a.Keys on a.Storage,
	// which must be of the archive tier (a precondition failure otherwise),
	// all in one transaction with a.Audit when given. A key with no row, a
	// failed one, or one online past its OnlineUntil becomes requested, due
	// at a.Now, with a.Days and a.Speed; one requested or in progress is
	// joined, and one online is left as it is: nothing is ever asked twice.
	// It answers how many keys took each way.
	RequestRecalls(ctx context.Context, a RecallAsk) (RecallAsked, error)
	// Recall answers the row of the object at key on storage, and whether
	// there is one.
	Recall(ctx context.Context, storage, key string) (Recall, bool, error)
	// DueRecalls answers up to limit (1 to MaxRecallBatch) rows requested or
	// in progress whose due time is at or before now, earliest first: the
	// worker's next asks and polls.
	DueRecalls(ctx context.Context, now time.Time, limit int) ([]Recall, error)
	// RecordRecall records what the store said of a row requested or in
	// progress (NextRecall says how each answer moves it), and answers the
	// row after. A row not there is NotFound; one online or failed already is
	// a precondition failure.
	RecordRecall(ctx context.Context, a RecallAnswer) (Recall, error)
	// RecallCounts counts the rows by state, an online row past its
	// OnlineUntil at now not counted.
	RecallCounts(ctx context.Context, now time.Time) (RecallCounts, error)
}

// The bounds of recalls.
const (
	// MaxRecallBatch is the most keys one RequestRecalls names and the most
	// rows one DueRecalls answers.
	MaxRecallBatch = 1000
	// MaxRecallReason bounds the reason a failed recall keeps.
	MaxRecallReason = 256
)

// RecallState is where a recall is, the contract's RecallCounts and
// NOT_ONLINE's recall_state words.
type RecallState string

// The states.
const (
	RecallRequested  RecallState = "requested"
	RecallInProgress RecallState = "in_progress"
	RecallOnline     RecallState = "online"
	RecallFailed     RecallState = "failed"
)

// RecallStates returns every state, in the contract's order.
func RecallStates() []RecallState {
	return []RecallState{RecallRequested, RecallInProgress, RecallOnline, RecallFailed}
}

// Valid reports whether s is one of RecallStates.
func (s RecallState) Valid() bool { return slices.Contains(RecallStates(), s) }

// Recall is one object's recall.
type Recall struct {
	// Storage names the storage; Key is the object's key there.
	Storage, Key string
	// State is where it is.
	State RecallState
	// Days and Speed are what it asks of the store.
	Days  int
	Speed RecallSpeed
	// Requested is when it was last requested; Updated when it last moved;
	// Due when the worker next asks or polls.
	Requested, Updated, Due time.Time
	// OnlineUntil is, once online, when the store said the object goes
	// back to its cold class; zero when it did not say.
	OnlineUntil time.Time
	// Reason is why it failed, empty otherwise.
	Reason string
}

// Readable reports whether the object reads at now, as far as the recall
// knows: online, and not past OnlineUntil.
func (r Recall) Readable(now time.Time) bool {
	return r.State == RecallOnline && (r.OnlineUntil.IsZero() || now.Before(r.OnlineUntil))
}

// Live reports whether the recall is requested or in progress: a read of
// the object then carries a retry delay, since waiting brings it back.
func (r Recall) Live() bool { return r.State == RecallRequested || r.State == RecallInProgress }

// Request returns what the recall asks of a store, for blob.Recaller.
func (r Recall) Request() blob.RecallRequest {
	speed := blob.RecallStandard
	switch r.Speed {
	case RecallBulk:
		speed = blob.RecallBulk
	case RecallExpedited:
		speed = blob.RecallExpedited
	}
	return blob.RecallRequest{Days: r.Days, Speed: speed}
}

// RecallAsk is what RequestRecalls asks.
type RecallAsk struct {
	// Storage names the archive-tier storage; Keys the objects, 1 to
	// MaxRecallBatch, each once.
	Storage string
	Keys    []string
	// Days is how long each object stays readable, 1 to MaxRecallDays;
	// Speed how fast the store is asked (empty for RecallStandard).
	Days  int
	Speed RecallSpeed
	// Now is when it is asked.
	Now time.Time
	// Audit is the entry of an operator's recall, written with it; nil for
	// a job unit's, the server's own. Guard is its check, with it.
	Audit *master.AuditEntry
	Guard Guard
}

// Validate returns an Invalid error unless a is well formed.
func (a RecallAsk) Validate() error {
	if err := ValidateName("storage", a.Storage); err != nil {
		return err
	}
	if len(a.Keys) == 0 || len(a.Keys) > MaxRecallBatch {
		return invalid("a recall names 1 to " + strconv.Itoa(MaxRecallBatch) + " objects")
	}
	for i, k := range a.Keys {
		if err := ValidateFindingKey(k); err != nil {
			return err
		}
		if slices.Contains(a.Keys[:i], k) {
			return invalid("a recall names each object once")
		}
	}
	if a.Days < 1 || a.Days > MaxRecallDays {
		return invalid("a recall keeps an object readable from 1 to " + strconv.Itoa(MaxRecallDays) + " days")
	}
	if !a.Speed.Valid() {
		return invalid("a recall's speed is standard, bulk or expedited")
	}
	if a.Audit != nil {
		if err := a.Audit.Validate(); err != nil {
			return err
		}
	}
	return master.ValidateTime("time", a.Now)
}

// RecallAsked counts what RequestRecalls did with its keys.
type RecallAsked struct {
	// Requested counts the keys made requested; Joined those requested or
	// in progress already; Online those online already.
	Requested, Joined, Online int
}

// RecallAnswer is what a store said of a recall (RecordRecall).
type RecallAnswer struct {
	// Storage and Key name the row.
	Storage, Key string
	// State is where the answer puts it: RecallInProgress (the store took
	// it, or is still working), RecallOnline (it reads), RecallFailed (the
	// store refused it or did not complete it).
	State RecallState
	// OnlineUntil is, for RecallOnline, when the store said the object goes
	// back; zero when it did not say.
	OnlineUntil time.Time
	// Reason is, for RecallFailed, a phrase of the server's, at most
	// MaxRecallReason bytes.
	Reason string
	// Next is, for RecallInProgress, when the worker polls again.
	Next time.Time
	// Now is when the store answered.
	Now time.Time
}

// Validate returns an Invalid error unless a can be recorded.
func (a RecallAnswer) Validate() error {
	if err := ValidateName("storage", a.Storage); err != nil {
		return err
	}
	if err := ValidateFindingKey(a.Key); err != nil {
		return err
	}
	switch a.State {
	case RecallInProgress:
		if err := master.ValidateTime("next poll", a.Next); err != nil {
			return err
		}
	case RecallOnline:
		if !a.OnlineUntil.IsZero() {
			if err := master.ValidateTime("online until", a.OnlineUntil); err != nil {
				return err
			}
		}
	case RecallFailed:
		if err := validateText("recall failure reason", a.Reason, 1, MaxRecallReason); err != nil {
			return err
		}
	default:
		return invalid("a recall's answer is in_progress, online or failed")
	}
	return master.ValidateTime("time", a.Now)
}

// RecallStateOf returns the state a store's answer to a recall puts its row
// in: a recall started or running is in progress, an object that reads at
// once is online.
func RecallStateOf(s blob.RecallState) RecallState {
	if s == blob.RecallOnline {
		return RecallOnline
	}
	return RecallInProgress
}

// NextRecall returns r as answer a moves it, or a precondition failure for a
// row that is neither requested nor in progress: an answer moves only a
// recall the store was asked for.
func NextRecall(r Recall, a RecallAnswer) (Recall, error) {
	if !r.Live() {
		return r, Precondition("the recall is " + string(r.State) + "; only a recall requested or in progress takes the store's answer")
	}
	r.State, r.Updated, r.Reason, r.OnlineUntil = a.State, a.Now, "", time.Time{}
	switch a.State {
	case RecallInProgress:
		r.Due = a.Next
	case RecallOnline:
		r.OnlineUntil, r.Due = a.OnlineUntil, a.Now
	case RecallFailed:
		r.Reason, r.Due = a.Reason, a.Now
	}
	return r, nil
}

// RecallCounts are a tenant's recalls by state, the contract's RecallCounts
// but for the units waiting, which are the job's to count.
type RecallCounts struct {
	Requested, InProgress, Online, Failed int64
}

// ValidateRecallKey returns an Invalid error unless storage and key name an
// object a recall can.
func ValidateRecallKey(storage, key string) error {
	if err := ValidateName("storage", storage); err != nil {
		return err
	}
	return ValidateFindingKey(key)
}
