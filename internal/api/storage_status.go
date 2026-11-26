package api

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// StorageReports answers what a status says of a storage beyond the
// tenant's database: what the storage said of its encryption at rest when
// the server opened it, and how far scrubs keep up with it.
type StorageReports interface {
	// Encryption answers what the storage s of the tenant named tenantName
	// said of its encryption when the server opened it
	// (blob.EncryptionReporter), and false for a storage that cannot be
	// asked (a directory) or that the server has not opened.
	Encryption(ctx context.Context, tenantName string, s tenant.Storage) (blob.Encryption, bool)
	// Coverage answers the scrub coverage of the storage named storage of
	// the tenant named tenantName, whose store is st (scrub.Coverage over
	// the tenant's scrub.interval).
	Coverage(ctx context.Context, tenantName string, st tenant.Store, storage string) (StorageCoverage, error)
}

// StorageCoverage is how far scrubs keep up with one storage, as
// scrub.Coverage computes it.
type StorageCoverage struct {
	// Objects counts the objects it holds; Verified those read whole
	// within the interval, and Share their part of Objects (1 when it holds
	// none); PresenceOnly those checked by presence only.
	Objects, Verified, PresenceOnly int64
	Share                           float64
	// OldestUnverified is how long the object waiting longest has gone
	// unread; Overdue counts the objects past one interval.
	OldestUnverified time.Duration
	Overdue          int64
}

// encryptionMessage returns what a status says of a storage's encryption at
// rest: a directory's admin's statement, or what an S3 storage said, unknown
// when it said nothing or was not asked.
func encryptionMessage(s tenant.Storage, e blob.Encryption, asked bool) (v1.EncryptionAtRest, string) {
	if s.Type == tenant.StorageDirectory {
		if s.EncryptedAtRestDeclared {
			return v1.EncryptionAtRest_ENCRYPTION_AT_REST_DECLARED, ""
		}
		return v1.EncryptionAtRest_ENCRYPTION_AT_REST_NOT_DECLARED, ""
	}
	switch {
	case !asked:
		return v1.EncryptionAtRest_ENCRYPTION_AT_REST_UNKNOWN, ""
	case e.State == blob.EncryptionOn:
		return v1.EncryptionAtRest_ENCRYPTION_AT_REST_ENCRYPTED, e.Algorithm
	case e.State == blob.EncryptionNone:
		return v1.EncryptionAtRest_ENCRYPTION_AT_REST_NONE, ""
	}
	return v1.EncryptionAtRest_ENCRYPTION_AT_REST_UNKNOWN, ""
}

// coverageMessage returns a storage's scrub coverage as the contract
// writes it.
func coverageMessage(cv StorageCoverage) *v1.ScrubCoverage {
	return &v1.ScrubCoverage{Objects: cv.Objects, Verified: cv.Verified, Share: cv.Share, PresenceOnly: cv.PresenceOnly,
		OldestUnverified: durationpb.New(cv.OldestUnverified), Overdue: cv.Overdue}
}

// storageFacts fills what a status says of a storage beyond its space: its
// tier, its primary when a replica, its switch, its encryption at rest and
// its scrub coverage. A part that cannot be read is left out.
func (c *core) storageFacts(ctx context.Context, tenantName string, st tenant.Store, s tenant.Storage, m *v1.StorageStatus) {
	m.Tier, m.ReplicaOf, m.SwitchGeneration = tierMessage(s.Tier), s.ReplicaOf, s.SwitchGeneration
	if sw, ok := st.(tenant.Switching); ok {
		if last, found, err := sw.LastSwitch(ctx, s.Name); err == nil && found {
			m.Switching = last.State.Running() && last.Primary == s.Name
			m.LastSwitchTime = timestamp(last.Ended)
		}
	}
	var enc blob.Encryption
	asked := false
	if c.storageReports != nil {
		enc, asked = c.storageReports.Encryption(ctx, tenantName, s)
	}
	m.EncryptionAtRest, m.EncryptionAlgorithm = encryptionMessage(s, enc, asked)
	if c.storageReports != nil {
		if cv, err := c.storageReports.Coverage(ctx, tenantName, st, s.Name); err == nil {
			m.Coverage = coverageMessage(cv)
		}
	}
}

// replicaStatuses returns every replica of the tenant with how far it is
// from its primary, at now; nil when they cannot be read.
func replicaStatuses(ctx context.Context, st tenant.Store, now time.Time) []*v1.ReplicaStatus {
	rs, ok := st.(tenant.Replication)
	if !ok {
		return nil
	}
	var out []*v1.ReplicaStatus
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		items, next, err := rs.ListReplicas(ctx, "", page)
		if err != nil {
			return nil
		}
		for _, it := range items {
			out = append(out, replicaStatusMessage(it, now))
		}
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}
