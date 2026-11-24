package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The bounds of one cleanup of unfinished uploads. A page is the most S3
// lists at once; ten pages bound the work of one call, at 10,000 uploads
// looked at and eleven requests at most, so Open takes a bounded time on a
// bucket of any size.
const (
	sweepPageSize = 1000
	sweepPages    = 10
)

// CleanupReport says what one cleanup of unfinished uploads did. A cleanup
// never fails Open: what went wrong is counted here.
type CleanupReport struct {
	// Seen is the uploads looked at under the storage's prefix, old or not.
	Seen int
	// Aborted is the old uploads that are gone: aborted by this call, or
	// already gone because another process aborted them first.
	Aborted int
	// Failed is the aborts that the server refused or failed. They are not
	// retried now; a later call looks again.
	Failed int
	// More is true when the bound of the call was reached before the end of
	// the uploads under the prefix: the uploads beyond are unseen until the
	// next call of AbortStaleUploads, which continues from where this one
	// stopped.
	More bool
	// Err is the failure to list the uploads, in words that are safe to log,
	// or nil. An account that may not list uploads gives one here, and then
	// depends on the bucket's lifecycle rule.
	Err error
}

// Whole reports whether the cleanup saw every upload under the prefix and
// every old one is gone: no failure to list, no bound reached with uploads
// unseen, no abort that failed. A caller that runs the cleanup once (at the
// first open of a storage in a process) runs it again at its next open
// until it is whole.
func (r CleanupReport) Whole() bool {
	return r.Err == nil && !r.More && r.Failed == 0
}

// Notes is what Open found and did not treat as a failure. It is read with
// Store.Notes.
type Notes struct {
	// Cleanup is Open's cleanup of unfinished uploads.
	Cleanup CleanupReport
	// WriteUnchecked is true when the credentials may not write, so the check
	// that the server confirms an upload's checksum could not be made: Put
	// will fail, if the server does not, until the credentials may write.
	WriteUnchecked bool
	// ProbeLeft is true when the small object Open wrote to check the server,
	// whose name no key of the port can have, could not be deleted.
	ProbeLeft bool
}

// Notes returns what Open found and did not treat as a failure.
func (s *Store) Notes() Notes { return s.notes }

// sweepState is where the last cleanup stopped, so the next continues there.
type sweepState struct {
	mu                  sync.Mutex
	started             bool
	bucketWide          bool
	keyMarker, uploadID *string
}

// reset forgets the position, so the next cleanup starts at the beginning.
// The caller holds mu.
func (st *sweepState) reset() {
	st.started, st.bucketWide, st.keyMarker, st.uploadID = false, false, nil, nil
}

// AbortStaleUploads aborts the unfinished multipart uploads under the
// storage's prefix that were begun more than Config.StaleUploadAge ago (a day
// when it is 0 or negative). It looks at no more than 10,000 uploads in a
// call, in pages of 1,000, and says in the report when more remain; the next
// call continues from where this one stopped, and starts again at the
// beginning after it reaches the end. Open calls it once, and a long-running
// server can call it now and then.
//
// It touches only uploads whose key is one this storage could have written,
// so a bucket shared with others is left as it is. Some servers (MinIO and
// its forks) match the prefix of an upload listing against the whole key and
// answer nothing for a prefix like ours; then the listing is the bucket's,
// begun at the place the prefix sorts (key-marker) and ended at the first
// key beyond it. A server that ignores the marker costs more pages.
//
// It does not fail: a listing that fails, an abort that fails and an upload
// that another process aborted first are counted in the report.
func (s *Store) AbortStaleUploads(ctx context.Context) CleanupReport {
	var rep CleanupReport
	if s.closed.Load() {
		rep.Err = closedError()
		return rep
	}
	age := s.set.staleAge
	if age <= 0 {
		age = DefaultStaleUploadAge
	}
	s.sweep.mu.Lock()
	defer s.sweep.mu.Unlock()
	st := &s.sweep
	prefix := s.set.prefix
	for page := 0; ; page++ {
		if page == sweepPages {
			rep.More = true
			return rep
		}
		if ctx.Err() != nil {
			rep.Err = canceled(ctx, "the cleanup")
			return rep
		}
		in := &awss3.ListMultipartUploadsInput{
			Bucket: aws.String(s.set.bucket), MaxUploads: aws.Int32(sweepPageSize),
			KeyMarker: st.keyMarker, UploadIdMarker: st.uploadID,
		}
		if prefix != "" && !st.bucketWide {
			in.Prefix = aws.String(prefix)
		}
		if st.bucketWide && st.keyMarker == nil {
			in.KeyMarker = aws.String(markerBefore(prefix))
		}
		lctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
		out, err := s.client.ListMultipartUploads(lctx, in)
		if err != nil {
			err = fail(lctx, err, "the cleanup of unfinished uploads")
			cancel()
			st.reset()
			rep.Err = err
			return rep
		}
		cancel()
		if prefix != "" && !st.started && !st.bucketWide && len(out.Uploads) == 0 && !aws.ToBool(out.IsTruncated) {
			// Nothing for the prefix: either there is nothing, or the server
			// does not match a prefix as a start. Ask for the bucket's uploads
			// from where the prefix sorts; if there is nothing the first
			// answer shows a key beyond it.
			st.bucketWide = true
			st.started = true
			continue
		}
		st.started = true
		end := s.sweepPage(ctx, out.Uploads, age, &rep)
		if end || !aws.ToBool(out.IsTruncated) {
			st.reset()
			return rep
		}
		st.keyMarker, st.uploadID = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// sweepPage looks at one page of uploads, aborting the old ones that are
// ours, and returns whether the listing has gone past the prefix.
func (s *Store) sweepPage(ctx context.Context, ups []types.MultipartUpload, age time.Duration, rep *CleanupReport) (past bool) {
	for _, u := range ups {
		k := aws.ToString(u.Key)
		if !strings.HasPrefix(k, s.set.prefix) {
			if s.sweep.bucketWide && k > s.set.prefix {
				return true // Keys are in order: nothing of ours is beyond.
			}
			continue
		}
		if _, ok := s.ours(k, false); !ok {
			continue
		}
		rep.Seen++
		if u.Initiated == nil || time.Since(*u.Initiated) < age {
			continue
		}
		switch err := s.abort(ctx, u.Key, u.UploadId); {
		case err == nil, isCode(err, "NoSuchUpload"):
			rep.Aborted++
		default:
			rep.Failed++
		}
	}
	return false
}

// markerBefore returns a key-marker that sorts before every key that begins
// with prefix and close to them: prefix with its last byte lowered by one.
func markerBefore(prefix string) string {
	if prefix == "" {
		return ""
	}
	b := []byte(prefix)
	b[len(b)-1]--
	return string(b)
}

// isCode reports whether err carries the S3 error code code.
func isCode(err error, code string) bool {
	var sc serverCode
	return errors.As(err, &sc) && string(sc) == code
}

// probeUpload checks, once at Open, that the server confirms an upload by
// echoing its checksum, as every Put depends on, by putting a small object
// and deleting it. The object's name begins with the reserved prefix, so no
// key of the port can name it and no listing shows it.
//
// A probe that writes is acceptable at Open because a server that does not
// echo would fail every Put, one by one, with an error that says less than
// this one; and one that cannot be written to is let through, with a note,
// since reading does not need it. The cost is one small PUT and one DELETE
// per Open.
func (s *Store) probeUpload(ctx context.Context) error {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fault.Wrap(fault.Internal, err, "opening the storage failed: no random bytes")
	}
	key := aws.String(s.set.prefix + blob.ReservedPrefix + "open-probe-" + hex.EncodeToString(rnd[:]))
	j := &putJob{s: s, key: key}
	pctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	err := j.putObject(pctx, []byte("nopacs open probe"))
	switch {
	case err == nil:
	case errors.Is(err, errNotConfirmed):
		s.deleteProbe(ctx, key)
		return fault.Wrap(fault.Invalid, errNotConfirmed, "opening the storage failed: the server does not return the checksum of an object it stores, which this driver needs to know an upload was kept")
	case isCode(err, "AccessDenied") || isCode(err, "AllAccessDisabled"):
		s.notes.WriteUnchecked = true
		return nil
	default:
		return openError(err)
	}
	s.deleteProbe(ctx, key)
	return nil
}

func (s *Store) deleteProbe(ctx context.Context, key *string) {
	dctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	if _, err := s.client.DeleteObject(dctx, &awss3.DeleteObjectInput{Bucket: aws.String(s.set.bucket), Key: key}); err != nil {
		s.notes.ProbeLeft = true
	}
}
