package s3

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// A purge on a bucket says what the bucket did. A DeleteObject on a key
// of a versioned bucket only adds a delete marker and every older version
// stays (docs.aws.amazon.com/AmazonS3/latest/userguide/DeletingObjectVersions.html),
// and a version under object lock cannot be deleted until its retention
// ends (.../object-lock.html). So the driver reads the bucket's versioning
// and object lock once, at its first purge, and then either deletes the
// key (an unversioned bucket: removed), or adds a delete marker (a
// versioned one with KeepVersions: hidden), or deletes every version and
// delete marker of the exact key by its version id, recording each
// (removed, or retained when the lock refuses). What it cannot read (no
// permission for the versioning or the versions) it answers unknown or
// hidden, never removed.

// purgeState is what a store learned of its bucket for purges.
type purgeState struct {
	mu sync.Mutex
	// read says the bucket was asked; versioning is its status ("" for
	// never enabled, "Enabled", "Suspended") and known whether it said;
	// lock whether object lock is enabled.
	read, known, lock bool
	versioning        string
}

// maxVersionPages bounds the listing of one key's versions.
const maxVersionPages = 1000

// bucketState reads, once, the bucket's versioning and object lock.
func (s *Store) bucketState(ctx context.Context) (versioning string, known, lock bool) {
	p := &s.purge
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.read {
		return p.versioning, p.known, p.lock
	}
	cctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	v, err := s.client.GetBucketVersioning(cctx, &awss3.GetBucketVersioningInput{Bucket: aws.String(s.set.bucket)})
	if err != nil {
		if ctx.Err() != nil {
			return "", false, false // Not learned: asked again by the next purge.
		}
		p.read = true
		return "", false, false
	}
	p.versioning, p.known = string(v.Status), true
	l, err := s.client.GetObjectLockConfiguration(cctx, &awss3.GetObjectLockConfigurationInput{Bucket: aws.String(s.set.bucket)})
	if err == nil && l.ObjectLockConfiguration != nil {
		p.lock = l.ObjectLockConfiguration.ObjectLockEnabled == types.ObjectLockEnabledEnabled
	}
	p.read = true
	return p.versioning, p.known, p.lock
}

// PurgeExpected implements blob.PurgeExpecter, from what the bucket says
// of its versioning and object lock.
func (s *Store) PurgeExpected(ctx context.Context) (blob.Disposition, string) {
	versioning, known, lock := s.bucketState(ctx)
	switch {
	case !known:
		return blob.Unknown, "bucket versioning could not be read"
	case versioning == "":
		return blob.Removed, "unversioned bucket"
	case s.set.keepOlder:
		return blob.Hidden, "versioned bucket, purge_versions off"
	case lock:
		return blob.Retained, "versioned bucket with object lock"
	}
	return blob.Removed, "versioned bucket, every version deleted"
}

// Purge removes key and says what the bucket did; see blob.Store and the
// comment above.
func (s *Store) Purge(ctx context.Context, key string) ([]blob.Removal, error) {
	if err := s.beginWrite(ctx, "the purge"); err != nil {
		return nil, err
	}
	if err := blob.CheckKey(key); err != nil {
		return nil, err
	}
	if !s.fits(key) {
		return nil, nil
	}
	versioning, known, lock := s.bucketState(ctx)
	if known && versioning != "" && !s.set.keepOlder {
		return s.purgeVersions(ctx, key, lock)
	}
	if _, err := s.Stat(ctx, key); err != nil {
		if fault.KindOf(err) == fault.NotFound {
			return s.purgeAbsent(ctx, key, versioning, known)
		}
		return nil, err
	}
	if err := s.Delete(ctx, key); err != nil {
		return nil, err
	}
	switch {
	case !known:
		return []blob.Removal{{Disposition: blob.Unknown}}, nil
	case versioning != "":
		return []blob.Removal{{Disposition: blob.Hidden}}, nil
	}
	return []blob.Removal{{Disposition: blob.Removed}}, nil
}

// purgeAbsent answers a purge of a key that holds nothing now, on a bucket
// whose versions are not deleted: nothing to purge on a bucket known
// unversioned; hidden when a versioned one keeps older versions of it (they
// stay, as KeepVersions asks); unknown when the bucket's versioning, or the
// key's versions, cannot be read. A purge before a stop, or a key never
// written, is then never claimed removed on a bucket that might keep it.
func (s *Store) purgeAbsent(ctx context.Context, key, versioning string, known bool) ([]blob.Removal, error) {
	if known && versioning == "" {
		return nil, nil
	}
	if !known {
		return []blob.Removal{{Disposition: blob.Unknown}}, nil
	}
	versions, err := s.versionsOf(ctx, key)
	if fault.KindOf(err) == fault.Unavailable && isRefused(err) {
		return []blob.Removal{{Disposition: blob.Unknown}}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, nil
	}
	return []blob.Removal{{Disposition: blob.Hidden}}, nil
}

// PurgeOlder implements blob.VersionPurger: every version and delete
// marker of key but the current one is deleted by its version id, on a
// versioned bucket whose versions are purged; a bucket that keeps no
// versions has none older; one whose versions are kept (KeepVersions)
// answers hidden for what it keeps; one whose versioning cannot be read
// answers unknown.
func (s *Store) PurgeOlder(ctx context.Context, key string) ([]blob.Removal, error) {
	if err := s.beginWrite(ctx, "the purge"); err != nil {
		return nil, err
	}
	if err := blob.CheckKey(key); err != nil {
		return nil, err
	}
	if !s.fits(key) {
		return nil, nil
	}
	versioning, known, lock := s.bucketState(ctx)
	switch {
	case !known:
		return []blob.Removal{{Disposition: blob.Unknown}}, nil
	case versioning == "":
		return nil, nil
	}
	versions, err := s.versionsOf(ctx, key)
	if fault.KindOf(err) == fault.Unavailable && isRefused(err) {
		return []blob.Removal{{Disposition: blob.Unknown}}, nil
	}
	if err != nil {
		return nil, err
	}
	var older []objectVersion
	for _, v := range versions {
		if !v.latest {
			older = append(older, v)
		}
	}
	if len(older) == 0 {
		return nil, nil
	}
	if s.set.keepOlder {
		return []blob.Removal{{Disposition: blob.Hidden}}, nil
	}
	out := make([]blob.Removal, 0, len(older))
	for _, v := range older {
		r, err := s.deleteVersion(ctx, key, v.id, lock)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// PurgeVersion implements blob.VersionPurger: the one version is deleted by
// its id; one the bucket no longer holds answers removed.
func (s *Store) PurgeVersion(ctx context.Context, key, version string) (blob.Removal, error) {
	if err := s.beginWrite(ctx, "the purge"); err != nil {
		return blob.Removal{}, err
	}
	if err := blob.CheckKey(key); err != nil {
		return blob.Removal{}, err
	}
	if version == "" || !s.fits(key) {
		return blob.Removal{}, fault.New(fault.Invalid, "a version purge names a version of a key of the store")
	}
	_, _, lock := s.bucketState(ctx)
	return s.deleteVersion(ctx, key, version, lock)
}

// deleteVersion deletes one version of key by its id and says what the
// bucket did: removed, retained (with its retention's end) when the object
// lock refused it, unknown when refused otherwise.
func (s *Store) deleteVersion(ctx context.Context, key, version string, lock bool) (blob.Removal, error) {
	cctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	_, err := s.client.DeleteObject(cctx, &awss3.DeleteObjectInput{Bucket: aws.String(s.set.bucket), Key: s.object(key),
		VersionId: aws.String(version)})
	cancel()
	if err == nil {
		return blob.Removal{Version: version, Disposition: blob.Removed}, nil
	}
	ferr := fail(ctx, err, "the purge of a version")
	if fault.KindOf(ferr) == fault.NotFound {
		return blob.Removal{Version: version, Disposition: blob.Removed}, nil
	}
	if !isRefused(ferr) {
		return blob.Removal{}, ferr
	}
	r := blob.Removal{Version: version, Disposition: blob.Unknown}
	if lock {
		r.Disposition, r.RetainUntil = blob.Retained, s.retainUntil(ctx, key, version)
	}
	return r, nil
}

// objectVersion is one version or delete marker of a key; latest says it
// is the key's current one.
type objectVersion struct {
	id     string
	latest bool
}

// versionsOf lists every version and delete marker of exactly key.
func (s *Store) versionsOf(ctx context.Context, key string) ([]objectVersion, error) {
	object := s.object(key)
	var out []objectVersion
	in := &awss3.ListObjectVersionsInput{Bucket: aws.String(s.set.bucket), Prefix: object}
	for range maxVersionPages {
		cctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
		page, err := s.client.ListObjectVersions(cctx, in)
		cancel()
		if err != nil {
			return nil, fail(ctx, err, "the listing of an object's versions")
		}
		for _, v := range page.Versions {
			if aws.ToString(v.Key) == *object {
				out = append(out, objectVersion{id: aws.ToString(v.VersionId), latest: aws.ToBool(v.IsLatest)})
			}
		}
		for _, m := range page.DeleteMarkers {
			if aws.ToString(m.Key) == *object {
				out = append(out, objectVersion{id: aws.ToString(m.VersionId), latest: aws.ToBool(m.IsLatest)})
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			return out, nil
		}
		in.KeyMarker, in.VersionIdMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	return nil, fault.New(fault.Unavailable, "the listing of an object's versions did not end")
}

// purgeVersions deletes every version and delete marker of key by its
// version id.
func (s *Store) purgeVersions(ctx context.Context, key string, lock bool) ([]blob.Removal, error) {
	versions, err := s.versionsOf(ctx, key)
	if fault.KindOf(err) == fault.Unavailable && isRefused(err) {
		// No permission to list versions: the key can still be hidden.
		if err := s.Delete(ctx, key); err != nil {
			return nil, err
		}
		return []blob.Removal{{Disposition: blob.Hidden}}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]blob.Removal, 0, len(versions))
	for _, v := range versions {
		r, err := s.deleteVersion(ctx, key, v.id, lock)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// isRefused reports whether err is the bucket refusing the request for
// want of permission, as it answers a version under object lock
// (AccessDenied).
func isRefused(err error) bool { return errors.Is(err, ErrRefused) }

// retainUntil asks when a version's retention ends; zero when the bucket
// does not say.
func (s *Store) retainUntil(ctx context.Context, key, version string) time.Time {
	cctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	r, err := s.client.GetObjectRetention(cctx, &awss3.GetObjectRetentionInput{Bucket: aws.String(s.set.bucket), Key: s.object(key),
		VersionId: aws.String(version)})
	if err != nil || r.Retention == nil || r.Retention.RetainUntilDate == nil {
		return time.Time{}
	}
	return r.Retention.RetainUntilDate.UTC()
}
