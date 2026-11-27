package tenant

import (
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// A replicate job is how a replica's backfill is followed as a job: made by
// the attach that queued every scan of the primary for the replica (the
// server's own, Scheduled) or by a person's BackfillReplica, it counts the
// scans the backfill marked and ends done once the replica owes its
// backfill nothing more. It has no unit of work of its own: the replicator
// copies the replica's rows (Replication), and every change of the
// replica's backfill count moves the job, in the same transaction. A
// canceled job leaves the rows as they are: the copy is the replica's
// state, kept until it is in step, not the job's.

// ReplicateJob is what a replicate job copies.
type ReplicateJob struct {
	// Replica names the replica it copies to; a replica detached later
	// keeps its name here.
	Replica string
	// Primary names the replica's primary when the job was made.
	Primary string
	// Scheduled says the server made it when the replica was attached,
	// rather than a person.
	Scheduled bool
}

// ReplicaBackfill is what BackfillReplica asks.
type ReplicaBackfill struct {
	// Replica names the replica.
	Replica string
	// JobID is the id the replicate job takes when one is made.
	JobID string
	// Now is when it is asked.
	Now time.Time
}

// Validate returns an Invalid error unless b is well formed.
func (b ReplicaBackfill) Validate() error {
	if err := ValidateName("storage", b.Replica); err != nil {
		return err
	}
	if err := master.ValidateID("job id", b.JobID); err != nil {
		return err
	}
	return master.ValidateTime("time", b.Now)
}

// ReplicateJobID is the id of the replicate job an attach of replica at
// attached makes: derived from both, so a store makes it in the attach's
// transaction with no id from a caller, and an attach run again names the
// same job.
func ReplicateJobID(replica string, attached time.Time) string {
	return "rpl-" + digest(replica+"\x00"+strconv.FormatInt(attached.UnixMicro(), 10))
}

// BackfillCounts returns a replicate job's counts: the scans its backfill
// marked, and how many of them the replica no longer owes, given what it
// still owes (never below zero, since rows the backfill marked may leave
// the catalog with their scan).
func BackfillCounts(scans, owed int64) JobCounts {
	return JobCounts{Scans: scans, ScansDone: max(scans-owed, 0), Listed: true}
}

// StoredObject is one archived instance's object on a storage, as the
// catalog holds it: what a recall asks for and a sample answers.
type StoredObject struct {
	// Storage names the storage that holds it.
	Storage string
	// Path is its scan's labels; SeriesUID its scan.
	Path      label.Path
	SeriesUID string
	// SOPInstanceUID names the instance.
	SOPInstanceUID string
	// Key is its key on the storage; Size its size as archived;
	// ArchivedSHA256 the checksum of what was archived.
	Key            string
	Size           int64
	ArchivedSHA256 SHA256
}

// StorageObjectQuery names the objects StorageObjects answers.
type StorageObjectQuery struct {
	// Storage names the storage, which the tenant must have. For a primary
	// the objects of its scans are answered; for a replica, those of its
	// primary's scans that are in step on it at the catalog's manifest
	// digest now, the only ones a read or a recall finds there.
	Storage string
	// Path keeps the objects of a subject, a session of it or a scan of
	// that; the zero path keeps every object of the storage.
	Path label.Path
}

// Validate returns an Invalid error unless q is well formed.
func (q StorageObjectQuery) Validate() error {
	if err := ValidateName("storage", q.Storage); err != nil {
		return err
	}
	if q.Path == (label.Path{}) {
		return nil
	}
	return JobTarget{SubjectLabel: q.Path.Subject, SessionLabel: q.Path.Session, ScanLabel: q.Path.Scan}.Validate()
}
