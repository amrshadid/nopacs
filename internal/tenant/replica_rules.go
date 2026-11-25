package tenant

import (
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// The rules of replicas and tiers (Replication), which every store applies
// in the transaction of the change they govern, on the facts it reads
// there.

// StorageTier is whether a storage reads every object at once, the
// contract's StorageTier in lowercase without its prefix. Its text is what a
// store keeps.
type StorageTier string

// The tiers.
const (
	// TierOnline reads every object at once. The empty tier is read as it.
	TierOnline StorageTier = "online"
	// TierArchive may hold objects it must first recall; for an S3 replica
	// only.
	TierArchive StorageTier = "archive"
)

// Archive reports whether t is the archive tier.
func (t StorageTier) Archive() bool { return t == TierArchive }

// RecallSpeed is how fast an archive-tier storage is asked to bring an
// object back, the contract's S3RecallTier in lowercase without its prefix.
// The empty speed is read as RecallStandard.
type RecallSpeed string

// The speeds.
const (
	RecallStandard  RecallSpeed = "standard"
	RecallBulk      RecallSpeed = "bulk"
	RecallExpedited RecallSpeed = "expedited"
)

// Valid reports whether s is a speed, the empty one included.
func (s RecallSpeed) Valid() bool {
	return s == "" || s == RecallStandard || s == RecallBulk || s == RecallExpedited
}

// DefaultRecallDays is how many days a recall keeps an object readable when
// the storage says 0, the contract's.
const DefaultRecallDays = 7

// MaxRecallDays is the most days a recall keeps an object readable.
const MaxRecallDays = 365

// validateReplica checks the fields of a storage that are about replicas
// and tiers: the tier and the recall fields within their bounds, the
// archive tier never for a directory, the declaration of encryption only
// for a directory, and a primary's name by the rule of a name.
func (s Storage) validateReplica() error {
	if s.Tier != "" && s.Tier != TierOnline && s.Tier != TierArchive {
		return invalid("a storage's tier is online or archive")
	}
	if s.Tier == TierArchive && s.Type == StorageDirectory {
		return invalid("a directory storage is always online; the archive tier is for an S3 storage")
	}
	if s.EncryptedAtRestDeclared && s.Type != StorageDirectory {
		return invalid("only a directory storage takes a declaration of encryption at rest; an S3 storage is asked")
	}
	if s.S3.RecallDays < 0 || s.S3.RecallDays > MaxRecallDays {
		return invalid("a storage's recall days are 0 to " + strconv.Itoa(MaxRecallDays))
	}
	if !s.S3.RecallSpeed.Valid() {
		return invalid("a storage's recall tier is standard, bulk or expedited")
	}
	if s.ReplicaOf != "" {
		if err := ValidateName("primary storage", s.ReplicaOf); err != nil {
			return err
		}
		if s.ReplicaOf == s.Name {
			return invalid("a storage is not a replica of itself")
		}
	}
	return nil
}

// normalizeTier returns s with its tier in the one form a store keeps (the
// empty tier as TierOnline) and its recall fields zero unless it is of the
// archive tier: the contract ignores them, and answers them zero, for an
// online storage.
func (s Storage) normalizeTier() Storage {
	if s.Tier == "" {
		s.Tier = TierOnline
	}
	if s.Tier != TierArchive {
		s.S3.RecallDays, s.S3.RecallSpeed = 0, ""
	}
	return s
}

// SharesBucket reports whether a and b are S3 storages on one endpoint and
// bucket (under prefixes that do not overlap, as two storages of a tenant
// must be): allowed for a replica, and shown, since a copy in the same
// bucket survives little that its primary would not.
func SharesBucket(a, b Storage) bool {
	return a.Type == StorageS3 && b.Type == StorageS3 && a.S3.Endpoint == b.S3.Endpoint && a.S3.Bucket == b.S3.Bucket
}

// AttachFacts is what a store reads, in the transaction of an attach (or of
// an AddStorage with ReplicaOf), for CheckAttach.
type AttachFacts struct {
	// Replica is the storage that becomes a replica, as stored (or as it is
	// added); Primary its primary, as stored.
	Replica, Primary Storage
	// PrimaryReplicas counts the primary's replicas now, the replica not
	// among them.
	PrimaryReplicas int
	// ReplicaHasReplicas says another storage names the replica as its
	// primary.
	ReplicaHasReplicas bool
	// ReplicaInUse says why the replica holds data (it holds an instance, a
	// session names it, a reception in progress archives to it), empty when
	// it holds none.
	ReplicaInUse string
	// ReplicaNamedBy names the receiver that, or a route of which, archives
	// to the replica; empty when none does.
	ReplicaNamedBy string
	// Switching says a switch of the primary is in progress.
	Switching bool
}

// CheckAttach returns a precondition failure, saying which rule, unless the
// facts allow the replica to be attached to the primary: the primary is not
// itself a replica and is online, with fewer than MaxReplicas replicas; the
// replica is not a replica of another storage, has no replicas of its own,
// holds no data, is named by no receiver or route, and does not overlap its
// primary; no switch of the primary is in progress. A replica already of
// this primary is the caller's to answer as it stands, before this.
func CheckAttach(f AttachFacts) error {
	r, p := f.Replica, f.Primary
	switch {
	case r.Name == p.Name:
		return Precondition("a storage is not a replica of itself")
	case p.ReplicaOf != "":
		return Precondition("the primary " + p.Name + " is itself a replica of " + p.ReplicaOf + "; a replica is not replicated")
	case p.Tier.Archive():
		return Precondition("the primary " + p.Name + " is of the archive tier; a primary reads every object at once")
	case r.ReplicaOf != "" && r.ReplicaOf != p.Name:
		return Precondition("the storage " + r.Name + " is a replica of " + r.ReplicaOf + "; a replica serves one primary")
	case f.ReplicaHasReplicas:
		return Precondition("the storage " + r.Name + " is a primary with replicas")
	case f.PrimaryReplicas >= MaxReplicas:
		return Precondition("the primary " + p.Name + " has " + strconv.Itoa(MaxReplicas) + " replicas already")
	case f.ReplicaInUse != "":
		return Precondition("the storage " + r.Name + " cannot become a replica while " + f.ReplicaInUse)
	case f.ReplicaNamedBy != "":
		return Precondition("the receiver " + f.ReplicaNamedBy + " archives to the storage " + r.Name + "; nothing is archived to a replica first")
	case f.Switching:
		return Precondition("a switch of the primary " + p.Name + " is in progress")
	case StoragesOverlap(r, p):
		return Precondition("the storage " + r.Name + " would overlap its primary " + p.Name)
	}
	return nil
}

// CheckArchiveTier returns a precondition failure for a storage of the
// archive tier that is not a replica: the reception, a relabel, a rebuild
// and a scrub read a primary at once.
func CheckArchiveTier(s Storage) error {
	if s.Tier.Archive() && s.ReplicaOf == "" {
		return Precondition("the archive tier is only for a replica; the storage " + s.Name + " is not one")
	}
	return nil
}

// CheckStorageChange returns a precondition failure unless a change from
// stored to next keeps the rules of replicas: the archive tier only on a
// replica, and where a replica's files are, or a primary's with replicas,
// never changed but by a switch (a replica holds its primary's keys, and a
// switch swaps places). locationChanged says the change moves where the
// files are; hasReplicas that another storage names stored as its primary.
func CheckStorageChange(stored, next Storage, locationChanged, hasReplicas bool) error {
	if next.Tier.Archive() && stored.ReplicaOf == "" {
		return Precondition("the archive tier is only for a replica; the storage " + stored.Name + " is not one")
	}
	if locationChanged && stored.ReplicaOf != "" {
		return Precondition("where a replica's files are changes only by a switch: the storage " + stored.Name + " is a replica of " + stored.ReplicaOf)
	}
	if locationChanged && hasReplicas {
		return Precondition("where a primary's files are changes only by a switch while it has replicas")
	}
	return nil
}

// CheckStorageRemoval returns a precondition failure for the removal of a
// replica (it is detached first) or of a primary with replicas.
func CheckStorageRemoval(stored Storage, hasReplicas bool) error {
	if stored.ReplicaOf != "" {
		return Precondition("the storage is a replica of " + stored.ReplicaOf + "; it is detached first")
	}
	if hasReplicas {
		return Precondition("the storage is the primary of a replica")
	}
	return nil
}

// CheckArchivedTo returns a precondition failure for a storage that data
// would be archived to first (a receiver's, a route's, a release's, a
// commit's) when it is a replica or of the archive tier; what, when not
// empty, is the field that names it, for the message.
func CheckArchivedTo(what, name, replicaOf string, tier StorageTier) error {
	if replicaOf != "" {
		return Precondition(Prefixed(what, "the storage "+name+" is a replica of "+replicaOf+"; nothing is archived to a replica first"))
	}
	if tier.Archive() {
		return Precondition(Prefixed(what, "the storage "+name+" is of the archive tier; nothing is archived to it first"))
	}
	return nil
}

// Prefixed returns message after the field that names what it is about and
// a colon, or message alone when field is empty.
func Prefixed(field, message string) string {
	if field == "" {
		return message
	}
	return field + ": " + message
}

// ReplicaDetach is what DetachReplica asks.
type ReplicaDetach struct {
	// Replica names the replica.
	Replica string
	// Abandon detaches it even while corrections count it among their
	// copies; Reason, the person's, is required with it and empty without.
	Abandon bool
	Reason  string
	// Now is when it is detached.
	Now time.Time
}

// Validate returns an Invalid error unless d is well formed.
func (d ReplicaDetach) Validate() error {
	if err := ValidateName("storage", d.Replica); err != nil {
		return err
	}
	if d.Abandon != (d.Reason != "") {
		return invalid("abandon_purges and its reason go together")
	}
	if d.Abandon {
		if err := validateReason("abandon reason", d.Reason); err != nil {
			return err
		}
	}
	return master.ValidateTime("time", d.Now)
}

// CheckDetach returns a precondition failure unless the replica may be
// detached: it is a replica, no switch of its primary is in progress, and
// no correction counts it among its copies (corrections, those committed or
// purging with scans on its primary) unless abandon.
func CheckDetach(replica Storage, corrections int, abandon, switching bool) error {
	switch {
	case replica.ReplicaOf == "":
		return Precondition("the storage " + replica.Name + " is not a replica")
	case switching:
		return Precondition("a switch of the primary " + replica.ReplicaOf + " is in progress")
	case corrections > 0 && !abandon:
		return Precondition(strconv.Itoa(corrections) + " committed or purging corrections count the replica among their copies; abandon_purges with a reason detaches it all the same")
	}
	return nil
}
