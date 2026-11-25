package blob

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// OnlineState is whether a storage can read an object at once. A storage
// that keeps every object at hand (a directory) answers Online for each; a
// bucket whose own lifecycle rule moves objects to a cold class answers
// Archived for one moved there, until a recall brings it back for a time.
// Neither Archived nor Restoring is missing or damaged: the object is held
// whole, and a Read of it fails with ErrNotOnline in its chain.
type OnlineState uint8

// The online states. Online is the zero value, so an Info that says nothing
// of it, as a driver that keeps every object at hand gives, reads as online.
const (
	// Online: a Read of the object answers at once. An object brought back
	// by a recall is Online until Info.OnlineUntil.
	Online OnlineState = iota
	// Archived: the object is in a cold class and must be recalled before a
	// Read answers.
	Archived
	// Restoring: a recall of the object is running; it is not readable yet.
	Restoring
)

// String returns the state's name in lowercase, as a log and a status show
// it.
func (s OnlineState) String() string {
	switch s {
	case Online:
		return "online"
	case Archived:
		return "archived"
	case Restoring:
		return "restoring"
	}
	return "unknown"
}

// MaxRecallDays is the most days a recall may keep an object readable. A day
// costs the site what its store charges, so a bound keeps a mistyped number
// from becoming a bill; a year is past any wait the archive has.
const MaxRecallDays = 365

// RecallSpeed is how fast, and at what cost, a recall is asked to run: the
// tiers a store with cold classes offers (the Tier of S3's RestoreObject,
// docs.aws.amazon.com/AmazonS3/latest/API/API_RestoreObject.html).
type RecallSpeed uint8

// The recall speeds. RecallStandard is the zero value.
const (
	// RecallStandard is the store's ordinary speed and price.
	RecallStandard RecallSpeed = iota
	// RecallBulk is the slowest and cheapest.
	RecallBulk
	// RecallExpedited is the fastest and dearest, where the store offers it
	// for the object's class; a store that does not refuses the recall.
	RecallExpedited
)

// RecallRequest is what a recall asks of a store.
type RecallRequest struct {
	// Days is how long the object stays readable once it is back: 1 to
	// MaxRecallDays.
	Days int
	// Speed is how fast the store is asked to bring it back.
	Speed RecallSpeed
}

// CheckRecall returns a fault.Invalid error for a request a driver must
// refuse before it touches anything: days out of range, a speed this
// version does not know.
func CheckRecall(req RecallRequest) error {
	if req.Days < 1 || req.Days > MaxRecallDays {
		return fault.New(fault.Invalid, "a recall keeps an object readable from 1 to 365 days")
	}
	if req.Speed > RecallExpedited {
		return fault.New(fault.Invalid, "a recall's speed is not one this version knows")
	}
	return nil
}

// RecallState is what a store said of a recall it was asked for.
type RecallState uint8

// The answers to a recall.
const (
	// RecallOnline: the object reads at once and nothing is left to wait
	// for. A store that keeps every object at hand answers it for each; one
	// that had brought the object back already keeps it readable for the
	// days asked from now.
	RecallOnline RecallState = iota
	// RecallStarted: the store took a new recall of the object; it reads
	// once the recall is done, which Stat tells (Restoring, then Online).
	RecallStarted
	// RecallInProgress: a recall of the object was already running; no
	// second one was asked.
	RecallInProgress
)

// String returns the answer's name in lowercase, as a log shows it.
func (s RecallState) String() string {
	switch s {
	case RecallOnline:
		return "online"
	case RecallStarted:
		return "started"
	case RecallInProgress:
		return "in progress"
	}
	return "unknown"
}

// Recaller is a Store that can bring an object back from a cold class for a
// time. Recall asks for key to be readable for req.Days, at req.Speed, and
// says what the store did. It asks no more than once for an object the
// store already brings back (RecallInProgress), and nothing for one already
// readable that was never in a cold class (RecallOnline). A key that holds
// nothing is NotFound; a bad key or request is Invalid, touching nothing.
// A store that refuses the speed for the object's class (no expedited
// recall for a deep class) fails as Unavailable with ErrRecallRefused in the
// chain.
//
// Every driver implements it, so a caller asks without knowing the kind of
// storage; a store that keeps every object at hand answers RecallOnline.
type Recaller interface {
	Recall(ctx context.Context, key string, req RecallRequest) (Recall, error)
}

// Recall is what a store answered to a recall.
type Recall struct {
	// State is what the store did.
	State RecallState
	// OnlineUntil is, when the store said, when the object goes back to its
	// cold class: for RecallOnline of an object a recall brought back. Zero
	// otherwise, and for an object that never leaves the online tier.
	OnlineUntil time.Time
}

// ErrRecallRefused is in the chain of the error of a recall the store would
// not take as asked: a speed it does not offer for the object's class, or
// a recall it has no capacity for now.
var ErrRecallRefused = errors.New("the storage would not take the recall as asked")

// EncryptionState is what a storage says of whether it encrypts what it
// holds. NoPACs does not encrypt image data itself: a storage does, or the
// volume under it, and this is the storage's own word on it, recorded and
// shown, never a claim of NoPACs.
type EncryptionState uint8

// The encryption states. EncryptionUnknown is the zero value: a store that
// cannot be asked says nothing.
const (
	// EncryptionUnknown: the storage could not be asked, or would not say
	// (credentials that may not read the setting, a server that does not
	// have it).
	EncryptionUnknown EncryptionState = iota
	// EncryptionNone: the storage says it does not encrypt new objects by
	// default.
	EncryptionNone
	// EncryptionOn: the storage says it encrypts new objects by default,
	// with Encryption.Algorithm.
	EncryptionOn
)

// String returns the state's name in lowercase, as a status shows it.
func (s EncryptionState) String() string {
	switch s {
	case EncryptionNone:
		return "none"
	case EncryptionOn:
		return "encrypted"
	}
	return "unknown"
}

// Encryption is what a storage said of its encryption at rest.
type Encryption struct {
	// State is what it said.
	State EncryptionState
	// Algorithm is, for EncryptionOn, the algorithm as the storage names it
	// ("AES256", "aws:kms", "aws:kms:dsse" for S3), or "other" for a name
	// this version does not know; "" otherwise. It is never the storage's
	// own text beyond the names known, so a hostile server cannot put its
	// words in a status.
	Algorithm string
	// KeyNamed is, for EncryptionOn, whether the setting names a key the
	// site manages (an S3 KMS key id); the key's name is never kept.
	KeyNamed bool
}

// EncryptionReporter is a Store that can ask its storage whether it
// encrypts at rest. A store that cannot (a directory, whose volume says
// nothing a process can trust) does not implement it, and its record is
// the statement a person made of it. Encryption answers EncryptionUnknown,
// not an error, when the storage will not say; an error is a storage that
// cannot be reached, a closed store, or an ended ctx.
type EncryptionReporter interface {
	Encryption(ctx context.Context) (Encryption, error)
}
