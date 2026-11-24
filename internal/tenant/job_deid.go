package tenant

import (
	"cmp"
	"slices"
	"strconv"
)

// DeidMode is what a deid job does with each archived instance, the
// contract's DeidJobMode in lowercase without its prefix.
type DeidMode string

// The modes.
const (
	// DeidDryRun runs a profile version over each archived file in memory,
	// under keys made for the job, and judges what it would archive and
	// what it would hold.
	DeidDryRun DeidMode = "dry_run"
	// DeidCheck verifies each archived file against the profile version
	// the catalog records for it.
	DeidCheck DeidMode = "check"
)

// Valid reports whether m is one of the modes.
func (m DeidMode) Valid() bool { return m == DeidDryRun || m == DeidCheck }

// DeidWorkKind is the kind of every unit of work of the tenant's deid
// jobs: one kind, since a deid job waits for no sender, so the server's
// workers of deid jobs claim the units of every deid job of the tenant.
const DeidWorkKind = "deid-jobs"

// JobWorkKind returns the kind of the units of work of j: SendKind of its
// sender for a send job, DeidWorkKind for a deid job, ScrubWorkKind for a
// scrub job.
func JobWorkKind(kind JobKind, sender string) string {
	switch kind {
	case JobDeid:
		return DeidWorkKind
	case JobScrub:
		return ScrubWorkKind
	case JobRebuild:
		return RebuildWorkKind
	}
	return SendKind(sender)
}

// validateDeidJob returns an Invalid error unless a deid job's sender,
// mode and profile can be made: no sender, a mode, and a profile version
// only for a dry run.
func validateDeidJob(sender string, mode DeidMode, p ProfileRef) error {
	if sender != "" {
		return invalid("a deid job names no sender")
	}
	if !mode.Valid() {
		return invalid("a deid job's mode is dry_run or check")
	}
	if p.IsZero() {
		return nil
	}
	if mode == DeidCheck {
		return invalid("a deid job's check verifies against the version each instance records, and names none")
	}
	return p.Validate()
}

// The bounds of a deid job's tallies.
const (
	// MaxJobTallies is the most keys a job keeps; the counts of keys past
	// it are added to the key TallyOther.
	MaxJobTallies = 1000
	// MaxJobTallyBatch is the most keys one progress write adds to.
	MaxJobTallyBatch = 512
	// MaxTallyKeyLength bounds a key.
	MaxTallyKeyLength = 160
	// TallyOther is the key of the counts of keys past MaxJobTallies.
	TallyOther = "other"
)

// JobTally is one count of what a deid job found, under a key of fixed
// words and a profile's names, never a value read from a file (the
// contract's JobTally says the keys).
type JobTally struct {
	// Key is what is counted.
	Key string
	// Count is how many.
	Count int64
}

// ValidateTallyKey returns an Invalid error unless key is 1 to
// MaxTallyKeyLength bytes of printable ASCII: the words, names, tags and
// UIDs a key is made of, so no file's text in another repertoire can reach
// one.
func ValidateTallyKey(key string) error {
	if key == "" || len(key) > MaxTallyKeyLength {
		return invalid("a tally's key is 1 to " + strconv.Itoa(MaxTallyKeyLength) + " bytes")
	}
	for i := range len(key) {
		if c := key[i]; c < 0x20 || c > 0x7e {
			return invalid("a tally's key is printable ASCII")
		}
	}
	return nil
}

// validateTallies returns an Invalid error unless every key of t is valid
// and every count at least 1, within MaxJobTallyBatch keys.
func validateTallies(t map[string]int64) error {
	if len(t) > MaxJobTallyBatch {
		return invalid("a progress write adds to at most " + strconv.Itoa(MaxJobTallyBatch) + " tallies")
	}
	for k, n := range t {
		if err := ValidateTallyKey(k); err != nil {
			return err
		}
		if n < 1 {
			return invalid("a tally a progress write adds to counts at least 1")
		}
	}
	return nil
}

// SortTallies orders tallies as a read answers them: most first, then by
// key.
func SortTallies(t []JobTally) {
	slices.SortFunc(t, func(a, b JobTally) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key))
	})
}

// UIDsSeen is what SeenUIDs found, each list in byte order.
type UIDsSeen struct {
	// Keyed are the keyed UIDs the catalog holds.
	Keyed []string
	// AsReceived are the plain UIDs it holds for an instance archived as
	// received, and Deidentified those it holds for one archived
	// de-identified.
	AsReceived, Deidentified []string
}

// MaxSeenUIDs is the most UIDs of each list one SeenUIDs call looks up.
const MaxSeenUIDs = 1000

// ValidateSeenUIDs returns an Invalid error unless a SeenUIDs call is
// well formed: at most MaxSeenUIDs of each list, each a UID.
func ValidateSeenUIDs(keyed, plain []string) error {
	if len(keyed) > MaxSeenUIDs || len(plain) > MaxSeenUIDs {
		return invalid("at most " + strconv.Itoa(MaxSeenUIDs) + " UIDs of each list are looked up at once")
	}
	for _, u := range append(slices.Clone(keyed), plain...) {
		if err := validateUID("UID", u); err != nil {
			return err
		}
	}
	return nil
}
