package servecmd

import (
	"context"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/scrub"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// How long what a storage said of its encryption is kept before it is
// asked again: an answer for an hour, since a bucket's default encryption
// changes rarely and a status may be asked every few seconds by a screen;
// a failed ask for a minute, so a storage that does not answer is not
// asked by every status, and is asked again soon once it answers.
const (
	encryptionKept       = time.Hour
	encryptionFailedKept = time.Minute
	// encryptionAskTimeout bounds one open and ask, so a storage that does
	// not answer cannot hold a status.
	encryptionAskTimeout = 10 * time.Second
)

// storageReports is what a status says of a storage beyond the tenant's
// database (api.StorageReports): the encryption an S3 storage reported when
// opened (blob.EncryptionReporter, GetBucketEncryption), kept per storage
// for the row it was asked under, and the scrub coverage of a storage over
// the tenant's scrub.interval (scrub.Coverage).
type storageReports struct {
	// open opens a storage of the tenant named for reading.
	open func(ctx context.Context, tenantName string, s tenant.Storage) (blob.Store, error)
	// interval answers scrub.interval as it applies on st.
	interval func(ctx context.Context, st tenant.Store) (time.Duration, error)
	// now is the server's clock.
	now func() time.Time

	mu   sync.Mutex
	kept map[[2]string]keptEncryption
}

// keptEncryption is what one storage said of its encryption, for the row
// it was asked under, and until when it stands.
type keptEncryption struct {
	updated time.Time
	enc     blob.Encryption
	asked   bool
	until   time.Time
}

var _ api.StorageReports = (*storageReports)(nil)

// newStorageReports returns the server's storage reports: storages are
// opened for reading only, as the scrubs open them, so asking a storage
// what it encrypts writes nothing to it and removes nothing a crash left.
func newStorageReports(m master.Store, reg *registry.Registry, values settingvalue.Reader, keys place.Keys, now func() time.Time) *storageReports {
	opener := place.Opener{Values: values, Keys: keys, StaleUploadAge: -1}
	return &storageReports{
		open: func(ctx context.Context, tenantName string, s tenant.Storage) (blob.Store, error) {
			row, err := m.TenantByName(ctx, tenantName)
			if err != nil {
				return nil, err
			}
			st, err := reg.Reader(tenantName)
			if err != nil {
				return nil, err
			}
			return opener.OpenForReading(ctx, place.TenantOf(row.ID, tenantName, st), s)
		},
		interval: func(ctx context.Context, st tenant.Store) (time.Duration, error) {
			return values.OnTenant(st).ScrubInterval(ctx)
		},
		now: now,
	}
}

// Encryption answers what the S3 storage s said of its encryption when it
// was opened for this row, opening and asking it when nothing is kept for
// the row or what is kept is old. A directory is never asked: its record
// is the statement its admin made. A storage that cannot be opened or
// asked is answered as not asked, so its status says unknown.
func (r *storageReports) Encryption(ctx context.Context, tenantName string, s tenant.Storage) (blob.Encryption, bool) {
	if s.Type != tenant.StorageS3 {
		return blob.Encryption{}, false
	}
	key := [2]string{tenantName, s.Name}
	now := r.now()
	r.mu.Lock()
	k, ok := r.kept[key]
	r.mu.Unlock()
	if ok && k.updated.Equal(s.Updated) && now.Before(k.until) {
		return k.enc, k.asked
	}
	enc, err := r.ask(ctx, tenantName, s)
	k = keptEncryption{updated: s.Updated, enc: enc, asked: err == nil, until: now.Add(encryptionKept)}
	if err != nil {
		k.enc, k.until = blob.Encryption{}, now.Add(encryptionFailedKept)
	}
	if ctx.Err() == nil {
		r.mu.Lock()
		if r.kept == nil {
			r.kept = map[[2]string]keptEncryption{}
		}
		r.kept[key] = k
		r.mu.Unlock()
	}
	return k.enc, k.asked
}

// ask opens s for reading, asks what it encrypts and closes it.
func (r *storageReports) ask(ctx context.Context, tenantName string, s tenant.Storage) (blob.Encryption, error) {
	ctx, cancel := context.WithTimeout(ctx, encryptionAskTimeout)
	defer cancel()
	bs, err := r.open(ctx, tenantName, s)
	if err != nil {
		return blob.Encryption{}, err
	}
	defer func() { _ = bs.Close() }()
	rep, ok := bs.(blob.EncryptionReporter)
	if !ok {
		return blob.Encryption{}, fault.New(fault.Precondition, "the storage cannot say whether it encrypts")
	}
	return rep.Encryption(ctx)
}

// Coverage answers the scrub coverage of the storage named storage over
// the tenant's scrub.interval now, from when each of its scans was last
// read whole (tenant.VerificationSource). A tenant database that keeps no
// verifications has none to tell: the error leaves the status without it.
func (r *storageReports) Coverage(ctx context.Context, _ string, st tenant.Store, storage string) (api.StorageCoverage, error) {
	src, ok := st.(tenant.VerificationSource)
	if !ok {
		return api.StorageCoverage{}, fault.New(fault.Precondition, "the tenant's database keeps no scrub verifications")
	}
	interval, err := r.interval(ctx, st)
	if err != nil {
		return api.StorageCoverage{}, err
	}
	cv, err := scrub.Coverage(ctx, src, storage, interval, r.now())
	if err != nil {
		return api.StorageCoverage{}, err
	}
	return api.StorageCoverage{Objects: cv.Objects, Verified: cv.Verified, PresenceOnly: cv.PresenceOnly, Share: cv.Share,
		OldestUnverified: cv.OldestUnverified, Overdue: cv.Overdue}, nil
}
